package prefix

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/netlink"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

func driveSource(t *testing.T, s Source, obs ...netlink.Observation) []Change {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reported := make(chan Change, len(obs)+1)
	in := make(chan netlink.Observation, len(obs)+1)
	go s.start(ctx, in, func(c Change) { reported <- c })

	for _, o := range obs {
		in <- o
	}

	var collected []Change
	for {
		select {
		case c := <-reported:
			collected = append(collected, c)
		case <-time.After(100 * time.Millisecond):
			return collected
		}
	}
}

func TestRouteSourceAttributesByAggregate(t *testing.T) {
	wan0 := newRouteSource("wan0", mustCIDR(t, "2001:db8::/32"))
	wan1 := newRouteSource("wan1", mustCIDR(t, "2a02:1234::/32"))

	theirs := netlink.Observation{Kind: netlink.ObsRoute, Prefix: net.ParseIP("2a02:1234:5678::"), Length: 56}
	mine := netlink.Observation{Kind: netlink.ObsRoute, Prefix: net.ParseIP("2001:db8:abcd::"), Length: 56}

	if changes := driveSource(t, wan0, theirs); len(changes) != 0 {
		t.Errorf("wan0 claimed a prefix from wan1's aggregate: %v", changes)
	}

	changes := driveSource(t, wan0, mine)
	if len(changes) != 1 {
		t.Fatalf("wan0 reported %d changes for its own aggregate, want 1", len(changes))
	}
	if changes[0].Lease.Uplink != "wan0" || changes[0].Lease.Length != 56 {
		t.Errorf("lease = %+v, want wan0 at /56", changes[0].Lease)
	}

	if changes := driveSource(t, wan1, theirs); len(changes) != 1 {
		t.Errorf("wan1 reported %d changes for its own aggregate, want 1", len(changes))
	}
}

func TestRouteSourceWithoutAggregateTakesAny(t *testing.T) {
	s := newRouteSource("wan0", nil)
	obs := netlink.Observation{Kind: netlink.ObsRoute, Prefix: net.ParseIP("2a02:1234:5678::"), Length: 56}

	if changes := driveSource(t, s, obs); len(changes) != 1 {
		t.Errorf("reported %d changes, want 1", len(changes))
	}
}

func TestRouteSourceIgnoresRAObservations(t *testing.T) {
	s := newRouteSource("wan0", nil)
	obs := netlink.Observation{Kind: netlink.ObsRA, Prefix: net.ParseIP("2001:db8:1:2::"), Length: 64, IfName: "wan0"}

	if changes := driveSource(t, s, obs); len(changes) != 0 {
		t.Errorf("a route source consumed an RA observation: %v", changes)
	}
}

func TestRASourceAttributesByInterface(t *testing.T) {
	s := newRASource("wan1", nil)

	onLink := netlink.Observation{Kind: netlink.ObsRA, Prefix: net.ParseIP("2001:db8:1:2::"), Length: 64, IfName: "wan1"}
	elsewhere := netlink.Observation{Kind: netlink.ObsRA, Prefix: net.ParseIP("2001:db8:3:4::"), Length: 64, IfName: "wan2"}

	changes := driveSource(t, s, onLink)
	if len(changes) != 1 {
		t.Fatalf("reported %d changes for an RA on its own link, want 1", len(changes))
	}
	if changes[0].Lease.Source != config.SourceRA {
		t.Errorf("lease source = %q, want %q", changes[0].Lease.Source, config.SourceRA)
	}

	if changes := driveSource(t, s, elsewhere); len(changes) != 0 {
		t.Errorf("an RA source claimed an RA from another link: %v", changes)
	}
}

func TestRASourceStopsSeedingOnceWithdrawn(t *testing.T) {
	s := newRASource("wan0", nil)

	if !s.seedable(false) {
		t.Fatal("a fresh source refuses to seed, which would break restart recovery")
	}
	if s.seedable(true) {
		t.Error("a withdrawn uplink is still seedable, so a lingering address will resurrect it")
	}
}

func TestOtherSourcesSeedRegardless(t *testing.T) {
	route := newRouteSource("wan0", nil)
	static := &staticSource{iface: "tun0", prefix: mustCIDR(t, "2001:db8:abcd::/56")}

	for _, withdrawn := range []bool{false, true} {
		if !route.seedable(withdrawn) {
			t.Errorf("a route source refused to seed with withdrawn=%v", withdrawn)
		}
		if !static.seedable(withdrawn) {
			t.Errorf("a static source refused to seed with withdrawn=%v", withdrawn)
		}
	}
}

func TestStaticSourceAssertsItsPrefix(t *testing.T) {
	s := &staticSource{iface: "tun0", prefix: mustCIDR(t, "2001:db8:abcd::/56")}

	l, ok := s.standing()
	if !ok {
		t.Fatal("a static source has no standing lease")
	}
	if l.Length != 56 || !l.Prefix.Equal(net.ParseIP("2001:db8:abcd::")) {
		t.Errorf("lease = %+v, want 2001:db8:abcd::/56", l)
	}
	if l.Uplink != "tun0" {
		t.Errorf("uplink = %q, want tun0", l.Uplink)
	}
}

