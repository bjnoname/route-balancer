package reconcile

import (
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/firewall"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/nptv6"
	"github.com/bjnoname/route-balancer/internal/route"
)

func ecmpResourceID(family int) string  { return "ecmp/" + route.FamilyName(family) }
func nftResourceID(table string) string { return "nft/" + table }
func proxyKnobResourceID(uplink string) string {
	return "proxy-ndp/" + uplink
}

func tableResourceID(family int, ifName string) string {
	if family == route.FamilyV6 {
		return "table/v6/" + ifName
	}
	return "table/" + ifName
}

func fwmarkResourceID(family int) string {
	if family == route.FamilyV6 {
		return fwmarkResourceIDv4 + "/v6"
	}
	return fwmarkResourceIDv4
}

const proxyEntriesResourceID = "proxy-entries"

const (
	fwmarkResourceIDv4 = "fwmark-rules"
	mangleResourceID   = "mangle-marks"
	mangle6ResourceID  = "mangle-marks/v6"
)

type resource struct {
	ID      string
	Verdict verdict
	Reason  string

	Queries []command.Query

	InSync func() bool

	Install func() []command.Action

	Remove func() []command.Action

	Unmanaged func()
}

func queries(resources []resource) []command.Query {
	var out []command.Query
	for _, r := range resources {
		out = append(out, r.Queries...)
	}
	return out
}

func (d *reconciler) resources(inv Inventory, now time.Time) []resource {
	inst := d.state.Installed

	var all []resource
	all = append(all, d.staleTableResources(inv)...)
	all = append(all, d.tableResources(inv, inst)...)
	all = append(all, mangleResource(d.r.Fw, inv, inst)...)
	all = append(all, mangle6Resource(d.r.Fw, inv, inst)...)
	all = append(all, fwmarkResources(inv, inst)...)
	all = append(all, ecmpResources(d.r.Rt, inv, inst)...)
	all = append(all, abandonedECMPResources(d.r.Rt, inst)...)
	all = append(all, nftResources(d.r.Npt, d.r.Nd, inv, inst)...)
	all = append(all, d.proxyKnobResources()...)
	all = append(all, d.proxyEntryResource(inv, now)...)
	return all
}

func (d *reconciler) proxyEntryResource(inv Inventory, now time.Time) []resource {
	if !d.r.Nd.Enabled() {
		return nil
	}
	want := d.wantedProxyEntries(inv, now)
	return []resource{{
		ID:      proxyEntriesResourceID,
		Verdict: Want,
		InSync:  func() bool { return d.proxyEntriesInSync(want) },
		Install: func() []command.Action { return d.proxyEntryActions(want, now) },
	}}
}

func (d *reconciler) proxyKnobResources() []resource {
	if !d.r.Nd.Enabled() {
		return nil
	}

	var out []resource
	for _, uplink := range d.r.Nd.UplinkNames() {
		on := d.state.Links.ProxyNDP[uplink]
		out = append(out, resource{
			ID:      proxyKnobResourceID(uplink),
			Verdict: Want,
			InSync:  func() bool { return on },
			Install: func() []command.Action {
				return []command.Action{ndp.EnableProxyNDPAction(uplink)}
			},
		})
	}
	return out
}

func (d *reconciler) queryPlan() []command.Query {
	var qs []command.Query

	for _, family := range d.r.Rt.ObservedFamilies() {
		qs = append(qs, d.r.Rt.GatewayQueries(family)...)
	}
	if d.r.Npt.Enabled {
		qs = append(qs, d.sources.Queries()...)
	}
	if d.r.Nd.Enabled() {
		qs = append(qs, d.r.Nd.LearnedHostsQuery().Query, d.r.Nd.EntriesQuery().Query)
	}

	for _, family := range d.r.Rt.RuleFamilies() {
		qs = append(qs, route.RulesQuery(family).Query)
	}
	qs = append(qs, route.IPv6RouteSourceQueries(d.ipv6RouteSourceNames())...)

	for _, ifIndex := range command.SortedKeys(d.state.Links.Addrs) {
		if table, ok := d.r.Rt.TableForIface(ifIndex); ok {
			qs = append(qs, route.TableContentsQuery(route.FamilyV4, table))
		}
	}
	for _, ifIndex := range d.markedIfIndexes() {
		if table, ok := d.r.Rt.TableForIface(ifIndex); ok {
			qs = append(qs, route.TableContentsQuery(route.FamilyV6, table))
		}
	}
	for _, family := range d.r.Rt.ManagedFamilies() {
		qs = append(qs, d.r.Rt.ECMPQueries(family)...)
	}
	for _, family := range d.r.Rt.UnmanagedFamilies() {
		qs = append(qs, d.r.Rt.AbandonedECMPQuery(family).Query)
	}
	qs = append(qs, d.r.Fw.NftQueries()...)
	qs = append(qs, d.r.Fw.Nft6Queries()...)
	qs = append(qs, d.r.Fw.IptablesQueries(d.r.Fw.DesiredMarkRules())...)
	if d.r.Npt.Enabled {
		qs = append(qs, d.r.Npt.Queries()...)
		qs = append(qs, d.r.Nd.LearnQueries()...)
	}
	return qs
}

