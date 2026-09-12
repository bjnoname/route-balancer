package nptv6

import (
	"net"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

func resolve(t *testing.T, n *config.NPTv6) Options {
	t.Helper()
	c := &config.Config{NPTv6: n}
	c.SetDefaults()
	opts, err := Resolve(c)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return opts
}

func testSubnets(t *testing.T, uplink string, priority ...string) Options {
	t.Helper()
	return Options{
		Subnets: map[string]*net.IPNet{
			"main":  mustCIDR(t, "fd00:dead:beef:1::/64"),
			"guest": mustCIDR(t, "fd00:dead:beef:2::/64"),
			"iot":   mustCIDR(t, "fd00:dead:beef:3::/64"),
		},
		Uplinks: []Uplink{{Name: uplink, SubnetPriority: priority}},
	}
}

func prefixOf(t *testing.T, cidr string) (net.IP, int) {
	t.Helper()
	n := mustCIDR(t, cidr)
	ones, _ := n.Mask.Size()
	return n.IP, ones
}

func TestSlotsFor(t *testing.T) {
	cases := []struct {
		length int
		want   int
	}{
		{48, 65536},
		{56, 256},
		{60, 16},
		{63, 2},
		{64, 1},
		{65, 0},
		{0, 0},
		{-1, 0},
	}
	for _, c := range cases {
		if got := slotsFor(c.length); got != c.want {
			t.Errorf("slotsFor(%d) = %d, want %d", c.length, got, c.want)
		}
	}
}

func TestSubnetAt(t *testing.T) {
	cases := []struct {
		prefix string
		length int
		index  int
		want   string
	}{
		{"2001:db8:abcd::", 48, 0, "2001:db8:abcd::/64"},
		{"2001:db8:abcd::", 48, 1, "2001:db8:abcd:1::/64"},
		{"2001:db8:abcd::", 48, 0x1234, "2001:db8:abcd:1234::/64"},
		{"2001:db8:abcd:ab00::", 56, 0, "2001:db8:abcd:ab00::/64"},
		{"2001:db8:abcd:ab00::", 56, 3, "2001:db8:abcd:ab03::/64"},
		{"2001:db8:abcd:ab00::", 56, 255, "2001:db8:abcd:abff::/64"},
		{"2001:db8:1:2::", 64, 0, "2001:db8:1:2::/64"},
	}
	for _, c := range cases {
		got := subnetAt(net.ParseIP(c.prefix), c.length, c.index)
		if got == nil || got.String() != c.want {
			t.Errorf("subnetAt(%s/%d, %d) = %v, want %s", c.prefix, c.length, c.index, got, c.want)
		}
	}
}

func TestSubnetAtMasksDirtyPrefix(t *testing.T) {
	got := subnetAt(net.ParseIP("2001:db8:abcd:beef::1"), 56, 2)
	if got == nil || got.String() != "2001:db8:abcd:be02::/64" {
		t.Errorf("subnetAt with unmasked prefix = %v, want 2001:db8:abcd:be02::/64", got)
	}
}

func TestAssignFitsAll(t *testing.T) {
	prefix, length := prefixOf(t, "2001:db8:abcd::/56")
	assigned, dropped := testSubnets(t, "wan0", "main", "guest", "iot").Assign("wan0", prefix, length)

	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none", dropped)
	}
	want := []string{"2001:db8:abcd::/64", "2001:db8:abcd:1::/64", "2001:db8:abcd:2::/64"}
	if len(assigned) != len(want) {
		t.Fatalf("assigned %d subnets, want %d", len(assigned), len(want))
	}
	for i, a := range assigned {
		if a.External.String() != want[i] {
			t.Errorf("assigned[%d].External = %s, want %s", i, a.External, want[i])
		}
	}
	if assigned[0].Subnet != "main" || assigned[0].Internal.String() != "fd00:dead:beef:1::/64" {
		t.Errorf("highest-priority subnet = %s/%s, want main/fd00:dead:beef:1::/64",
			assigned[0].Subnet, assigned[0].Internal)
	}
}

