package probe

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
)

func TestIsICMP(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"", true},
		{"icmp", true},
		{"http", false},
		{"https", false},
		{"tcp", false},
		{"dns", false},
		{"exec", false},
	}
	for _, tc := range tests {
		if got := IsICMP(config.Probe{Type: tc.typ}); got != tc.want {
			t.Errorf("IsICMP(%q) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

func TestNewSelectsByType(t *testing.T) {
	for _, tc := range []struct {
		pcfg   config.Probe
		prefix string
	}{
		{config.Probe{}, "icmp("},
		{config.Probe{Type: "icmp"}, "icmp("},
		{config.Probe{Type: "http", URL: "http://example.com"}, "http("},
		{config.Probe{Type: "https", URL: "https://example.com"}, "http("},
		{config.Probe{Type: "tcp", Host: "example.com", Port: 443}, "tcp("},
		{config.Probe{Type: "dns", Resolver: "8.8.8.8:53", Query: "example.com"}, "dns("},
		{config.Probe{Type: "exec", Command: []string{"true"}}, "exec("},
	} {
		if got := New(tc.pcfg).String(); !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("New(%q) = %s, want a %s probe", tc.pcfg.Type, got, tc.prefix)
		}
	}
}

func TestNewAppliesTypeSpecificDefaults(t *testing.T) {
	if got := New(config.Probe{}).String(); !strings.Contains(got, "payload=56") {
		t.Errorf("icmp probe = %s, want the default 56-byte payload", got)
	}
	if got := New(config.Probe{Type: "http", URL: "http://example.com"}).String(); !strings.Contains(got, "GET") {
		t.Errorf("http probe = %s, want the default GET method", got)
	}
}

func TestICMPRejectsMissingGateway(t *testing.T) {
	p := &icmpProbe{PayloadSize: 56}
	err := p.Check(context.Background(), Target{IfName: "ppp-ee"})
	if err == nil {
		t.Fatal("Check accepted a target with no gateway address")
	}
	if !strings.Contains(err.Error(), "ppp-ee") {
		t.Errorf("error %q should name the interface", err)
	}
}

func TestICMPRejectsAFamilyItsAddressContradicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target Target
		want   string
	}{
		{
			name:   "v4 probe, v6 address",
			target: Target{Family: syscall.AF_INET, GatewayIP: net.ParseIP("fe80::1"), IfName: "eth1"},
			want:   "IPv4",
		},
		{
			name:   "v6 probe, v4 address",
			target: Target{Family: syscall.AF_INET6, GatewayIP: net.ParseIP("10.0.1.1"), IfName: "eth1"},
			want:   "IPv6",
		},
	} {
		err := (&icmpProbe{PayloadSize: 56}).Check(context.Background(), tc.target)
		if err == nil {
			t.Fatalf("%s: Check accepted a target contradicting itself", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "eth1") {
			t.Errorf("%s: error %q should name the interface and the family probed", tc.name, err)
		}
	}
}

func TestEchoLayoutIsOneShapeWithTwoTypes(t *testing.T) {
	for _, tc := range []struct {
		msgType   byte
		replyType byte
	}{
		{icmpv4Echo, icmpv4EchoReply},
		{icmpv6Echo, icmpv6EchoReply},
	} {
		msg := buildEcho(tc.msgType, 0x1234, 0x5678, make([]byte, 56))

		if len(msg) != 64 {
			t.Errorf("echo of a 56-byte payload is %d bytes, want 64", len(msg))
		}
		if msg[0] != tc.msgType || msg[1] != 0 {
			t.Errorf("type/code = %d/%d, want %d/0", msg[0], msg[1], tc.msgType)
		}
		if msg[2] != 0 || msg[3] != 0 {
			t.Error("checksum field should be left zero by the builder")
		}

		reply := buildEcho(tc.replyType, 0x1234, 0x5678, nil)
		if !matchEcho(reply, tc.replyType, 0x1234, 0x5678) {
			t.Error("a reply carrying our id and sequence did not match")
		}
		if matchEcho(reply, tc.replyType, 0x1234, 0x5679) {
			t.Error("a reply to a different sequence number matched")
		}
		if matchEcho(reply, tc.replyType, 0x4321, 0x5678) {
			t.Error("a reply to another process's echo matched")
		}
		if matchEcho(reply[:7], tc.replyType, 0x1234, 0x5678) {
			t.Error("a message too short to hold an id and sequence matched")
		}
	}
}

func TestEchoSequenceAdvancesPerProbeAndNotAcrossThem(t *testing.T) {
	p := &icmpProbe{PayloadSize: 56, seq: 0xfffe}
	first, second := p.nextSeq(), p.nextSeq()
	if second == first {
		t.Errorf("two requests from one probe share sequence %d", first)
	}
	if first != 0xffff || second != 0 {
		t.Errorf("sequence %d, %d: want 0xffff then a wrap to 0", first, second)
	}

	other := &icmpProbe{PayloadSize: 56}
	if got := other.nextSeq(); got != 1 {
		t.Errorf("a second probe started at %d, so the counters are shared", got-1)
	}
}

func TestEchoReplyFilterPassesOnlyEchoReplies(t *testing.T) {
	filt := echoReplyFilter()
	if len(filt) != 32 {
		t.Fatalf("filter is %d bytes, want 32", len(filt))
	}

	blocked := func(msgType int) bool {
		word := binary.NativeEndian.Uint32(filt[(msgType>>5)*4:])
		return word&(1<<(msgType&31)) != 0
	}

	if blocked(icmpv6EchoReply) {
		t.Error("echo replies are blocked, so no probe could ever pass")
	}
	for _, msgType := range []int{icmpv6Echo, 1, 3, 128, 130, 134, 135, 136} {
		if !blocked(msgType) {
			t.Errorf("ICMPv6 type %d reaches the read loop", msgType)
		}
	}
}

func TestNetworkPinsTheFamilyBeingProbed(t *testing.T) {
	for _, tc := range []struct {
		base   string
		family int
		want   string
	}{
		{"tcp", syscall.AF_INET, "tcp4"},
		{"tcp", syscall.AF_INET6, "tcp6"},
		{"udp", syscall.AF_INET, "udp4"},
		{"udp", syscall.AF_INET6, "udp6"},
	} {
		if got := network(tc.base, tc.family); got != tc.want {
			t.Errorf("network(%q, %d) = %q, want %q", tc.base, tc.family, got, tc.want)
		}
	}
}

func TestTargetSameAsIgnoresIPRepresentation(t *testing.T) {
	four := Target{GatewayIP: net.IPv4(10, 0, 1, 1).To4(), IfName: "eth1", IfIndex: 3}
	sixteen := Target{GatewayIP: net.ParseIP("10.0.1.1"), IfName: "eth1", IfIndex: 3}
	if !four.SameAs(sixteen) {
		t.Error("the same address in two representations read as two targets")
	}

	if four.SameAs(Target{GatewayIP: net.IPv4(10, 0, 1, 254), IfName: "eth1", IfIndex: 3}) {
		t.Error("a changed nexthop read as the same target")
	}
	if four.SameAs(Target{GatewayIP: net.IPv4(10, 0, 1, 1), IfName: "eth2", IfIndex: 3}) {
		t.Error("a different interface read as the same target")
	}

	v4 := Target{Family: syscall.AF_INET, IfName: "eth1", IfIndex: 3}
	if v4.SameAs(Target{Family: syscall.AF_INET6, IfName: "eth1", IfIndex: 3}) {
		t.Error("targets in two families read as the same target")
	}
}

func TestResolverAddressAcceptsEveryHostForm(t *testing.T) {
	for _, tc := range []struct{ in, host, port string }{
		{"8.8.8.8", "8.8.8.8", "53"},
		{"8.8.8.8:5353", "8.8.8.8", "5353"},
		{"2001:4860:4860::8888", "2001:4860:4860::8888", "53"},
		{"[2001:4860:4860::8888]", "2001:4860:4860::8888", "53"},
		{"[2001:4860:4860::8888]:5353", "2001:4860:4860::8888", "5353"},
		{"dns.example.com", "dns.example.com", "53"},
		{"dns.example.com:5353", "dns.example.com", "5353"},
	} {
		got := withDefaultPort(tc.in, "53")
		host, port, err := net.SplitHostPort(got)
		if err != nil {
			t.Errorf("withDefaultPort(%q) = %q, which the dialer cannot parse: %v", tc.in, got, err)
			continue
		}
		if host != tc.host || port != tc.port {
			t.Errorf("withDefaultPort(%q) = host %q port %q, want %q and %q", tc.in, host, port, tc.host, tc.port)
		}
	}
}

func TestTCPAddressBracketsOnlyIPv6(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"8.8.8.8", "8.8.8.8:53"},
		{"2001:4860:4860::8888", "[2001:4860:4860::8888]:53"},
		{"dns.example.com", "dns.example.com:53"},
	} {
		p := &tcpProbe{Host: tc.host, Port: 53}
		if got := p.addr(); got != tc.want {
			t.Errorf("addr() for host %q = %q, want %q", tc.host, got, tc.want)
		}
		if got := p.String(); !strings.Contains(got, tc.want) {
			t.Errorf("String() = %q, want it to name the dial target %q", got, tc.want)
		}
	}
}

