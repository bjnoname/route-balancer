package main

import (
	"net"
	"reflect"
	"strings"
	"testing"
)

func TestEcmpArgsWithNexthops(t *testing.T) {
	active := []Gateway{
		{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", EffectWeight: 3},
		{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", EffectWeight: 1},
	}
	want := []string{
		"route", "add", "default", "metric", "0", "proto", "111",
		"nexthop", "via", "10.0.1.1", "dev", "eth1", "weight", "3",
		"nexthop", "via", "10.0.2.1", "dev", "eth2", "weight", "1",
	}
	if got := ecmpArgs(active, "111"); !reflect.DeepEqual(got, want) {
		t.Errorf("ecmpArgs =\n  %v\nwant\n  %v", got, want)
	}
}

// A point-to-point gateway has no nexthop address, so its nexthop clause must
// omit "via" entirely — "nexthop via <nil> dev ppp-ee" is rejected by ip(8).
func TestEcmpArgsPointToPoint(t *testing.T) {
	active := []Gateway{
		{IfName: "ppp-ee", EffectWeight: 10},
		{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", EffectWeight: 1},
	}
	want := []string{
		"route", "add", "default", "metric", "0", "proto", "111",
		"nexthop", "dev", "ppp-ee", "weight", "10",
		"nexthop", "via", "10.0.1.1", "dev", "eth1", "weight", "1",
	}
	got := ecmpArgs(active, "111")
	if strings.Contains(strings.Join(got, " "), "<nil>") {
		t.Fatalf("ecmpArgs rendered a missing nexthop as <nil>: %v", got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ecmpArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestDesiredNexthopsPointToPoint(t *testing.T) {
	want := map[nexthopKey]int{
		{via: "", dev: "ppp-ee"}:       10,
		{via: "10.0.1.1", dev: "eth1"}: 1,
		{via: "10.0.2.1", dev: "eth2"}: 4,
	}
	active := []Gateway{
		{IfName: "ppp-ee", EffectWeight: 10},
		{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", EffectWeight: 1},
		{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", EffectWeight: 4},
	}
	if got := desiredNexthops(active); !reflect.DeepEqual(got, want) {
		t.Errorf("desiredNexthops = %v, want %v", got, want)
	}
}

func TestParseNexthopsMultipath(t *testing.T) {
	// Verbatim `ip route show default metric 0` output for a two-path route.
	out := "default proto 111 \n" +
		"\tnexthop dev ptp0 weight 3 \n" +
		"\tnexthop via 10.0.1.1 dev eth9 weight 1\n"
	want := map[nexthopKey]int{
		{via: "", dev: "ptp0"}:         3,
		{via: "10.0.1.1", dev: "eth9"}: 1,
	}
	if got := parseNexthops(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseNexthops = %v, want %v", got, want)
	}
}

// With a single path the kernel collapses the multipath route and prints no
// weight token, so the weight it would have carried is unknowable from the
// output.
func TestParseNexthopsSinglePathHasUnknownWeight(t *testing.T) {
	tests := []struct {
		name string
		out  string
		key  nexthopKey
	}{
		{"with nexthop", "default via 10.0.1.1 dev eth9 \n", nexthopKey{via: "10.0.1.1", dev: "eth9"}},
		{"point-to-point", "default dev ptp0 \n", nexthopKey{via: "", dev: "ptp0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNexthops(tc.out)
			w, ok := got[tc.key]
			if !ok {
				t.Fatalf("parseNexthops = %v, missing %v", got, tc.key)
			}
			if w != weightUnknown {
				t.Errorf("weight = %d, want weightUnknown (%d)", w, weightUnknown)
			}
		})
	}
}

// Regression: a lone gateway configured at weight > 1 must not be seen as
// externally modified. The kernel drops the weight token for single-path
// routes, so a strict weight comparison declares drift on every reconcile tick
// and re-applies the route forever.
func TestNexthopsMatchSingleGatewayWeightAboveOne(t *testing.T) {
	tests := []struct {
		name   string
		active []Gateway
		out    string
	}{
		{
			"with nexthop",
			[]Gateway{{IP: net.IPv4(172, 16, 19, 91), IfName: "ppp-ee", EffectWeight: 10}},
			"default via 172.16.19.91 dev ppp-ee \n",
		},
		{
			"point-to-point",
			[]Gateway{{IfName: "ppp-ee", EffectWeight: 10}},
			"default dev ppp-ee \n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !nexthopsMatch(desiredNexthops(tc.active), parseNexthops(tc.out)) {
				t.Error("single-path route reported as externally modified — reconcile would loop forever")
			}
		})
	}
}

func TestNexthopsMatchDetectsDrift(t *testing.T) {
	active := []Gateway{
		{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", EffectWeight: 3},
		{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", EffectWeight: 1},
	}
	want := desiredNexthops(active)

	tests := []struct {
		name string
		out  string
	}{
		{"nexthop missing", "default proto 111 \n\tnexthop via 10.0.1.1 dev eth1 weight 3\n"},
		{"extra nexthop", "default proto 111 \n\tnexthop via 10.0.1.1 dev eth1 weight 3\n" +
			"\tnexthop via 10.0.2.1 dev eth2 weight 1\n\tnexthop via 10.0.3.1 dev eth3 weight 1\n"},
		{"wrong weight", "default proto 111 \n\tnexthop via 10.0.1.1 dev eth1 weight 1\n" +
			"\tnexthop via 10.0.2.1 dev eth2 weight 1\n"},
		{"wrong device", "default proto 111 \n\tnexthop via 10.0.1.1 dev eth7 weight 3\n" +
			"\tnexthop via 10.0.2.1 dev eth2 weight 1\n"},
		{"empty", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if nexthopsMatch(want, parseNexthops(tc.out)) {
				t.Error("drift not detected")
			}
		})
	}
}

func TestParseDefaultRoutes(t *testing.T) {
	// Verbatim `ip -4 route show default` output covering a DHCP gateway, a
	// pppd point-to-point route, and our own multipath route.
	out := "default via 10.0.1.1 dev eth1 proto dhcp src 10.0.1.2 metric 500 \n" +
		"default dev ppp-ee proto boot scope link metric 51 \n" +
		"default proto 111 \n" +
		"\tnexthop via 10.0.1.1 dev eth1 weight 1 \n" +
		"\tnexthop dev ppp-ee weight 10 \n"

	got := parseDefaultRoutes(out)
	want := []seededRoute{
		{gwIP: net.IPv4(10, 0, 1, 1).To4(), ifName: "eth1"},
		{gwIP: nil, ifName: "ppp-ee"},
		{gwIP: net.IPv4(10, 0, 1, 1).To4(), ifName: "eth1"},
		{gwIP: nil, ifName: "ppp-ee"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseDefaultRoutes =\n  %v\nwant\n  %v", got, want)
	}
}

// A gateway appearing both in its own default route and as a nexthop of ours
// must survive the subtraction — otherwise the reconciler would see it as gone
// and tear down a live uplink.
func TestSubtractRoutesKeepsGatewayPresentInBothRoutes(t *testing.T) {
	all := parseDefaultRoutes(
		"default via 10.0.1.1 dev eth1 proto dhcp metric 500 \n" +
			"default dev ppp-ee proto boot scope link metric 51 \n" +
			"default proto 111 \n" +
			"\tnexthop via 10.0.1.1 dev eth1 weight 1 \n" +
			"\tnexthop dev ppp-ee weight 10 \n")
	own := parseDefaultRoutes(
		"default \n\tnexthop via 10.0.1.1 dev eth1 weight 1 \n\tnexthop dev ppp-ee weight 10 \n")

	want := []seededRoute{
		{gwIP: net.IPv4(10, 0, 1, 1).To4(), ifName: "eth1"},
		{gwIP: nil, ifName: "ppp-ee"},
	}
	if got := subtractRoutes(all, own); !reflect.DeepEqual(got, want) {
		t.Errorf("subtractRoutes =\n  %v\nwant\n  %v", got, want)
	}
}

// Once the underlying route is gone, only our own nexthop remains, and
// subtracting it must leave nothing — that is the signal to prune.
func TestSubtractRoutesDetectsVanishedUplink(t *testing.T) {
	all := parseDefaultRoutes("default via 10.0.1.1 dev eth1 proto 111 \n")
	own := parseDefaultRoutes("default via 10.0.1.1 dev eth1 \n")
	if got := subtractRoutes(all, own); len(got) != 0 {
		t.Errorf("subtractRoutes = %v, want none", got)
	}
}

func TestSubtractRoutesWithNothingToDrop(t *testing.T) {
	all := parseDefaultRoutes("default via 10.0.1.1 dev eth1 metric 500 \n")
	got := subtractRoutes(all, nil)
	if !reflect.DeepEqual(got, all) {
		t.Errorf("subtractRoutes = %v, want %v", got, all)
	}
}

func TestStaleGatewaysPrunesOnlyAfterRepeatedMisses(t *testing.T) {
	gw := Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3}
	tracked := map[string]Gateway{gwKey(gw.IP, gw.IfIndex): gw}
	misses := map[string]int{}
	live := map[string]struct{}{}

	if stale := staleGateways(tracked, live, misses); len(stale) != 0 {
		t.Fatalf("pruned on the first miss: %v", stale)
	}
	stale := staleGateways(tracked, live, misses)
	if len(stale) != 1 || stale[0].IfName != "eth1" {
		t.Fatalf("staleGateways = %v, want eth1 after a second miss", stale)
	}
	if len(misses) != 0 {
		t.Errorf("misses = %v, want the counter cleared once pruned", misses)
	}
}

// A gateway registered between the routing-table snapshot and the map read is
// absent from the snapshot through no fault of its own. It must survive.
func TestStaleGatewaysToleratesRaceWithRegistration(t *testing.T) {
	gw := Gateway{IP: net.IPv4(10, 0, 2, 1), IfName: "eth2", IfIndex: 4}
	key := gwKey(gw.IP, gw.IfIndex)
	tracked := map[string]Gateway{key: gw}
	misses := map[string]int{}

	// Pass 1 raced with the route being added: not in the snapshot.
	if stale := staleGateways(tracked, map[string]struct{}{}, misses); len(stale) != 0 {
		t.Fatalf("pruned a freshly registered gateway: %v", stale)
	}
	// Pass 2 sees it, so the miss must be forgotten.
	live := map[string]struct{}{key: {}}
	if stale := staleGateways(tracked, live, misses); len(stale) != 0 {
		t.Fatalf("pruned a live gateway: %v", stale)
	}
	if len(misses) != 0 {
		t.Errorf("misses = %v, want reset once the gateway was seen", misses)
	}
	// A later single miss must not prune either, now that the count is reset.
	if stale := staleGateways(tracked, map[string]struct{}{}, misses); len(stale) != 0 {
		t.Errorf("pruned on a single miss after recovery: %v", stale)
	}
}

// Counters for gateways removed by a route delete event must not linger.
func TestStaleGatewaysForgetsUntrackedCounters(t *testing.T) {
	gone := gwKey(net.IPv4(10, 0, 9, 1), 9)
	misses := map[string]int{gone: 1}
	staleGateways(map[string]Gateway{}, map[string]struct{}{}, misses)
	if _, ok := misses[gone]; ok {
		t.Errorf("misses = %v, want the untracked counter dropped", misses)
	}
}

// Lines with no output interface (the header of a multipath route, blank
// lines) carry nothing actionable.
func TestParseDefaultRoutesSkipsUnusableLines(t *testing.T) {
	out := "default proto 111 \n\n   \nunreachable default proto kernel \n"
	if got := parseDefaultRoutes(out); len(got) != 0 {
		t.Errorf("parseDefaultRoutes = %v, want none", got)
	}
}
