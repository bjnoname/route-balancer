package main

import (
	"fmt"
	"net"
	"sync"
)

// Gateway represents a single default route nexthop.
type Gateway struct {
	IP           net.IP
	IfIndex      int
	IfName       string
	Metric       int
	ConfigWeight int // weight from config (never changes)
	EffectWeight int // 0 when unhealthy, ConfigWeight otherwise
	Healthy      bool
}

func (g Gateway) String() string {
	return fmt.Sprintf("%s dev %s metric %d", g.IP, g.IfName, g.Metric)
}

// gateways holds the current set of known default-route nexthops,
// keyed by "IP@ifindex".
var (
	gatewaysMu sync.Mutex
	gateways   = map[string]Gateway{}
)

func gwKey(ip net.IP, ifindex int) string {
	return fmt.Sprintf("%s@%d", ip.String(), ifindex)
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
