package reconcile

import (
	"context"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/netlink"
	"github.com/bjnoname/route-balancer/internal/prefix"
	"github.com/bjnoname/route-balancer/internal/route"
)

func TestEventsCarryOnlyWhatTheKernelCannotBeAsked(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useGateways(t, route.Gateway{IP: net.ParseIP("10.0.0.1"), IfIndex: 2, IfName: "eth0", ConfigWeight: 3})

	before := len(d.state.Routes.Gateways)
	d.handle(RouteChanged{
		Type: netlink.RTM_NEWROUTE,
		GW:   route.Gateway{IP: net.ParseIP("10.0.9.1"), IfIndex: 9, IfName: "eth9"},
	})
	if got := len(d.state.Routes.Gateways); got != before {
		t.Errorf("a route event folded into the gateway map (%d → %d); the pass re-reads it", before, got)
	}

	e := ndp.ProxyEntry{Addr: "2001:db8::5054:ff:fe12:102", Uplink: "eth0"}
	d.state.Claims.Probing[e] = probing(ndp.ClaimSpeculative)
	d.handle(ClaimVerified{Entry: e, Kind: ndp.ClaimSpeculative, Contested: true})

	if !d.state.Claims.isBurned(e) {
		t.Error("a conflict verdict was not folded; nothing in the kernel could supply it")
	}
}

func TestBurstCoalescesIntoOnePass(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	d.state.Prefixes.Leases = map[string]prefix.Lease{}

	first := prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:1::"), Length: 56}
	second := prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:2::"), Length: 56}

	d.post(LeaseChanged{Change: prefix.Change{Lease: first}})
	d.post(LeaseChanged{Change: prefix.Change{Lease: first}})
	d.post(LeaseChanged{Change: prefix.Change{Lease: second}})
	d.post(Resync{})
	d.post(Resync{})

	d.fold()

	if d.queued() != 0 {
		t.Errorf("%d events left queued, want the fold to have taken them all", d.queued())
	}
	if got, ok := d.state.Prefixes.currentLease("wan0"); !ok || !got.SameAs(second) {
		t.Errorf("currentLease = %v/%v, want the last prefix in the burst", got, ok)
	}
}

func TestUnchangedRenewalRendersTheSameRuleset(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main"}},
		},
	})

	l := prefix.Lease{Uplink: "wan0", Prefix: net.ParseIP("2001:db8:1::"), Length: 64}

	d.handle(LeaseChanged{Change: prefix.Change{Lease: l}})
	first := Desired(d.r, &d.state).Nft[d.r.Npt.Table]

	d.handle(LeaseChanged{Change: prefix.Change{Lease: l}})
	second := Desired(d.r, &d.state).Nft[d.r.Npt.Table]

	if !strings.Contains(first, "2001:db8:1::") {
		t.Fatalf("setup: the lease produced no translation rules: %q", first)
	}
	if first != second {
		t.Error("a renewal returning the same prefix rendered a different ruleset, which would reset conntrack")
	}
}

func TestIPv4HealthEventMovesOnlyTheIPv4Route(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	d.useConfig(t, bothFamilies())
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 3}})
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.ParseIP("10.0.0.1"), IfIndex: 2, IfName: "eth0", ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfIndex: 2, IfName: "eth0", ConfigWeight: 3},
	)
	id := d.runningMonitor(t, v4Of("eth0"))

	d.handle(HealthChanged{ID: id, IfName: "eth0", Family: route.FamilyV4, Healthy: false})

	if d.state.Health.healthy("eth0", route.FamilyV4) {
		t.Error("eth0 is still healthy in v4 after its v4 probe reported a failure")
	}
	if !d.state.Health.healthy("eth0", route.FamilyV6) {
		t.Error("a v4 verdict was applied to the v6 path, which nothing measured")
	}
	if got := activeNexthops(d.state.Routes, d.state.Health, route.FamilyV4); len(got) != 0 {
		t.Errorf("v4 nexthops = %v, want the gateway withdrawn from the v4 route", got)
	}
	if got := activeNexthops(d.state.Routes, d.state.Health, route.FamilyV6); len(got) != 1 {
		t.Errorf("v6 nexthops = %v, want the gateway left in the v6 route", got)
	}
	if !uplinkActive(d.cfg.Gateways, d.state.Routes, d.state.Health, "eth0") {
		t.Error("a v4 verdict stopped translation, which leaves over IPv6")
	}
}

