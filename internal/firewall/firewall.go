package firewall

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/nft"
)

type Options struct {
	Backend string

	IptablesChain string
	NftablesTable string
	Gateways      []string

	Rules  []config.Rule
	Rules6 []config.Rule
}

type MappedSubnet struct {
	Uplink string
	Prefix string
}

func (o Options) Mark(ifName string) int {
	for i, name := range o.Gateways {
		if name == ifName {
			return i + 1
		}
	}
	return 0
}

type MarkRule struct {
	proto string
	ports string
	mark  int

	saddr string
}

func (o Options) DesiredMarkRules() []MarkRule {
	return o.markRules(o.Rules, func(string) []string { return []string{""} })
}

func (o Options) DesiredMarkRules6(mapped []MappedSubnet) []MarkRule {
	return o.markRules(o.Rules6, func(gateway string) []string {
		var out []string
		for _, m := range mapped {
			if m.Uplink == gateway {
				out = append(out, m.Prefix)
			}
		}
		return out
	})
}

func (o Options) markRules(rules []config.Rule, sources func(gateway string) []string) []MarkRule {
	var out []MarkRule
	for _, rule := range rules {
		mark := o.Mark(rule.Gateway)
		if mark == 0 {
			slog.Warn("no mark for gateway, skipping port rule", "gateway", rule.Gateway)
			continue
		}
		if len(rule.MatchDstPort) == 0 {
			continue
		}

		ports := make([]string, len(rule.MatchDstPort))
		for i, p := range rule.MatchDstPort {
			ports[i] = strconv.Itoa(p)
		}
		portSet := strings.Join(ports, ",")

		for _, saddr := range sources(rule.Gateway) {
			for _, proto := range protocolsFor(rule.MatchProtocol) {
				out = append(out, MarkRule{proto: proto, ports: portSet, mark: mark, saddr: saddr})
			}
		}
	}
	return out
}

func protocolsFor(proto string) []string {
	switch proto {
	case "tcp":
		return []string{"tcp"}
	case "udp":
		return []string{"udp"}
	default:
		return []string{"tcp", "udp"}
	}
}

func sameMarkRules(want, got []MarkRule) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

type nftFamily struct {
	name   string
	legacy string
	chains []string
}

var (
	nftV4 = nftFamily{
		name:   "ip",
		legacy: "inet",
		chains: []string{"mangle_output", "mangle_prerouting"},
	}

	nftV6 = nftFamily{
		name:   "ip6",
		chains: []string{"mangle_prerouting"},
	}
)

func (f nftFamily) tables() []string {
	if f.legacy == "" {
		return []string{f.name}
	}
	return []string{f.name, f.legacy}
}

func (r MarkRule) nftLine() string {
	line := fmt.Sprintf("%s dport { %s } meta mark set %d",
		r.proto, strings.ReplaceAll(r.ports, ",", ", "), r.mark)
	if r.saddr == "" {
		return line
	}
	return "ip6 saddr " + r.saddr + " " + line
}

func nftBody(rules []MarkRule) string {
	lines := make([]string, len(rules))
	for i, r := range rules {
		lines[i] = r.nftLine()
	}
	return strings.Join(lines, "\n    ")
}

func (o Options) NftRuleset() string {
	rules := o.DesiredMarkRules()
	if len(rules) == 0 {
		return ""
	}

	body := nftBody(rules)
	return fmt.Sprintf(`table ip %s {
  chain mangle_output {
    type route hook output priority mangle; policy accept;
    %s
  }

  chain mangle_prerouting {
    type filter hook prerouting priority mangle; policy accept;
    %s
  }
}`, o.NftablesTable, body, body)
}

func (o Options) NftRuleset6(mapped []MappedSubnet) string {
	rules := o.DesiredMarkRules6(mapped)
	if len(rules) == 0 {
		return ""
	}

	return fmt.Sprintf(`table ip6 %s {
  chain mangle_prerouting {
    type filter hook prerouting priority mangle; policy accept;
    %s
  }
}`, o.NftablesTable, nftBody(rules))
}

