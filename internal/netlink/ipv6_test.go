package netlink

import (
	"encoding/binary"
	"net"
	"syscall"
	"testing"
)

func buildDiscardRouteMsg(msgType uint16, prefix string, dstLen int, routeType, table uint8) []byte {
	b := make([]byte, syscall.NLMSG_HDRLEN+12)
	binary.LittleEndian.PutUint16(b[4:6], msgType)

	rtm := b[syscall.NLMSG_HDRLEN:]
	rtm[0] = syscall.AF_INET6
	rtm[1] = uint8(dstLen)
	rtm[4] = table
	rtm[5] = RTPROT_DHCP
	rtm[7] = routeType

	b = append(b, rtAttr(RTA_DST, net.ParseIP(prefix).To16())...)
	b = append(b, rtAttr(RTA_OIF, u32le(1))...)

	binary.LittleEndian.PutUint32(b[0:4], uint32(len(b)))
	return b
}

func buildPrefixMsg(prefix string, prefixLen, ifIndex int, validLifetime uint32) []byte {
	b := make([]byte, syscall.NLMSG_HDRLEN+12)
	binary.LittleEndian.PutUint16(b[4:6], RTM_NEWPREFIX)

	pm := b[syscall.NLMSG_HDRLEN:]
	pm[0] = syscall.AF_INET6
	binary.LittleEndian.PutUint32(pm[4:8], uint32(ifIndex))
	pm[8] = 3
	pm[9] = uint8(prefixLen)
	pm[10] = 0x03

	b = append(b, rtAttr(PREFIX_ADDRESS, net.ParseIP(prefix).To16())...)

	cache := make([]byte, 8)
	binary.LittleEndian.PutUint32(cache[0:4], validLifetime)
	binary.LittleEndian.PutUint32(cache[4:8], validLifetime)
	b = append(b, rtAttr(PREFIX_CACHEINFO, cache)...)

	binary.LittleEndian.PutUint32(b[0:4], uint32(len(b)))
	return b
}

func buildAddrMsg(msgType uint16, family uint8, addr string, ifIndex int, scope, flags uint8) []byte {
	b := make([]byte, syscall.NLMSG_HDRLEN+8)
	binary.LittleEndian.PutUint16(b[4:6], msgType)

	ifa := b[syscall.NLMSG_HDRLEN:]
	ifa[0] = family
	ifa[1] = 64
	ifa[2] = flags
	ifa[3] = scope
	binary.LittleEndian.PutUint32(ifa[4:8], uint32(ifIndex))

	ip := net.ParseIP(addr)
	payload := ip.To16()
	if family == syscall.AF_INET {
		payload = ip.To4()
	}
	b = append(b, rtAttr(IFA_ADDRESS, payload)...)

	binary.LittleEndian.PutUint32(b[0:4], uint32(len(b)))
	return b
}

func raAddr(msgType uint16, addr string, ifIndex int) []byte {
	return buildAddrMsg(msgType, syscall.AF_INET6, addr, ifIndex, RT_SCOPE_UNIVERSE, 0)
}

func TestParseAddrEvent(t *testing.T) {
	ch, ok := ParseAddrEvent(raAddr(RTM_NEWADDR, "2001:db8:cafe::5054:ff:fe12:102", 3))
	if !ok {
		t.Fatal("ParseAddrEvent rejected a global IPv6 address")
	}
	if ch.IfIndex != 3 {
		t.Errorf("IfIndex = %d, want 3", ch.IfIndex)
	}
	if !ch.Addr.Equal(net.ParseIP("2001:db8:cafe::5054:ff:fe12:102")) {
		t.Errorf("Addr = %v, want 2001:db8:cafe::5054:ff:fe12:102", ch.Addr)
	}
	if ch.Withdraw {
		t.Error("RTM_NEWADDR reported as a withdrawal")
	}
}

func TestParseAddrEventWithdraw(t *testing.T) {
	ch, ok := ParseAddrEvent(raAddr(RTM_DELADDR, "2001:db8:cafe::1", 3))
	if !ok {
		t.Fatal("ParseAddrEvent rejected an RTM_DELADDR")
	}
	if !ch.Withdraw {
		t.Error("RTM_DELADDR not reported as a withdrawal")
	}
}

func TestParseAddrEventFiltersOnlyFamilyAndScope(t *testing.T) {
	for name, msg := range map[string][]byte{
		"IPv4": buildAddrMsg(RTM_NEWADDR, syscall.AF_INET, "10.0.0.1", 3, RT_SCOPE_UNIVERSE, 0),
		"link-local": buildAddrMsg(RTM_NEWADDR, syscall.AF_INET6, "fe80::1", 3,
			RT_SCOPE_LINK, 0),
		"no interface": raAddr(RTM_NEWADDR, "2001:db8:cafe::1", 0),
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseAddrEvent(msg); ok {
				t.Errorf("ParseAddrEvent accepted a %s address change", name)
			}
		})
	}
}

