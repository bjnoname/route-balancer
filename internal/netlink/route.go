package netlink

import (
	"encoding/binary"
	"log/slog"
	"net"
	"syscall"
	"unsafe"

	"github.com/bjnoname/route-balancer/internal/route"
)

const (
	RTMGRP_IPV4_ROUTE = 0x40
	RTMGRP_IPV6_ROUTE = 0x400
)

const (
	RTM_NEWROUTE = 24
	RTM_DELROUTE = 25
)

func RouteGroup(family int) uint32 {
	if family == route.FamilyV6 {
		return RTMGRP_IPV6_ROUTE
	}
	return RTMGRP_IPV4_ROUTE
}

const (
	RTA_DST       = 1
	RTA_OIF       = 4
	RTA_GATEWAY   = 5
	RTA_PRIORITY  = 6
	RTA_MULTIPATH = 9
)

const (
	RT_TABLE_MAIN     = 254
	RTPROT_BOOT       = 3
	RTPROT_STATIC     = 4
	RTPROT_DHCP       = 16
	RT_SCOPE_UNIVERSE = 0
	RT_SCOPE_LINK     = 253
	RTN_UNICAST       = 1
)

type rtMsg struct {
	Family   uint8
	DstLen   uint8
	SrcLen   uint8
	Tos      uint8
	Table    uint8
	Protocol uint8
	Scope    uint8
	Type     uint8
	Flags    uint32
}

func ParseRouteEvent(data []byte, ownProto int) (msgType uint16, gw route.Gateway, ok bool) {

	if len(data) >= syscall.NLMSG_HDRLEN {
		msgType = (*syscall.NlMsghdr)(unsafe.Pointer(&data[0])).Type
	}

	_, payload, attrData, ok := message(data, syscall.SizeofRtMsg, RTM_NEWROUTE, RTM_DELROUTE)
	if !ok {
		return msgType, route.Gateway{}, false
	}
	rtm := (*rtMsg)(unsafe.Pointer(&payload[0]))
	ok = false

	var family, addrLen int
	switch rtm.Family {
	case syscall.AF_INET:
		family, addrLen = route.FamilyV4, net.IPv4len
	case syscall.AF_INET6:
		family, addrLen = route.FamilyV6, net.IPv6len
	default:
		return
	}

	if rtm.Type != RTN_UNICAST ||
		rtm.DstLen != 0 ||
		int(rtm.Protocol) == ownProto {
		return
	}

	var gwIP net.IP
	var ifIndex, metric int
	var multipath, notDefault bool
	table := int(rtm.Table)

	walkAttrs(attrData, func(attrType uint16, payload []byte) bool {
		switch int(attrType) {
		case RTA_DST:

			if len(payload) == addrLen && !net.IP(payload).IsUnspecified() {
				notDefault = true
				return false
			}
		case RTA_GATEWAY:
			if len(payload) == addrLen {
				gwIP = make(net.IP, addrLen)
				copy(gwIP, payload)
			}
		case RTA_OIF:
			if len(payload) == 4 {
				ifIndex = int(binary.LittleEndian.Uint32(payload))
			}
		case RTA_PRIORITY:
			if len(payload) == 4 {
				metric = int(binary.LittleEndian.Uint32(payload))
			}
		case RTA_TABLE:
			if len(payload) == 4 {
				table = int(binary.LittleEndian.Uint32(payload))
			}
		case RTA_MULTIPATH:
			multipath = true
		}
		return true
	})

	if notDefault {
		return
	}

	if table != RT_TABLE_MAIN {
		return
	}

	if ifIndex == 0 {
		if multipath {
			slog.Debug("Ignoring a multipath default route event: it carries no single nexthop",
				"family", route.FamilyName(family))
		}
		return
	}

	iface, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		slog.Warn("unknown interface index", "index", ifIndex)
		return
	}

	gw = route.Gateway{Family: family, IP: gwIP, IfIndex: ifIndex, IfName: iface.Name, Metric: metric}
	ok = true
	return
}

func SplitMessages(data []byte) [][]byte {
	var msgs [][]byte
	for len(data) >= syscall.NLMSG_HDRLEN {
		length := int(binary.LittleEndian.Uint32(data[0:4]))
		if length < syscall.NLMSG_HDRLEN || length > len(data) {
			break
		}
		msgs = append(msgs, data[:length])

		aligned := (length + 3) &^ 3
		if aligned > len(data) {
			break
		}
		data = data[aligned:]
	}
	return msgs
}
