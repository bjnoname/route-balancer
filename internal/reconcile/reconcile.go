package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/netlink"
	"github.com/bjnoname/route-balancer/internal/nptv6"
	"github.com/bjnoname/route-balancer/internal/prefix"
	"github.com/bjnoname/route-balancer/internal/route"
	"github.com/bjnoname/route-balancer/internal/settings"
)

const QueueDepth = 256

type reconciler struct {
	state State

	cfg *config.Config

	r settings.Resolved

	monSeq uint64

	inbox  <-chan Event
	events *actor.Mailbox[Event]

	offer func([]ndp.Claim) command.Action

	self *actor.Actor[Event]

	sources prefix.Sources

	stopping bool
}

const defaultReconcileInterval = 30 * time.Second

func (d *reconciler) reconcileInterval() time.Duration {
	if interval := time.Duration(d.cfg.ReconcileInterval); interval > 0 {
		return interval
	}
	return defaultReconcileInterval
}

func New(
	ctx context.Context,
	set settings.Resolved,
	in *actor.Mailbox[Event],
	offer func([]ndp.Claim) command.Action,
) (*actor.Actor[Event], error) {
	var d *reconciler

	a := actor.New(ctx, "Event loop", in, func(self *actor.Actor[Event]) actor.Behaviour {
		d = newReconciler(set, in, self, offer)
		return d
	})

	if err := d.startProducers(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func newReconciler(
	set settings.Resolved,
	in *actor.Mailbox[Event],
	self *actor.Actor[Event],
	offer func([]ndp.Claim) command.Action,
) *reconciler {
	return &reconciler{
		state:  newState(),
		cfg:    set.Cfg,
		r:      set,
		inbox:  self.Inbox(),
		events: in,
		offer:  offer,
		self:   self,
	}
}

func Verdicts(to *actor.Mailbox[Event]) func(ndp.ProxyEntry, ndp.ClaimKind, bool) command.Action {
	return func(e ndp.ProxyEntry, kind ndp.ClaimKind, contested bool) command.Action {
		outcome := "free"
		if contested {
			outcome = "contested"
		}

		return actor.Send(to, Event(ClaimVerified{Entry: e, Kind: kind, Contested: contested}),
			command.Action{
				Tool:  command.ToolClaimVerdict,
				Args:  []string{e.Addr, e.Uplink, outcome},
				Order: command.OrderProxyEntry,
			})
	}
}

func (d *reconciler) startProducers(ctx context.Context) error {
	w := &netlink.Watcher{
		Families:  d.r.Rt.ObservedFamilies(),
		OwnProto:  d.cfg.RouteProto,
		OnRoute:   func(t uint16, gw route.Gateway) { actor.Post(ctx, d.events, Event(RouteChanged{Type: t, GW: gw})) },
		OnOverrun: func() { d.requestResync(ctx) },
	}
	if err := w.Start(ctx); err != nil {
		return err
	}

	d.checkUnprobedFamilies()

	if d.r.Npt.Enabled {
		nptv6.CheckForwarding()
		d.startNdpProxy()
		if err := d.startPrefixWatch(ctx); err != nil {
			return fmt.Errorf("start prefix watch: %w", err)
		}
	}

	actor.Post(ctx, d.events, Event(Resync{}))

	actor.Every(ctx, d.events, d.reconcileInterval(), func() Event { return Resync{} })

	return nil
}

func (d *reconciler) planShutdown(look actor.Observer) []command.Action {
	slog.Info("Shutting down, removing what this daemon installed")

	if d.r.Nd.Enabled() {
		a := look.Observe([]command.Query{d.r.Nd.EntriesQuery().Query})
		d.state.Installed.Proxy = d.r.Nd.ReadInstalledProxy(a)
	}

	var acts []command.Action
	for _, phase := range []struct {
		rank  int
		build func() []command.Action
	}{
		{command.CleanupRoutes, d.cleanupRouteActions},
		{command.CleanupFwmark, d.cleanupFwmarkActions},
		{command.CleanupMangle, d.cleanupMangleActions},
		{command.CleanupNptv6, d.cleanupNptv6Actions},
		{command.CleanupNdp, d.cleanupNdpProxyActions},
	} {
		acts = append(acts, command.InCleanupPhase(phase.rank, phase.build())...)
	}
	return acts
}
