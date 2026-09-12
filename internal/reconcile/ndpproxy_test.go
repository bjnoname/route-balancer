package reconcile

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/ndp"
)

func boolPtr(b bool) *bool { return &b }

func tetherConfig() *config.NPTv6 {
	return &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets: map[string]config.NPTv6Subnet{
			"main":  {Prefix: "fd00:dead:beef:1::/64"},
			"guest": {Prefix: "fd00:dead:beef:2::/64"},
		},
		Uplinks: map[string]config.NPTv6Uplink{
			"eth1": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main", "guest"}},
			"eth3": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:a000::/56",
				SubnetPriority: []string{"main", "guest"}},
		},
	}
}

func TestProxyNDPEnabledDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		uplink config.NPTv6Uplink
		want   bool
	}{
		{"ra defaults on", config.NPTv6Uplink{PrefixSource: config.SourceRA}, true},
		{"route defaults off", config.NPTv6Uplink{PrefixSource: config.SourceRoute}, false},
		{"unset source defaults off", config.NPTv6Uplink{}, false},
		{"static defaults off", config.NPTv6Uplink{PrefixSource: config.SourceStatic}, false},
		{"explicit off beats ra", config.NPTv6Uplink{PrefixSource: config.SourceRA, ProxyNDP: boolPtr(false)}, false},
		{"explicit on beats route", config.NPTv6Uplink{PrefixSource: config.SourceRoute, ProxyNDP: boolPtr(true)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.uplink.ProxyNDPEnabled(); got != tc.want {
				t.Errorf("proxyNDPEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNdpOptsResolvesProxyingUplinksOnly(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, tetherConfig())

	opts := d.r.Nd
	if !opts.Enabled() {
		t.Fatal("the resolved ndp options Enabled() = false with an ra uplink configured")
	}
	if got := opts.UplinkNames(); len(got) != 1 || got[0] != "eth1" {
		t.Fatalf("UplinkNames() = %v, want [eth1]", got)
	}

	var got []string
	for _, s := range opts.Uplinks[0].Subnets {
		got = append(got, s.Name+"="+s.Prefix.String())
	}
	want := "main=fd00:dead:beef:1::/64,guest=fd00:dead:beef:2::/64"
	if strings.Join(got, ",") != want {
		t.Errorf("subnets = %v, want %s", got, want)
	}

	n := tetherConfig()
	n.Uplinks["eth1"] = config.NPTv6Uplink{PrefixSource: config.SourceRA, ProxyNDP: boolPtr(false),
		SubnetPriority: []string{"main"}}
	d.useNPTv6(t, n)

	if d.r.Nd.Enabled() {
		t.Error("the resolved ndp options Enabled() = true with every uplink opted out")
	}
	if got := d.r.Nd.LearnRuleset(); got != "" {
		t.Errorf("LearnRuleset() = %q, want empty", got)
	}
}

func TestNdpDefaultsAndDurations(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, tetherConfig())

	opts := d.r.Nd
	if opts.MaxHosts != 256 {
		t.Errorf("MaxHosts = %d, want 256", opts.MaxHosts)
	}
	if opts.Timeout != time.Hour {
		t.Errorf("Timeout = %v, want 1h", opts.Timeout)
	}
	if opts.Table != "route-balancer-nptv6-ndp" {
		t.Errorf("Table = %q", opts.Table)
	}

	n := tetherConfig()
	n.ProxyNDPMaxHosts = 32
	n.ProxyNDPTimeout = config.Duration(10 * time.Minute)
	n.NftablesTable = "custom"
	d.useNPTv6(t, n)

	opts = d.r.Nd
	if opts.MaxHosts != 32 {
		t.Errorf("MaxHosts = %d, want 32", opts.MaxHosts)
	}
	if opts.Timeout != 10*time.Minute {
		t.Errorf("Timeout = %v, want 10m", opts.Timeout)
	}
	if opts.Table != "custom-ndp" {
		t.Errorf("Table = %q, want custom-ndp", opts.Table)
	}
}

func entryStrings(m map[ndp.ProxyEntry]struct{}) []string {
	var out []string
	for _, e := range ndp.SortedEntries(m) {
		out = append(out, e.Uplink+"="+e.Addr)
	}
	return out
}

func TestDesiredProxyEntriesFollowTheLease(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, tetherConfig())

	d.state.Prefixes.Leases["eth1"] = leaseFor(t, "eth1", "2001:db8:ff02::/64")
	d.state.Prefixes.Leases["eth3"] = leaseFor(t, "eth3", "2001:db8:a000::/56")

	d.state.Learn.Hosts = []ndp.LearnedHost{
		{Addr: net.ParseIP("fd00:dead:beef:1::10"), Uplink: "eth1"},
		{Addr: net.ParseIP("fd00:dead:beef:1::20"), Uplink: "eth1"},
		{Addr: net.ParseIP("fd00:dead:beef:2::10"), Uplink: "eth1"},
		{Addr: net.ParseIP("fd00:dead:beef:1::30"), Uplink: "eth3"},
	}

	entriesNow := func() []string { return entryStrings(Desired(d.r, &d.state).ProxyNeigh) }

	want := []string{"eth1=2001:db8:ff02::10", "eth1=2001:db8:ff02::20"}
	if got := entriesNow(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", got, want)
	}

	d.state.Prefixes.Leases["eth1"] = leaseFor(t, "eth1", "2001:db8:ff09::/64")
	want = []string{"eth1=2001:db8:ff09::10", "eth1=2001:db8:ff09::20"}
	if got := entriesNow(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries after renumber = %v, want %v", got, want)
	}

	d.state.Prefixes.Leases["eth1"] = leaseFor(t, "eth1", "2001:db8:ff09::/60")
	want = []string{"eth1=2001:db8:ff09:1::10", "eth1=2001:db8:ff09::10", "eth1=2001:db8:ff09::20"}
	if got := entriesNow(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries after growth = %v, want %v", got, want)
	}

	delete(d.state.Prefixes.Leases, "eth1")
	if got := entriesNow(); len(got) != 0 {
		t.Errorf("entries with no lease = %v, want none", got)
	}
}

func TestDesiredProxyEntriesSkipsUnhealthyUplink(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, tetherConfig())

	d.useGatewayConfig(t, map[string]config.Gateway{"eth1": {Weight: 1}})

	d.state.Prefixes.Leases["eth1"] = leaseFor(t, "eth1", "2001:db8:ff02::/64")
	d.state.Learn.Hosts = []ndp.LearnedHost{{Addr: net.ParseIP("fd00:dead:beef:1::10"), Uplink: "eth1"}}

	if got := Desired(d.r, &d.state).ProxyNeigh; len(got) != 0 {
		t.Errorf("entries for an unhealthy uplink = %v, want none", entryStrings(got))
	}
}

func TestProxyEntriesFollowTheLearnedSet(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, tetherConfig())

	d.state.Prefixes.Leases["eth1"] = leaseFor(t, "eth1", "2001:db8:ff02::/64")

	d.state.Learn.Hosts = nil
	if got := Desired(d.r, &d.state).ProxyNeigh; len(got) != 0 {
		t.Errorf("entries with nothing learned = %v, want none", entryStrings(got))
	}

	d.state.Learn.Hosts = []ndp.LearnedHost{{Addr: net.ParseIP("fd00:dead:beef:1::10"), Uplink: "eth1"}}
	want := []string{"eth1=2001:db8:ff02::10"}
	if got := entryStrings(Desired(d.r, &d.state).ProxyNeigh); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", got, want)
	}
}

