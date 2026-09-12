package netlink

import (
	"bytes"
	"net"
	"syscall"
	"unsafe"

	"github.com/bjnoname/route-balancer/internal/command"
)

const (
	IFA_LOCAL   = 2
	IFLA_IFNAME = 3
)

type Link struct {
	Index int
	Name  string
}

type Addr struct {
	Index int
	IP    net.IP
	Mask  net.IPMask
}

type Links struct {
	Links []Link
	Addrs []Addr
}

func LinksQuery() command.Query {
	return command.Query{
		Tool: command.ToolNetlink,
		Args: []string{"link", "show"},
		Read: func() (any, error) { return dumpLinks() },
	}
}

func AddrsQuery() command.Query {
	return command.Query{
		Tool: command.ToolNetlink,
		Args: []string{"addr", "show"},
		Read: func() (any, error) { return dumpAddrs() },
	}
}

func ReadLinks(a command.Answers) (Links, error) {
	links, err := command.ValueOf[[]Link](a, LinksQuery())
	if err != nil {
		return Links{}, err
	}
	addrs, err := command.ValueOf[[]Addr](a, AddrsQuery())
	if err != nil {
		return Links{}, err
	}
	return Links{Links: links, Addrs: addrs}, nil
}

func (l Links) IndexOf(name string) (int, bool) {
	for _, link := range l.Links {
		if link.Name == name {
			return link.Index, true
		}
	}
	return 0, false
}

func (l Links) PrimaryV4(ifIndex int) *net.IPNet {
	for _, a := range l.Addrs {
		if a.Index != ifIndex {
			continue
		}
		if ip4 := a.IP.To4(); ip4 != nil {
			return &net.IPNet{IP: ip4, Mask: a.Mask}
		}
	}
	return nil
}

func (l Links) LocalV6() map[string]bool {
	out := make(map[string]bool, len(l.Addrs))
	for _, a := range l.Addrs {
		if a.IP.To4() == nil {
			out[a.IP.String()] = true
		}
	}
	return out
}

func dumpLinks() ([]Link, error) {
	data, err := syscall.NetlinkRIB(syscall.RTM_GETLINK, syscall.AF_UNSPEC)
	if err != nil {
		return nil, err
	}

	var out []Link
	for _, msg := range SplitMessages(data) {
		if isDone(msg) {
			break
		}
		_, payload, attrs, ok := message(msg, syscall.SizeofIfInfomsg, syscall.RTM_NEWLINK)
		if !ok {
			continue
		}
		ifi := (*syscall.IfInfomsg)(unsafe.Pointer(&payload[0]))
		if ifi.Index == 0 {
			continue
		}

		var name string
		walkAttrs(attrs, func(attrType uint16, payload []byte) bool {
			if int(attrType) == IFLA_IFNAME && name == "" {
				name = string(bytes.TrimRight(payload, "\x00"))
			}
			return true
		})
		if name == "" {
			continue
		}
		out = append(out, Link{Index: int(ifi.Index), Name: name})
	}
	return out, nil
}

func dumpAddrs() ([]Addr, error) {
	data, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_UNSPEC)
	if err != nil {
		return nil, err
	}

	var out []Addr
	for _, msg := range SplitMessages(data) {
		if isDone(msg) {
			break
		}
		_, payload, attrs, ok := message(msg, syscall.SizeofIfAddrmsg, RTM_NEWADDR)
		if !ok {
			continue
		}
		ifa := (*ifAddrMsg)(unsafe.Pointer(&payload[0]))
		if ifa.Index == 0 {
			continue
		}

		if a, ok := addrOf(ifa, attrs); ok {
			out = append(out, a)
		}
	}
	return out, nil
}

func addrOf(ifa *ifAddrMsg, attrs []byte) (Addr, bool) {
	var local, peer []byte
	walkAttrs(attrs, func(attrType uint16, payload []byte) bool {
		switch int(attrType) {
		case IFA_LOCAL:
			local = payload
		case IFA_ADDRESS:
			peer = payload
		}
		return true
	})

	payload := local
	if payload == nil {
		payload = peer
	}

	switch ifa.Family {
	case syscall.AF_INET:
		if len(payload) != net.IPv4len {
			return Addr{}, false
		}
		return Addr{
			Index: int(ifa.Index),
			IP:    net.IPv4(payload[0], payload[1], payload[2], payload[3]),
			Mask:  net.CIDRMask(int(ifa.PrefixLen), 8*net.IPv4len),
		}, true

	case syscall.AF_INET6:
		if len(payload) != net.IPv6len {
			return Addr{}, false
		}
		ip := make(net.IP, net.IPv6len)
		copy(ip, payload)
		return Addr{
			Index: int(ifa.Index),
			IP:    ip,
			Mask:  net.CIDRMask(int(ifa.PrefixLen), 8*net.IPv6len),
		}, true
	}
	return Addr{}, false
}
