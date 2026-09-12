package route

import (
	"net"
	"strings"
	"testing"
)

func TestGwKey(t *testing.T) {
	tests := []struct {
		name    string
		family  int
		ip      net.IP
		ifIndex int
		want    string
	}{
		{"with nexthop", FamilyV4, net.IPv4(10, 0, 1, 1), 3, "v4:10.0.1.1@3"},
		{"point-to-point", FamilyV4, nil, 7, "v4:dev@7"},
		{"empty IP is treated as absent", FamilyV4, net.IP{}, 7, "v4:dev@7"},
		{"link-local v6 nexthop", FamilyV6, net.ParseIP("fe80::1"), 3, "v6:fe80::1@3"},
		{"point-to-point v6", FamilyV6, nil, 7, "v6:dev@7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Key(tc.family, tc.ip, tc.ifIndex); got != tc.want {
				t.Errorf("Key(%d, %v, %d) = %q, want %q", tc.family, tc.ip, tc.ifIndex, got, tc.want)
			}
		})
	}
}

func TestGwKeyDistinctPerInterface(t *testing.T) {
	if Key(FamilyV4, nil, 3) == Key(FamilyV4, nil, 4) {
		t.Error("point-to-point gateways on different interfaces collide on the same key")
	}
}

func TestGwKeyDistinctPerFamily(t *testing.T) {
	if Key(FamilyV4, nil, 3) == Key(FamilyV6, nil, 3) {
		t.Error("v4 and v6 point-to-point gateways on one interface collide on the same key")
	}
}

func TestGatewayFamilyDefaultsToV4(t *testing.T) {
	if got := (Gateway{}).AddrFamily(); got != FamilyV4 {
		t.Errorf("zero-value Gateway family = %d, want FamilyV4 (%d)", got, FamilyV4)
	}
	if got := (Gateway{Family: FamilyV6}).AddrFamily(); got != FamilyV6 {
		t.Errorf("family() = %d, want FamilyV6 (%d)", got, FamilyV6)
	}
}

func TestGatewayString(t *testing.T) {
	withGW := Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", Metric: 500}
	if got, want := withGW.String(), "v4 10.0.1.1 dev eth1 metric 500"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	v6 := Gateway{Family: FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", Metric: 1024}
	if got, want := v6.String(), "v6 fe80::1 dev eth1 metric 1024"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	ptp := Gateway{IfName: "ppp-ee", Metric: 51}
	got := ptp.String()
	if strings.Contains(got, "<nil>") {
		t.Errorf("String() = %q, must not render a missing nexthop as <nil>", got)
	}
	if want := "v4 dev ppp-ee metric 51"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestGatewayHasNexthop(t *testing.T) {
	if !(Gateway{IP: net.IPv4(10, 0, 1, 1)}).HasNexthop() {
		t.Error("gateway with an IP should report a nexthop")
	}
	if (Gateway{IfName: "ppp-ee"}).HasNexthop() {
		t.Error("gateway without an IP should report no nexthop")
	}
}