func TestParseAddrEventAcceptsPermanent(t *testing.T) {
	const ifaFPermanent = 0x80

	msg := buildAddrMsg(RTM_NEWADDR, syscall.AF_INET6, "fd00:dead:beef:1::1", 4,
		RT_SCOPE_UNIVERSE, ifaFPermanent)
	if _, ok := ParseAddrEvent(msg); !ok {
		t.Error("ParseAddrEvent filtered a permanent address; the resync is what decides")
	}
}

func TestParseAddrEventWithoutAddressAttribute(t *testing.T) {
	full := raAddr(RTM_NEWADDR, "2001:db8:cafe::1", 3)
	bare := full[:syscall.NLMSG_HDRLEN+8]
	binary.LittleEndian.PutUint32(bare[0:4], uint32(len(bare)))

	ch, ok := ParseAddrEvent(bare)
	if !ok {
		t.Fatal("ParseAddrEvent rejected a message with no IFA_ADDRESS")
	}
	if ch.Addr != nil {
		t.Errorf("Addr = %v, want nil", ch.Addr)
	}
	if ch.IfIndex != 3 {
		t.Errorf("IfIndex = %d, want 3", ch.IfIndex)
	}
}

func TestParseDiscardRouteEvent(t *testing.T) {
	msg := buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8:abcd::", 56, RTN_UNREACHABLE, RT_TABLE_MAIN)

	obs, ok := ParseDiscardRoute(msg)
	if !ok {
		t.Fatal("ParseDiscardRoute rejected a valid PD discard route")
	}
	if obs.Kind != ObsRoute {
		t.Errorf("obs.Kind = %q, want %q", obs.Kind, ObsRoute)
	}
	if !obs.Prefix.Equal(net.ParseIP("2001:db8:abcd::")) {
		t.Errorf("obs.Prefix = %v, want 2001:db8:abcd::", obs.Prefix)
	}
	if obs.Length != 56 {
		t.Errorf("obs.Length = %d, want 56", obs.Length)
	}
	if obs.Withdraw {
		t.Error("obs.Withdraw is true for an RTM_NEWROUTE")
	}
}

func TestParseDiscardRouteEventRejectTypes(t *testing.T) {
	for _, rt := range []uint8{RTN_UNREACHABLE, RTN_BLACKHOLE, RTN_PROHIBIT} {
		msg := buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8::", 48, rt, RT_TABLE_MAIN)
		if _, ok := ParseDiscardRoute(msg); !ok {
			t.Errorf("ParseDiscardRoute rejected route type %d", rt)
		}
	}
}

func TestParseDiscardRouteEventWithdraw(t *testing.T) {
	msg := buildDiscardRouteMsg(RTM_DELROUTE, "2001:db8:abcd::", 56, RTN_UNREACHABLE, RT_TABLE_MAIN)

	obs, ok := ParseDiscardRoute(msg)
	if !ok {
		t.Fatal("ParseDiscardRoute dropped an RTM_DELROUTE")
	}
	if !obs.Withdraw {
		t.Error("obs.Withdraw is false for an RTM_DELROUTE")
	}
}

func TestParseDiscardRouteEventLengthBounds(t *testing.T) {
	if _, ok := ParseDiscardRoute(
		buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8:1:2::", 64, RTN_UNREACHABLE, RT_TABLE_MAIN)); !ok {
		t.Error("ParseDiscardRoute rejected a /64 delegation")
	}
	if _, ok := ParseDiscardRoute(
		buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8::", 72, RTN_UNREACHABLE, RT_TABLE_MAIN)); ok {
		t.Error("ParseDiscardRoute accepted a prefix longer than a /64")
	}
	if _, ok := ParseDiscardRoute(
		buildDiscardRouteMsg(RTM_NEWROUTE, "::", 0, RTN_UNREACHABLE, RT_TABLE_MAIN)); ok {
		t.Error("ParseDiscardRoute accepted a zero-length prefix")
	}
}

func TestParseDiscardRouteEventIgnoresOthers(t *testing.T) {
	if _, ok := ParseDiscardRoute(
		buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8::", 48, RTN_UNICAST, RT_TABLE_MAIN)); ok {
		t.Error("ParseDiscardRoute accepted a unicast route")
	}
	if _, ok := ParseDiscardRoute(
		buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8::", 48, RTN_UNREACHABLE, 200)); ok {
		t.Error("ParseDiscardRoute accepted a route outside the main table")
	}
}