func TestIPv6HealthEventMovesTheIPv6RouteAndTranslation(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	d.useConfig(t, bothFamilies())
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 3}})
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.ParseIP("10.0.0.1"), IfIndex: 2, IfName: "eth0", ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfIndex: 2, IfName: "eth0", ConfigWeight: 3},
	)
	id := d.runningMonitor(t, v6Of("eth0"))

	d.handle(HealthChanged{ID: id, IfName: "eth0", Family: route.FamilyV6, Healthy: false})

	if got := activeNexthops(d.state.Routes, d.state.Health, route.FamilyV6); len(got) != 0 {
		t.Errorf("v6 nexthops = %v, want the gateway withdrawn from the v6 route", got)
	}
	if got := activeNexthops(d.state.Routes, d.state.Health, route.FamilyV4); len(got) != 1 {
		t.Errorf("v4 nexthops = %v, want the gateway left in the v4 route", got)
	}
	if uplinkActive(d.cfg.Gateways, d.state.Routes, d.state.Health, "eth0") {
		t.Error("an uplink whose v6 path is dead is still attracting translated traffic")
	}
}

func TestStaleHealthVerdictIsDiscarded(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	d.useConfig(t, nil)
	d.useGateways(t, route.Gateway{IP: net.ParseIP("10.0.0.1"), IfIndex: 2, IfName: "eth0", ConfigWeight: 3})
	stale := d.runningMonitor(t, v4Of("eth0"))

	replacement := d.runningMonitor(t, v4Of("eth0"))
	d.handle(HealthChanged{ID: stale, IfName: "eth0", Family: route.FamilyV4, Healthy: false})

	if !d.state.Health.healthy("eth0", route.FamilyV4) {
		t.Error("a verdict from a stopped monitor was applied to its successor's link")
	}

	d.handle(HealthChanged{ID: replacement, IfName: "eth0", Family: route.FamilyV4, Healthy: false})
	if d.state.Health.healthy("eth0", route.FamilyV4) {
		t.Error("the running monitor's verdict was discarded too")
	}
}

func TestAnUnmanagedFamilyGetsNoResource(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	d.useNPTv6(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:abcd::/56",
				SubnetPriority: []string{"main"}},
		},
	})

	var ids []string
	for _, r := range ecmpResources(d.r.Rt, Desired(d.r, &d.state), d.look()) {
		ids = append(ids, r.ID)
	}

	if slices.Contains(ids, ecmpResourceID(route.FamilyV6)) {
		t.Errorf("built %v, want no v6 route resource on a host that manages no v6 route", ids)
	}
	if !slices.Contains(ids, ecmpResourceID(route.FamilyV4)) {
		t.Errorf("built %v, want the managed family's route", ids)
	}
}

func TestHealthVerdictIsMatchedPerFamily(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	d.useConfig(t, bothFamilies())
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.ParseIP("10.0.0.1"), IfIndex: 2, IfName: "eth0", ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfIndex: 2, IfName: "eth0", ConfigWeight: 3},
	)
	d.runningMonitor(t, v4Of("eth0"))
	v6 := d.runningMonitor(t, v6Of("eth0"))

	d.handle(HealthChanged{ID: v6, IfName: "eth0", Family: route.FamilyV6, Healthy: false})

	if d.state.Health.healthy("eth0", route.FamilyV6) {
		t.Error("the v6 monitor's own verdict was discarded")
	}
}

func TestACancelWinsOverAQueuedEvent(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	ctx, cancel := context.WithCancel(context.Background())
	d.post(Resync{})
	cancel()

	acts, more := d.Step(ctx, d)

	if more {
		t.Error("the loop asked for another turn after its context was cancelled")
	}
	if d.queued() != 1 {
		t.Errorf("%d events left queued, want the cancel to have won over the queue", d.queued())
	}
	if len(acts) == 0 {
		t.Error("the final batch planned nothing, want the teardown")
	}
}
