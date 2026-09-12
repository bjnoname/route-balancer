package reconcile

import (
	"log/slog"
	"net"
	"strconv"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/netlink"
	"github.com/bjnoname/route-balancer/internal/prefix"
	"github.com/bjnoname/route-balancer/internal/route"
)

func (d *reconciler) linkPlan() []command.Query {
	qs := []command.Query{netlink.LinksQuery(), netlink.AddrsQuery()}
	if d.r.Nd.Enabled() {
		for _, uplink := range d.r.Nd.UplinkNames() {
			qs = append(qs, ndp.ProxyNDPQuery(uplink).Query)
		}
	}
	return qs
}

func (d *reconciler) absorbLinks(a command.Answers) {
	links, err := netlink.ReadLinks(a)
	if err != nil {
		d.state.Links.Stale = true
		slog.Warn("Could not read the interface table — keeping the links already known "+
			"and tearing nothing down this pass",
			"error", err)
		return
	}
	d.state.Links.Stale = false

	ifaces := map[string]int{}
	addrs := map[int]*net.IPNet{}

	for _, name := range command.SortedKeys(d.cfg.Gateways) {
		ifIndex, ok := links.IndexOf(name)
		if !ok {
			continue
		}
		ifaces[name] = ifIndex
		if addr := links.PrimaryV4(ifIndex); addr != nil {
			addrs[ifIndex] = addr
		}
	}

	d.state.Links.Ifaces = ifaces
	d.state.Links.Addrs = addrs

	d.state.Links.LocalV6 = nil
	d.state.Links.ProxyNDP = nil
	if d.r.Nd.Enabled() {
		d.state.Links.LocalV6 = links.LocalV6()

		knobs := map[string]bool{}
		for _, uplink := range d.r.Nd.UplinkNames() {
			knobs[uplink] = ndp.ReadProxyNDP(a, uplink)
		}
		d.state.Links.ProxyNDP = knobs
	}
}

func (d *reconciler) absorbGateways(a command.Answers) {
	for _, family := range d.r.Rt.ObservedFamilies() {
		d.absorbGatewayFamily(a, family)
	}

	if !d.state.Routes.V6SourcesChecked {
		d.checkIPv6RouteSources(a)
		d.state.Routes.V6SourcesChecked = true
	}
}

func (d *reconciler) absorbGatewayFamily(a command.Answers, family int) {
	routes, err := d.r.Rt.ForeignDefaultRoutes(a, family)
	if err != nil {
		slog.Warn("Could not read default routes — leaving the gateway set alone",
			"family", route.FamilyName(family), "error", err)
		return
	}

	for key, gw := range d.state.Routes.Gateways {
		if gw.AddrFamily() == family {
			delete(d.state.Routes.Gateways, key)
		}
	}

	for _, r := range routes {
		if !d.cfg.ShouldInclude(r.IfName) {
			slog.Debug("Ignoring default route on an unconfigured interface", "iface", r.IfName)
			continue
		}
		ifIndex, ok := d.state.Links.Ifaces[r.IfName]
		if !ok {
			continue
		}
		key := route.Key(family, r.GwIP, ifIndex)
		d.state.Routes.Gateways[key] = route.Gateway{
			Family:       family,
			IP:           r.GwIP,
			IfIndex:      ifIndex,
			IfName:       r.IfName,
			Metric:       r.Metric,
			ConfigWeight: d.cfg.Weight(r.IfName),
		}
	}
}

func (d *reconciler) absorbLeases(a command.Answers) {
	if !d.r.Npt.Enabled {
		return
	}

	observedLeases := d.sources.Observed(a, d.state.Prefixes.Withdrawn)

	var gone []prefix.Lease
	for uplink, held := range d.state.Prefixes.Leases {
		if _, still := observedLeases[uplink]; !still {
			gone = append(gone, held)
		}
	}
	for _, held := range gone {
		d.handleLeaseEvent(prefix.Change{Lease: held, Gone: true})
	}

	for _, uplink := range d.sources.Uplinks() {
		if l, ok := observedLeases[uplink]; ok {
			d.handleLeaseEvent(prefix.Change{Lease: l})
		}
	}
}

func (d *reconciler) absorbInstalled(a command.Answers) {
	rt, fw := d.r.Rt, d.r.Fw

	inst := installedState{
		Rules:     map[int]route.Rules{},
		ECMP:      map[int]route.InstalledECMP{},
		Tables:    map[tableKey]route.TableContents{},
		Abandoned: map[int]bool{},

		Nft:      fw.ReadInstalledNft(a),
		Nft6:     fw.ReadInstalledNft6(a),
		Iptables: fw.ReadInstalledIptables(fw.DesiredMarkRules(), a),
	}

	for _, family := range rt.RuleFamilies() {
		inst.Rules[family] = route.ReadRules(family, a)
	}
	for _, family := range rt.ManagedFamilies() {
		inst.ECMP[family] = rt.ReadInstalledECMP(family, a)
	}
	for _, family := range rt.UnmanagedFamilies() {
		inst.Abandoned[family] = rt.ReadAbandonedECMP(family, a)
	}
	for _, ifIndex := range command.SortedKeys(d.state.Links.Addrs) {
		id, ok := rt.TableForIface(ifIndex)
		if !ok {
			continue
		}
		inst.Tables[tableKey{route.FamilyV4, ifIndex}] =
			route.ReadTableContents(route.FamilyV4, strconv.Itoa(id), a)
	}
	for _, ifIndex := range d.markedIfIndexes() {
		id, ok := rt.TableForIface(ifIndex)
		if !ok {
			continue
		}
		inst.Tables[tableKey{route.FamilyV6, ifIndex}] =
			route.ReadTableContents(route.FamilyV6, strconv.Itoa(id), a)
	}

	if d.r.Npt.Enabled {
		inst.Npt = d.r.Npt.ReadInstalledTable(a)
		inst.Learn = d.r.Nd.ReadInstalledLearn(a)
	}

	if d.r.Nd.Enabled() {
		inst.Proxy = d.r.Nd.ReadInstalledProxy(a)
	}

	d.state.Installed = inst
}

func (d *reconciler) absorbLearnedHosts(a command.Answers) {
	if !d.r.Nd.Enabled() {
		return
	}
	hosts, ok := d.r.Nd.ReadLearnedHosts(a)
	if !ok {
		slog.Debug("NDP proxy: learning set unreadable — keeping the hosts already known")
		return
	}
	d.state.Learn.Hosts = hosts
	d.reportSetFull(len(hosts) >= d.r.Nd.MaxHosts, d.r.Nd.MaxHosts)
}