func TestAssignSingleSlot(t *testing.T) {
	prefix, length := prefixOf(t, "2001:db8:1:2::/64")
	assigned, dropped := testSubnets(t, "wan1", "main", "guest", "iot").Assign("wan1", prefix, length)

	if len(assigned) != 1 {
		t.Fatalf("assigned %d subnets, want 1", len(assigned))
	}
	if assigned[0].Subnet != "main" {
		t.Errorf("assigned subnet = %q, want the highest-priority %q", assigned[0].Subnet, "main")
	}
	if assigned[0].External.String() != "2001:db8:1:2::/64" {
		t.Errorf("external = %s, want the delegation itself", assigned[0].External)
	}
	if strings.Join(dropped, ",") != "guest,iot" {
		t.Errorf("dropped = %v, want [guest iot]", dropped)
	}
}

func TestAssignHonoursPriorityOrder(t *testing.T) {
	prefix, length := prefixOf(t, "2001:db8:1:2::/64")
	assigned, dropped := testSubnets(t, "wan1", "iot", "main", "guest").Assign("wan1", prefix, length)

	if len(assigned) != 1 || assigned[0].Subnet != "iot" {
		t.Fatalf("assigned = %v, want only iot", assigned)
	}
	if strings.Join(dropped, ",") != "main,guest" {
		t.Errorf("dropped = %v, want [main guest]", dropped)
	}
}

func TestAssignLengthChangeShrinks(t *testing.T) {
	opts := Options{Subnets: map[string]*net.IPNet{}}
	var priority []string
	for _, spec := range []struct{ name, prefix string }{
		{"a", "fd00::1:0:0:0/64"}, {"b", "fd00::2:0:0:0/64"}, {"c", "fd00::3:0:0:0/64"},
	} {
		opts.Subnets[spec.name] = mustCIDR(t, spec.prefix)
		priority = append(priority, spec.name)
	}
	opts.Uplinks = []Uplink{{Name: "wan0", SubnetPriority: priority}}

	prefix, length := prefixOf(t, "2001:db8:abcd::/56")
	wide, _ := opts.Assign("wan0", prefix, length)
	if len(wide) != 3 {
		t.Fatalf("with a /56: assigned %d, want 3", len(wide))
	}

	prefix, length = prefixOf(t, "2001:db8:abcd:ab00::/62")
	narrow, dropped := opts.Assign("wan0", prefix, length)
	if len(narrow) != 3 || len(dropped) != 0 {
		t.Fatalf("with a /62: assigned %d dropped %v, want 3 and none", len(narrow), dropped)
	}

	prefix, length = prefixOf(t, "2001:db8:abcd:ab00::/63")
	tight, dropped := opts.Assign("wan0", prefix, length)
	if len(tight) != 2 {
		t.Fatalf("with a /63: assigned %d, want 2", len(tight))
	}
	if strings.Join(dropped, ",") != "c" {
		t.Errorf("dropped = %v, want [c]", dropped)
	}
}

func TestAssignTooSmall(t *testing.T) {
	assigned, dropped := testSubnets(t, "wan0", "main", "guest").Assign("wan0", net.ParseIP("2001:db8::"), 72)

	if len(assigned) != 0 {
		t.Errorf("assigned %d subnets from a /72, want none", len(assigned))
	}
	if len(dropped) != 2 {
		t.Errorf("dropped = %v, want both subnets", dropped)
	}
}

func rulesetOpts(t *testing.T, inbound bool) Options {
	t.Helper()
	return resolve(t, &config.NPTv6{
		Enable:         true,
		NftablesTable:  "rb-npt",
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {
				PrefixSource:            config.SourceRoute,
				SubnetPriority:          []string{"main"},
				AllowInboundConnections: inbound,
			},
		},
	})
}

