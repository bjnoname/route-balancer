package reconcile

import (
	"log/slog"
	"net"
	"sort"
	"strconv"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/firewall"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/nptv6"
	"github.com/bjnoname/route-balancer/internal/route"
	"github.com/bjnoname/route-balancer/internal/settings"
)

type verdict int

const (
	Want verdict = iota
	Absent
	Unmanaged
)

func (v verdict) String() string {
	switch v {
	case Want:
		return "want"
	case Absent:
		return "absent"
	default:
		return "unmanaged"
	}
}

type tableKey struct {
	Family  int
	IfIndex int
}

func sortedTableKeys[V any](m map[tableKey]V) []tableKey {
	return command.SortedKeysFunc(m, func(a, b tableKey) int {
		if a.Family != b.Family {
			return a.Family - b.Family
		}
		return a.IfIndex - b.IfIndex
	})
}

type Inventory struct {
	ECMP map[int]ecmpEntry

	Tables map[tableKey]route.TableSpec

	Fwmarks map[int][]route.FwmarkSpec

	MarkRules  []firewall.MarkRule
	MarkRules6 []firewall.MarkRule

	Nft map[string]string

	Nft6 string

	ProxyNeigh map[ndp.ProxyEntry]struct{}

	NptRules map[nptv6.RuleKey]struct{}

	LearnRules map[ndp.LearnRule]struct{}

	Assignments []nptv6.Assignment
	Dropped     map[string][]string
	Guarded     []string
}

type ecmpEntry struct {
	Verdict verdict
	Reason  string
	Spec    route.ECMPSpec
}

const retentionReason = "every gateway in this family is unhealthy"

func Desired(r settings.Resolved, s *State) Inventory {
	inv := Inventory{
		ECMP:       map[int]ecmpEntry{},
		Tables:     map[tableKey]route.TableSpec{},
		Fwmarks:    map[int][]route.FwmarkSpec{},
		Nft:        map[string]string{},
		ProxyNeigh: map[ndp.ProxyEntry]struct{}{},
		Dropped:    map[string][]string{},
	}

	desiredECMP(r.Rt, s.Routes, s.Health, &inv)
	desiredTables(r.Rt, s.Routes, s.Links, &inv)
	desiredFwmarks(r.Rt, r.Fw, s.Links, &inv)

	desiredNptv6(r.Npt, r.Nd, r.Gws, s, &inv)
	desiredIPv6Marks(r.Rt, r.Fw, s.Routes, s.Links, &inv)
	desiredProxy(r.Npt, r.Nd, s.Learn, &inv)

	return inv
}

func desiredECMP(rt route.Options, routes routeState, health linkHealth, inv *Inventory) {
	for _, family := range rt.ManagedFamilies() {
		nexthops := activeNexthops(routes, health, family)
		if len(nexthops) == 0 {
			inv.ECMP[family] = ecmpEntry{Verdict: Unmanaged, Reason: retentionReason}
			continue
		}
		inv.ECMP[family] = ecmpEntry{Verdict: Want, Spec: route.ECMPSpec{
			Family:   family,
			Metric:   rt.ECMPMetric(family),
			Proto:    rt.ProtoString(),
			Nexthops: nexthops,
		}}
	}
}

func activeNexthops(routes routeState, health linkHealth, family int) []route.NexthopSpec {
	var out []route.NexthopSpec
	for _, key := range command.SortedKeys(routes.Gateways) {
		gw := routes.Gateways[key]
		weight := health.effectiveWeight(gw)
		if gw.AddrFamily() != family || weight <= 0 {
			continue
		}
		var via string
		if gw.HasNexthop() {
			via = gw.IP.String()
		}
		out = append(out, route.NexthopSpec{Via: via, Dev: gw.IfName, Weight: weight})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dev != out[j].Dev {
			return out[i].Dev < out[j].Dev
		}
		return out[i].Via < out[j].Via
	})
	return out
}

func desiredTables(rt route.Options, routes routeState, links linkState, inv *Inventory) {
	for _, key := range command.SortedKeys(routes.Gateways) {
		gw := routes.Gateways[key]
		if gw.AddrFamily() != route.FamilyV4 {
			continue
		}
		addr := links.Addrs[gw.IfIndex]
		if addr == nil {
			continue
		}

		table, ok := rt.TableForIface(gw.IfIndex)
		if !ok {
			warnReservedTable(gw.IfName, table)
			continue
		}
		prio, _ := rt.GatewayPriority(gw.IfIndex)

		spec := route.TableSpec{
			Family:  route.FamilyV4,
			IfIndex: gw.IfIndex,
			IfName:  gw.IfName,
			Table:   strconv.Itoa(table),
			Src:     addr.IP.String(),
			Prio:    prio,
		}
		if gw.HasNexthop() {
			spec.Via = gw.IP.String()
		}
		if ones, bits := addr.Mask.Size(); ones != bits {
			spec.Subnet = (&net.IPNet{IP: addr.IP.Mask(addr.Mask), Mask: addr.Mask}).String()
		}
		inv.Tables[tableKey{route.FamilyV4, gw.IfIndex}] = spec
	}
}

func desiredFwmarks(rt route.Options, fw firewall.Options, links linkState, inv *Inventory) {
	for _, rule := range fw.Rules {
		mark := fw.Mark(rule.Gateway)
		if mark == 0 {
			continue
		}
		ifIndex, ok := links.Ifaces[rule.Gateway]
		if !ok {
			continue
		}
		spec, ok := fwmarkSpec(rt, mark, ifIndex)
		if !ok {
			continue
		}
		inv.Fwmarks[route.FamilyV4] = append(inv.Fwmarks[route.FamilyV4], spec)
	}
	sortFwmarks(inv.Fwmarks[route.FamilyV4])

	inv.MarkRules = fw.DesiredMarkRules()

	if fw.Backend == "nftables" {
		inv.Nft[fw.NftablesTable] = fw.NftRuleset()
	}
}

