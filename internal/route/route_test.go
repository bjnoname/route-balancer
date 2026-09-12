package route

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
)

func TestParseGatewayAddrRejectsWrongFamily(t *testing.T) {
	if got := parseGatewayAddr(FamilyV6, "10.0.1.1"); got != nil {
		t.Errorf("parseGatewayAddr(v6, IPv4 literal) = %v, want nil", got)
	}
	if got := parseGatewayAddr(FamilyV4, "fe80::1"); got != nil {
		t.Errorf("parseGatewayAddr(v4, IPv6 literal) = %v, want nil", got)
	}
	if got := parseGatewayAddr(FamilyV6, "fe80::1"); !got.Equal(net.ParseIP("fe80::1")) {
		t.Errorf("parseGatewayAddr(v6, fe80::1) = %v, want fe80::1", got)
	}
}

func TestSeedingIPv6SubtractsOurOwnRoute(t *testing.T) {
	all := `default proto 111 metric 1 pref medium
	nexthop via fe80::aa dev veth0 weight 3
	nexthop via fe80::bb dev veth2 weight 1
default via fe80::aa dev veth0 proto ra metric 1024 pref medium
default via fe80::bb dev veth2 proto ra metric 1024 pref medium
`
	own := `default pref medium
	nexthop via fe80::aa dev veth0 weight 3
	nexthop via fe80::bb dev veth2 weight 1
`

	got := subtractRoutes(parseDefaultRoutes(FamilyV6, all), parseDefaultRoutes(FamilyV6, own))

	want := []ObservedRoute{
		{GwIP: net.ParseIP("fe80::aa"), IfName: "veth0"},
		{GwIP: net.ParseIP("fe80::bb"), IfName: "veth2"},
	}
	if len(got) != len(want) {
		t.Fatalf("seeded %d routes, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].key() != want[i].key() {
			t.Errorf("route %d = %q, want %q", i, got[i].key(), want[i].key())
		}
	}
}

func TestParseDefaultRoutesSkipsRejectRoutesAndLoopback(t *testing.T) {
	out := `unreachable default dev lo proto static metric 4278198272 pref medium
blackhole default proto static metric 100 pref medium
prohibit default dev lo metric 100 pref medium
default via fe80::aa dev veth0 proto ra metric 1024 pref medium
`
	got := parseDefaultRoutes(FamilyV6, out)
	if len(got) != 1 {
		t.Fatalf("parsed %d routes, want only the real uplink: %v", len(got), got)
	}
	if got[0].IfName != "veth0" {
		t.Errorf("route = %+v, want the veth0 uplink", got[0])
	}
}

func TestParseNexthopsMultipath(t *testing.T) {
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

func TestNexthopsMatchSingleGatewayWeightAboveOne(t *testing.T) {
	tests := []struct {
		name string
		want NexthopSpec
		out  string
	}{
		{
			"with nexthop",
			NexthopSpec{Via: "172.16.19.91", Dev: "ppp-ee", Weight: 10},
			"default via 172.16.19.91 dev ppp-ee \n",
		},
		{
			"point-to-point",
			NexthopSpec{Dev: "ppp-ee", Weight: 10},
			"default dev ppp-ee \n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !nexthopsMatch(wantMap(tc.want), parseNexthops(tc.out)) {
				t.Error("single-path route reported as externally modified — reconcile would loop forever")
			}
		})
	}
}

func wantMap(nhs ...NexthopSpec) map[nexthopKey]int {
	return ECMPSpec{Family: FamilyV4, Nexthops: nhs}.nexthopMap()
}

