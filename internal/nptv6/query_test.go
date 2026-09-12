package nptv6

import (
	"net"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command/commandtest"
)

const nptTable = "nft list table ip6 nptv6"

func nptOptions(t *testing.T, out map[string]string) (Options, *commandtest.Asker) {
	t.Helper()
	o := closedOptions(t)
	o.Uplinks[0].AllowInbound = true
	return o, commandtest.New(out)
}

func closedOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Enabled:  true,
		Table:    "nptv6",
		Internal: mustCIDR(t, "fd00:dead:beef::/48"),
		Subnets:  map[string]*net.IPNet{"main": mustCIDR(t, "fd00:dead:beef:1::/64")},
		Uplinks: []Uplink{
			{Name: "wan0", SubnetPriority: []string{"main"}},
			{Name: "wan1", SubnetPriority: []string{"main"}},
		},
		LeakProtection: true,
	}
}

func oneMapping(t *testing.T) []Assignment {
	t.Helper()
	return []Assignment{{
		Uplink:   "wan0",
		Subnet:   "main",
		Internal: mustCIDR(t, "fd00:dead:beef:1::/64"),
		External: mustCIDR(t, "2001:db8:1::/64"),
	}}
}

const installed = `table ip6 nptv6 {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		ip6 daddr 2001:db8:1::/64 iifname "wan0" dnat prefix to fd00:dead:beef:1::/64 comment "rb-nptv6 wan0 main"
	}

	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" snat prefix to 2001:db8:1::/64 comment "rb-nptv6 wan0 main"
	}

	chain guard {
		type filter hook postrouting priority srcnat + 10; policy accept;
		ip6 saddr fd00:dead:beef::/48 oifname "wan1" drop comment "rb-nptv6-guard wan1"
	}
}`

func TestNeedsRestore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		out  map[string]string
		want bool
	}{
		{
			name: "the kernel holds exactly the mappings and guards wanted",
			out:  map[string]string{nptTable: installed},
		},
		{
			name: "the table is gone",
			out:  map[string]string{},
			want: true,
		},
		{
			name: "the dnat half was deleted",
			out: map[string]string{nptTable: `table ip6 nptv6 {
	chain postrouting {
		ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" snat prefix to 2001:db8:1::/64
	}
	chain guard {
		ip6 saddr fd00:dead:beef::/48 oifname "wan1" drop
	}
}`},
			want: true,
		},
		{
			name: "the guard rule was deleted",
			out: map[string]string{nptTable: `table ip6 nptv6 {
	chain prerouting {
		ip6 daddr 2001:db8:1::/64 iifname "wan0" dnat prefix to fd00:dead:beef:1::/64
	}
	chain postrouting {
		ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" snat prefix to 2001:db8:1::/64
	}
}`},
			want: true,
		},
		{
			name: "the rules translate into a prefix the uplink no longer holds",
			out: map[string]string{nptTable: `table ip6 nptv6 {
	chain prerouting {
		ip6 daddr 2001:db8:ffff::/64 iifname "wan0" dnat prefix to fd00:dead:beef:1::/64
	}
	chain postrouting {
		ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" snat prefix to 2001:db8:ffff::/64
	}
	chain guard {
		ip6 saddr fd00:dead:beef::/48 oifname "wan1" drop
	}
}`},
			want: true,
		},
		{
			name: "a rule nobody asked for was added",
			out: map[string]string{nptTable: installed + `
	chain postrouting {
		ip6 saddr fd00:dead:beef:2::/64 oifname "wan0" snat prefix to 2001:db8:1::/64
	}`},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, a := nptOptions(t, c.out)
			want := o.DesiredRules(oneMapping(t))
			if got := o.NeedsRestore(want, o.ReadInstalledTable(a.Answers(o.Queries()...))); got != c.want {
				t.Errorf("NeedsRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNeedsRestoreFollowsTheInboundOption(t *testing.T) {
	t.Parallel()

	const closedTable = `table ip6 nptv6 {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" snat prefix to 2001:db8:1::/64 comment "rb-nptv6 wan0 main"
	}

	chain inbound {
		type filter hook prerouting priority dstnat - 10; policy accept;
		ip6 daddr 2001:db8:1::/64 iifname "wan0" ct state new counter packets 0 bytes 0 drop comment "rb-nptv6-closed wan0 main"
	}

	chain guard {
		type filter hook postrouting priority srcnat + 10; policy accept;
		ip6 saddr fd00:dead:beef::/48 oifname "wan1" drop comment "rb-nptv6-guard wan1"
	}
}`

	cases := []struct {
		name    string
		inbound bool
		out     string
		want    bool
	}{
		{name: "closed, and the kernel holds the drop", out: closedTable},
		{name: "closed, but the kernel still holds the dnat", out: installed, want: true},
		{name: "open, and the kernel holds the dnat", inbound: true, out: installed},
		{name: "open, but the kernel still holds the drop", inbound: true, out: closedTable, want: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := closedOptions(t)
			o.Uplinks[0].AllowInbound = c.inbound
			a := commandtest.New(map[string]string{nptTable: c.out})

			want := o.DesiredRules(oneMapping(t))
			if got := o.NeedsRestore(want, o.ReadInstalledTable(a.Answers(o.Queries()...))); got != c.want {
				t.Errorf("NeedsRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestGeneratedRulesetParsesBackToTheDesiredKeys(t *testing.T) {
	t.Parallel()

	for _, inbound := range []bool{false, true} {
		o := closedOptions(t)
		o.Uplinks[0].AllowInbound = inbound

		assignments := oneMapping(t)
		want := o.DesiredRules(assignments)
		got := parseRules(o.Ruleset(assignments))

		if len(got) != len(want) {
			t.Fatalf("inbound=%v: parsed %d rules, want %d:\ngot  %+v\nwant %+v",
				inbound, len(got), len(want), got, want)
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				t.Errorf("inbound=%v: rule %+v not recovered: %+v", inbound, k, got)
			}
		}
	}
}

func TestNeedsRestoreWhenDisabledReadsNothing(t *testing.T) {
	t.Parallel()

	a := commandtest.New(map[string]string{nptTable: installed})
	o := Options{Table: "nptv6"}

	if o.NeedsRestore(map[RuleKey]struct{}{}, o.ReadInstalledTable(a.Answers(o.Queries()...))) {
		t.Errorf("NeedsRestore reported drift with translation disabled")
	}
	if len(a.Asked) != 0 {
		t.Errorf("read %v with translation disabled, want no read at all", a.Asked)
	}
}