func TestStaticSourceClaimsNoObservation(t *testing.T) {
	s := &staticSource{iface: "tun0", prefix: mustCIDR(t, "2001:db8:abcd::/56")}
	obs := netlink.Observation{Kind: netlink.ObsRoute, Prefix: net.ParseIP("2001:db8::"), Length: 48}

	if _, mine := s.attribute(obs); mine {
		t.Error("a static source attributed a discard route to itself")
	}
}

func TestStaticSourceDrainsObservations(t *testing.T) {
	s := &staticSource{iface: "tun0", prefix: mustCIDR(t, "2001:db8:abcd::/56")}
	obs := netlink.Observation{Kind: netlink.ObsRoute, Prefix: net.ParseIP("2001:db8::"), Length: 48}

	if changes := driveSource(t, s, obs, obs, obs); len(changes) != 0 {
		t.Errorf("reported %v, want nothing from a static source's netlink path", changes)
	}
}

func TestAttributeResolvesOneLeasePerUplink(t *testing.T) {
	ss := Sources{
		newRouteSource("wan0", mustCIDR(t, "2001:db8::/32")),
		newRouteSource("wan1", mustCIDR(t, "2a02:1234::/32")),
		&staticSource{iface: "tun0", prefix: mustCIDR(t, "2001:db8:f00d::/56")},
	}

	got := ss.Attribute([]netlink.Observation{
		{Kind: netlink.ObsRoute, Prefix: net.ParseIP("2001:db8:abcd::"), Length: 56},
	}, nil)

	if l, ok := got["wan0"]; !ok || l.Length != 56 || !l.Prefix.Equal(net.ParseIP("2001:db8:abcd::")) {
		t.Errorf("wan0 = %+v/%v, want the observed delegation", l, ok)
	}
	if l, ok := got["wan1"]; ok {
		t.Errorf("wan1 = %+v, want no lease at all", l)
	}
	if l, ok := got["tun0"]; !ok || l.Length != 56 {
		t.Errorf("tun0 = %+v/%v, want its standing lease", l, ok)
	}
}

func TestAttributeSkipsAWithdrawnSource(t *testing.T) {
	s := newRASource("wan1", nil)
	obs := []netlink.Observation{
		{Kind: netlink.ObsRA, Prefix: net.ParseIP("2001:db8:1:2::"), Length: 64, IfName: "wan1"},
	}

	if got := (Sources{s}).Attribute(obs, nil); len(got) != 1 {
		t.Fatalf("a fresh source resolved %d leases, want 1", len(got))
	}

	withdrawn := map[string]bool{"wan1": true}
	if got := (Sources{s}).Attribute(obs, withdrawn); len(got) != 0 {
		t.Errorf("a withdrawn source resolved %v, want nothing", got)
	}

	if got := (Sources{s}).Attribute(obs, map[string]bool{"somewan": true}); len(got) != 1 {
		t.Errorf("another uplink's withdrawal suppressed this one: %v", got)
	}
}

func TestBuildSources(t *testing.T) {
	sources, err := BuildSources(&config.NPTv6{
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRoute, MatchPrefix: "2001:db8::/32"},
			"wan1": {PrefixSource: config.SourceRA},
			"tun0": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:f00d::/56"},
		},
	})
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if len(sources) != 3 {
		t.Fatalf("built %d sources, want 3", len(sources))
	}

	want := []struct{ uplink, source string }{
		{"tun0", config.SourceStatic}, {"wan0", config.SourceRoute}, {"wan1", config.SourceRA},
	}
	for i, w := range want {
		if sources[i].uplink() != w.uplink || sources[i].name() != w.source {
			t.Errorf("sources[%d] = %s/%s, want %s/%s",
				i, sources[i].uplink(), sources[i].name(), w.uplink, w.source)
		}
	}
	if got := sources.Uplinks(); len(got) != 3 || got[0] != "tun0" {
		t.Errorf("Uplinks() = %v, want them in the same sorted order", got)
	}
}

func TestBuildSourcesDefaultsToRoute(t *testing.T) {
	sources, err := BuildSources(&config.NPTv6{
		Uplinks: map[string]config.NPTv6Uplink{"wan0": {}},
	})
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	if len(sources) != 1 || sources[0].name() != config.SourceRoute {
		t.Fatalf("sources = %v, want a single route source", sources)
	}
}

func TestBuildSourcesWithNoBlock(t *testing.T) {
	sources, err := BuildSources(nil)
	if err != nil || len(sources) != 0 {
		t.Errorf("BuildSources(nil) = %v, %v; want no sources and no error", sources, err)
	}
}

