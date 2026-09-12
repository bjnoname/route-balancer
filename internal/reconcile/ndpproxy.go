package reconcile

import (
	"log/slog"
	"strings"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/ndp"
)

func (d *reconciler) reportSetFull(full bool, maxHosts int) {
	if full == d.state.Learn.SetFull {
		return
	}
	d.state.Learn.SetFull = full
	if full {
		slog.Warn("NDP proxy: learned-host set is full — further host identifiers "+
			"will not be answered for until existing ones expire",
			"max_hosts", maxHosts,
			"hint", "raise proxy_ndp_max_hosts if the LAN really is this large, but "+
				"note that each entry counts against net.ipv6.neigh.default.gc_thresh")
		return
	}
	slog.Info("NDP proxy: learned-host set is no longer full")
}

func (d *reconciler) wantedProxyEntries(inv Inventory, now time.Time) map[ndp.ProxyEntry]struct{} {
	want := make(map[ndp.ProxyEntry]struct{}, len(inv.ProxyNeigh))
	for e := range inv.ProxyNeigh {
		if d.state.Claims.conflictRemembered(e, now) {
			continue
		}
		want[e] = struct{}{}
	}
	return want
}

func (d *reconciler) proxyEntryDiff(want map[ndp.ProxyEntry]struct{}) (stale, missing []ndp.ProxyEntry) {
	got := d.state.Installed.Proxy.Entries

	staleSet := map[ndp.ProxyEntry]struct{}{}
	for e := range got {
		if _, keep := want[e]; !keep {
			staleSet[e] = struct{}{}
		}
	}
	missingSet := map[ndp.ProxyEntry]struct{}{}
	for e := range want {
		if _, have := got[e]; !have {
			missingSet[e] = struct{}{}
		}
	}
	return ndp.SortedEntries(staleSet), ndp.SortedEntries(missingSet)
}

func (d *reconciler) proxyEntriesInSync(want map[ndp.ProxyEntry]struct{}) bool {
	if d.state.Installed.Proxy.Err != nil {
		return true
	}
	stale, missing := d.proxyEntryDiff(want)
	if len(stale) > 0 {
		return false
	}
	for _, e := range missing {
		if _, inFlight := d.state.Claims.Probing[e]; !inFlight {
			return false
		}
	}
	return true
}

func (d *reconciler) proxyEntryActions(want map[ndp.ProxyEntry]struct{}, now time.Time) []command.Action {
	if err := d.state.Installed.Proxy.Err; err != nil {
		slog.Warn("NDP proxy: cannot read the neighbour table", "error", err)
		return nil
	}
	stale, miss := d.proxyEntryDiff(want)

	var acts []command.Action
	for _, e := range stale {
		slog.Info("NDP proxy: withdrawing entry", "address", e.Addr, "uplink", e.Uplink)
		acts = append(acts, d.r.Nd.WithdrawEntryAction(e))
	}

	claims, proved, refused := planClaims(d.state.Claims, miss, d.state.Links.LocalV6, now)
	for _, r := range refused {
		d.recordConflict(r.Entry, r.Why)
	}
	for _, e := range proved {
		delete(d.state.Claims.Proved, e)
		acts = append(acts, d.r.Nd.InstallEntryAction(e, "probed"))
	}
	for _, c := range claims {
		if c.Kind == ndp.ClaimSpeculative {
			acts = append(acts, d.r.Nd.InstallEntryAction(c.Entry, "speculative"))
		}
		d.state.Claims.Probing[c.Entry] = inflight{Kind: c.Kind, Deadline: now.Add(claimGiveUp)}
	}

	if len(claims) > 0 {
		acts = append(acts, d.offer(claims))
	}

	return acts
}

func (d *reconciler) startNdpProxy() {
	if !d.r.Npt.Enabled {
		return
	}
	opts := d.r.Nd
	if !opts.Enabled() {
		return
	}

	slog.Info("NDP proxy started", "uplinks", strings.Join(opts.UplinkNames(), ","),
		"host_timeout", opts.Timeout)
}

func (d *reconciler) cleanupProxyEntryActions() []command.Action {
	var acts []command.Action
	if installed := d.state.Installed.Proxy; installed.Err == nil {
		for _, e := range ndp.SortedEntries(installed.Entries) {
			acts = append(acts, d.r.Nd.WithdrawEntryAction(e))
		}
	}
	d.state.Claims.Conflicts = map[ndp.ProxyEntry]time.Time{}
	d.state.Claims.Probing = map[ndp.ProxyEntry]inflight{}
	d.state.Claims.Proved = map[ndp.ProxyEntry]bool{}
	return acts
}

func (d *reconciler) cleanupNdpProxyActions() []command.Action {
	acts := d.cleanupProxyEntryActions()
	return append(acts, d.r.Nd.DeleteTableAction())
}