func parseNftMarkRules(out string) map[string][]MarkRule {
	got := map[string][]MarkRule{}
	chain := ""

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "chain" {
			chain = fields[1]
			continue
		}
		if chain == "" || len(fields) == 0 {
			continue
		}

		saddr := ""
		if len(fields) >= 3 && fields[0] == "ip6" && fields[1] == "saddr" {
			saddr, fields = fields[2], fields[3:]
		}
		if len(fields) == 0 || (fields[0] != "tcp" && fields[0] != "udp") {
			continue
		}

		ports := betweenFields(fields, "dport", "meta")
		markStr := command.FieldAfter(fields, "set")
		mark, err := strconv.ParseInt(strings.TrimSuffix(markStr, ";"), 0, 32)
		if ports == "" || err != nil {
			continue
		}
		got[chain] = append(got[chain],
			MarkRule{proto: fields[0], ports: ports, mark: int(mark), saddr: saddr})
	}
	return got
}

func betweenFields(fields []string, from, to string) string {
	start := -1
	for i, f := range fields {
		if f == from {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(fields)
	for i := start; i < len(fields); i++ {
		if fields[i] == to {
			end = i
			break
		}
	}
	joined := strings.Join(fields[start:end], "")
	joined = strings.Trim(joined, "{}")
	return joined
}

type InstalledNft struct {
	Present bool
	Chains  map[string][]MarkRule
	Legacy  bool
}

func (o Options) nftQueries(f nftFamily) []command.Query {
	var qs []command.Query
	if f.legacy != "" {
		qs = append(qs, o.nftTableQuery(f.legacy).Query)
	}
	return append(qs, o.nftTableQuery(f.name).Query)
}

func (o Options) readInstalledNft(f nftFamily, a command.Answers) InstalledNft {
	var got InstalledNft
	if f.legacy != "" {
		got.Legacy = a.OK(o.nftTableQuery(f.legacy).Query)
	}
	if chains, err := o.nftTableQuery(f.name).Value(a); err == nil {
		got.Present, got.Chains = true, chains
	}
	return got
}

func (o Options) nftNeedsRestore(f nftFamily, want []MarkRule, got InstalledNft) bool {
	if got.Legacy {
		return true
	}
	if !got.Present {
		return len(want) > 0
	}
	if len(want) == 0 {
		return true
	}
	for _, chain := range f.chains {
		if !sameMarkRules(want, got.Chains[chain]) {
			return true
		}
	}
	return false
}

func (o Options) nftActions(f nftFamily, ruleset string) []command.Action {
	if ruleset == "" {
		return o.cleanupNftActions(f)
	}
	return append(o.cleanupNftActions(f), nft.Write(o.table(f.name), ruleset))
}

func (o Options) cleanupNftActions(f nftFamily) []command.Action {
	var out []command.Action
	for _, family := range f.tables() {
		out = append(out, nft.Delete(o.table(family)))
	}
	return out
}

func (o Options) table(family string) nft.Table {
	return nft.Table{
		Family:      family,
		Name:        o.NftablesTable,
		ApplyOrder:  command.OrderMangleChain,
		RemoveOrder: command.Teardown(command.OrderMangleChain),
	}
}

func (o Options) nftTableQuery(family string) command.Typed[map[string][]MarkRule] {
	return nft.Query(o.table(family), parseNftMarkRules)
}

func (o Options) NftQueries() []command.Query  { return o.nftQueries(nftV4) }
func (o Options) Nft6Queries() []command.Query { return o.nftQueries(nftV6) }

func (o Options) ReadInstalledNft(a command.Answers) InstalledNft {
	return o.readInstalledNft(nftV4, a)
}

func (o Options) ReadInstalledNft6(a command.Answers) InstalledNft {
	return o.readInstalledNft(nftV6, a)
}

func (o Options) NftNeedsRestore(want []MarkRule, got InstalledNft) bool {
	return o.nftNeedsRestore(nftV4, want, got)
}

func (o Options) Nft6NeedsRestore(want []MarkRule, got InstalledNft) bool {
	return o.nftNeedsRestore(nftV6, want, got)
}

func (o Options) NftActions(ruleset string) []command.Action {
	return o.nftActions(nftV4, ruleset)
}

func (o Options) Nft6Actions(ruleset string) []command.Action {
	return o.nftActions(nftV6, ruleset)
}

func (o Options) CleanupNftActions() []command.Action  { return o.cleanupNftActions(nftV4) }
func (o Options) CleanupNft6Actions() []command.Action { return o.cleanupNftActions(nftV6) }

var iptHooks = []string{"OUTPUT", "PREROUTING"}

var legacyIptHooks = []string{"FORWARD"}

func parseIptMarkRules(out string) []MarkRule {
	var got []MarkRule
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "-A" {
			continue
		}

		proto := command.FieldAfter(fields, "-p")
		ports := command.FirstOf(fields, "--dport", "--dports")
		markStr, _, _ := strings.Cut(command.FieldAfter(fields, "--set-xmark"), "/")
		if markStr == "" {
			markStr = command.FieldAfter(fields, "--set-mark")
		}
		mark, err := strconv.ParseInt(markStr, 0, 32)
		if proto == "" || ports == "" || err != nil {
			continue
		}
		got = append(got, MarkRule{proto: proto, ports: ports, mark: int(mark)})
	}
	return got
}

func (o Options) iptChainQuery() command.Typed[[]MarkRule] {
	return command.NewTyped(
		command.Query{
			Tool: command.ToolIPTables,
			Args: []string{"-t", "mangle", "-S", o.IptablesChain},
		},
		parseIptMarkRules,
	)
}

func (o Options) iptJumpQuery(hook string) command.Query {
	return command.Query{
		Tool: command.ToolIPTables,
		Args: []string{"-t", "mangle", "-C", hook, "-j", o.IptablesChain},
	}
}

func (o Options) IptablesQueries(want []MarkRule) []command.Query {
	qs := []command.Query{o.iptChainQuery().Query}
	if len(want) == 0 {
		return qs
	}
	for _, hook := range iptHooks {
		qs = append(qs, o.iptJumpQuery(hook))
	}
	return qs
}

type InstalledIptables struct {
	Present bool
	Rules   []MarkRule
	Jumped  map[string]bool
}

func (o Options) ReadInstalledIptables(want []MarkRule, a command.Answers) InstalledIptables {
	rules, err := o.iptChainQuery().Value(a)
	if err != nil {
		return InstalledIptables{}
	}
	got := InstalledIptables{Present: true, Rules: rules}
	if len(want) == 0 {
		return got
	}
	got.Jumped = map[string]bool{}
	for _, hook := range iptHooks {
		got.Jumped[hook] = a.OK(o.iptJumpQuery(hook))
	}
	return got
}

func (o Options) IptablesNeedsRestore(want []MarkRule, got InstalledIptables) bool {
	if !got.Present {
		return len(want) > 0
	}
	if len(want) == 0 {
		return true
	}
	if !sameMarkRules(want, got.Rules) {
		return true
	}
	for _, hook := range iptHooks {
		if !got.Jumped[hook] {
			return true
		}
	}
	return false
}

func (o Options) IptablesActions(want []MarkRule) []command.Action {
	if len(want) == 0 {
		return o.CleanupIptablesActions()
	}

	chain := o.IptablesChain
	out := o.CleanupIptablesActions()
	out = append(out, iptAction(command.OrderMangleChain, command.FailWarn, "-t", "mangle", "-N", chain))

	for _, r := range want {
		args := []string{"-t", "mangle", "-A", chain, "-p", r.proto}
		if strings.Contains(r.ports, ",") {
			args = append(args, "-m", "multiport", "--dports", r.ports)
		} else {
			args = append(args, "--dport", r.ports)
		}
		args = append(args, "-j", "MARK", "--set-mark", strconv.Itoa(r.mark))
		out = append(out, iptAction(command.OrderMangleChain, command.FailWarn, args...))
	}

	for _, hook := range iptHooks {
		out = append(out, iptAction(command.OrderMangleChain, command.FailWarn, "-t", "mangle", "-I", hook, "-j", chain))
	}
	return out
}

func (o Options) CleanupIptablesActions() []command.Action {
	chain := o.IptablesChain
	teardown := command.Teardown(command.OrderMangleChain)

	var out []command.Action
	for _, hook := range append(append([]string{}, iptHooks...), legacyIptHooks...) {
		out = append(out, iptAction(teardown, command.FailIgnore, "-t", "mangle", "-D", hook, "-j", chain))
	}
	out = append(out,
		iptAction(teardown, command.FailIgnore, "-t", "mangle", "-F", chain),
		iptAction(teardown, command.FailIgnore, "-t", "mangle", "-X", chain),
	)
	return out
}

func iptAction(order int, onFail command.FailPolicy, args ...string) command.Action {
	return command.Action{
		Tool:   command.ToolIPTables,
		Args:   args,
		OnFail: onFail,
		Order:  order,
	}
}
