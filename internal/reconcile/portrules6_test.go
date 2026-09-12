package reconcile

import (
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/route"
)

func steeredConfig(rules ...config.Rule) *config.Config {
	return &config.Config{
		Firewall: "nftables",
		Gateways: map[string]config.Gateway{"wan0": {Weight: 1}, "wan1": {Weight: 1}},
		Rules:    rules,
		NPTv6: &config.NPTv6{
			Enable:         true,
			InternalPrefix: "fd00:dead:beef::/48",
			Subnets: map[string]config.NPTv6Subnet{
				"main":  {Prefix: "fd00:dead:beef:1::/64"},
				"guest": {Prefix: "fd00:dead:beef:2::/64"},
			},
			Uplinks: map[string]config.NPTv6Uplink{
				"wan0": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:a000::/56",
					SubnetPriority: []string{"main", "guest"}},
				"wan1": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:b000::/64",
					SubnetPriority: []string{"main"}},
			},
		},
	}
}

func v6PortRule(gateway string) config.Rule {
	return config.Rule{
		Gateway: gateway, MatchDstPort: []int{443},
		MatchProtocol: "tcp", MatchFamily: config.RuleFamilyIPv6,
	}
}

func steered(t *testing.T, rules ...config.Rule) *testReconciler {
	t.Helper()

	d := newTestReconciler(t)
	d.useConfig(t, steeredConfig(rules...))
	d.state.Links.Ifaces = map[string]int{"wan0": 2, "wan1": 3}
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"),
			IfName: "wan0", IfIndex: 2, ConfigWeight: 1},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::2"),
			IfName: "wan1", IfIndex: 3, ConfigWeight: 1},
	)
	d.state.Prefixes.Leases["wan0"] = leaseFor(t, "wan0", "2001:db8:a000::/56")
	d.state.Prefixes.Leases["wan1"] = leaseFor(t, "wan1", "2001:db8:b000::/64")
	return d
}

func markLines(inv Inventory) []string {
	var out []string
	for _, line := range strings.Split(inv.Nft6, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "ip6 saddr") {
			out = append(out, line)
		}
	}
	return out
}

func TestAnIPv6RuleStopsAtWhatItsUplinkTranslates(t *testing.T) {
	t.Parallel()

	d := steered(t, v6PortRule("wan1"))
	inv := Desired(d.r, &d.state)

	want := []string{"ip6 saddr fd00:dead:beef:1::/64 tcp dport { 443 } meta mark set 2"}
	if got := markLines(inv); !slices.Equal(got, want) {
		t.Errorf("marks are\n\t%v\nwant\n\t%v", got, want)
	}

	if strings.Contains(inv.Nft6, "fd00:dead:beef:2::/64") {
		t.Errorf("guest was steered to an uplink that does not translate it:\n%s", inv.Nft6)
	}
}

func TestAnIPv6RuleBuildsItsTableAndRule(t *testing.T) {
	t.Parallel()

	d := steered(t, v6PortRule("wan1"))
	inv := Desired(d.r, &d.state)

	spec, ok := inv.Tables[tableKey{route.FamilyV6, 3}]
	if !ok {
		t.Fatal("no IPv6 table for the uplink a rule steers to")
	}
	if spec.Via != "fe80::2" || spec.Table != "103" {
		t.Errorf("table is %s, want the default via fe80::2 in table 103", spec)
	}
	if spec.Src != "" {
		t.Errorf("the IPv6 table pins src %q; an interface's global addresses churn", spec.Src)
	}

	want := []route.FwmarkSpec{{Mark: 2, Table: "103", Prio: "502"}}
	if got := inv.Fwmarks[route.FamilyV6]; !slices.Equal(got, want) {
		t.Errorf("IPv6 fwmarks = %v, want %v", got, want)
	}
	if got := inv.Fwmarks[route.FamilyV4]; len(got) != 0 {
		t.Errorf("an IPv6-only rule installed IPv4 fwmarks: %v", got)
	}
}

func TestAnIPv6RuleWaitsForTheUplinkToHaveARoute(t *testing.T) {
	t.Parallel()

	d := steered(t, v6PortRule("wan1"))
	d.useGateways(t, route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"),
		IfName: "wan0", IfIndex: 2, ConfigWeight: 1})

	inv := Desired(d.r, &d.state)
	if _, ok := inv.Tables[tableKey{route.FamilyV6, 3}]; ok {
		t.Error("built an IPv6 table for an uplink carrying no IPv6 default route")
	}
	if got := inv.Fwmarks[route.FamilyV6]; len(got) != 0 {
		t.Errorf("installed %v, a rule selecting into a table that was not built", got)
	}
}

func TestAnIPv6RuleUnmarksWhenTheUplinkStopsTranslating(t *testing.T) {
	t.Parallel()

	d := steered(t, v6PortRule("wan1"))
	delete(d.state.Prefixes.Leases, "wan1")

	inv := Desired(d.r, &d.state)
	if len(inv.MarkRules6) != 0 {
		t.Errorf("still marking for an uplink that translates nothing: %v", markLines(inv))
	}
	if inv.Nft6 != "" {
		t.Errorf("ruleset is %q, want the table withdrawn", inv.Nft6)
	}
}