func fwmarkSpec(rt route.Options, mark, ifIndex int) (route.FwmarkSpec, bool) {
	table, ok := rt.TableForIface(ifIndex)
	if !ok {
		return route.FwmarkSpec{}, false
	}
	return route.FwmarkSpec{
		Mark:  mark,
		Table: strconv.Itoa(table),
		Prio:  strconv.Itoa(500 + mark),
	}, true
}

func warnReservedTable(ifName string, table int) {
	slog.Warn("Gateway not managed: its routing table ID is one the kernel reserves",
		"iface", ifName, "table", table,
		"hint", "the table is route_table_offset plus the interface index, and 253, 254 "+
			"and 255 are default, main and local; lower route_table_offset")
}

func sortFwmarks(specs []route.FwmarkSpec) {
	sort.Slice(specs, func(i, j int) bool { return specs[i].Mark < specs[j].Mark })
}

func desiredIPv6Marks(rt route.Options, fw firewall.Options, routes routeState, links linkState, inv *Inventory) {
	if len(fw.Rules6) == 0 {
		return
	}

	steered := make(map[string]bool, len(fw.Rules6))
	for _, rule := range fw.Rules6 {
		steered[rule.Gateway] = true
	}

	for _, key := range command.SortedKeys(routes.Gateways) {
		gw := routes.Gateways[key]
		if gw.AddrFamily() != route.FamilyV6 || !steered[gw.IfName] {
			continue
		}
		table, ok := rt.TableForIface(gw.IfIndex)
		if !ok {
			warnReservedTable(gw.IfName, table)
			continue
		}
		spec := route.TableSpec{
			Family:  route.FamilyV6,
			IfIndex: gw.IfIndex,
			IfName:  gw.IfName,
			Table:   strconv.Itoa(table),
		}
		if gw.HasNexthop() {
			spec.Via = gw.IP.String()
		}
		inv.Tables[tableKey{route.FamilyV6, gw.IfIndex}] = spec
	}

	for _, rule := range fw.Rules6 {
		mark := fw.Mark(rule.Gateway)
		if mark == 0 {
			continue
		}
		ifIndex, ok := links.Ifaces[rule.Gateway]
		if !ok {
			continue
		}
		if _, built := inv.Tables[tableKey{route.FamilyV6, ifIndex}]; !built {
			continue
		}
		spec, ok := fwmarkSpec(rt, mark, ifIndex)
		if !ok {
			continue
		}
		inv.Fwmarks[route.FamilyV6] = append(inv.Fwmarks[route.FamilyV6], spec)
	}
	sortFwmarks(inv.Fwmarks[route.FamilyV6])

	mapped := mappedSubnets(inv.Assignments)
	inv.MarkRules6 = fw.DesiredMarkRules6(mapped)
	inv.Nft6 = fw.NftRuleset6(mapped)
}

func mappedSubnets(assignments []nptv6.Assignment) []firewall.MappedSubnet {
	out := make([]firewall.MappedSubnet, 0, len(assignments))
	for _, a := range assignments {
		out = append(out, firewall.MappedSubnet{Uplink: a.Uplink, Prefix: a.Internal.String()})
	}
	return out
}

func desiredNptv6(npt nptv6.Options, nd ndp.Options, gws map[string]config.Gateway, s *State, inv *Inventory) {
	if !npt.Enabled {
		return
	}

	assignments, dropped := assignmentsFor(npt, gws, s)
	inv.Assignments = assignments
	inv.Dropped = dropped
	inv.Guarded = npt.GuardUplinks(assignments)
	inv.Nft[npt.Table] = npt.Ruleset(assignments)
	inv.NptRules = npt.DesiredRules(assignments)

	if nd.Enabled() {
		inv.Nft[nd.Table] = nd.LearnRuleset()
		inv.LearnRules = nd.DesiredLearnRules()
	}
}

func assignmentsFor(npt nptv6.Options, gws map[string]config.Gateway, s *State) (assigned []nptv6.Assignment, dropped map[string][]string) {
	dropped = map[string][]string{}
	if !npt.Enabled {
		return nil, dropped
	}

	for _, u := range npt.Uplinks {
		lease, ok := s.Prefixes.Leases[u.Name]
		if !ok {
			continue
		}
		if !uplinkActive(gws, s.Routes, s.Health, u.Name) {
			continue
		}

		a, d := npt.Assign(u.Name, lease.Prefix, lease.Length)
		assigned = append(assigned, a...)
		if len(d) > 0 {
			dropped[u.Name] = d
		}
	}
	return assigned, dropped
}

func uplinkActive(gws map[string]config.Gateway, routes routeState, health linkHealth, ifName string) bool {
	if _, gated := gws[ifName]; !gated {
		return true
	}
	if !health.healthy(ifName, route.FamilyV6) {
		return false
	}
	for _, key := range command.SortedKeys(routes.Gateways) {
		if gw := routes.Gateways[key]; gw.IfName == ifName && gw.ConfigWeight > 0 {
			return true
		}
	}
	return false
}

func desiredProxy(npt nptv6.Options, nd ndp.Options, learn learnState, inv *Inventory) {
	if !nd.Enabled() || !npt.Enabled {
		return
	}
	inv.ProxyNeigh = nd.DesiredEntries(inv.Assignments, learn.Hosts)
}