func TestProxyNDPKnobIsRepairedWhenItDrifts(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, tetherConfig())

	d.state.Links.ProxyNDP = map[string]bool{"eth1": false}

	run := useRunner()
	run.RunAll(plan(d.proxyKnobResources()))

	want := []string{"sysctl net.ipv6.conf.eth1.proxy_ndp=1"}
	if got := run.Commands(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the knob pass ran\n\t%v\nwant\n\t%v", got, want)
	}

	d.state.Links.ProxyNDP = map[string]bool{"eth1": true}
	if acts := plan(d.proxyKnobResources()); len(acts) != 0 {
		t.Errorf("an uplink already answering planned %d actions, want none", len(acts))
	}
}

func TestProxyNDPKnobCoversOnlyProxyingUplinks(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, tetherConfig())

	var ids []string
	for _, r := range d.proxyKnobResources() {
		ids = append(ids, r.ID)
	}
	if want := []string{"proxy-ndp/eth1"}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("knob resources = %v, want %v", ids, want)
	}
}

func TestAClaimInFlightIsNotDrift(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useAsker(map[string]string{"ip -6 neigh show proxy": ""})
	d.useNPTv6(t, tetherConfig())

	d.state.Prefixes.Leases["eth1"] = leaseFor(t, "eth1", "2001:db8:ff02::/64")
	d.state.Learn.Hosts = []ndp.LearnedHost{
		{Addr: net.ParseIP("fd00:dead:beef:1::10"), Uplink: "eth1"},
	}

	entry := ndp.ProxyEntry{Addr: "2001:db8:ff02::10", Uplink: "eth1"}
	d.state.Claims.Probing[entry] = probing(ndp.ClaimScreened)

	if acts := d.proxyPass(Desired(d.r, &d.state)); len(acts) != 0 {
		t.Errorf("a claim being probed planned %d actions, want none", len(acts))
	}

	delete(d.state.Claims.Probing, entry)
	if acts := d.proxyPass(Desired(d.r, &d.state)); len(acts) == 0 {
		t.Error("an entry nothing is probing planned nothing, want a claim")
	}
}

func TestAnUnreadableNeighbourTableIsNotDrift(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useAsker(map[string]string{})
	d.useNPTv6(t, tetherConfig())

	d.state.Prefixes.Leases["eth1"] = leaseFor(t, "eth1", "2001:db8:ff02::/64")
	d.state.Learn.Hosts = []ndp.LearnedHost{
		{Addr: net.ParseIP("fd00:dead:beef:1::10"), Uplink: "eth1"},
	}

	if acts := d.proxyPass(Desired(d.r, &d.state)); len(acts) != 0 {
		t.Errorf("a failed neighbour read planned %d actions, want none", len(acts))
	}
}