func TestParseDiscardRouteEventIgnoresIPv4(t *testing.T) {
	lo := loopback(t)
	msg := buildRouteMsg(RTM_NEWROUTE, RTPROT_DHCP, net.IPv4(10, 0, 1, 1), lo.Index, 500)

	if _, ok := ParseDiscardRoute(msg); ok {
		t.Error("ParseDiscardRoute accepted an IPv4 route event")
	}
}

func TestParseRAPrefixEvent(t *testing.T) {
	lo := loopback(t)
	msg := buildPrefixMsg("2001:db8:1:2::", 64, lo.Index, 86400)

	obs, ok := ParseRAPrefix(msg)
	if !ok {
		t.Fatal("ParseRAPrefix rejected a valid RTM_NEWPREFIX")
	}
	if obs.Kind != ObsRA {
		t.Errorf("obs.Kind = %q, want %q", obs.Kind, ObsRA)
	}
	if !obs.Prefix.Equal(net.ParseIP("2001:db8:1:2::")) || obs.Length != 64 {
		t.Errorf("obs prefix = %v/%d, want 2001:db8:1:2::/64", obs.Prefix, obs.Length)
	}
	if obs.IfName != lo.Name {
		t.Errorf("obs.IfName = %q, want %q", obs.IfName, lo.Name)
	}
	if obs.Withdraw {
		t.Error("obs.Withdraw is true for a prefix with a live lifetime")
	}
}

func TestParseRAPrefixEventZeroLifetimeIsWithdrawal(t *testing.T) {
	lo := loopback(t)
	msg := buildPrefixMsg("2001:db8:1:2::", 64, lo.Index, 0)

	obs, ok := ParseRAPrefix(msg)
	if !ok {
		t.Fatal("ParseRAPrefix dropped a zero-lifetime prefix")
	}
	if !obs.Withdraw {
		t.Error("a zero valid lifetime was not treated as a withdrawal")
	}
}

func TestPrefixSocketParsersAreDisjoint(t *testing.T) {
	lo := loopback(t)

	msgs := map[string][]byte{
		"route":  buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8:abcd::", 56, RTN_UNREACHABLE, RT_TABLE_MAIN),
		"prefix": buildPrefixMsg("2001:db8:1:2::", 64, lo.Index, 86400),
		"addr":   raAddr(RTM_NEWADDR, "2001:db8:cafe::1", lo.Index),
	}
	parsers := map[string]func([]byte) bool{
		"route":  func(b []byte) bool { _, ok := ParseDiscardRoute(b); return ok },
		"prefix": func(b []byte) bool { _, ok := ParseRAPrefix(b); return ok },
		"addr":   func(b []byte) bool { _, ok := ParseAddrEvent(b); return ok },
	}

	for kind, msg := range msgs {
		for parser, accepts := range parsers {
			if got := accepts(msg); got != (kind == parser) {
				t.Errorf("the %s parser returned %v for a %s message", parser, got, kind)
			}
		}
	}
}

func TestSplitMessages(t *testing.T) {
	a := buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8:aaaa::", 56, RTN_UNREACHABLE, RT_TABLE_MAIN)
	b := buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8:bbbb::", 60, RTN_UNREACHABLE, RT_TABLE_MAIN)

	msgs := SplitMessages(append(append([]byte{}, a...), b...))
	if len(msgs) != 2 {
		t.Fatalf("SplitMessages returned %d messages, want 2", len(msgs))
	}

	first, ok := ParseDiscardRoute(msgs[0])
	if !ok || !first.Prefix.Equal(net.ParseIP("2001:db8:aaaa::")) {
		t.Errorf("first message = %v, want 2001:db8:aaaa::", first.Prefix)
	}
	second, ok := ParseDiscardRoute(msgs[1])
	if !ok || second.Length != 60 {
		t.Errorf("second message length = %d, want 60", second.Length)
	}
}

func TestSplitMessagesTruncated(t *testing.T) {
	full := buildDiscardRouteMsg(RTM_NEWROUTE, "2001:db8::", 56, RTN_UNREACHABLE, RT_TABLE_MAIN)

	if msgs := SplitMessages(full[:len(full)-4]); len(msgs) != 0 {
		t.Errorf("SplitMessages on a truncated message returned %d messages, want 0", len(msgs))
	}
	if msgs := SplitMessages(nil); len(msgs) != 0 {
		t.Errorf("SplitMessages(nil) returned %d messages, want 0", len(msgs))
	}
}
