package netlink

import (
	"encoding/binary"
	"net"
	"syscall"
	"testing"

	"github.com/bjnoname/route-balancer/internal/route"
)

const testProto = 111

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

type routeMsgOpts struct {
	msgType  uint16
	family   uint8
	rtnType  uint8
	proto    uint8
	table    uint8
	rtaTable int
	gwIP     net.IP
	dst      net.IP
	dstLen   uint8
	ifIndex  int
	metric   int
}

func buildRoute(o routeMsgOpts) []byte {
	family := o.family
	if family == 0 {
		family = syscall.AF_INET
	}
	rtnType := o.rtnType
	if rtnType == 0 {
		rtnType = RTN_UNICAST
	}
	table := o.table
	if table == 0 {
		table = RT_TABLE_MAIN
	}

	encode := func(ip net.IP) []byte {
		if family == syscall.AF_INET6 {
			return ip.To16()
		}
		return ip.To4()
	}

	b := make([]byte, syscall.NLMSG_HDRLEN+12)
	binary.LittleEndian.PutUint16(b[4:6], o.msgType)

	rtm := b[syscall.NLMSG_HDRLEN:]
	rtm[0] = family
	rtm[1] = o.dstLen
	rtm[4] = table
	rtm[5] = o.proto
	rtm[6] = RT_SCOPE_LINK
	rtm[7] = rtnType

	if o.dst != nil {
		b = append(b, rtAttr(RTA_DST, encode(o.dst))...)
	}
	if o.gwIP != nil {
		b = append(b, rtAttr(RTA_GATEWAY, encode(o.gwIP))...)
	}
	b = append(b, rtAttr(RTA_OIF, u32le(o.ifIndex))...)
	b = append(b, rtAttr(RTA_PRIORITY, u32le(o.metric))...)
	if o.rtaTable != 0 {
		b = append(b, rtAttr(RTA_TABLE, u32le(o.rtaTable))...)
	}

	binary.LittleEndian.PutUint32(b[0:4], uint32(len(b)))
	return b
}

