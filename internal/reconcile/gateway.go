package reconcile

import (
	"github.com/bjnoname/route-balancer/internal/route"
)

func (h linkHealth) effectiveWeight(gw route.Gateway) int {
	if !h.healthy(gw.IfName, gw.AddrFamily()) {
		return 0
	}
	return gw.ConfigWeight
}

func (r routeState) gatewaysSnapshot() []route.Gateway {
	out := make([]route.Gateway, 0, len(r.Gateways))
	for _, gw := range r.Gateways {
		out = append(out, gw)
	}
	return out
}
