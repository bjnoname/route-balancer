package nptv6

import (
	"encoding/binary"
	"math"
	"net"
)

type Assignment struct {
	Uplink   string
	Subnet   string
	Internal *net.IPNet
	External *net.IPNet
}

func slotsFor(delegationLen int) int {
	if delegationLen <= 0 || delegationLen > 64 {
		return 0
	}
	shift := 64 - delegationLen
	if shift >= 31 {
		return math.MaxInt32
	}
	return 1 << uint(shift)
}

func subnetAt(prefix net.IP, delegationLen, index int) *net.IPNet {
	base := prefix.To16()
	if base == nil {
		return nil
	}
	out := make(net.IP, net.IPv6len)
	copy(out, base.Mask(net.CIDRMask(delegationLen, 128)))

	hi := binary.BigEndian.Uint64(out[:8])
	hi |= uint64(index)
	binary.BigEndian.PutUint64(out[:8], hi)

	return &net.IPNet{IP: out, Mask: net.CIDRMask(64, 128)}
}

func (o Options) Assign(uplink string, prefix net.IP, length int) (assigned []Assignment, dropped []string) {
	slots := slotsFor(length)
	u, ok := o.uplink(uplink)
	if !ok {
		return nil, nil
	}

	for _, name := range u.SubnetPriority {
		internal, ok := o.Subnets[name]
		if !ok {
			continue
		}
		if len(assigned) >= slots {
			dropped = append(dropped, name)
			continue
		}
		assigned = append(assigned, Assignment{
			Uplink:   uplink,
			Subnet:   name,
			Internal: internal,
			External: subnetAt(prefix, length, len(assigned)),
		})
	}
	return assigned, dropped
}
