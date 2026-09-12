package claim

import (
	"context"
	"log/slog"
	"time"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/ndp"
)

const (
	QueueDepth = 64

	pollInterval = ndp.ProbeTimeout / 5
)

type Probe struct {
	Claims []ndp.Claim

	Poll bool
}

type Report func(e ndp.ProxyEntry, kind ndp.ClaimKind, contested bool) command.Action

type pending struct {
	Claim   ndp.Claim
	Started time.Time
}

type prober struct {
	nd ndp.Options

	in <-chan Probe

	report Report

	pending map[ndp.ProxyEntry]pending
}

func newProber(nd ndp.Options, in <-chan Probe, report Report) *prober {
	return &prober{nd: nd, in: in, report: report, pending: map[ndp.ProxyEntry]pending{}}
}

func New(ctx context.Context, nd ndp.Options, in *actor.Mailbox[Probe], report Report) *actor.Actor[Probe] {
	a := actor.New(ctx, "Claim actor", in, func(self *actor.Actor[Probe]) actor.Behaviour {
		return newProber(nd, self.Inbox(), report)
	})

	actor.Every(ctx, in, pollInterval, func() Probe { return Probe{Poll: true} })

	return a
}

func Offer(to *actor.Mailbox[Probe]) func(claims []ndp.Claim) command.Action {
	return func(claims []ndp.Claim) command.Action {
		batch := ndp.SortedClaims(claims)

		addrs := make([]string, 0, len(batch))
		for _, c := range batch {
			addrs = append(addrs, c.Entry.Addr)
		}

		return actor.Send(to, Probe{Claims: batch}, command.Action{
			Tool:  command.ToolProbeClaim,
			Args:  addrs,
			Order: command.OrderProxyEntry,
		})
	}
}

func (a *prober) Step(ctx context.Context, look actor.Observer) ([]command.Action, bool) {
	select {
	case <-ctx.Done():
		return nil, false
	case b := <-a.in:
		if b.Poll {
			return a.poll(look), true
		}
		return a.admit(look, b), true
	}
}

func (a *prober) admit(look actor.Observer, b Probe) []command.Action {
	now := time.Now()

	byUplink := map[string][]ndp.Claim{}
	for _, c := range b.Claims {
		if _, inFlight := a.pending[c.Entry]; inFlight {
			continue
		}
		byUplink[c.Entry.Uplink] = append(byUplink[c.Entry.Uplink], c)
	}
	if len(byUplink) == 0 {
		return nil
	}

	var acts []command.Action
	for _, uplink := range command.SortedKeys(byUplink) {
		batch := ndp.SortedClaims(byUplink[uplink])

		q := ndp.NeighborsQuery(uplink)
		cached := ndp.ReadNeighbors(uplink, look.Observe([]command.Query{q.Query}))
		acts = append(acts, a.nd.PurgeClaimActions(uplink, batch, cached)...)

		for _, c := range batch {
			a.pending[c.Entry] = pending{Claim: c, Started: now}
			acts = append(acts, ndp.SolicitAction(c.Entry.Addr, uplink, func() {
				delete(a.pending, c.Entry)
			}))
		}
		slog.Debug("NDP proxy: probing claims", "uplink", uplink, "claims", len(batch))
	}

	return acts
}

func (a *prober) poll(look actor.Observer) []command.Action {
	if len(a.pending) == 0 {
		return nil
	}
	now := time.Now()

	uplinks := map[string]bool{}
	for e := range a.pending {
		uplinks[e.Uplink] = true
	}

	names := command.SortedKeys(uplinks)
	qs := make([]command.Query, 0, len(names))
	for _, u := range names {
		qs = append(qs, ndp.NeighborsQuery(u).Query)
	}
	ans := look.Observe(qs)

	resolved := make(map[string]map[string]bool, len(names))
	for _, u := range names {
		resolved[u] = ndp.ReadNeighbors(u, ans)
	}

	var contested, free []ndp.Claim
	for e, p := range a.pending {
		switch {
		case resolved[e.Uplink][e.Addr]:
			contested = append(contested, p.Claim)
		case now.Sub(p.Started) >= ndp.ProbeTimeout:
			free = append(free, p.Claim)
		}
	}

	var acts []command.Action
	for _, c := range ndp.SortedClaims(contested) {
		acts = append(acts, a.verdict(c, true))
	}
	for _, c := range ndp.SortedClaims(free) {
		acts = append(acts, a.verdict(c, false))
	}
	return acts
}

func (a *prober) verdict(c ndp.Claim, contested bool) command.Action {
	delete(a.pending, c.Entry)
	return a.report(c.Entry, c.Kind, contested)
}
