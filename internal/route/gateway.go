package route

import (
	"fmt"
	"net"
	"syscall"
)

const (
	FamilyV4 = syscall.AF_INET
	FamilyV6 = syscall.AF_INET6
)

func FamilyName(family int) string {
	if family == FamilyV6 {
		return "v6"
	}
	return "v4"
}

func FamilyFlag(family int) string {
	if family == FamilyV6 {
		return "-6"
	}
	return "-4"
}

type Gateway struct {
	Family       int
	IP           net.IP
	IfIndex      int
	IfName       string
	Metric       int
	ConfigWeight int
}

func (g Gateway) AddrFamily() int {
	if g.Family == FamilyV6 {
		return FamilyV6
	}
	return FamilyV4
}

func (g Gateway) HasNexthop() bool {
	return len(g.IP) > 0
}

func (g Gateway) String() string {
	if !g.HasNexthop() {
		return fmt.Sprintf("%s dev %s metric %d", FamilyName(g.AddrFamily()), g.IfName, g.Metric)
	}
	return fmt.Sprintf("%s %s dev %s metric %d", FamilyName(g.AddrFamily()), g.IP, g.IfName, g.Metric)
}

func Key(family int, ip net.IP, ifindex int) string {
	if len(ip) == 0 {
		return fmt.Sprintf("%s:dev@%d", FamilyName(family), ifindex)
	}
	return fmt.Sprintf("%s:%s@%d", FamilyName(family), ip.String(), ifindex)
}
