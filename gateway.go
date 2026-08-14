package main

import (
	"fmt"
	"net"
	"sync"
)

// Gateway represents a single default route nexthop.
type Gateway struct {
	IP           net.IP // nil on point-to-point links, which have no nexthop address
	IfIndex      int
	IfName       string
	Metric       int
	ConfigWeight int // weight from config (never changes)
	EffectWeight int // 0 when unhealthy, ConfigWeight otherwise
	Healthy      bool
	Src          net.IP // local address used for the split-access rule, cached at setup
}

// hasNexthop reports whether the gateway has a nexthop address. Point-to-point
// links (PPP, tunnels) have none — their default route is "dev <if>" only.
func (g Gateway) hasNexthop() bool {
	return len(g.IP) > 0
}

// srcAddr returns the local address to use as the split-access rule selector,
// preferring the value cached at setup time over a fresh lookup so that it
// survives the interface losing its address.
func (g Gateway) srcAddr() net.IP {
	if len(g.Src) > 0 {
		return g.Src
	}
	if ifAddr := primaryAddr(g.IfIndex); ifAddr != nil {
		return ifAddr.IP
	}
	return nil
}

func (g Gateway) String() string {
	if !g.hasNexthop() {
		return fmt.Sprintf("dev %s metric %d", g.IfName, g.Metric)
	}
	return fmt.Sprintf("%s dev %s metric %d", g.IP, g.IfName, g.Metric)
}

// gateways holds the current set of known default-route nexthops,
// keyed by "IP@ifindex" ("dev@ifindex" for point-to-point links).
var (
	gatewaysMu sync.Mutex
	gateways   = map[string]Gateway{}
)

// gwKey derives the map key for a gateway. Point-to-point links have no
// nexthop address, so they are keyed on the interface index alone rather than
// on ip.String(), which would render every one of them as "<nil>".
func gwKey(ip net.IP, ifindex int) string {
	if len(ip) == 0 {
		return fmt.Sprintf("dev@%d", ifindex)
	}
	return fmt.Sprintf("%s@%d", ip.String(), ifindex)
}

// rememberGatewaySrc caches the source address used for a gateway's
// split-access rule on the stored Gateway, so teardown can remove the rule
// even after the address has been withdrawn (the usual state when a DHCP
// lease is lost, which is precisely when teardown runs).
func rememberGatewaySrc(key string, src net.IP) {
	gatewaysMu.Lock()
	defer gatewaysMu.Unlock()
	if gw, ok := gateways[key]; ok {
		gw.Src = src
		gateways[key] = gw
	}
}

// activeGateways returns a snapshot of all gateways with EffectWeight > 0.
func activeGateways() []Gateway {
	gatewaysMu.Lock()
	defer gatewaysMu.Unlock()
	active := make([]Gateway, 0, len(gateways))
	for _, gw := range gateways {
		if gw.EffectWeight > 0 {
			active = append(active, gw)
		}
	}
	return active
}
