package ndp

import (
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/nptv6"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

func tetherOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Table:    "route-balancer-nptv6-ndp",
		Proto:    "111",
		MaxHosts: 256,
		Timeout:  time.Hour,
		Uplinks: []Uplink{{
			Name: "eth1",
			Subnets: []Subnet{
				{Name: "main", Prefix: mustCIDR(t, "fd00:dead:beef:1::/64")},
				{Name: "guest", Prefix: mustCIDR(t, "fd00:dead:beef:2::/64")},
			},
		}},
	}
}

func TestEnabledIsJustHavingUplinks(t *testing.T) {
	if (Options{Table: "t"}).Enabled() {
		t.Error("Options with no uplinks reported Enabled")
	}
	if got := (Options{Table: "t"}).LearnRuleset(); got != "" {
		t.Errorf("LearnRuleset() = %q with no uplinks, want empty", got)
	}
	if !tetherOptions(t).Enabled() {
		t.Error("Options with a proxying uplink reported not Enabled")
	}
}

func TestNftTimeout(t *testing.T) {
	cases := map[time.Duration]string{
		time.Hour:                            "3600s",
		90 * time.Second:                     "90s",
		time.Millisecond:                     "1s",
		2*time.Minute + 500*time.Millisecond: "121s",
	}
	for d, want := range cases {
		if got := nftTimeout(d); got != want {
			t.Errorf("nftTimeout(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestLearnRuleset(t *testing.T) {
	o := tetherOptions(t)
	o.MaxHosts = 64
	o.Timeout = 30 * time.Minute

	got := o.LearnRuleset()

	for _, want := range []string{
		`ip6 saddr fd00:dead:beef:1::/64 oifname "eth1" update @learned { ip6 saddr . meta oifname timeout 1800s } comment "rb-ndp eth1 main"`,
		`ip6 saddr fd00:dead:beef:2::/64 oifname "eth1" update @learned { ip6 saddr . meta oifname timeout 1800s } comment "rb-ndp eth1 guest"`,
		"size 64",
		"flags dynamic,timeout",
		"type filter hook forward priority filter",
		"delete table ip6 route-balancer-nptv6-ndp",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ruleset missing %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "2001:db8") {
		t.Errorf("ruleset matches on an external prefix:\n%s", got)
	}
}

const listedLearnTable = `table ip6 route-balancer-nptv6-ndp {
	set learned {
		type ipv6_addr . ifname
		size 256
		flags dynamic,timeout
		timeout 1h
	}

	chain learn {
		type filter hook forward priority filter; policy accept;
		ip6 saddr fd00:dead:beef:1::/64 oifname "eth1" update @learned { ip6 saddr . oifname timeout 1h } comment "rb-ndp eth1 main"
		ip6 saddr fd00:dead:beef:2::/64 oifname "eth1" update @learned { ip6 saddr . oifname timeout 1h } comment "rb-ndp eth1 guest"
	}
}`

func TestParseLearnRulesMatchesDesired(t *testing.T) {
	got := parseLearnRules(listedLearnTable)
	want := tetherOptions(t).DesiredLearnRules()

	if len(got) != len(want) {
		t.Fatalf("parsed %d rules, want %d: %v vs %v", len(got), len(want), got, want)
	}
	for r := range want {
		if _, ok := got[r]; !ok {
			t.Errorf("rule %+v missing from parsed output", r)
		}
	}

	partial := strings.Replace(listedLearnTable,
		`ip6 saddr fd00:dead:beef:2::/64 oifname "eth1" update @learned { ip6 saddr . oifname timeout 1h } comment "rb-ndp eth1 guest"`, "", 1)
	if len(parseLearnRules(partial)) != 1 {
		t.Error("a removed rule was not noticed")
	}
}

func TestParseLearnedHosts(t *testing.T) {
	const listed = `table ip6 route-balancer-nptv6-ndp {
	set learned {
		type ipv6_addr . ifname
		size 256
		flags dynamic,timeout
		timeout 1h
		elements = { fd00:dead:beef:1::10 . "eth1" expires 59m59s989ms,
			     fd00:dead:beef:1::20 . "eth1" expires 12m3s,
			     fd00:dead:beef:2::10 . "eth1" expires 1s }
	}
}`

	hosts := parseLearnedHosts(listed)
	if len(hosts) != 3 {
		t.Fatalf("parsed %d hosts, want 3: %+v", len(hosts), hosts)
	}
	var got []string
	for _, h := range hosts {
		if h.Uplink != "eth1" {
			t.Errorf("host %v attributed to %q", h.Addr, h.Uplink)
		}
		got = append(got, h.Addr.String())
	}
	sort.Strings(got)
	if want := "fd00:dead:beef:1::10,fd00:dead:beef:1::20,fd00:dead:beef:2::10"; strings.Join(got, ",") != want {
		t.Errorf("hosts = %v, want %s", got, want)
	}

	if h := parseLearnedHosts(strings.Split(listed, "elements")[0]); len(h) != 0 {
		t.Errorf("empty set parsed as %+v", h)
	}
	if h := parseLearnedHosts(""); len(h) != 0 {
		t.Errorf("empty output parsed as %+v", h)
	}
}

func TestExternalAddr(t *testing.T) {
	cases := []struct {
		external string
		host     string
		want     string
	}{
		{"2001:db8:ff02::/64", "fd00:dead:beef:1::10", "2001:db8:ff02::10"},
		{"2001:db8:a000:1::/64", "fd00:dead:beef:2::dead:beef", "2001:db8:a000:1::dead:beef"},
		{"2001:db8:ff02::/64", "fd00:dead:beef:1:1122:3344:5566:7788", "2001:db8:ff02:0:1122:3344:5566:7788"},
	}
	for _, tc := range cases {
		got := externalAddr(mustCIDR(t, tc.external), net.ParseIP(tc.host))
		if got == nil || got.String() != tc.want {
			t.Errorf("externalAddr(%s, %s) = %v, want %s", tc.external, tc.host, got, tc.want)
		}
	}

	if got := externalAddr(nil, net.ParseIP("fd00::1")); got != nil {
		t.Errorf("externalAddr(nil, …) = %v, want nil", got)
	}
}

func entryStrings(m map[ProxyEntry]struct{}) []string {
	var out []string
	for _, e := range SortedEntries(m) {
		out = append(out, e.Uplink+"="+e.Addr)
	}
	return out
}

func TestDesiredEntriesCoversOnlyProxyingUplinks(t *testing.T) {
	o := tetherOptions(t)

	assignments := []nptv6.Assignment{
		{Uplink: "eth1", Subnet: "main",
			Internal: mustCIDR(t, "fd00:dead:beef:1::/64"),
			External: mustCIDR(t, "2001:db8:ff02::/64")},
		{Uplink: "eth3", Subnet: "main",
			Internal: mustCIDR(t, "fd00:dead:beef:1::/64"),
			External: mustCIDR(t, "2001:db8:a000::/64")},
	}
	hosts := []LearnedHost{
		{Addr: net.ParseIP("fd00:dead:beef:1::10"), Uplink: "eth1"},
		{Addr: net.ParseIP("fd00:dead:beef:1::20"), Uplink: "eth1"},
		{Addr: net.ParseIP("fd00:dead:beef:2::10"), Uplink: "eth1"},
		{Addr: net.ParseIP("fd00:dead:beef:1::30"), Uplink: "eth3"},
	}

	got := entryStrings(o.DesiredEntries(assignments, hosts))
	want := []string{"eth1=2001:db8:ff02::10", "eth1=2001:db8:ff02::20"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", got, want)
	}

	if got := o.DesiredEntries(nil, hosts); len(got) != 0 {
		t.Errorf("entries with no assignments = %v, want none", entryStrings(got))
	}
}

func TestParseProxyNeigh(t *testing.T) {
	const listed = `2001:db8:ff02::10 dev eth1 proxy proto 111
2001:db8:ff02::20 dev eth1 proxy proto 111
2001:db8:ff02::99 dev eth1 proxy
fe80::1 dev eth2 proxy proto 111
10.0.0.1 dev eth1 proxy proto 111
`
	got := parseProxyNeigh(listed, "111")

	want := []string{"eth1=2001:db8:ff02::10", "eth1=2001:db8:ff02::20", "eth2=fe80::1"}
	if strings.Join(entryStrings(got), ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", entryStrings(got), want)
	}

	if got := parseProxyNeigh(listed, "112"); len(got) != 0 {
		t.Errorf("entries under another proto = %v, want none", entryStrings(got))
	}
}

func TestParseNeighbors(t *testing.T) {
	const listed = `2001:db8:ff02::1 lladdr 12:34:56:78:9a:bc router REACHABLE
2001:db8:ff02::2 lladdr 12:34:56:78:9a:bd STALE
2001:db8:ff02::3  FAILED
2001:db8:ff02::4 lladdr 12:34:56:78:9a:be INCOMPLETE
2001:db8:ff02::5  proxy
2001:0db8:ff02:0000::6 lladdr 12:34:56:78:9a:bf REACHABLE
`
	got := parseNeighbors(listed)

	for _, addr := range []string{"2001:db8:ff02::1", "2001:db8:ff02::2", "2001:db8:ff02::6"} {
		if !got[addr] {
			t.Errorf("%s: resolved neighbour not reported", addr)
		}
	}
	for _, addr := range []string{"2001:db8:ff02::3", "2001:db8:ff02::4", "2001:db8:ff02::5"} {
		if got[addr] {
			t.Errorf("%s: reported as a resolved neighbour", addr)
		}
	}
	if len(got) != 3 {
		t.Errorf("parsed %d neighbours, want 3: %v", len(got), got)
	}
}

func TestSortedClaims(t *testing.T) {
	in := []Claim{
		{Entry: ProxyEntry{Addr: "2001:db8::20", Uplink: "eth3"}},
		{Entry: ProxyEntry{Addr: "2001:db8::20", Uplink: "eth1"}},
		{Entry: ProxyEntry{Addr: "2001:db8::10", Uplink: "eth1"}},
	}
	got := SortedClaims(in)

	var out []string
	for _, c := range got {
		out = append(out, c.Entry.Uplink+"="+c.Entry.Addr)
	}
	want := "eth1=2001:db8::10,eth1=2001:db8::20,eth3=2001:db8::20"
	if strings.Join(out, ",") != want {
		t.Errorf("SortedClaims = %v, want %s", out, want)
	}

	if in[0].Entry.Uplink != "eth3" {
		t.Error("SortedClaims sorted its argument in place")
	}
}
