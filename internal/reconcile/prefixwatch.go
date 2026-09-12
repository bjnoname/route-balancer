package reconcile

import (
	"context"
	"log/slog"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/prefix"
)

func (p prefixState) currentLease(uplink string) (prefix.Lease, bool) {
	l, ok := p.Leases[uplink]
	return l, ok
}

func (d *reconciler) recordLease(c prefix.Change) (prev prefix.Lease, had, changed bool) {
	cur, had := d.state.Prefixes.Leases[c.Lease.Uplink]
	if c.Gone {
		if !had || !cur.SameAs(c.Lease) {
			return cur, had, false
		}
		delete(d.state.Prefixes.Leases, c.Lease.Uplink)
		return cur, had, true
	}

	if had && cur.SameAs(c.Lease) {
		return cur, had, false
	}
	d.state.Prefixes.Leases[c.Lease.Uplink] = c.Lease
	return cur, had, true
}

func (d *reconciler) handleLeaseWire(c prefix.Change) {
	d.state.Prefixes.Withdrawn[c.Lease.Uplink] = c.Gone
	d.handleLeaseEvent(c)
}

func (d *reconciler) handleLeaseEvent(c prefix.Change) {
	uplink := c.Lease.Uplink

	cur, had, changed := d.recordLease(c)
	if !changed {
		slog.Debug("NPTv6: lease unchanged, no rule churn",
			"uplink", uplink, "prefix", c.Lease.String())
		return
	}

	switch {
	case c.Gone:
		slog.Info("NPTv6: external prefix withdrawn",
			"uplink", uplink, "prefix", c.Lease.String())
	case had:
		slog.Info("NPTv6: external prefix changed",
			"uplink", uplink, "from", cur.String(), "to", c.Lease.String(),
			"source", c.Lease.Source)
	default:
		slog.Info("NPTv6: external prefix acquired",
			"uplink", uplink, "prefix", c.Lease.String(), "source", c.Lease.Source)
	}
}

func (d *reconciler) emit(ctx context.Context, c prefix.Change) {
	actor.Post(ctx, d.events, Event(LeaseChanged{Change: c}))
}

func (d *reconciler) requestResync(ctx context.Context) {
	actor.Post(ctx, d.events, Event(Resync{}))
}

func (d *reconciler) startPrefixWatch(ctx context.Context) error {
	sources, err := prefix.BuildSources(d.cfg.NPTv6)
	if err != nil {
		return err
	}
	d.sources = sources

	w := &prefix.Watcher{
		Sources:   sources,
		OnChange:  func(c prefix.Change) { d.emit(ctx, c) },
		OnRecheck: func() { d.requestResync(ctx) },
	}
	return w.Start(ctx)
}
