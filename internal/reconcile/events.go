package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/prefix"
	"github.com/bjnoname/route-balancer/internal/route"
)

type Event interface{ eventName() string }

type RouteChanged struct {
	Type uint16
	GW   route.Gateway
}

type LeaseChanged struct{ Change prefix.Change }

type HealthChanged struct {
	ID      uint64
	IfName  string
	Family  int
	Healthy bool
}

type Resync struct{}

type ClaimVerified struct {
	Entry     ndp.ProxyEntry
	Kind      ndp.ClaimKind
	Contested bool
}

func (RouteChanged) eventName() string  { return "route" }
func (LeaseChanged) eventName() string  { return "lease" }
func (HealthChanged) eventName() string { return "health" }
func (Resync) eventName() string        { return "resync" }
func (ClaimVerified) eventName() string { return "claim" }

func (d *reconciler) planReconcile(look actor.Observer) []command.Action {
	now := time.Now()

	d.absorbLinks(look.Observe(d.linkPlan()))

	d.absorb(look.Observe(d.queryPlan()))

	monitors := d.syncMonitors()
	d.sweepInFlightClaims(now)

	inv := Desired(d.r, &d.state)

	return append(monitors, plan(d.resources(inv, now))...)
}

func (d *reconciler) absorb(a command.Answers) {
	d.absorbGateways(a)
	d.absorbLeases(a)
	d.absorbLearnedHosts(a)
	d.absorbInstalled(a)
}

func (d *reconciler) Step(ctx context.Context, look actor.Observer) ([]command.Action, bool) {
	d.foldBatch(ctx)

	if d.stopping {
		return d.planShutdown(look), false
	}
	return d.planReconcile(look), true
}

func (d *reconciler) foldBatch(ctx context.Context) {

	select {
	case <-ctx.Done():
		d.stopping = true
		return
	default:
	}

	var ev Event

	select {
	case ev = <-d.inbox:
	case <-ctx.Done():
		d.stopping = true
		return
	}

	for {
		d.handle(ev)

		select {
		case ev = <-d.inbox:
		default:
			return
		}
	}
}

func (d *reconciler) handle(ev Event) {
	slog.Debug("Event", "kind", ev.eventName())

	switch e := ev.(type) {
	case RouteChanged:
	case LeaseChanged:
		d.handleLeaseWire(e.Change)
	case HealthChanged:
		d.handleHealthEvent(e)
	case Resync:
	case ClaimVerified:
		d.handleClaimVerified(e)
	}
}