func TestValidateRejectsAProbeThatCouldNeverPass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pcfg    config.Probe
		wantErr bool
	}{
		{name: "icmp needs nothing", pcfg: config.Probe{Type: "icmp"}},
		{name: "an empty type is icmp", pcfg: config.Probe{}},
		{name: "none runs nothing", pcfg: config.Probe{Type: config.ProbeNone}},
		{name: "http with a url", pcfg: config.Probe{Type: "http", URL: "http://example.com/"}},
		{name: "https with a url", pcfg: config.Probe{Type: "https", URL: "https://example.com/"}},
		{name: "http with no url", pcfg: config.Probe{Type: "http"}, wantErr: true},
		{name: "http with a bare host", pcfg: config.Probe{Type: "http", URL: "example.com"}, wantErr: true},
		{name: "http with the wrong scheme", pcfg: config.Probe{Type: "http", URL: "ftp://example.com/"}, wantErr: true},
		{name: "tcp with host and port", pcfg: config.Probe{Type: "tcp", Host: "192.0.2.1", Port: 443}},
		{name: "tcp with no host", pcfg: config.Probe{Type: "tcp", Port: 443}, wantErr: true},
		{name: "tcp with no port", pcfg: config.Probe{Type: "tcp", Host: "192.0.2.1"}, wantErr: true},
		{name: "dns with a resolver", pcfg: config.Probe{Type: "dns", Resolver: "192.0.2.1"}},
		{name: "dns with no resolver", pcfg: config.Probe{Type: "dns"}, wantErr: true},
		{name: "exec with a command", pcfg: config.Probe{Type: "exec", Command: []string{"true"}}},
		{name: "exec with no command", pcfg: config.Probe{Type: "exec"}, wantErr: true},
		{name: "a typo is not silently icmp", pcfg: config.Probe{Type: "htttp"}, wantErr: true},
	} {
		if err := Validate(tc.pcfg); (err != nil) != tc.wantErr {
			t.Errorf("%s: Validate = %v, want error: %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestDNSQueryDefaultsTheWayMethodDoes(t *testing.T) {
	p, ok := New(config.Probe{Type: "dns", Resolver: "192.0.2.1"}).(*dnsProbe)
	if !ok {
		t.Fatal("a dns config did not build a dns probe")
	}
	if p.Query != "example.com" {
		t.Errorf("query = %q, want example.com", p.Query)
	}
}
