package firewall

import (
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
)

func opts(rules ...config.Rule) Options {
	return Options{
		IptablesChain: "ROUTE-BALANCER",
		NftablesTable: "route-balancer",
		Gateways:      []string{"eth0", "eth1", "wan2"},
		Rules:         rules,
	}
}

func TestMarkIsOneBasedSortedPosition(t *testing.T) {
	o := opts()
	for name, want := range map[string]int{
		"eth0": 1,
		"eth1": 2,
		"wan2": 3,
		"eth9": 0,
	} {
		if got := o.Mark(name); got != want {
			t.Errorf("Mark(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestNftRulesetEmptyWithoutPortRules(t *testing.T) {
	if got := opts().NftRuleset(); got != "" {
		t.Errorf("NftRuleset() = %q, want empty", got)
	}
}

func TestNftRulesetIsIPv4Only(t *testing.T) {
	got := opts(config.Rule{Gateway: "eth1", MatchDstPort: []int{443}}).NftRuleset()
	if !strings.HasPrefix(got, "table ip route-balancer {") {
		t.Errorf("ruleset starts %q, want a `table ip` declaration", strings.SplitN(got, "\n", 2)[0])
	}
	if strings.Contains(got, "table inet") {
		t.Error("ruleset declares an inet table, which would mark IPv6 packets no ip rule can steer")
	}
}

func TestNftRulesetMarksBothProtocolsByDefault(t *testing.T) {
	got := opts(config.Rule{Gateway: "eth1", MatchDstPort: []int{443, 8443}}).NftRuleset()
	for _, want := range []string{
		"tcp dport { 443, 8443 } meta mark set 2",
		"udp dport { 443, 8443 } meta mark set 2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ruleset does not contain %q:\n%s", want, got)
		}
	}
}

func TestNftRulesetHonoursOneProtocol(t *testing.T) {
	got := opts(config.Rule{Gateway: "eth0", MatchDstPort: []int{53}, MatchProtocol: "udp"}).NftRuleset()
	if !strings.Contains(got, "udp dport { 53 } meta mark set 1") {
		t.Errorf("ruleset lacks the udp rule:\n%s", got)
	}
	if strings.Contains(got, "tcp dport { 53 }") {
		t.Errorf("ruleset marks tcp for a udp-only rule:\n%s", got)
	}
}

func TestNftRulesetSkipsUnknownGateway(t *testing.T) {
	if got := opts(config.Rule{Gateway: "eth9", MatchDstPort: []int{80}}).NftRuleset(); got != "" {
		t.Errorf("NftRuleset() = %q, want empty when no rule could be rendered", got)
	}
}

func TestNftRulesetIsDeterministic(t *testing.T) {
	o := opts(
		config.Rule{Gateway: "wan2", MatchDstPort: []int{1194}, MatchProtocol: "udp"},
		config.Rule{Gateway: "eth0", MatchDstPort: []int{443}, MatchProtocol: "tcp"},
	)
	first := o.NftRuleset()
	for i := 0; i < 20; i++ {
		if got := o.NftRuleset(); got != first {
			t.Fatalf("pass %d differs:\n%s\nfirst:\n%s", i, got, first)
		}
	}
}

func TestParseNftMarkRules(t *testing.T) {
	out := `table ip route-balancer {
	chain mangle_output {
		type filter hook output priority mangle; policy accept;
		tcp dport { 443, 8443 } meta mark set 0x00000002
		udp dport 1194 meta mark set 0x00000003
	}

	chain mangle_prerouting {
		type filter hook forward priority mangle; policy accept;
		tcp dport { 443, 8443 } meta mark set 0x00000002
		udp dport 1194 meta mark set 0x00000003
	}
}`

	got := parseNftMarkRules(out)
	want := []MarkRule{
		{proto: "tcp", ports: "443,8443", mark: 2},
		{proto: "udp", ports: "1194", mark: 3},
	}

	for _, chain := range nftV4.chains {
		if !sameMarkRules(want, got[chain]) {
			t.Errorf("%s = %v, want %v", chain, got[chain], want)
		}
	}
}

func TestParseNftMarkRulesIgnoresChainHeaders(t *testing.T) {
	out := `table ip route-balancer {
	chain mangle_output {
		type filter hook output priority mangle; policy accept;
	}
}`
	if got := parseNftMarkRules(out); len(got["mangle_output"]) != 0 {
		t.Errorf("parsed %v out of a chain holding no rules", got["mangle_output"])
	}
}

func TestParseIptMarkRules(t *testing.T) {
	out := `-N ROUTE-BALANCER
-A ROUTE-BALANCER -p tcp -m multiport --dports 443,8443 -j MARK --set-xmark 0x2/0xffffffff
-A ROUTE-BALANCER -p udp -m udp --dport 1194 -j MARK --set-xmark 0x3/0xffffffff`

	want := []MarkRule{
		{proto: "tcp", ports: "443,8443", mark: 2},
		{proto: "udp", ports: "1194", mark: 3},
	}
	if got := parseIptMarkRules(out); !sameMarkRules(want, got) {
		t.Errorf("parseIptMarkRules = %v, want %v", got, want)
	}
}

func TestBothBackendsParseBackToTheDesiredRules(t *testing.T) {
	o := opts(
		config.Rule{Gateway: "eth1", MatchDstPort: []int{443, 8443}, MatchProtocol: "tcp"},
		config.Rule{Gateway: "wan2", MatchDstPort: []int{1194}, MatchProtocol: "udp"},
	)
	want := o.DesiredMarkRules()

	nft := parseNftMarkRules(`table ip route-balancer {
	chain mangle_output {
		tcp dport { 443, 8443 } meta mark set 0x00000002
		udp dport 1194 meta mark set 0x00000003
	}
}`)
	if !sameMarkRules(want, nft["mangle_output"]) {
		t.Errorf("nft round trip = %v, want %v", nft["mangle_output"], want)
	}

	ipt := parseIptMarkRules(`-A ROUTE-BALANCER -p tcp -m multiport --dports 443,8443 -j MARK --set-xmark 0x2/0xffffffff
-A ROUTE-BALANCER -p udp -m udp --dport 1194 -j MARK --set-xmark 0x3/0xffffffff`)
	if !sameMarkRules(want, ipt) {
		t.Errorf("iptables round trip = %v, want %v", ipt, want)
	}
}

func TestDesiredMarkRulesKeepsConfigOrder(t *testing.T) {
	o := opts(
		config.Rule{Gateway: "wan2", MatchDstPort: []int{443}, MatchProtocol: "tcp"},
		config.Rule{Gateway: "eth0", MatchDstPort: []int{443}, MatchProtocol: "tcp"},
	)
	got := o.DesiredMarkRules()
	if len(got) != 2 || got[0].mark != 3 || got[1].mark != 1 {
		t.Errorf("DesiredMarkRules = %v, want the config's own order (3 then 1)", got)
	}
	if sameMarkRules(got, []MarkRule{got[1], got[0]}) {
		t.Error("comparison ignores order, so a reordered chain would read as in sync")
	}
}

func TestDesiredMarkRulesSkipsWhatCannotBeRendered(t *testing.T) {
	o := opts(
		config.Rule{Gateway: "eth9", MatchDstPort: []int{80}},
		config.Rule{Gateway: "eth0"},
	)
	if got := o.DesiredMarkRules(); len(got) != 0 {
		t.Errorf("DesiredMarkRules = %v, want nothing renderable", got)
	}
}

func TestMarksAreStampedWhereTheyCanStillSteer(t *testing.T) {
	t.Run("nftables", func(t *testing.T) {
		got := opts(config.Rule{Gateway: "eth1", MatchDstPort: []int{443}}).NftRuleset()

		for _, want := range []string{

			"chain mangle_output {\n    type route hook output priority mangle;",
			"chain mangle_prerouting {\n    type filter hook prerouting priority mangle;",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("ruleset does not declare %q:\n%s", want, got)
			}
		}

		if strings.Contains(got, "hook forward") {
			t.Errorf("ruleset marks in the forward hook, after the decision it exists to influence:\n%s", got)
		}
	})

	t.Run("iptables", func(t *testing.T) {
		var jumped []string
		for _, a := range opts(config.Rule{Gateway: "eth1", MatchDstPort: []int{443}}).
			IptablesActions([]MarkRule{{proto: "tcp", ports: "443", mark: 2}}) {
			if i := indexOf(a.Args, "-I"); i >= 0 {
				jumped = append(jumped, a.Args[i+1])
			}
		}
		if strings.Join(jumped, ",") != "OUTPUT,PREROUTING" {
			t.Errorf("chain is reached from %v, want OUTPUT and PREROUTING", jumped)
		}
	})
}

