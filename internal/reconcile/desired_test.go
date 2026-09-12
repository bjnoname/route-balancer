package reconcile

import (
	"net"
	"reflect"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/prefix"
	"github.com/bjnoname/route-balancer/internal/route"
)

func TestNoActiveGatewaysMeansAbsentNotEmpty(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)
	d.useGateways(t,
		route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", IfIndex: 4, ConfigWeight: 1},
	)
	d.markUnhealthy(t, route.FamilyV4, "eth1", "eth2")

	entry := Desired(d.r, &d.state).ECMP[route.FamilyV4]
	if entry.Verdict != Unmanaged {
		t.Fatalf("verdict = %v with every gateway unhealthy, want unmanaged; "+
			"anything else lets a differ withdraw the default route", entry.Verdict)
	}
	if entry.Reason == "" {
		t.Error("an unmanaged verdict carries no reason, so nothing can explain the refusal")
	}
}

func TestRetentionSpecifiesNoRoute(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)
	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 3})
	d.markUnhealthy(t, route.FamilyV4, "eth1")

	entry := Desired(d.r, &d.state).ECMP[route.FamilyV4]
	if entry.Verdict != Unmanaged {
		t.Fatalf("verdict = %v with every gateway unhealthy, want unmanaged", entry.Verdict)
	}
	if len(entry.Spec.Nexthops) != 0 {
		t.Errorf("retention entry carries nexthops %v, which a drift check would try to restore",
			entry.Spec.Nexthops)
	}
}

func TestOneHealthyGatewayIsStillARoute(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)
	d.useGateways(t,
		route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", IfIndex: 4, ConfigWeight: 1},
	)
	d.markUnhealthy(t, route.FamilyV4, "eth1")

	entry := Desired(d.r, &d.state).ECMP[route.FamilyV4]
	if entry.Verdict != Want {
		t.Fatalf("verdict = %v with one healthy gateway, want want", entry.Verdict)
	}
	if len(entry.Spec.Nexthops) != 1 || entry.Spec.Nexthops[0].Dev != "eth2" {
		t.Errorf("nexthops = %v, want only eth2", entry.Spec.Nexthops)
	}
}

func TestRulesetTextIsByteStable(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets: map[string]config.NPTv6Subnet{
			"main":  {Prefix: "fd00:dead:beef:1::/64"},
			"guest": {Prefix: "fd00:dead:beef:2::/64"},
			"iot":   {Prefix: "fd00:dead:beef:3::/64"},
		},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:a::/56",
				SubnetPriority: []string{"main", "guest", "iot"}},
			"wan1": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:b::/60",
				SubnetPriority: []string{"guest", "main"}},
			"wan2": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:c::/64",
				SubnetPriority: []string{"iot"}},
		},
	})

	d.state.Prefixes.Leases = map[string]prefix.Lease{
		"wan0": {Uplink: "wan0", Prefix: net.ParseIP("2001:db8:a::"), Length: 56},
		"wan1": {Uplink: "wan1", Prefix: net.ParseIP("2001:db8:b::"), Length: 60},
		"wan2": {Uplink: "wan2", Prefix: net.ParseIP("2001:db8:c::"), Length: 64},
	}

	first := Desired(d.r, &d.state)
	for i := 0; i < 50; i++ {
		inv := Desired(d.r, &d.state)
		if got, want := inv.Nft[d.cfg.NPTv6Table()], first.Nft[d.cfg.NPTv6Table()]; got != want {
			t.Fatalf("pass %d produced different ruleset text:\n%s\n--- first ---\n%s", i, got, want)
		}
		if !reflect.DeepEqual(inv.Assignments, first.Assignments) {
			t.Fatalf("pass %d produced a different assignment order:\n  %v\nfirst\n  %v",
				i, inv.Assignments, first.Assignments)
		}
	}
}

func TestDesiredTableFromObservedAddress(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)
	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 1})
	d.useAddrs(t, map[int]*net.IPNet{
		3: {IP: net.IPv4(10, 0, 1, 2), Mask: net.CIDRMask(24, 32)},
	})

	spec, ok := Desired(d.r, &d.state).Tables[tableKey{route.FamilyV4, 3}]
	if !ok {
		t.Fatal("no table for a gateway whose interface has an address")
	}
	if spec.Src != "10.0.1.2" {
		t.Errorf("src = %q, want 10.0.1.2", spec.Src)
	}
	if spec.Subnet != "10.0.1.0/24" {
		t.Errorf("subnet = %q, want 10.0.1.0/24", spec.Subnet)
	}
	if spec.Via != "10.0.1.1" {
		t.Errorf("via = %q, want 10.0.1.1", spec.Via)
	}
}

func TestDesiredTableSkipsSubnetForSlashThirtyTwo(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)
	d.useGateways(t, route.Gateway{IfName: "ppp-ee", IfIndex: 7, ConfigWeight: 1})
	d.useAddrs(t, map[int]*net.IPNet{
		7: {IP: net.IPv4(172, 16, 19, 92), Mask: net.CIDRMask(32, 32)},
	})

	spec, ok := Desired(d.r, &d.state).Tables[tableKey{route.FamilyV4, 7}]
	if !ok {
		t.Fatal("no table for a point-to-point gateway")
	}
	if spec.Subnet != "" {
		t.Errorf("subnet = %q, want none for a /32", spec.Subnet)
	}
	if spec.Via != "" {
		t.Errorf("via = %q, want none for a point-to-point link", spec.Via)
	}
}

func TestDesiredTableAbsentWithoutAnAddress(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)
	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 1})
	d.useAddrs(t, map[int]*net.IPNet{})

	if spec, ok := Desired(d.r, &d.state).Tables[tableKey{route.FamilyV4, 3}]; ok {
		t.Errorf("table %+v described for an interface with no address", spec)
	}
}

func TestDesiredTableIsIPv4Only(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, bothFamilies())
	d.useGateways(t, route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"),
		IfName: "eth1", IfIndex: 3, ConfigWeight: 1})
	d.useAddrs(t, map[int]*net.IPNet{
		3: {IP: net.IPv4(10, 0, 1, 2), Mask: net.CIDRMask(24, 32)},
	})

	if len(Desired(d.r, &d.state).Tables) != 0 {
		t.Errorf("tables = %v, want none for a v6-only gateway", Desired(d.r, &d.state).Tables)
	}
}

func (d *testReconciler) useAddrs(t *testing.T, addrs map[int]*net.IPNet) {
	t.Helper()
	d.state.Links.Addrs = addrs
}

func TestDesiredTableDeclinesAReservedTableID(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)
	d.useGateways(t,
		route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 152, ConfigWeight: 1},
		route.Gateway{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", IfIndex: 154, ConfigWeight: 1},
	)
	d.useAddrs(t, map[int]*net.IPNet{
		152: {IP: net.IPv4(10, 0, 1, 2), Mask: net.CIDRMask(24, 32)},
		154: {IP: net.IPv4(10, 0, 2, 2), Mask: net.CIDRMask(24, 32)},
	})

	inv := Desired(d.r, &d.state)

	if _, ok := inv.Tables[tableKey{route.FamilyV4, 152}]; !ok {
		t.Error("no table for ifIndex 152, which lands on 252 and is fine")
	}
	if spec, ok := inv.Tables[tableKey{route.FamilyV4, 154}]; ok {
		t.Errorf("table %+v described for ifIndex 154, which lands on main", spec)
	}
}
