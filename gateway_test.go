package main

import (
	"net"
	"strings"
	"testing"
)

func TestGwKey(t *testing.T) {
	tests := []struct {
		name    string
		ip      net.IP
		ifIndex int
		want    string
	}{
		{"with nexthop", net.IPv4(10, 0, 1, 1), 3, "10.0.1.1@3"},
		{"point-to-point", nil, 7, "dev@7"},
		{"empty IP is treated as absent", net.IP{}, 7, "dev@7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gwKey(tc.ip, tc.ifIndex); got != tc.want {
				t.Errorf("gwKey(%v, %d) = %q, want %q", tc.ip, tc.ifIndex, got, tc.want)
			}
		})
	}
}

// Two point-to-point gateways on different interfaces must not share a key.
func TestGwKeyDistinctPerInterface(t *testing.T) {
	if gwKey(nil, 3) == gwKey(nil, 4) {
		t.Error("point-to-point gateways on different interfaces collide on the same key")
	}
}

func TestGatewayString(t *testing.T) {
	withGW := Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", Metric: 500}
	if got, want := withGW.String(), "10.0.1.1 dev eth1 metric 500"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	ptp := Gateway{IfName: "ppp-ee", Metric: 51}
	got := ptp.String()
	if strings.Contains(got, "<nil>") {
		t.Errorf("String() = %q, must not render a missing nexthop as <nil>", got)
	}
	if want := "dev ppp-ee metric 51"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestGatewayHasNexthop(t *testing.T) {
	if !(Gateway{IP: net.IPv4(10, 0, 1, 1)}).hasNexthop() {
		t.Error("gateway with an IP should report a nexthop")
	}
	if (Gateway{IfName: "ppp-ee"}).hasNexthop() {
		t.Error("gateway without an IP should report no nexthop")
	}
}
