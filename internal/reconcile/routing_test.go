package reconcile

import (
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/route"
)

func bothFamilies() *config.Config { return &config.Config{IPv6ECMP: true} }

func (d *testReconciler) ecmpArgsFor(t *testing.T, family int, active ...route.Gateway) []string {
	t.Helper()
	d.useGateways(t, active...)
	entry := Desired(d.r, &d.state).ECMP[family]
	if entry.Verdict != Want {
		t.Fatalf("calculator verdict on the %s route is %v for %d gateways, want want",
			route.FamilyName(family), entry.Verdict, len(active))
	}
	return entry.Spec.Args("add")
}

func TestEcmpArgsWithNexthops(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	want := []string{
		"-4", "route", "add", "default", "metric", "0", "proto", "111",
		"nexthop", "via", "10.0.1.1", "dev", "eth1", "weight", "3",
		"nexthop", "via", "10.0.2.1", "dev", "eth2", "weight", "1",
	}
	got := d.ecmpArgsFor(t, route.FamilyV4,
		route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", IfIndex: 4, ConfigWeight: 1},
	)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args =\n  %v\nwant\n  %v", got, want)
	}
}

func TestEcmpArgsPointToPoint(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	want := []string{
		"-4", "route", "add", "default", "metric", "0", "proto", "111",
		"nexthop", "via", "10.0.1.1", "dev", "eth1", "weight", "1",
		"nexthop", "dev", "ppp-ee", "weight", "10",
	}
	got := d.ecmpArgsFor(t, route.FamilyV4,
		route.Gateway{IfName: "ppp-ee", IfIndex: 7, ConfigWeight: 10},
		route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 1},
	)
	if strings.Contains(strings.Join(got, " "), "<nil>") {
		t.Fatalf("a missing nexthop rendered as <nil>: %v", got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args =\n  %v\nwant\n  %v", got, want)
	}
}

func TestEcmpArgsAreStableAcrossPasses(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	gws := []route.Gateway{
		{IP: net.IPv4(10, 0, 3, 1), IfName: "eth3", IfIndex: 5, ConfigWeight: 1},
		{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 2},
		{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", IfIndex: 4, ConfigWeight: 3},
	}
	d.useGateways(t, gws...)

	first := Desired(d.r, &d.state).ECMP[route.FamilyV4].Spec.Args("add")
	for i := 0; i < 20; i++ {
		if got := Desired(d.r, &d.state).ECMP[route.FamilyV4].Spec.Args("add"); !reflect.DeepEqual(got, first) {
			t.Fatalf("pass %d produced a different argument list:\n  %v\nfirst\n  %v", i, got, first)
		}
	}
}

func TestEcmpArgsIPv6(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, bothFamilies())
	want := []string{
		"-6", "route", "add", "default", "metric", "1", "proto", "111",
		"nexthop", "via", "fe80::aa", "dev", "eth1", "weight", "3",
		"nexthop", "via", "fe80::bb", "dev", "eth2", "weight", "1",
	}
	got := d.ecmpArgsFor(t, route.FamilyV6,
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::aa"), IfName: "eth1", IfIndex: 3, ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::bb"), IfName: "eth2", IfIndex: 4, ConfigWeight: 1},
	)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args =\n  %v\nwant\n  %v", got, want)
	}
}

func TestEcmpArgsVerbPosition(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, bothFamilies())
	for _, family := range []int{route.FamilyV4, route.FamilyV6} {
		args := d.ecmpArgsFor(t, family,
			route.Gateway{Family: family, IfName: "eth1", IfIndex: 3, ConfigWeight: 1})
		if args[2] != "add" {
			t.Errorf("family %s: args[2] = %q, want the verb %q", route.FamilyName(family), args[2], "add")
		}
	}
}

func TestEcmpMetric(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	tests := []struct {
		name   string
		cfg    *config.Config
		family int
		want   string
	}{
		{"ipv4 is always 0", nil, route.FamilyV4, "0"},
		{"ipv6 defaults to 1", nil, route.FamilyV6, "1"},
		{"ipv6 honours the configured value", &config.Config{IPv6Metric: 100}, route.FamilyV6, "100"},
		{"ipv6 rejects 0", &config.Config{IPv6Metric: 0}, route.FamilyV6, "1"},
		{"ipv6 rejects a negative", &config.Config{IPv6Metric: -5}, route.FamilyV6, "1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d.useConfig(t, tc.cfg)
			if got := d.r.Rt.ECMPMetric(tc.family); got != tc.want {
				t.Errorf("ECMPMetric(%s) = %q, want %q", route.FamilyName(tc.family), got, tc.want)
			}
		})
	}
}

func TestManagedFamilies(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)
	if got := d.r.Rt.ManagedFamilies(); !reflect.DeepEqual(got, []int{route.FamilyV4}) {
		t.Errorf("managedFamilies with no config = %v, want v4 only", got)
	}

	d.useConfig(t, &config.Config{IPv6ECMP: true})
	if got := d.r.Rt.ManagedFamilies(); !reflect.DeepEqual(got, []int{route.FamilyV4, route.FamilyV6}) {
		t.Errorf("managedFamilies with ipv6_ecmp = %v, want both", got)
	}
}

