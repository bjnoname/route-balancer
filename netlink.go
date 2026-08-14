package main

import (
	"encoding/binary"
	"log/slog"
	"net"
	"syscall"
	"unsafe"
)

// Netlink multicast groups
const (
	RTMGRP_IPV4_ROUTE = 0x40
	RTMGRP_IPV6_ROUTE = 0x400
)

// RTM message types
const (
	RTM_NEWROUTE = 24
	RTM_DELROUTE = 25
)

// RTA attribute types
const (
	RTA_DST      = 1
	RTA_OIF      = 4 // output interface index
	RTA_GATEWAY  = 5
	RTA_PRIORITY = 6 // route metric (kernel priority)
)

// Route message field values
const (
	RT_TABLE_MAIN = 254
	RTPROT_BOOT   = 3 // used by pppd for the route it installs on link-up
	RTPROT_STATIC = 4
	RTPROT_DHCP   = 16
	RT_SCOPE_LINK = 253
	RTN_UNICAST   = 1
)

// rtMsg is the fixed-size header that follows a netlink header for
// RTM_NEWROUTE / RTM_DELROUTE messages.
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

// parseRouteEvent decodes a raw netlink message and returns the message type
// and gateway details when the message describes an IPv4 default-route event
// on the main routing table.
func parseRouteEvent(data []byte) (msgType uint16, gw Gateway, ok bool) {
	if len(data) < syscall.NLMSG_HDRLEN {
		return
	}
	nlh := (*syscall.NlMsghdr)(unsafe.Pointer(&data[0]))
	msgType = nlh.Type

	if msgType != RTM_NEWROUTE && msgType != RTM_DELROUTE {
		return
	}

	hdrEnd := syscall.NLMSG_HDRLEN
	if len(data) < hdrEnd+12 {
		return
	}
	rtm := (*rtMsg)(unsafe.Pointer(&data[hdrEnd]))

	// Only care about main table, unicast, IPv4 default route (dstLen == 0).
	// Skip routes installed by this daemon (identified by our proto number)
	// to avoid reacting to our own ECMP route updates.
	if rtm.Family != syscall.AF_INET ||
		rtm.Type != RTN_UNICAST ||
		rtm.Table != RT_TABLE_MAIN ||
		rtm.DstLen != 0 ||
		int(rtm.Protocol) == cfg.routeProtoInt() {
		return
	}

	attrData := data[hdrEnd+12:]
	var gwIP net.IP
	var ifIndex, metric int

	for len(attrData) >= 4 {
		attrLen := int(binary.LittleEndian.Uint16(attrData[0:2]))
		attrType := binary.LittleEndian.Uint16(attrData[2:4])

		if attrLen < 4 || attrLen > len(attrData) {
			break
		}
		payload := attrData[4:attrLen]

		switch int(attrType) {
		case RTA_DST:
			// DstLen == 0 means default route; RTA_DST may be absent or 0.0.0.0.
			if len(payload) == 4 {
				ip := net.IP(payload).To4()
				if ip != nil && !ip.Equal(net.IPv4zero) {
					return // not a default route
				}
			}
		case RTA_GATEWAY:
			if len(payload) == 4 {
				gwIP = make(net.IP, 4)
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
		}

		aligned := (attrLen + 3) &^ 3
		if aligned > len(attrData) {
			break
		}
		attrData = attrData[aligned:]
	}

	// A missing RTA_GATEWAY is a legitimate state: point-to-point links (PPP,
	// tunnels) install "default dev <if> scope link" because there is no
	// address to route via. Only the output interface is mandatory.
	if ifIndex == 0 {
		return
	}

	iface, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		slog.Warn("unknown interface index", "index", ifIndex)
		return
	}

	gw = Gateway{IP: gwIP, IfIndex: ifIndex, IfName: iface.Name, Metric: metric}
	ok = true
	return
}
