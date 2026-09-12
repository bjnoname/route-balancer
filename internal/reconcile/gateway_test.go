package reconcile

import (
	"net"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/route"
)

func (d *testReconciler) useGateways(t *testing.T, gws ...route.Gateway) {
	t.Helper()

	d.state.Routes.Gateways = make(map[string]route.Gateway, len(gws))
	d.state.Health.Unhealthy = map[healthKey]bool{}
	for _, gw := range gws {
		d.state.Routes.Gateways[route.Key(gw.AddrFamily(), gw.IP, gw.IfIndex)] = gw
	}
}

func (d *testReconciler) markUnhealthy(t *testing.T, family int, ifNames ...string) {
	t.Helper()
	for _, name := range ifNames {
		d.state.Health.Unhealthy[healthKey{IfName: name, Family: family}] = true
	}
}

func TestActiveNexthopsSplitByFamily(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::2"), IfName: "eth2", IfIndex: 4, ConfigWeight: 1},
	)

	v4 := activeNexthops(d.state.Routes, d.state.Health, route.FamilyV4)
	if len(v4) != 1 || v4[0].Dev != "eth1" {
		t.Errorf("activeNexthops(v4) = %v, want the one eth1 nexthop", v4)
	}

	v6 := activeNexthops(d.state.Routes, d.state.Health, route.FamilyV6)
	if len(v6) != 2 {
		t.Fatalf("activeNexthops(v6) = %v, want both v6 nexthops", v6)
	}
}

func TestHealthVerdictAppliesOnlyToItsFamily(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::2"), IfName: "eth2", IfIndex: 4, ConfigWeight: 1},
	)
	d.markUnhealthy(t, route.FamilyV4, "eth1")

	if got := activeNexthops(d.state.Routes, d.state.Health, route.FamilyV4); len(got) != 0 {
		t.Errorf("activeNexthops(v4) = %v, want none — eth1's v4 probe is failing", got)
	}
	if got := activeNexthops(d.state.Routes, d.state.Health, route.FamilyV6); len(got) != 2 {
		t.Errorf("activeNexthops(v6) = %v, want both nexthops — only eth1's v4 probe failed", got)
	}
}

func TestUplinkActiveReadsTheIPv6Verdict(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, &config.Config{Gateways: map[string]config.Gateway{
		"eth1": {Weight: 3}, "eth2": {Weight: 1}, "eth3": {Weight: 1}, "eth4": {Weight: 1},
	}})
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::2"), IfName: "eth2", IfIndex: 4, ConfigWeight: 1},
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 3, 1), IfName: "eth3", IfIndex: 5, ConfigWeight: 1},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::4"), IfName: "eth4", IfIndex: 6, ConfigWeight: 1},
	)
	d.markUnhealthy(t, route.FamilyV4, "eth1")
	d.markUnhealthy(t, route.FamilyV6, "eth2")

	for _, tc := range []struct {
		iface string
		want  bool
	}{
		{"eth1", true},
		{"eth2", false},
		{"eth3", true},
		{"eth4", true},
		{"eth9", true},
	} {
		if got := uplinkActive(d.cfg.Gateways, d.state.Routes, d.state.Health, tc.iface); got != tc.want {
			t.Errorf("uplinkActive(%q) = %v, want %v", tc.iface, got, tc.want)
		}
	}
}

func TestGatewayHasNexthop(t *testing.T) {
	t.Parallel()
	if !(route.Gateway{IP: net.IPv4(10, 0, 1, 1)}).HasNexthop() {
		t.Error("gateway with an IP should report a nexthop")
	}
	if (route.Gateway{IfName: "ppp-ee"}).HasNexthop() {
		t.Error("gateway without an IP should report no nexthop")
	}
}
