package ndp

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/nft"
)

const (
	setName = "learned"

	chainName = "learn"
)

func nftTimeout(d time.Duration) string {
	secs := max(int(d.Round(time.Second).Seconds()), 1)
	return strconv.Itoa(secs) + "s"
}

type LearnRule struct {
	internal string
	uplink   string
}

func (o Options) DesiredLearnRules() map[LearnRule]struct{} {
	want := map[LearnRule]struct{}{}
	for _, u := range o.Uplinks {
		for _, s := range u.Subnets {
			want[LearnRule{internal: s.Prefix.String(), uplink: u.Name}] = struct{}{}
		}
	}
	return want
}

func (o Options) LearnRuleset() string {
	timeout := nftTimeout(o.Timeout)

	var rules []string
	for _, u := range o.Uplinks {
		for _, s := range u.Subnets {
			rules = append(rules, fmt.Sprintf(
				`ip6 saddr %s oifname "%s" update @%s { ip6 saddr . meta oifname timeout %s } comment "rb-ndp %s %s"`,
				s.Prefix, u.Name, setName, timeout, u.Name, s.Name))
		}
	}
	if len(rules) == 0 {
		return ""
	}

	return nft.Ruleset(o.table(), fmt.Sprintf(`  set %s {
    type ipv6_addr . ifname
    size %d
    flags dynamic,timeout
    timeout %s
  }

  chain %s {
    type filter hook forward priority filter; policy accept;
    %s
  }
`, setName, o.MaxHosts, timeout, chainName, strings.Join(rules, "\n    ")))
}

func (o Options) LearnActions(ruleset string) []command.Action {
	if ruleset == "" {

		return nil
	}
	return nft.Apply(o.table(), ruleset)
}

func parseLearnRules(out string) map[LearnRule]struct{} {
	got := map[LearnRule]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "update @"+setName) {
			continue
		}
		if i := strings.Index(line, " comment "); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)

		r := LearnRule{
			internal: command.FieldAfter(fields, "saddr"),
			uplink:   strings.Trim(command.FieldAfter(fields, "oifname"), `"`),
		}
		if r.internal != "" && r.uplink != "" {
			got[r] = struct{}{}
		}
	}
	return got
}

func (o Options) table() nft.Table {
	return nft.Table{
		Family:      "ip6",
		Name:        o.Table,
		ApplyOrder:  command.OrderNftTable,
		RemoveOrder: command.OrderNftTable,
	}
}

func (o Options) LearnQuery() command.Typed[map[LearnRule]struct{}] {
	return nft.Query(o.table(), parseLearnRules)
}

func (o Options) LearnQueries() []command.Query {
	return []command.Query{o.LearnQuery().Query}
}

type InstalledLearn = nft.Installed[map[LearnRule]struct{}]

func (o Options) ReadInstalledLearn(a command.Answers) InstalledLearn {
	return nft.Read(o.table(), parseLearnRules, a)
}

func (o Options) LearnNeedsRestore(want map[LearnRule]struct{}, inst InstalledLearn) bool {

	if len(want) == 0 {
		return false
	}
	if !inst.Present {
		return true
	}
	return !nft.SameSet(want, inst.Rules)
}

func (o Options) TableExists(inst InstalledLearn) bool { return inst.Present }

func (o Options) DeleteTableAction() command.Action { return nft.Delete(o.table()) }

type LearnedHost struct {
	Addr   net.IP
	Uplink string
}

func (o Options) ReadLearnedHosts(a command.Answers) ([]LearnedHost, bool) {
	hosts, err := o.LearnedHostsQuery().Value(a)
	if err != nil {
		return nil, false
	}
	return hosts, true
}

func (o Options) LearnedHostsQuery() command.Typed[[]LearnedHost] {
	return command.NewTyped(
		command.Query{
			Tool: command.ToolNFT,
			Args: []string{"list", "set", "ip6", o.Table, setName},
		},
		parseLearnedHosts,
	)
}

func parseLearnedHosts(out string) []LearnedHost {
	_, body, ok := strings.Cut(out, "elements = {")
	if !ok {
		return nil
	}
	if end := strings.Index(body, "}"); end >= 0 {
		body = body[:end]
	}

	seen := map[string]bool{}
	var hosts []LearnedHost
	for _, chunk := range strings.Split(body, ",") {
		fields := strings.Fields(chunk)
		if len(fields) < 3 || fields[1] != "." {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil || ip.To16() == nil || ip.To4() != nil {
			continue
		}
		uplink := strings.Trim(fields[2], `"`)
		if uplink == "" {
			continue
		}
		key := ip.String() + "%" + uplink
		if seen[key] {
			continue
		}
		seen[key] = true
		hosts = append(hosts, LearnedHost{Addr: ip, Uplink: uplink})
	}
	return hosts
}
