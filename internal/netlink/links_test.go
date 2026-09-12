package netlink

import (
	"encoding/binary"
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
)

func attr(kind uint16, payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint16(out[0:2], uint16(4+len(payload)))
	binary.LittleEndian.PutUint16(out[2:4], kind)
	copy(out[4:], payload)
	for len(out)%4 != 0 {
		out = append(out, 0)
	}
	return out
}

func v4(a, b, c, d byte) []byte { return []byte{a, b, c, d} }

func TestOnAPointToPointLinkTheLocalAddressWins(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		attrs []byte
	}{
		{"local first", append(attr(IFA_LOCAL, v4(10, 0, 0, 1)), attr(IFA_ADDRESS, v4(10, 0, 0, 2))...)},
		{"peer first", append(attr(IFA_ADDRESS, v4(10, 0, 0, 2)), attr(IFA_LOCAL, v4(10, 0, 0, 1))...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := addrOf(&ifAddrMsg{Family: syscall.AF_INET, PrefixLen: 32, Index: 4}, tc.attrs)
			if !ok {
				t.Fatal("a point-to-point address was not read at all")
			}
			if !got.IP.Equal(net.IPv4(10, 0, 0, 1)) {
				t.Errorf("read %v, want 10.0.0.1 — IFA_ADDRESS is the peer, not us", got.IP)
			}
		})
	}
}

func TestAnOrdinaryAddressComesFromIFA_ADDRESS(t *testing.T) {
	t.Parallel()

	got, ok := addrOf(&ifAddrMsg{Family: syscall.AF_INET, PrefixLen: 24, Index: 2},
		attr(IFA_ADDRESS, v4(10, 0, 0, 5)))
	if !ok {
		t.Fatal("an ordinary address was not read")
	}
	if !got.IP.Equal(net.IPv4(10, 0, 0, 5)) {
		t.Errorf("read %v, want 10.0.0.5", got.IP)
	}
	if ones, bits := got.Mask.Size(); ones != 24 || bits != 32 {
		t.Errorf("mask is /%d of %d bits, want /24 of 32", ones, bits)
	}
}

func TestAnAddressOfTheWrongLengthIsNotRead(t *testing.T) {
	t.Parallel()

	if _, ok := addrOf(&ifAddrMsg{Family: syscall.AF_INET, Index: 2},
		attr(IFA_ADDRESS, []byte{10, 0, 0})); ok {
		t.Error("a truncated IPv4 address was read as an address")
	}
	if _, ok := addrOf(&ifAddrMsg{Family: syscall.AF_INET6, Index: 2},
		attr(IFA_ADDRESS, v4(10, 0, 0, 1))); ok {
		t.Error("a four-byte payload was read as an IPv6 address")
	}
}

func testLinks() Links {
	return Links{
		Links: []Link{{Index: 2, Name: "eth0"}, {Index: 3, Name: "eth1"}},
		Addrs: []Addr{
			{Index: 2, IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
			{Index: 2, IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)},
			{Index: 2, IP: net.IPv4(10, 0, 0, 6), Mask: net.CIDRMask(24, 32)},
			{Index: 3, IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)},
		},
	}
}

func TestPrimaryV4TakesTheFirstIPv4OnTheLink(t *testing.T) {
	t.Parallel()

	l := testLinks()

	got := l.PrimaryV4(2)
	if got == nil || got.IP.String() != "10.0.0.5" {
		t.Errorf("PrimaryV4(2) = %v, want 10.0.0.5/24 — the v6 address on the same link is not it", got)
	}
	if got := l.PrimaryV4(3); got != nil {
		t.Errorf("PrimaryV4(3) = %v, want nil — that link has no IPv4 address", got)
	}
	if got := l.PrimaryV4(99); got != nil {
		t.Errorf("PrimaryV4(99) = %v, want nil", got)
	}
}

func TestLocalV6IsEveryIPv6AddressOnTheHost(t *testing.T) {
	t.Parallel()

	got := testLinks().LocalV6()

	for _, want := range []string{"fe80::1", "2001:db8::1"} {
		if !got[want] {
			t.Errorf("LocalV6 is missing %s — link-local counts, it is still ours", want)
		}
	}
	if len(got) != 2 {
		t.Errorf("LocalV6 = %v, want only the two IPv6 addresses", got)
	}
}

func TestIndexOfNamesTheLink(t *testing.T) {
	t.Parallel()

	l := testLinks()
	if idx, ok := l.IndexOf("eth1"); !ok || idx != 3 {
		t.Errorf("IndexOf(eth1) = %d, %v, want 3, true", idx, ok)
	}
	if _, ok := l.IndexOf("eth9"); ok {
		t.Error("IndexOf claimed an interface that is not there")
	}
}

func TestAFailedDumpIsAnErrorNotAnEmptyPicture(t *testing.T) {
	t.Parallel()

	boom := errors.New("netlinkrib: no buffer space available")
	ans := command.Answers{
		LinksQuery().String(): {Err: boom},
		AddrsQuery().String(): {Val: []Addr{}},
	}

	got, err := ReadLinks(ans)
	if !errors.Is(err, boom) {
		t.Errorf("ReadLinks returned %v, want the dump's own error", err)
	}
	if len(got.Links) != 0 || len(got.Addrs) != 0 {
		t.Errorf("a failed read came back with a picture: %v", got)
	}
}

func TestTheTwoDumpsAreOneReadEach(t *testing.T) {
	t.Parallel()

	sys := command.NewSystem()
	command.Observe(sys, []command.Query{LinksQuery(), AddrsQuery(), LinksQuery(), AddrsQuery()})

	if n := sys.Reads(); n != 2 {
		t.Errorf("the whole interface picture cost %d reads, want 2", n)
	}
}