func TestObservedFamiliesCoverNPTv6WithoutManagingIPv6(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)
	if got := d.r.Rt.ObservedFamilies(); !reflect.DeepEqual(got, []int{route.FamilyV4}) {
		t.Errorf("observedFamilies with no config = %v, want v4 only", got)
	}

	d.useNPTv6(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:abcd::/56",
				SubnetPriority: []string{"main"}},
		},
	})

	if got := d.r.Rt.ObservedFamilies(); !reflect.DeepEqual(got, []int{route.FamilyV4, route.FamilyV6}) {
		t.Errorf("observedFamilies with nptv6 = %v, want both", got)
	}
	if got := d.r.Rt.ManagedFamilies(); !reflect.DeepEqual(got, []int{route.FamilyV4}) {
		t.Errorf("managedFamilies with nptv6 and no ipv6_ecmp = %v, want v4 only — "+
			"observing a family must not install a route in it", got)
	}
}

func TestDesiredNexthopsPointToPoint(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useGateways(t,
		route.Gateway{IfName: "ppp-ee", IfIndex: 7, ConfigWeight: 10},
		route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 1},
		route.Gateway{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", IfIndex: 4, ConfigWeight: 4},
	)

	want := []route.NexthopSpec{
		{Via: "10.0.1.1", Dev: "eth1", Weight: 1},
		{Via: "10.0.2.1", Dev: "eth2", Weight: 4},
		{Via: "", Dev: "ppp-ee", Weight: 10},
	}
	got := Desired(d.r, &d.state).ECMP[route.FamilyV4].Spec.Nexthops
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nexthops =\n  %v\nwant\n  %v", got, want)
	}
}

func TestAbsorbReplacesTheFamilysGateways(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useGatewayConfig(t, map[string]config.Gateway{"eth1": {Weight: 1}, "eth3": {Weight: 1}})
	d.useAsker(map[string]string{
		"ip -4 route show default":                    "default via 10.0.3.1 dev eth3 proto dhcp metric 60 \n",
		"ip -4 route show default metric 0 proto 111": "",
	})
	d.state.Links.Ifaces = map[string]int{"eth1": 3, "eth3": 5}

	gone := route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3}
	d.useGateways(t, gone)

	d.absorbGatewayFamily(d.Observe(d.r.Rt.GatewayQueries(route.FamilyV4)), route.FamilyV4)

	if _, still := d.state.Routes.Gateways[route.Key(route.FamilyV4, gone.IP, gone.IfIndex)]; still {
		t.Error("a gateway the kernel no longer lists survived the re-read")
	}
	fresh, ok := d.state.Routes.Gateways[route.Key(route.FamilyV4, net.IPv4(10, 0, 3, 1), 5)]
	if !ok {
		t.Fatalf("the kernel's gateway was not adopted: %v", d.state.Routes.Gateways)
	}
	if fresh.IfName != "eth3" || fresh.ConfigWeight != 1 {
		t.Errorf("adopted %+v, want eth3 at its configured weight", fresh)
	}
}

func TestAbsorbReplacesOnlyTheFamilyItRead(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useAsker(map[string]string{
		"ip -4 route show default":                    "",
		"ip -4 route show default metric 0 proto 111": "",
	})

	v6 := route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3}
	d.useGateways(t, v6)

	d.absorbGatewayFamily(d.Observe(d.r.Rt.GatewayQueries(route.FamilyV4)), route.FamilyV4)

	if _, still := d.state.Routes.Gateways[route.Key(route.FamilyV6, v6.IP, v6.IfIndex)]; !still {
		t.Error("a v4 read withdrew a v6 gateway it says nothing about")
	}
}

func TestAbsorbLeavesTheFamilyAloneOnAFailedRead(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useAsker(map[string]string{})

	gw := route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3}
	d.useGateways(t, gw)

	d.absorbGatewayFamily(d.Observe(d.r.Rt.GatewayQueries(route.FamilyV4)), route.FamilyV4)

	if _, still := d.state.Routes.Gateways[route.Key(route.FamilyV4, gw.IP, gw.IfIndex)]; !still {
		t.Error("a failed read was treated as an empty routing table")
	}
}

func TestTheIPv6SourceWarningReadsOnlyWhatThePassDeclared(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useConfig(t, &config.Config{
		IPv6ECMP: true,
		Gateways: map[string]config.Gateway{"wan0": {Weight: 1}, "wan1": {Weight: 1}},
	})
	d.state.Links.Ifaces = map[string]int{"wan0": 2, "wan1": 3}

	names := d.ipv6RouteSourceNames()
	if !slices.Equal(names, []string{"wan0", "wan1"}) {
		t.Fatalf("the warning looks at %v, want both configured gateways", names)
	}

	a := d.Observe(d.queryPlan())
	for _, q := range route.IPv6RouteSourceQueries(names) {
		if _, ok := a[q.String()]; !ok {
			t.Errorf("%s was read by the check but never declared by queryPlan", q)
		}
	}

	d.state.Routes.V6SourcesChecked = true
	if got := d.ipv6RouteSourceNames(); got != nil {
		t.Errorf("a second pass still looks at %v, want nothing", got)
	}
}