func TestCleanupStillDeletesTheHookMarksMovedOffOf(t *testing.T) {
	var deleted []string
	for _, a := range opts().CleanupIptablesActions() {
		if i := indexOf(a.Args, "-D"); i >= 0 {
			deleted = append(deleted, a.Args[i+1])
		}
	}
	if strings.Join(deleted, ",") != "OUTPUT,PREROUTING,FORWARD" {
		t.Errorf("cleanup deletes jumps from %v, want the current hooks and the legacy FORWARD one", deleted)
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want && i+1 < len(args) {
			return i
		}
	}
	return -1
}

func opts6(rules ...config.Rule) Options {
	o := opts()
	o.Rules, o.Rules6 = nil, rules
	return o
}

func v6Rule(gateway string, port int) config.Rule {
	return config.Rule{
		Gateway: gateway, MatchDstPort: []int{port},
		MatchProtocol: "tcp", MatchFamily: config.RuleFamilyIPv6,
	}
}

var mapped = []MappedSubnet{
	{Uplink: "eth0", Prefix: "fd00:beef:3::/64"},
	{Uplink: "eth1", Prefix: "fd00:beef:1::/64"},
	{Uplink: "eth1", Prefix: "fd00:beef:2::/64"},
}

func TestNftRuleset6MarksOnlyWhatTheUplinkTranslates(t *testing.T) {
	got := opts6(v6Rule("eth1", 443)).NftRuleset6(mapped)

	for _, want := range []string{
		"ip6 saddr fd00:beef:1::/64 tcp dport { 443 } meta mark set 2",
		"ip6 saddr fd00:beef:2::/64 tcp dport { 443 } meta mark set 2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ruleset does not contain %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "fd00:beef:3::/64") {
		t.Errorf("a subnet eth1 does not translate was steered to it anyway:\n%s", got)
	}
}