func oneAssignment(t *testing.T) []Assignment {
	t.Helper()
	return []Assignment{{
		Uplink:   "wan0",
		Subnet:   "main",
		Internal: mustCIDR(t, "fd00:dead:beef:1::/64"),
		External: mustCIDR(t, "2001:db8:abcd::/64"),
	}}
}

func TestRuleset(t *testing.T) {
	opts := rulesetOpts(t, true)
	ruleset := opts.Ruleset(oneAssignment(t))

	if !strings.HasPrefix(ruleset, "table ip6 rb-npt { }\ndelete table ip6 rb-npt\n") {
		t.Errorf("ruleset does not begin with the atomic-replace preamble:\n%s", ruleset)
	}
	for _, want := range []string{
		`ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" snat ip6 prefix to 2001:db8:abcd::/64`,
		`ip6 daddr 2001:db8:abcd::/64 iifname "wan0" dnat ip6 prefix to fd00:dead:beef:1::/64`,
		`comment "rb-nptv6 wan0 main"`,
		"type nat hook postrouting priority srcnat",
		"type nat hook prerouting priority dstnat",
	} {
		if !strings.Contains(ruleset, want) {
			t.Errorf("ruleset missing %q:\n%s", want, ruleset)
		}
	}
	if strings.Contains(ruleset, "chain inbound") {
		t.Errorf("inbound was asked for and still guarded:\n%s", ruleset)
	}
}

func TestRulesetClosesInboundByDefault(t *testing.T) {
	opts := rulesetOpts(t, false)
	ruleset := opts.Ruleset(oneAssignment(t))

	for _, want := range []string{
		`ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" snat ip6 prefix to 2001:db8:abcd::/64`,
		"chain inbound {\n    type filter hook prerouting priority dstnat - 10; policy accept;",
		`ip6 daddr 2001:db8:abcd::/64 iifname "wan0" ct state new counter drop`,
		`comment "rb-nptv6-closed wan0 main"`,
	} {
		if !strings.Contains(ruleset, want) {
			t.Errorf("ruleset missing %q:\n%s", want, ruleset)
		}
	}
	if strings.Contains(ruleset, "dnat") {
		t.Errorf("inbound was not asked for and a dnat was installed anyway:\n%s", ruleset)
	}
}

func TestClosedRuleMatchesWhatTheDnatWouldHave(t *testing.T) {
	opts := rulesetOpts(t, false)
	assignments := oneAssignment(t)

	open := rulesetOpts(t, true).Ruleset(assignments)
	dnat := `ip6 daddr 2001:db8:abcd::/64 iifname "wan0"`
	if !strings.Contains(open, dnat) {
		t.Fatalf("the dnat does not match what this test assumes:\n%s", open)
	}
	if closed := opts.Ruleset(assignments); !strings.Contains(closed, dnat+" ct state new counter drop") {
		t.Errorf("the closed rule does not stand exactly where the dnat did:\n%s", closed)
	}
	if strings.Contains(opts.Ruleset(assignments), `daddr fd00:dead:beef`) {
		t.Errorf("the closed rule matches an internal destination, which cannot arrive on an uplink")
	}
}

func TestDropsFacingOppositeWaysKeyDifferently(t *testing.T) {
	opts := resolve(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRoute, SubnetPriority: []string{"main"}},
			"wan1": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}},
		},
	})

	want := opts.DesiredRules(oneAssignment(t))
	var in, out int
	for k := range want {
		switch k.dir {
		case dirDropIn:
			in++
		case dirDropOut:
			out++
		}
	}
	if in != 1 || out != 1 {
		t.Fatalf("DesiredRules = %+v, want one drop-in for wan0 and one drop-out for wan1", want)
	}

	got := parseRules(opts.Ruleset(oneAssignment(t)))
	if len(got) != len(want) {
		t.Fatalf("parsed %d rules, want %d:\ngot  %+v\nwant %+v", len(got), len(want), got, want)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("rule %+v not recovered from the ruleset we generated: %+v", k, got)
		}
	}
}

