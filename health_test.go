package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestIsICMPProbe(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"", true}, // icmp is the default
		{"icmp", true},
		{"http", false},
		{"https", false},
		{"tcp", false},
		{"dns", false},
		{"exec", false},
	}
	for _, tc := range tests {
		if got := isICMPProbe(ProbeConfig{Type: tc.typ}); got != tc.want {
			t.Errorf("isICMPProbe(%q) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

// An ICMP probe has no destination on a point-to-point link. It must report
// that clearly instead of sending an echo request to 0.0.0.0, which fails with
// an opaque errno and gives the operator nothing to act on.
func TestICMPProbeRejectsMissingGateway(t *testing.T) {
	p := &ICMPProbe{PayloadSize: 56}
	err := p.Check(context.Background(), Target{IfName: "ppp-ee"})
	if err == nil {
		t.Fatal("Check accepted a target with no gateway address")
	}
	if !strings.Contains(err.Error(), "ppp-ee") {
		t.Errorf("error %q should name the interface", err)
	}
}

func TestNewHealthMonitorKeysPointToPointGateway(t *testing.T) {
	gw := Gateway{IfIndex: 9, IfName: "ppp-ee"}
	m := newHealthMonitor(gw, HealthConfig{Probe: ProbeConfig{Type: "exec"}})
	if want := gwKey(nil, 9); m.gwKey != want {
		t.Errorf("gwKey = %q, want %q", m.gwKey, want)
	}
	if m.target.GatewayIP != nil {
		t.Errorf("target.GatewayIP = %v, want nil", m.target.GatewayIP)
	}
	if m.target.IfName != "ppp-ee" {
		t.Errorf("target.IfName = %q, want %q", m.target.IfName, "ppp-ee")
	}
}

func TestGatewaySrcAddrPrefersCachedValue(t *testing.T) {
	// IfIndex 0 never resolves, so a returned address can only be the cached one.
	gw := Gateway{IfName: "eth1", Src: net.IPv4(10, 0, 1, 2)}
	if got := gw.srcAddr(); !got.Equal(net.IPv4(10, 0, 1, 2)) {
		t.Errorf("srcAddr() = %v, want 10.0.1.2", got)
	}
	if got := (Gateway{IfName: "eth1"}).srcAddr(); got != nil {
		t.Errorf("srcAddr() = %v, want nil when nothing is cached and the interface is gone", got)
	}
}
