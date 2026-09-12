package netlink

import (
	"encoding/binary"
	"log/slog"
	"net"
	"slices"
	"syscall"
	"unsafe"
)

const (
	RTMGRP_IPV6_IFADDR = 0x100
	RTMGRP_IPV6_PREFIX = 0x20000
)

const (
	RTM_NEWADDR   = 20
	RTM_DELADDR   = 21
	RTM_NEWPREFIX = 52
)

const (
	RTN_BLACKHOLE   = 6
	RTN_UNREACHABLE = 7
	RTN_PROHIBIT    = 8
)

const RTA_TABLE = 15

const (
	PREFIX_ADDRESS   = 1
	PREFIX_CACHEINFO = 2
)

const IFA_ADDRESS = 1

type ifAddrMsg struct {
	Family    uint8
	PrefixLen uint8
	Flags     uint8
	Scope     uint8
	Index     uint32
}

const sizeofPrefixMsg = 12

type prefixMsg struct {
	Family  uint8
	Pad1    uint8
	Pad2    uint16
	IfIndex int32
	Type    uint8
	Len     uint8
	Flags   uint8
	Pad3    uint8
}

const (
	ObsRoute = "route"
	ObsRA    = "ra"
)

type Observation struct {
	Kind     string
	Prefix   net.IP
	Length   int
	IfName   string
	Withdraw bool
}

func walkAttrs(data []byte, fn func(attrType uint16, payload []byte) bool) {
	for len(data) >= 4 {
		attrLen := int(binary.LittleEndian.Uint16(data[0:2]))
		attrType := binary.LittleEndian.Uint16(data[2:4])

		if attrLen < 4 || attrLen > len(data) {
			return
		}
		if !fn(attrType, data[4:attrLen]) {
			return
		}

		aligned := (attrLen + 3) &^ 3
		if aligned > len(data) {
			return
		}
		data = data[aligned:]
	}
}

func message(data []byte, payloadLen int, want ...uint16) (
	nlh *syscall.NlMsghdr, payload, attrs []byte, ok bool) {

	if len(data) < syscall.NLMSG_HDRLEN {
		return nil, nil, nil, false
	}
	nlh = (*syscall.NlMsghdr)(unsafe.Pointer(&data[0]))
	if !slices.Contains(want, nlh.Type) {
		return nil, nil, nil, false
	}

	end := syscall.NLMSG_HDRLEN + payloadLen
	if len(data) < end {
		return nil, nil, nil, false
	}
	return nlh, data[syscall.NLMSG_HDRLEN:end], data[end:], true
}

func isDone(data []byte) bool {
	return len(data) >= syscall.NLMSG_HDRLEN &&
		(*syscall.NlMsghdr)(unsafe.Pointer(&data[0])).Type == syscall.NLMSG_DONE
}

func ParseDiscardRoute(data []byte) (obs Observation, ok bool) {
	nlh, payload, attrs, ok := message(data, syscall.SizeofRtMsg, RTM_NEWROUTE, RTM_DELROUTE)
	if !ok {
		return Observation{}, false
	}
	rtm := (*rtMsg)(unsafe.Pointer(&payload[0]))
	ok = false

	if rtm.Family != syscall.AF_INET6 {
		return
	}
	switch rtm.Type {
	case RTN_UNREACHABLE, RTN_BLACKHOLE, RTN_PROHIBIT:
	default:
		return
	}
	if rtm.DstLen == 0 || rtm.DstLen > 64 {
		return
	}

	table := int(rtm.Table)
	var prefix net.IP

	walkAttrs(attrs, func(attrType uint16, payload []byte) bool {
		switch int(attrType) {
		case RTA_DST:
			if len(payload) == net.IPv6len {
				prefix = make(net.IP, net.IPv6len)
				copy(prefix, payload)
			}
		case RTA_TABLE:
			if len(payload) == 4 {
				table = int(binary.LittleEndian.Uint32(payload))
			}
		}
		return true
	})

	if prefix == nil || table != RT_TABLE_MAIN {
		return
	}

	obs = Observation{
		Kind:     ObsRoute,
		Prefix:   prefix,
		Length:   int(rtm.DstLen),
		Withdraw: nlh.Type == RTM_DELROUTE,
	}
	ok = true
	return
}

func ParseRAPrefix(data []byte) (obs Observation, ok bool) {
	_, payload, attrs, ok := message(data, sizeofPrefixMsg, RTM_NEWPREFIX)
	if !ok {
		return Observation{}, false
	}
	pm := (*prefixMsg)(unsafe.Pointer(&payload[0]))
	ok = false

	if pm.Family != syscall.AF_INET6 || pm.Len == 0 || pm.Len > 64 {
		return
	}

	var prefix net.IP
	validLifetime := ^uint32(0)

	walkAttrs(attrs, func(attrType uint16, payload []byte) bool {
		switch int(attrType) {
		case PREFIX_ADDRESS:
			if len(payload) == net.IPv6len {
				prefix = make(net.IP, net.IPv6len)
				copy(prefix, payload)
			}
		case PREFIX_CACHEINFO:
			if len(payload) >= 8 {
				validLifetime = binary.LittleEndian.Uint32(payload[4:8])
			}
		}
		return true
	})

	if prefix == nil || pm.IfIndex <= 0 {
		return
	}

	iface, err := net.InterfaceByIndex(int(pm.IfIndex))
	if err != nil {
		slog.Warn("unknown interface index on an RA prefix", "index", pm.IfIndex)
		return
	}

	obs = Observation{
		Kind:     ObsRA,
		Prefix:   prefix,
		Length:   int(pm.Len),
		IfName:   iface.Name,
		Withdraw: validLifetime == 0,
	}
	ok = true
	return
}

type AddrChange struct {
	IfIndex  int
	Addr     net.IP
	Withdraw bool
}

func ParseAddrEvent(data []byte) (ch AddrChange, ok bool) {
	nlh, payload, attrs, ok := message(data, syscall.SizeofIfAddrmsg, RTM_NEWADDR, RTM_DELADDR)
	if !ok {
		return AddrChange{}, false
	}
	ifa := (*ifAddrMsg)(unsafe.Pointer(&payload[0]))
	ok = false

	if ifa.Family != syscall.AF_INET6 || ifa.Scope != RT_SCOPE_UNIVERSE || ifa.Index == 0 {
		return
	}

	var addr net.IP
	walkAttrs(attrs, func(attrType uint16, payload []byte) bool {
		if int(attrType) == IFA_ADDRESS && len(payload) == net.IPv6len {
			addr = make(net.IP, net.IPv6len)
			copy(addr, payload)
		}
		return true
	})

	ch = AddrChange{
		IfIndex:  int(ifa.Index),
		Addr:     addr,
		Withdraw: nlh.Type == RTM_DELADDR,
	}
	ok = true
	return
}