func TestParseObservedPrefix(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		length int
		ok     bool
	}{
		{in: "2001:db8:abcd::/56", want: "2001:db8:abcd::", length: 56, ok: true},
		{in: "2001:db8:1:2::/64", want: "2001:db8:1:2::", length: 64, ok: true},
		{in: "2001:db8::1/128", ok: false},
		{in: "::/0", ok: false},
		{in: "10.0.0.0/8", ok: false},
		{in: "not-a-prefix", ok: false},
	}

	for _, c := range cases {
		ip, length, ok := parseObservedPrefix(c.in)
		if ok != c.ok {
			t.Errorf("parseObservedPrefix(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if !ip.Equal(net.ParseIP(c.want)) || length != c.length {
			t.Errorf("parseObservedPrefix(%q) = %v/%d, want %s/%d", c.in, ip, length, c.want, c.length)
		}
	}
}

func TestFreshestRAPrefix(t *testing.T) {
	const renumbering = `2: eth1    inet6 2001:db8:ff02:0:5054:ff:fe12:102/64 scope global dynamic mngtmpaddr proto kernel_ra \       valid_lft 299sec preferred_lft 149sec
2: eth1    inet6 2001:db8:ff00:0:5054:ff:fe12:102/64 scope global dynamic mngtmpaddr proto kernel_ra \       valid_lft 42sec preferred_lft 0sec
`

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single address",
			in:   "2: eth1    inet6 2001:db8:ff00:0:5054:ff:fe12:102/64 scope global dynamic \\       valid_lft 299sec preferred_lft 149sec\n",
			want: "2001:db8:ff00::",
		},
		{
			name: "renumbering, both prefixes present",
			in:   renumbering,
			want: "2001:db8:ff02::",
		},
		{
			name: "renumbering where the deprecated prefix has the longer valid lifetime",
			in: "2: eth1    inet6 2001:db8:beef:0:5054:ff:fe12:102/64 scope global dynamic \\       valid_lft 600sec preferred_lft 600sec\n" +
				"2: eth1    inet6 2001:db8:dead:0:5054:ff:fe12:102/64 scope global dynamic \\       valid_lft 7000sec preferred_lft 0sec\n",
			want: "2001:db8:beef::",
		},
		{
			name: "only deprecated addresses yields nothing",
			in: "2: eth1    inet6 2001:db8:1:0:5054:ff:fe12:102/64 scope global dynamic \\       valid_lft 30sec preferred_lft 0sec\n" +
				"2: eth1    inet6 2001:db8:2:0:5054:ff:fe12:102/64 scope global dynamic \\       valid_lft 90sec preferred_lft 0sec\n",
			want: "",
		},
		{
			name: "a static address outlives any lease",
			in:   "2: eth1    inet6 2001:db8:ff00:0:5054:ff:fe12:102/64 scope global dynamic \\       valid_lft forever preferred_lft forever\n",
			want: "2001:db8:ff00::",
		},
		{
			name: "link-local only",
			in:   "2: eth1    inet6 fe80::5054:ff:fe12:102/64 scope link \\       valid_lft forever preferred_lft forever\n",
			want: "",
		},
		{name: "no addresses at all", in: "", want: ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := freshestRAPrefix(c.in)
			if c.want == "" {
				if ok {
					t.Fatalf("freshestRAPrefix() = %v, want no prefix", got)
				}
				return
			}
			if !ok {
				t.Fatalf("freshestRAPrefix() found nothing, want %s", c.want)
			}
			if !got.Equal(net.ParseIP(c.want)) {
				t.Errorf("freshestRAPrefix() = %v, want %s", got, c.want)
			}
		})
	}
}

func TestRouteSourceTakesOnlyAGloballyRoutablePrefix(t *testing.T) {
	s := newRouteSource("wan0", nil)

	for _, tc := range []struct {
		name   string
		prefix string
		length int
		want   int
	}{
		{"a delegation", "2001:db8:abcd::", 56, 1},
		{"a parked ULA blackhole", "fd00::", 8, 0},
		{"a link-local aggregate", "fe80::", 10, 0},
		{"the default route", "::", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := netlink.Observation{
				Kind: netlink.ObsRoute, Prefix: net.ParseIP(tc.prefix), Length: tc.length,
			}
			if got := len(driveSource(t, s, obs)); got != tc.want {
				t.Errorf("%s produced %d changes, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestAttributeTakesTheLastOfSeveralCandidates(t *testing.T) {
	s := newRouteSource("wan0", nil)

	obs := []netlink.Observation{
		{Kind: netlink.ObsRoute, Prefix: net.ParseIP("2001:db8:abcd::"), Length: 56},
		{Kind: netlink.ObsRoute, Prefix: net.ParseIP("2a02:1234:5678::"), Length: 56},
	}

	got := Sources{s}.Attribute(obs, map[string]bool{})
	want := Lease{Uplink: "wan0", Prefix: net.ParseIP("2a02:1234:5678::"), Length: 56}
	if !got["wan0"].SameAs(want) {
		t.Errorf("attributed %v, want the last candidate %v", got["wan0"], want)
	}

	one := Sources{s}.Attribute(obs[:1], map[string]bool{})
	if !one["wan0"].SameAs(Lease{Prefix: net.ParseIP("2001:db8:abcd::"), Length: 56}) {
		t.Errorf("a single candidate attributed %v", one["wan0"])
	}
}