func TestNftRuleset6IsEmptyWhileTheUplinkTranslatesNothing(t *testing.T) {
	if got := opts6(v6Rule("wan2", 443)).NftRuleset6(mapped); got != "" {
		t.Errorf("NftRuleset6() = %q, want nothing steered to an uplink holding no mapping", got)
	}
	if got := opts6(v6Rule("eth1", 443)).NftRuleset6(nil); got != "" {
		t.Errorf("NftRuleset6() = %q, want nothing steered while nothing is translated", got)
	}
}

func TestNftRuleset6IsIPv6Only(t *testing.T) {
	got := opts6(v6Rule("eth1", 443)).NftRuleset6(mapped)

	if !strings.HasPrefix(got, "table ip6 route-balancer {") {
		t.Errorf("ruleset starts %q, want a `table ip6` declaration", strings.SplitN(got, "\n", 2)[0])
	}
	if strings.Contains(got, "table inet") {
		t.Error("ruleset declares an inet table, which would mark IPv4 packets with an IPv6 rule's mark")
	}

	if strings.Contains(got, "hook output") {
		t.Errorf("ruleset marks locally originated traffic:\n%s", got)
	}
}

func TestNftRuleset6IsDeterministic(t *testing.T) {
	o := opts6(v6Rule("eth1", 443), v6Rule("eth0", 25))
	first := o.NftRuleset6(mapped)
	for i := 0; i < 20; i++ {
		if got := o.NftRuleset6(mapped); got != first {
			t.Fatalf("run %d rendered a different ruleset:\n%s\n\nwant\n%s", i, got, first)
		}
	}
}

func TestNft6ParsesBackToTheDesiredRules(t *testing.T) {
	o := opts6(v6Rule("eth1", 443))

	got := parseNftMarkRules(`table ip6 route-balancer {
	chain mangle_prerouting {
		type filter hook prerouting priority mangle; policy accept;
		ip6 saddr fd00:beef:1::/64 tcp dport 443 meta mark set 0x00000002
		ip6 saddr fd00:beef:2::/64 tcp dport 443 meta mark set 0x00000002
	}
}`)["mangle_prerouting"]

	if want := o.DesiredMarkRules6(mapped); !sameMarkRules(want, got) {
		t.Errorf("parsed back %v, want %v", got, want)
	}
}

func TestASourceIsPartOfTheRule(t *testing.T) {
	o := opts6(v6Rule("eth1", 443))
	want := o.DesiredMarkRules6(mapped)

	got := parseNftMarkRules(`table ip6 route-balancer {
	chain mangle_prerouting {
		ip6 saddr fd00:beef:1::/64 tcp dport 443 meta mark set 0x00000002
		tcp dport 443 meta mark set 0x00000002
	}
}`)["mangle_prerouting"]

	if sameMarkRules(want, got) {
		t.Error("a rule that lost its saddr, and so steers every source, was accepted as the wanted one")
	}
}
