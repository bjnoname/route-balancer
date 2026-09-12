package reconcile

import (
	"log/slog"
	"net"
	"time"

	"github.com/bjnoname/route-balancer/internal/ndp"
)

const proxyConflictRecheck = 30 * time.Second

type refusal struct {
	Entry ndp.ProxyEntry
	Why   string
}

func planClaims(c claimState, missing []ndp.ProxyEntry, local map[string]bool, now time.Time) (claims []ndp.Claim, proved []ndp.ProxyEntry, refused []refusal) {
	if len(missing) == 0 {
		return nil, nil, nil
	}

	burnedHere := map[ndp.ProxyEntry]bool{}

	for _, e := range missing {
		if _, inFlight := c.Probing[e]; inFlight {
			continue
		}
		if local[e.Addr] {
			refused = append(refused, refusal{Entry: e, Why: "the address is configured on this host"})
			burnedHere[burnKey(e)] = true
			continue
		}
		if c.Proved[e] {
			proved = append(proved, e)
			continue
		}
		if !c.isBurned(e) && !burnedHere[burnKey(e)] {
			claims = append(claims, ndp.Claim{Entry: e, Kind: ndp.ClaimSpeculative})
			continue
		}
		if c.conflictRemembered(e, now) {
			continue
		}
		claims = append(claims, ndp.Claim{Entry: e, Kind: ndp.ClaimScreened})
	}
	return claims, proved, refused
}

func (d *reconciler) handleClaimVerified(e ClaimVerified) {
	f, outstanding := d.state.Claims.Probing[e.Entry]
	if !outstanding || f.Kind != e.Kind {
		slog.Debug("NDP proxy: discarding a verdict for a claim that is no longer outstanding",
			"address", e.Entry.Addr, "uplink", e.Entry.Uplink)
		return
	}
	delete(d.state.Claims.Probing, e.Entry)

	switch {
	case e.Contested:
		d.recordConflict(e.Entry, "another host on the link answered for it")
		if e.Kind == ndp.ClaimSpeculative {
			slog.Warn("NDP proxy: withdrawing a speculative claim",
				"address", e.Entry.Addr, "uplink", e.Entry.Uplink)
		}
	case e.Kind == ndp.ClaimScreened:
		d.clearConflict(e.Entry)
		d.state.Claims.Proved[e.Entry] = true
	}
}

func burnKey(e ndp.ProxyEntry) ndp.ProxyEntry {
	ip := net.ParseIP(e.Addr)
	if ip == nil || ip.To16() == nil {
		return e
	}
	iid := make(net.IP, net.IPv6len)
	copy(iid[8:], ip.To16()[8:])
	return ndp.ProxyEntry{Addr: iid.String(), Uplink: e.Uplink}
}

func (c claimState) isBurned(e ndp.ProxyEntry) bool { return c.Burned[burnKey(e)] }

func (c claimState) conflictRemembered(e ndp.ProxyEntry, now time.Time) bool {
	at, ok := c.Conflicts[e]
	return ok && now.Sub(at) < proxyConflictRecheck
}

func (d *reconciler) clearConflict(e ndp.ProxyEntry) {
	if _, had := d.state.Claims.Conflicts[e]; had {
		delete(d.state.Claims.Conflicts, e)
		slog.Info("NDP proxy: previously claimed address is free, answering for it now",
			"address", e.Addr, "uplink", e.Uplink)
	}
}

func (d *reconciler) recordConflict(e ndp.ProxyEntry, why string) {
	_, repeat := d.state.Claims.Conflicts[e]
	d.state.Claims.Conflicts[e] = time.Now()
	d.state.Claims.Burned[burnKey(e)] = true

	if repeat {
		slog.Debug("NDP proxy: still refusing to answer for a claimed address",
			"address", e.Addr, "uplink", e.Uplink)
		return
	}
	slog.Warn("NDP proxy: refusing to answer for an address that is already claimed",
		"address", e.Addr, "uplink", e.Uplink, "reason", why,
		"hint", "the internal host identifier collides with a real address in the "+
			"external prefix; answering for it would divert that host's traffic to us")
}

const claimGiveUp = 4 * ndp.ProbeTimeout

func (d *reconciler) sweepInFlightClaims(now time.Time) {
	for e, f := range d.state.Claims.Probing {
		if now.Before(f.Deadline) {
			continue
		}
		delete(d.state.Claims.Probing, e)
		slog.Warn("NDP proxy: no verdict for a claim in flight, probing it again",
			"address", e.Addr, "uplink", e.Uplink)
	}
}
