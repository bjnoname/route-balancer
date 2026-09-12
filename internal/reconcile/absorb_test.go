package reconcile

import (
	"net"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/prefix"
)

func (d *testReconciler) useSources(t *testing.T, n *config.NPTv6) {
	t.Helper()
	sources, err := prefix.BuildSources(n)
	if err != nil {
		t.Fatalf("prefix.BuildSources: %v", err)
	}
	d.sources = sources
}

func TestResyncWithdrawsALeaseTheKernelNoLongerEvidences(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	n := &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}},
		},
	}
	d.useNPTv6(t, n)
	d.useSources(t, n)

	d.state.Prefixes.Leases = map[string]prefix.Lease{
		"wan0": {Uplink: "wan0", Prefix: net.ParseIP("2001:db8:abcd::"), Length: 64, Source: config.SourceRA},
	}

	before := Desired(d.r, &d.state).Nft[d.r.Npt.Table]

	d.absorbLeases(d.Observe(d.sources.Queries()))

	if l, ok := d.state.Prefixes.currentLease("wan0"); ok {
		t.Errorf("lease %v survived a resync that observed nothing, want it withdrawn", l)
	}
	if after := Desired(d.r, &d.state).Nft[d.r.Npt.Table]; after == before {
		t.Error("withdrawing a lease left the translation ruleset unchanged, so nothing would repair it")
	}
}

func TestResyncDoesNotRecordAWithdrawal(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	n := &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}},
		},
	}
	d.useNPTv6(t, n)
	d.useSources(t, n)

	d.state.Prefixes.Withdrawn = map[string]bool{}
	d.state.Prefixes.Leases = map[string]prefix.Lease{
		"wan0": {Uplink: "wan0", Prefix: net.ParseIP("2001:db8:abcd::"), Length: 64, Source: config.SourceRA},
	}

	d.absorbLeases(d.Observe(d.sources.Queries()))

	if _, ok := d.state.Prefixes.currentLease("wan0"); ok {
		t.Fatal("the re-read did not withdraw a lease it no longer evidences")
	}
	if d.state.Prefixes.Withdrawn["wan0"] {
		t.Error("the re-read recorded a withdrawal it inferred; the uplink can now only recover from an RA")
	}
}

func TestResyncKeepsAStandingLease(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	n := &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"tun0": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:abcd::/56", SubnetPriority: []string{"main"}},
		},
	}
	d.useNPTv6(t, n)
	d.useSources(t, n)
	d.state.Prefixes.Leases = map[string]prefix.Lease{}

	d.absorbLeases(d.Observe(d.sources.Queries()))

	l, ok := d.state.Prefixes.currentLease("tun0")
	if !ok {
		t.Fatal("a static uplink held no lease after a resync")
	}
	if l.Length != 56 || !l.Prefix.Equal(net.ParseIP("2001:db8:abcd::")) {
		t.Errorf("lease = %+v, want 2001:db8:abcd::/56", l)
	}

	first := Desired(d.r, &d.state).Nft[d.r.Npt.Table]
	d.absorbLeases(d.Observe(d.sources.Queries()))
	if second := Desired(d.r, &d.state).Nft[d.r.Npt.Table]; second != first {
		t.Error("an unchanged resync rendered a different ruleset, which would reset conntrack")
	}
}

func TestAbsorbDoesNotClearTheBurnList(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	e := ndp.ProxyEntry{Addr: "2001:db8::5054:ff:fe12:102", Uplink: "wan0"}
	d.recordConflict(e, "test")
	if !d.state.Claims.isBurned(e) {
		t.Fatal("recordConflict did not burn the entry")
	}

	a := d.Observe(d.sources.Queries())
	d.absorbLeases(a)
	d.absorbLearnedHosts(a)

	if !d.state.Claims.isBurned(e) {
		t.Error("a re-read cleared the burn list; a standing collision would be re-claimed every tick")
	}
}