func buildRouteMsg(msgType uint16, proto uint8, gwIP net.IP, ifIndex, metric int) []byte {
	return buildRoute(routeMsgOpts{
		msgType: msgType, proto: proto, gwIP: gwIP, ifIndex: ifIndex, metric: metric,
	})
}

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

	msgType, gw, ok := ParseRouteEvent(msg, testProto)
	if !ok {
		t.Fatal("ParseRouteEvent rejected a valid default route with a nexthop")
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

func TestParseRouteEventPointToPoint(t *testing.T) {
	lo := loopback(t)
	msg := buildRouteMsg(RTM_NEWROUTE, RTPROT_BOOT, nil, lo.Index, 51)

	_, gw, ok := ParseRouteEvent(msg, testProto)
	if !ok {
		t.Fatal("ParseRouteEvent dropped a nexthop-less (point-to-point) default route")
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

func TestParseRouteEventNoInterface(t *testing.T) {
	msg := buildRouteMsg(RTM_NEWROUTE, RTPROT_BOOT, nil, 0, 51)
	if _, _, ok := ParseRouteEvent(msg, testProto); ok {
		t.Error("ParseRouteEvent accepted an event with no output interface")
	}
}

func TestParseRouteEventSkipsOwnProto(t *testing.T) {
	lo := loopback(t)
	msg := buildRouteMsg(RTM_NEWROUTE, uint8(testProto), nil, lo.Index, 0)
	if _, _, ok := ParseRouteEvent(msg, testProto); ok {
		t.Error("ParseRouteEvent accepted an event tagged with our own proto")
	}
}

const RTPROT_RA = 9

func TestParseRouteEventIPv6(t *testing.T) {
	lo := loopback(t)
	msg := buildRoute(routeMsgOpts{
		msgType: RTM_NEWROUTE, family: syscall.AF_INET6, proto: RTPROT_RA,
		gwIP: net.ParseIP("fe80::1"), ifIndex: lo.Index, metric: 1024,
	})

	msgType, gw, ok := ParseRouteEvent(msg, testProto)
	if !ok {
		t.Fatal("ParseRouteEvent rejected a valid IPv6 default route")
	}
	if msgType != RTM_NEWROUTE {
		t.Errorf("msgType = %d, want %d", msgType, RTM_NEWROUTE)
	}
	if gw.AddrFamily() != route.FamilyV6 {
		t.Errorf("gw.AddrFamily() = %d, want route.FamilyV6 (%d)", gw.AddrFamily(), route.FamilyV6)
	}
	if !gw.IP.Equal(net.ParseIP("fe80::1")) {
		t.Errorf("gw.IP = %v, want fe80::1", gw.IP)
	}
	if len(gw.IP) != net.IPv6len {
		t.Errorf("gw.IP is %d bytes, want the full 16 so the v6 form is preserved", len(gw.IP))
	}
	if gw.IfIndex != lo.Index || gw.Metric != 1024 {
		t.Errorf("gw = %+v, want iface %d metric 1024", gw, lo.Index)
	}
}

func TestParseRouteEventIPv6PointToPoint(t *testing.T) {
	lo := loopback(t)
	msg := buildRoute(routeMsgOpts{
		msgType: RTM_NEWROUTE, family: syscall.AF_INET6,
		proto: RTPROT_STATIC, ifIndex: lo.Index, metric: 1,
	})

	_, gw, ok := ParseRouteEvent(msg, testProto)
	if !ok {
		t.Fatal("ParseRouteEvent dropped a nexthop-less IPv6 default route")
	}
	if gw.HasNexthop() {
		t.Errorf("gw.IP = %v, want nil for a point-to-point route", gw.IP)
	}
	if gw.AddrFamily() != route.FamilyV6 {
		t.Errorf("gw.AddrFamily() = %d, want route.FamilyV6", gw.AddrFamily())
	}
}

func TestParseRouteEventRejectsNonDefaultDestination(t *testing.T) {
	lo := loopback(t)

	for _, tc := range []struct {
		name   string
		family uint8
		dst    net.IP
	}{
		{"ipv4", syscall.AF_INET, net.IPv4(10, 0, 0, 0)},
		{"ipv6", syscall.AF_INET6, net.ParseIP("2001:db8::")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := buildRoute(routeMsgOpts{
				msgType: RTM_NEWROUTE, family: tc.family, proto: RTPROT_STATIC,
				dst: tc.dst, ifIndex: lo.Index,
			})
			if _, _, ok := ParseRouteEvent(msg, testProto); ok {
				t.Error("ParseRouteEvent accepted a route to a specific prefix as a default route")
			}
		})
	}
}

func TestParseRouteEventAcceptsUnspecifiedDestination(t *testing.T) {
	lo := loopback(t)
	msg := buildRoute(routeMsgOpts{
		msgType: RTM_NEWROUTE, family: syscall.AF_INET6, proto: RTPROT_RA,
		dst: net.IPv6unspecified, gwIP: net.ParseIP("fe80::1"), ifIndex: lo.Index,
	})
	if _, _, ok := ParseRouteEvent(msg, testProto); !ok {
		t.Error("ParseRouteEvent rejected a default route carrying an explicit :: destination")
	}
}

func TestParseRouteEventHonoursRTATable(t *testing.T) {
	lo := loopback(t)
	const rtTableCompat = 252

	inMain := buildRoute(routeMsgOpts{
		msgType: RTM_NEWROUTE, family: syscall.AF_INET6, proto: RTPROT_RA,
		table: rtTableCompat, rtaTable: RT_TABLE_MAIN,
		gwIP: net.ParseIP("fe80::1"), ifIndex: lo.Index,
	})
	if _, _, ok := ParseRouteEvent(inMain, testProto); !ok {
		t.Error("ParseRouteEvent rejected a main-table route that reported its ID via RTA_TABLE")
	}

	elsewhere := buildRoute(routeMsgOpts{
		msgType: RTM_NEWROUTE, family: syscall.AF_INET6, proto: RTPROT_RA,
		table: rtTableCompat, rtaTable: 300,
		gwIP: net.ParseIP("fe80::1"), ifIndex: lo.Index,
	})
	if _, _, ok := ParseRouteEvent(elsewhere, testProto); ok {
		t.Error("ParseRouteEvent accepted a route from table 300 as a main-table route")
	}
}

func TestParseRouteEventIgnoresNonUnicast(t *testing.T) {
	lo := loopback(t)
	msg := buildRoute(routeMsgOpts{
		msgType: RTM_NEWROUTE, family: syscall.AF_INET6, rtnType: RTN_UNREACHABLE,
		proto: RTPROT_DHCP, dstLen: 56, dst: net.ParseIP("2001:db8::"), ifIndex: lo.Index,
	})
	if _, _, ok := ParseRouteEvent(msg, testProto); ok {
		t.Error("ParseRouteEvent accepted a PD discard route as a default gateway")
	}
}

func TestParseRouteEventIgnoresMultipath(t *testing.T) {
	msg := buildRoute(routeMsgOpts{
		msgType: RTM_NEWROUTE, family: syscall.AF_INET6, proto: RTPROT_RA,
	})
	msg = append(msg[:syscall.NLMSG_HDRLEN+12], rtAttr(RTA_MULTIPATH, make([]byte, 16))...)
	binary.LittleEndian.PutUint32(msg[0:4], uint32(len(msg)))

	if _, gw, ok := ParseRouteEvent(msg, testProto); ok {
		t.Errorf("ParseRouteEvent accepted a multipath route as gateway %+v", gw)
	}
}

func TestSplitMessagesRecoversEveryRouteEvent(t *testing.T) {
	lo := loopback(t)

	var datagram []byte
	datagram = append(datagram, buildRouteMsg(RTM_NEWROUTE, RTPROT_DHCP, net.IPv4(10, 0, 1, 1), lo.Index, 100)...)
	datagram = append(datagram, buildRoute(routeMsgOpts{
		msgType: RTM_NEWROUTE, family: syscall.AF_INET6, proto: RTPROT_RA,
		gwIP: net.ParseIP("fe80::1"), ifIndex: lo.Index, metric: 1024,
	})...)

	var families []int
	for _, msg := range SplitMessages(datagram) {
		if _, gw, ok := ParseRouteEvent(msg, testProto); ok {
			families = append(families, gw.AddrFamily())
		}
	}

	if len(families) != 2 || families[0] != route.FamilyV4 || families[1] != route.FamilyV6 {
		t.Errorf("recovered families = %v, want one v4 (%d) then one v6 (%d)",
			families, route.FamilyV4, route.FamilyV6)
	}
}
