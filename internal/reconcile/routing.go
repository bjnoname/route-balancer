package reconcile

import (
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/route"
)

func (d *reconciler) teardownGatewayActions(gw route.Gateway) []command.Action {
	if gw.AddrFamily() != route.FamilyV4 {
		return nil
	}
	return d.r.Rt.TeardownTableActions(route.FamilyV4, gw.IfName, gw.IfIndex, "")
}

func (d *reconciler) cleanupRouteActions() []command.Action {
	opts := d.r.Rt

	var acts []command.Action
	for _, family := range opts.ManagedFamilies() {
		acts = append(acts, opts.DeleteECMPAction(family))
	}
	for _, gw := range d.state.Routes.gatewaysSnapshot() {
		acts = append(acts, d.teardownGatewayActions(gw)...)
	}
	return acts
}

func (d *reconciler) ipv6RouteSourceNames() []string {
	if !d.cfg.IPv6ECMP || d.state.Routes.V6SourcesChecked {
		return nil
	}

	var names []string
	for _, name := range command.SortedKeys(d.cfg.Gateways) {
		if !d.cfg.ShouldInclude(name) {
			continue
		}
		if _, present := d.state.Links.Ifaces[name]; !present {
			continue
		}
		names = append(names, name)
	}
	return names
}

func (d *reconciler) checkIPv6RouteSources(a command.Answers) {
	names := d.ipv6RouteSourceNames()
	if len(names) == 0 {
		return
	}

	hasV6Route := map[string]bool{}
	for _, gw := range d.state.Routes.gatewaysSnapshot() {
		if gw.AddrFamily() == route.FamilyV6 {
			hasV6Route[gw.IfName] = true
		}
	}

	route.CheckIPv6RouteSources(a, names, hasV6Route)
}