func plan(resources []resource) []command.Action {
	var out []command.Action

	for _, r := range resources {
		switch r.Verdict {
		case Unmanaged:
			slog.Debug("Leaving an unmanaged resource alone", "resource", r.ID, "reason", r.Reason)
			if r.Unmanaged != nil {
				r.Unmanaged()
			}

		case Absent:
			if r.Remove == nil {
				continue
			}
			acts := r.Remove()
			if len(acts) == 0 {
				continue
			}
			slog.Info("Removing", "resource", r.ID)
			out = append(out, acts...)

		case Want:
			if r.InSync != nil && r.InSync() {
				continue
			}
			slog.Info("Installing", "resource", r.ID)
			out = append(out, r.Install()...)
		}
	}
	return out
}

func ecmpResources(rt route.Options, inv Inventory, inst installedState) []resource {
	var out []resource
	for _, family := range rt.ManagedFamilies() {
		entry := inv.ECMP[family]
		out = append(out, resource{
			ID:        ecmpResourceID(family),
			Verdict:   entry.Verdict,
			Reason:    entry.Reason,
			Queries:   rt.ECMPQueries(family),
			InSync:    func() bool { return !route.ECMPNeedsRestore(entry.Spec, inst.ECMP[family]) },
			Install:   func() []command.Action { return rt.ECMPActions(entry.Spec, inst.ECMP[family]) },
			Unmanaged: func() { rt.ReportECMPRetained(family, entry.Reason, inst.ECMP[family]) },
		})
	}
	return out
}

func abandonedECMPResources(rt route.Options, inst installedState) []resource {
	var out []resource
	for _, family := range rt.UnmanagedFamilies() {
		if !inst.Abandoned[family] {
			continue
		}
		out = append(out, resource{
			ID:      ecmpResourceID(family),
			Verdict: Absent,
			Queries: []command.Query{rt.AbandonedECMPQuery(family).Query},
			Remove:  func() []command.Action { return []command.Action{rt.DeleteECMPAction(family)} },
		})
	}
	return out
}