func TestTheIPv6TableAndItsRuleAreInstalledTogether(t *testing.T) {
	t.Parallel()

	d := steered(t, v6PortRule("wan1"))
	run := d.pass(append(
		d.tableResources(Desired(d.r, &d.state), d.look()),
		fwmarkResources(Desired(d.r, &d.state), d.look())...))

	want := []string{
		"ip -6 route replace default via fe80::2 dev wan1 table 103",
		"ip -6 rule add fwmark 2 lookup 103 priority 502",
	}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("the pass ran\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestAStaleIPv6TableTakesItsRuleWithIt(t *testing.T) {
	t.Parallel()

	d := steered(t)
	d.useAsker(map[string]string{
		"ip -6 rule show": "0:\tfrom all lookup local\n" +
			"502:\tfrom all fwmark 0x2 lookup 103\n" +
			"32766:\tfrom all lookup main",
	})
	d.state.Installed.Rules = map[int]route.Rules{
		route.FamilyV6: route.ReadRules(route.FamilyV6,
			d.Observe([]command.Query{route.RulesQuery(route.FamilyV6).Query})),
	}

	run := d.pass(d.staleV6TableResources(Desired(d.r, &d.state)))

	want := []string{
		"ip -6 rule del priority 502",
		"ip -6 route flush table 103",
	}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("the teardown ran\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestIPv6PortRulesAreRefusedWhereTheyCannotWork(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  func(*config.Config)
		want string
	}{
		{
			name: "without nptv6 nothing rewrites the source",
			cfg:  func(c *config.Config) { c.NPTv6 = nil },
			want: "require nptv6",
		},
		{
			name: "the IPv6 marks have no iptables backend",
			cfg:  func(c *config.Config) { c.Firewall = "iptables" },
			want: "require firewall",
		},
		{
			name: "the gateway is not an uplink nptv6 knows",
			cfg:  func(c *config.Config) { c.Rules = []config.Rule{v6PortRule("wan0x")} },
			want: "not an nptv6 uplink",
		},
		{
			name: "the family is one nobody defined",
			cfg: func(c *config.Config) {
				c.Rules = []config.Rule{{Gateway: "wan1", MatchDstPort: []int{443}, MatchFamily: "inet"}}
			},
			want: "unknown match_family",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := steeredConfig(v6PortRule("wan1"))
			tc.cfg(c)

			_, err := newTestReconcilerWith(t, c)
			if err == nil {
				t.Fatal("the daemon started on a config it cannot make good on")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error is %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestAnIPv4RuleStillNeedsNoneOfThat(t *testing.T) {
	t.Parallel()

	c := steeredConfig(config.Rule{Gateway: "wan1", MatchDstPort: []int{443}})
	c.NPTv6, c.Firewall = nil, "iptables"

	if _, err := newTestReconcilerWith(t, c); err != nil {
		t.Errorf("an IPv4 port rule was refused: %v", err)
	}
}

func TestAConvergedIPv6PassInstallsNothing(t *testing.T) {
	t.Parallel()

	d := steered(t, v6PortRule("wan1"))
	d.useAsker(map[string]string{
		"ip -6 rule show":            "502:\tfrom all fwmark 0x2 lookup 103",
		"ip -6 route show table 103": "default via fe80::2 dev wan1 metric 1024 pref medium",
		"nft list table ip6 route-balancer": `table ip6 route-balancer {
	chain mangle_prerouting {
		type filter hook prerouting priority mangle; policy accept;
		ip6 saddr fd00:dead:beef:1::/64 tcp dport 443 meta mark set 0x00000002
	}
}`,
	})

	inv := Desired(d.r, &d.state)
	inst := d.look()

	rs := append(d.tableResources(inv, inst), fwmarkResources(inv, inst)...)
	rs = append(rs, mangle6Resource(d.r.Fw, inv, inst)...)
	rs = append(rs, d.staleV6TableResources(inv)...)

	var ids []string
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	want := []string{"table/v6/wan1", "fwmark-rules/v6", mangle6ResourceID}
	if !slices.Equal(ids, want) {
		t.Fatalf("the pass planned %v, want %v", ids, want)
	}

	for _, r := range rs {
		switch r.Verdict {
		case Want:
			if !r.InSync() {
				t.Errorf("%s reported drift against a mechanism that is fully installed", r.ID)
			}
		case Absent:
			if acts := r.Remove(); len(acts) > 0 {
				t.Errorf("%s wants to remove %v from a converged machine", r.ID, argsOf(acts))
			}
		}
	}
}

func TestAnIPv6TableIsStillReapedAfterItsLastRuleIsRemoved(t *testing.T) {
	t.Parallel()

	d := steered(t)
	if got := d.r.Rt.Marked; len(got) != 0 {
		t.Fatalf("this config marks %v; it must mark nothing for the test to mean anything", got)
	}

	d.useAsker(map[string]string{
		"ip -4 rule show": "",
		"ip -6 rule show": "0:\tfrom all lookup local\n" +
			"502:\tfrom all fwmark 0x2 lookup 103\n" +
			"32766:\tfrom all lookup main",
	})

	inv := Desired(d.r, &d.state)
	d.absorbInstalled(d.Observe(d.queryPlan()))

	run := d.pass(d.staleV6TableResources(inv))

	want := []string{
		"ip -6 rule del priority 502",
		"ip -6 route flush table 103",
	}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("the teardown ran\n\t%v\nwant\n\t%v", got, want)
	}
}
