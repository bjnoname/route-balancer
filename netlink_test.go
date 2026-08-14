package main

import (
	"encoding/binary"
	"net"
	"syscall"
	"testing"
)

// rtAttr encodes a single netlink route attribute (len, type, payload) with
// the trailing 4-byte alignment padding the kernel uses.
func rtAttr(attrType uint16, payload []byte) []byte {
	l := 4 + len(payload)
	b := make([]byte, (l+3)&^3)
	binary.LittleEndian.PutUint16(b[0:2], uint16(l))
	binary.LittleEndian.PutUint16(b[2:4], attrType)
	copy(b[4:], payload)
	return b
}

func u32le(v int) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))
	return b
}

// buildRouteMsg assembles a synthetic netlink message of the shape the kernel
// emits for an IPv4 default route in the main table. A nil gwIP produces a
// message with no RTA_GATEWAY attribute, which is what pppd-installed
// point-to-point routes look like on the wire.
func buildRouteMsg(msgType uint16, proto uint8, gwIP net.IP, ifIndex, metric int) []byte {
	b := make([]byte, syscall.NLMSG_HDRLEN+12)
	binary.LittleEndian.PutUint16(b[4:6], msgType) // NlMsghdr.Type

	rtm := b[syscall.NLMSG_HDRLEN:]
	rtm[0] = syscall.AF_INET // Family
	rtm[1] = 0               // DstLen — 0 means default route
	rtm[4] = RT_TABLE_MAIN   // Table
	rtm[5] = proto           // Protocol
	rtm[6] = RT_SCOPE_LINK   // Scope
	rtm[7] = RTN_UNICAST     // Type

	if gwIP != nil {
		b = append(b, rtAttr(RTA_GATEWAY, gwIP.To4())...)
	}
	b = append(b, rtAttr(RTA_OIF, u32le(ifIndex))...)
	b = append(b, rtAttr(RTA_PRIORITY, u32le(metric))...)

	binary.LittleEndian.PutUint32(b[0:4], uint32(len(b))) // NlMsghdr.Len
	return b
}

// loopback returns the loopback interface, which every test host has, so that
// parseRouteEvent's InterfaceByIndex lookup resolves.
func loopback(t *testing.T) *net.Interface {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			return &ifi
		}
	}
	t.Skip("no loopback interface available")
	return nil
}

func TestParseRouteEventWithGateway(t *testing.T) {
	lo := loopback(t)
	msg := buildRouteMsg(RTM_NEWROUTE, RTPROT_DHCP, net.IPv4(10, 0, 1, 1), lo.Index, 500)

	msgType, gw, ok := parseRouteEvent(msg)
	if !ok {
		t.Fatal("parseRouteEvent rejected a valid default route with a nexthop")
	}
	if msgType != RTM_NEWROUTE {
		t.Errorf("msgType = %d, want %d", msgType, RTM_NEWROUTE)
	}
	if !gw.IP.Equal(net.IPv4(10, 0, 1, 1)) {
		t.Errorf("gw.IP = %v, want 10.0.1.1", gw.IP)
	}
	if gw.IfName != lo.Name || gw.IfIndex != lo.Index {
		t.Errorf("gw iface = %s/%d, want %s/%d", gw.IfName, gw.IfIndex, lo.Name, lo.Index)
	}
	if gw.Metric != 500 {
		t.Errorf("gw.Metric = %d, want 500", gw.Metric)
	}
}

// TestParseRouteEventPointToPoint covers the pppd case: "default dev ppp-ee
// scope link metric 51" carries no RTA_GATEWAY because there is no address to
// route via. The event must still be surfaced, keyed on the interface index.
func TestParseRouteEventPointToPoint(t *testing.T) {
	lo := loopback(t)
	msg := buildRouteMsg(RTM_NEWROUTE, RTPROT_BOOT, nil, lo.Index, 51)

	_, gw, ok := parseRouteEvent(msg)
	if !ok {
		t.Fatal("parseRouteEvent dropped a nexthop-less (point-to-point) default route")
	}
	if gw.IP != nil {
		t.Errorf("gw.IP = %v, want nil for a point-to-point route", gw.IP)
	}
	if gw.IfName != lo.Name || gw.IfIndex != lo.Index {
		t.Errorf("gw iface = %s/%d, want %s/%d", gw.IfName, gw.IfIndex, lo.Name, lo.Index)
	}
	if gw.Metric != 51 {
		t.Errorf("gw.Metric = %d, want 51", gw.Metric)
	}
}

// A route event with no output interface cannot be acted on and must be dropped.
func TestParseRouteEventNoInterface(t *testing.T) {
	msg := buildRouteMsg(RTM_NEWROUTE, RTPROT_BOOT, nil, 0, 51)
	if _, _, ok := parseRouteEvent(msg); ok {
		t.Error("parseRouteEvent accepted an event with no output interface")
	}
}

// Events carrying our own protocol number are our own ECMP route updates and
// must be dropped to avoid a self-feeding loop.
func TestParseRouteEventSkipsOwnProto(t *testing.T) {
	lo := loopback(t)
	msg := buildRouteMsg(RTM_NEWROUTE, uint8(cfg.routeProtoInt()), nil, lo.Index, 0)
	if _, _, ok := parseRouteEvent(msg); ok {
		t.Error("parseRouteEvent accepted an event tagged with our own proto")
	}
}