func (d *reconciler) markedIfIndexes() []int {
	var out []int
	for _, rule := range d.r.Fw.Rules6 {
		if ifIndex, ok := d.state.Links.Ifaces[rule.Gateway]; ok {
			out = append(out, ifIndex)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (d *reconciler) tableResources(inv Inventory, inst installedState) []resource {
	var out []resource
	for _, key := range sortedTableKeys(inv.Tables) {
		spec := inv.Tables[key]
		out = append(out, resource{
			ID:      tableResourceID(key.Family, spec.IfName),
			Verdict: Want,
			Queries: route.TableQueries(spec),
			InSync: func() bool {
				return !route.TableNeedsRestore(spec, inst.Rules[key.Family], inst.Tables[key])
			},
			Install: func() []command.Action { return route.TableActions(spec) },
		})
	}
	return out
}

const unreadableLinksReason = "the interface table could not be read this pass"

func (d *reconciler) staleTableResources(inv Inventory) []resource {
	return append(d.staleV4TableResources(inv), d.staleV6TableResources(inv)...)
}

func (d *reconciler) staleV4TableResources(inv Inventory) []resource {
	installed := d.r.Rt.InstalledTableIfIndexes(d.state.Installed.Rules[route.FamilyV4])
	return d.staleTablesFor(inv, route.FamilyV4, installed, nil)
}

func (d *reconciler) staleV6TableResources(inv Inventory) []resource {
	fwmarks := d.r.Rt.InstalledFwmarks(d.state.Installed.Rules[route.FamilyV6])

	installed := make([]int, 0, len(fwmarks))
	prio := make(map[int]string, len(fwmarks))
	for _, fwm := range fwmarks {
		installed = append(installed, fwm.IfIndex)
		prio[fwm.IfIndex] = fwm.Prio
	}

	return d.staleTablesFor(inv, route.FamilyV6, installed, func(ifIndex int) []command.Action {
		return []command.Action{route.DeleteRuleByPriorityAction(route.FamilyV6, prio[ifIndex])}
	})
}

func (d *reconciler) staleTablesFor(
	inv Inventory, family int, installed []int, first func(ifIndex int) []command.Action,
) []resource {
	var out []resource
	for _, ifIndex := range installed {
		if _, wanted := inv.Tables[tableKey{family, ifIndex}]; wanted {
			continue
		}
		name := d.ifNameFor(ifIndex)
		id := tableResourceID(family, name)

		if d.state.Links.Stale {
			out = append(out, resource{ID: id, Verdict: Unmanaged, Reason: unreadableLinksReason})
			continue
		}

		var acts []command.Action
		if first != nil {
			acts = first(ifIndex)
		}
		acts = append(acts, d.r.Rt.TeardownTableActions(family, name, ifIndex, "")...)

		out = append(out, resource{
			ID:      id,
			Verdict: Absent,
			Remove:  func() []command.Action { return acts },
		})
	}
	return out
}

func (d *reconciler) ifNameFor(ifIndex int) string {
	for _, name := range command.SortedKeys(d.state.Links.Ifaces) {
		if d.state.Links.Ifaces[name] == ifIndex {
			return name
		}
	}
	return "if" + strconv.Itoa(ifIndex)
}

func mangleResourceFor(id string, want []firewall.MarkRule, qs []command.Query,
	inSync func() bool, install, cleanup func() []command.Action) resource {

	r := resource{
		ID:      id,
		Verdict: Want,
		Queries: qs,
		InSync:  inSync,
		Install: install,
		Remove: func() []command.Action {
			if inSync() {
				return nil
			}
			return cleanup()
		},
	}

	if len(want) == 0 {
		r.Verdict = Absent
	}
	return r
}

func mangleResource(fw firewall.Options, inv Inventory, inst installedState) []resource {
	want := inv.MarkRules
	qs, inSync, install, cleanup := mangleBackend(fw, fw.Backend, want, inv.Nft[fw.NftablesTable], inst)

	return []resource{
		mangleResourceFor(mangleResourceID, want, qs, inSync, install, cleanup),
		unusedMangleResource(fw, inst),
	}
}

func unusedMangleResource(fw firewall.Options, inst installedState) resource {
	other := "nftables"
	if fw.Backend == "nftables" {
		other = "iptables"
	}
	qs, inSync, _, cleanup := mangleBackend(fw, other, nil, "", inst)

	return mangleResourceFor(mangleResourceID+"/"+other, nil, qs, inSync, nil, cleanup)
}

func mangleBackend(fw firewall.Options, backend string, want []firewall.MarkRule, nft string, inst installedState) (
	qs []command.Query, inSync func() bool,
	install func() []command.Action, cleanup func() []command.Action) {

	if backend == "nftables" {
		return fw.NftQueries(),
			func() bool { return !fw.NftNeedsRestore(want, inst.Nft) },
			func() []command.Action { return fw.NftActions(nft) },
			fw.CleanupNftActions
	}
	return fw.IptablesQueries(want),
		func() bool { return !fw.IptablesNeedsRestore(want, inst.Iptables) },
		func() []command.Action { return fw.IptablesActions(want) },
		fw.CleanupIptablesActions
}

func fwmarkResources(inv Inventory, inst installedState) []resource {
	var out []resource
	for _, family := range []int{route.FamilyV4, route.FamilyV6} {
		specs := inv.Fwmarks[family]
		if len(specs) == 0 {
			continue
		}
		out = append(out, resource{
			ID:      fwmarkResourceID(family),
			Verdict: Want,
			Queries: []command.Query{route.RulesQuery(family).Query},
			InSync:  func() bool { return !route.FwmarkRulesNeedRestore(specs, inst.Rules[family]) },
			Install: func() []command.Action { return route.FwmarkActions(family, specs) },
		})
	}
	return out
}

func mangle6Resource(fw firewall.Options, inv Inventory, inst installedState) []resource {
	want := inv.MarkRules6

	return []resource{mangleResourceFor(mangle6ResourceID, want, fw.Nft6Queries(),
		func() bool { return !fw.Nft6NeedsRestore(want, inst.Nft6) },
		func() []command.Action { return fw.Nft6Actions(inv.Nft6) },
		fw.CleanupNft6Actions,
	)}
}

func nftResources(npt nptv6.Options, nd ndp.Options, inv Inventory, inst installedState) []resource {
	if !npt.Enabled {
		return nil
	}

	out := []resource{{
		ID:      nftResourceID(npt.Table),
		Verdict: Want,
		Queries: npt.Queries(),
		InSync:  func() bool { return !npt.NeedsRestore(inv.NptRules, inst.Npt) },
		Install: func() []command.Action {
			npt.ReportApplied(inv.Assignments, inv.Dropped, inv.Guarded)
			return npt.Actions(inv.Nft[npt.Table])
		},
	}}

	if nd.Enabled() {
		out = append(out, resource{
			ID:      nftResourceID(nd.Table),
			Verdict: Want,
			Queries: nd.LearnQueries(),
			InSync:  func() bool { return !nd.LearnNeedsRestore(inv.LearnRules, inst.Learn) },
			Install: func() []command.Action { return nd.LearnActions(inv.Nft[nd.Table]) },
		})
		return out
	}

	return append(out, resource{
		ID:      nftResourceID(nd.Table),
		Verdict: Absent,
		Queries: nd.LearnQueries(),
		Remove: func() []command.Action {
			if !nd.TableExists(inst.Learn) {
				return nil
			}
			return []command.Action{nd.DeleteTableAction()}
		},
	})
}