func TestNexthopsMatchDetectsDrift(t *testing.T) {
	want := wantMap(
		NexthopSpec{Via: "10.0.1.1", Dev: "eth1", Weight: 3},
		NexthopSpec{Via: "10.0.2.1", Dev: "eth2", Weight: 1},
	)

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
	out := "default via 10.0.1.1 dev eth1 proto dhcp src 10.0.1.2 metric 500 \n" +
		"default dev ppp-ee proto boot scope link metric 51 \n" +
		"default proto 111 \n" +
		"\tnexthop via 10.0.1.1 dev eth1 weight 1 \n" +
		"\tnexthop dev ppp-ee weight 10 \n"

	got := parseDefaultRoutes(FamilyV4, out)
	want := []ObservedRoute{
		{GwIP: net.IPv4(10, 0, 1, 1).To4(), IfName: "eth1", Metric: 500},
		{GwIP: nil, IfName: "ppp-ee", Metric: 51},
		{GwIP: net.IPv4(10, 0, 1, 1).To4(), IfName: "eth1", Metric: 0},
		{GwIP: nil, IfName: "ppp-ee", Metric: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseDefaultRoutes =\n  %v\nwant\n  %v", got, want)
	}
}

func TestSubtractRoutesKeepsGatewayPresentInBothRoutes(t *testing.T) {
	all := parseDefaultRoutes(FamilyV4,
		"default via 10.0.1.1 dev eth1 proto dhcp metric 500 \n"+
			"default dev ppp-ee proto boot scope link metric 51 \n"+
			"default proto 111 \n"+
			"\tnexthop via 10.0.1.1 dev eth1 weight 1 \n"+
			"\tnexthop dev ppp-ee weight 10 \n")
	own := parseDefaultRoutes(FamilyV4,
		"default \n\tnexthop via 10.0.1.1 dev eth1 weight 1 \n\tnexthop dev ppp-ee weight 10 \n")

	want := []ObservedRoute{
		{GwIP: net.IPv4(10, 0, 1, 1).To4(), IfName: "eth1"},
		{GwIP: nil, IfName: "ppp-ee"},
	}
	if got := subtractRoutes(all, own); !reflect.DeepEqual(got, want) {
		t.Errorf("subtractRoutes =\n  %v\nwant\n  %v", got, want)
	}
}

func TestSubtractRoutesDetectsVanishedUplink(t *testing.T) {
	all := parseDefaultRoutes(FamilyV4, "default via 10.0.1.1 dev eth1 proto 111 \n")
	own := parseDefaultRoutes(FamilyV4, "default via 10.0.1.1 dev eth1 \n")
	if got := subtractRoutes(all, own); len(got) != 0 {
		t.Errorf("subtractRoutes = %v, want none", got)
	}
}

func TestSubtractRoutesWithNothingToDrop(t *testing.T) {
	all := parseDefaultRoutes(FamilyV4, "default via 10.0.1.1 dev eth1 metric 500 \n")
	got := subtractRoutes(all, nil)
	if !reflect.DeepEqual(got, all) {
		t.Errorf("subtractRoutes = %v, want %v", got, all)
	}
}

func TestParseDefaultRoutesSkipsUnusableLines(t *testing.T) {
	out := "default proto 111 \n\n   \nunreachable default proto kernel \n"
	if got := parseDefaultRoutes(FamilyV4, out); len(got) != 0 {
		t.Errorf("parseDefaultRoutes = %v, want none", got)
	}
}

var errUnreadable = errors.New("commandtest: no recorded output")

func argsOf(acts []command.Action) []string {
	out := make([]string, len(acts))
	for i, a := range acts {
		out[i] = strings.Join(a.Args, " ")
	}
	return out
}

func TestTableForIfaceRejectsTheKernelsOwnTables(t *testing.T) {
	o := Options{TableOffset: 100}

	for _, tc := range []struct {
		ifIndex int
		table   int
		want    bool
	}{
		{ifIndex: 1, table: 101, want: true},
		{ifIndex: 152, table: 252, want: true},
		{ifIndex: 153, table: 253, want: false},
		{ifIndex: 154, table: 254, want: false},
		{ifIndex: 155, table: 255, want: false},
		{ifIndex: 900, table: 1000, want: false},
	} {
		table, ok := o.TableForIface(tc.ifIndex)
		if table != tc.table || ok != tc.want {
			t.Errorf("TableForIface(%d) = (%d, %v), want (%d, %v)",
				tc.ifIndex, table, ok, tc.table, tc.want)
		}
		if _, prioOK := o.GatewayPriority(tc.ifIndex); prioOK != tc.want {
			t.Errorf("GatewayPriority(%d) usable = %v, want %v", tc.ifIndex, prioOK, tc.want)
		}
	}
}

func TestTeardownPlansNothingForAReservedTable(t *testing.T) {
	o := Options{TableOffset: 100}

	if acts := o.TeardownTableActions(FamilyV4, "eth9", 154, "10.0.1.2"); acts != nil {
		t.Errorf("teardown planned %d actions for a table we never installed", len(acts))
	}
	if acts := o.TeardownTableActions(FamilyV4, "eth1", 3, "10.0.1.2"); len(acts) == 0 {
		t.Error("teardown planned nothing for a usable table")
	}
}

func TestInstalledTablesIgnoreTheKernelsOwn(t *testing.T) {
	o := Options{TableOffset: 100}

	rules := Rules{Lines: []string{
		"1101:	from 10.0.1.2 lookup 101",
		"1254:	from 10.0.9.2 lookup 254",
	}}

	if got := o.InstalledTableIfIndexes(rules); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("InstalledTableIfIndexes = %v, want [1]", got)
	}
}