func TestRulesetNoAssignments(t *testing.T) {
	opts := resolve(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRoute, SubnetPriority: []string{"main"}},
		},
	})

	got := opts.Ruleset(nil)
	for _, want := range []string{
		"type filter hook postrouting priority srcnat + 10",
		`ip6 saddr fd00:dead:beef::/48 oifname "wan0" drop`,
		`comment "rb-nptv6-guard wan0"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ruleset missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "snat") || strings.Contains(got, "dnat") {
		t.Errorf("translation rules generated with no assignments:\n%s", got)
	}
}

func TestRulesetNoAssignmentsUnguarded(t *testing.T) {
	off := false
	opts := resolve(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		LeakProtection: &off,
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRoute, SubnetPriority: []string{"main"}},
		},
	})

	if got := opts.Ruleset(nil); got != "" {
		t.Errorf("Ruleset(nil) = %q, want empty", got)
	}
}

func TestGuardUplinksSkipsTranslatingUplinks(t *testing.T) {
	opts := resolve(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets: map[string]config.NPTv6Subnet{
			"main":  {Prefix: "fd00:dead:beef:1::/64"},
			"guest": {Prefix: "fd00:dead:beef:2::/64"},
		},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRoute, SubnetPriority: []string{"main", "guest"}},
			"wan1": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}},
		},
	})

	assignments := []Assignment{{
		Uplink:   "wan0",
		Subnet:   "main",
		Internal: mustCIDR(t, "fd00:dead:beef:1::/64"),
		External: mustCIDR(t, "2001:db8:abcd::/64"),
	}}

	got := opts.GuardUplinks(assignments)
	if len(got) != 1 || got[0] != "wan1" {
		t.Fatalf("GuardUplinks() = %v, want [wan1]", got)
	}

	ruleset := opts.Ruleset(assignments)
	if !strings.Contains(ruleset, `oifname "wan1" drop`) {
		t.Errorf("ruleset missing the guard for the idle uplink:\n%s", ruleset)
	}
	if strings.Contains(ruleset, `oifname "wan0" drop`) {
		t.Errorf("ruleset guards an uplink that is translating:\n%s", ruleset)
	}
}

func TestParseRulesMatchesDesiredGuard(t *testing.T) {
	opts := resolve(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRoute, SubnetPriority: []string{"main"}},
		},
	})

	listed := `table ip6 route-balancer-nptv6 {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
	}

	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
	}

	chain guard {
		type filter hook postrouting priority srcnat + 10; policy accept;
		ip6 saddr fd00:dead:beef::/48 oifname "wan0" drop comment "rb-nptv6-guard wan0"
	}
}`

	want := opts.DesiredRules(nil)
	got := parseRules(listed)

	if len(want) != 1 {
		t.Fatalf("DesiredRules(nil) = %v, want one guard rule", want)
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d rules, want %d:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("guard rule %+v not recovered from nft output: %v", k, got)
		}
	}
}

func TestParseRulesMatchesDesired(t *testing.T) {
	assignments := []Assignment{{
		Uplink:   "wan0",
		Subnet:   "main",
		Internal: mustCIDR(t, "fd00:dead:beef:1::/64"),
		External: mustCIDR(t, "2001:db8:abcd::/64"),
	}}

	listed := `table ip6 route-balancer-nptv6 {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		ip6 daddr 2001:db8:abcd::/64 iifname "wan0" dnat prefix to fd00:dead:beef:1::/64 comment "rb-nptv6 wan0 main"
	}

	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" snat prefix to 2001:db8:abcd::/64 comment "rb-nptv6 wan0 main"
	}
}`

	want := Options{Uplinks: []Uplink{{Name: "wan0", AllowInbound: true}}}.DesiredRules(assignments)
	got := parseRules(listed)

	if len(got) != len(want) {
		t.Fatalf("parsed %d rules, want %d: %v", len(got), len(want), got)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("desired rule %+v not recovered from nft output", k)
		}
	}
}

func TestParseRulesMatchesDesiredClosed(t *testing.T) {
	listed := `table ip6 route-balancer-nptv6 {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" snat prefix to 2001:db8:abcd::/64 comment "rb-nptv6 wan0 main"
	}

	chain inbound {
		type filter hook prerouting priority dstnat - 10; policy accept;
		ip6 daddr 2001:db8:abcd::/64 iifname "wan0" ct state new counter packets 206 bytes 16480 drop comment "rb-nptv6-closed wan0 main"
	}
}`

	want := Options{Uplinks: []Uplink{{Name: "wan0"}}}.DesiredRules(oneAssignment(t))
	got := parseRules(listed)

	if len(got) != len(want) {
		t.Fatalf("parsed %d rules, want %d:\ngot  %+v\nwant %+v", len(got), len(want), got, want)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("desired rule %+v not recovered from nft output: %+v", k, got)
		}
	}
}

func TestParseRulesIgnoresCommentText(t *testing.T) {
	line := `ip6 saddr fd00::1:0:0:0/64 oifname "wan0" snat prefix to 2001:db8::/64 comment "rb-nptv6 wan0 to"`
	got := parseRules(line)

	if len(got) != 1 {
		t.Fatalf("parsed %d rules, want 1: %v", len(got), got)
	}
	for k := range got {
		if k.target != "2001:db8::/64" {
			t.Errorf("target = %q, want the rule's target and not the comment's", k.target)
		}
	}
}

func TestActionsOnAnEmptyRulesetDeleteTheTable(t *testing.T) {
	opts := Options{Table: "rb-npt"}

	acts := opts.Actions("")
	if len(acts) != 1 || strings.Join(acts[0].Args, " ") != "delete table ip6 rb-npt" {
		t.Fatalf("Actions(\"\") = %+v, want one delete of the table", acts)
	}

	acts = opts.Actions("table ip6 rb-npt { }\n")
	if len(acts) != 1 || acts[0].Stdin == "" {
		t.Fatalf("Actions(ruleset) = %+v, want one nft -f carrying the ruleset", acts)
	}
}

func TestResolve(t *testing.T) {
	base := func() *config.NPTv6 {
		return &config.NPTv6{
			Enable:         true,
			InternalPrefix: "fd00:dead:beef::/48",
			Subnets: map[string]config.NPTv6Subnet{
				"main":  {Prefix: "fd00:dead:beef:1::/64"},
				"guest": {Prefix: "fd00:dead:beef:2::/64"},
			},
			Uplinks: map[string]config.NPTv6Uplink{
				"wan0": {PrefixSource: config.SourceRoute, SubnetPriority: []string{"main", "guest"}},
			},
		}
	}

	cases := []struct {
		name    string
		mutate  func(*config.NPTv6)
		wantErr string
	}{
		{name: "valid", mutate: func(*config.NPTv6) {}},
		{
			name:    "internal prefix must be a ULA",
			mutate:  func(n *config.NPTv6) { n.InternalPrefix = "2001:db8::/48" },
			wantErr: "not a ULA",
		},
		{
			name: "non-ULA allowed with the explicit override",
			mutate: func(n *config.NPTv6) {
				n.InternalPrefix = "2001:db8::/48"
				n.AllowNonULA = true
				n.Subnets = map[string]config.NPTv6Subnet{
					"main":  {Prefix: "2001:db8:0:1::/64"},
					"guest": {Prefix: "2001:db8:0:2::/64"},
				}
			},
		},
		{
			name:    "subnet must be a /64",
			mutate:  func(n *config.NPTv6) { n.Subnets["main"] = config.NPTv6Subnet{Prefix: "fd00:dead:beef:1::/56"} },
			wantErr: "must be a /64",
		},
		{
			name:    "subnet must sit inside the internal prefix",
			mutate:  func(n *config.NPTv6) { n.Subnets["main"] = config.NPTv6Subnet{Prefix: "fd00:ffff:ffff:1::/64"} },
			wantErr: "outside internal_prefix",
		},
		{
			name: "subnet_priority cannot name an undefined subnet",
			mutate: func(n *config.NPTv6) {
				n.Uplinks["wan0"] = config.NPTv6Uplink{SubnetPriority: []string{"main", "nope"}}
			},
			wantErr: "undefined subnet",
		},
		{
			name: "subnet_priority cannot repeat a subnet",
			mutate: func(n *config.NPTv6) {
				n.Uplinks["wan0"] = config.NPTv6Uplink{SubnetPriority: []string{"main", "main"}}
			},
			wantErr: "appears twice",
		},
		{
			name: "subnet_priority cannot be empty",
			mutate: func(n *config.NPTv6) {
				n.Uplinks["wan0"] = config.NPTv6Uplink{SubnetPriority: nil}
			},
			wantErr: "must name at least one subnet",
		},
		{
			name: "static source needs a static prefix",
			mutate: func(n *config.NPTv6) {
				n.Uplinks["wan0"] = config.NPTv6Uplink{PrefixSource: config.SourceStatic, SubnetPriority: []string{"main"}}
			},
			wantErr: "requires static_prefix",
		},
		{
			name: "unknown prefix source",
			mutate: func(n *config.NPTv6) {
				n.Uplinks["wan0"] = config.NPTv6Uplink{PrefixSource: "dhcpv6", SubnetPriority: []string{"main"}}
			},
			wantErr: "unknown prefix_source",
		},
		{
			name:    "proxy_ndp_max_hosts cannot be negative",
			mutate:  func(n *config.NPTv6) { n.ProxyNDPMaxHosts = -1 },
			wantErr: "proxy_ndp_max_hosts",
		},
		{
			name: "two route sources need match_prefix to be distinguishable",
			mutate: func(n *config.NPTv6) {
				n.Uplinks["wan1"] = config.NPTv6Uplink{PrefixSource: config.SourceRoute, SubnetPriority: []string{"main"}}
			},
			wantErr: "match_prefix is required",
		},
		{
			name: "two route sources are fine once each declares its aggregate",
			mutate: func(n *config.NPTv6) {
				n.Uplinks["wan0"] = config.NPTv6Uplink{PrefixSource: config.SourceRoute,
					MatchPrefix: "2001:db8::/32", SubnetPriority: []string{"main"}}
				n.Uplinks["wan1"] = config.NPTv6Uplink{PrefixSource: config.SourceRoute,
					MatchPrefix: "2a02:1234::/32", SubnetPriority: []string{"main"}}
			},
		},
		{
			name: "two ra sources need no match_prefix",
			mutate: func(n *config.NPTv6) {
				n.Uplinks["wan0"] = config.NPTv6Uplink{PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}}
				n.Uplinks["wan1"] = config.NPTv6Uplink{PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := base()
			c.mutate(n)
			_, err := Resolve(&config.Config{NPTv6: n})

			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("Resolve returned %v, want no error", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("Resolve returned no error, want one containing %q", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("Resolve error = %q, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestResolveSkipsWhenDisabled(t *testing.T) {
	for _, c := range []*config.Config{
		{NPTv6: &config.NPTv6{Enable: false}},
		{},
	} {
		opts, err := Resolve(c)
		if err != nil {
			t.Errorf("Resolve on a disabled block returned %v, want nil", err)
		}
		if opts.Enabled {
			t.Errorf("Resolve on a disabled block reported Enabled")
		}
		if opts.Table == "" {
			t.Errorf("Resolve on a disabled block left the table unnamed")
		}
	}
}

func TestResolveSortsUplinks(t *testing.T) {
	opts := resolve(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan2": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}},
			"wan0": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}},
			"wan1": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}},
		},
	})

	var names []string
	for _, u := range opts.Uplinks {
		names = append(names, u.Name)
	}
	if got := strings.Join(names, ","); got != "wan0,wan1,wan2" {
		t.Errorf("Uplinks = %s, want wan0,wan1,wan2", got)
	}
}
