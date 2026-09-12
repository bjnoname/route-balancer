package prefix

import (
	"context"
	"errors"
	"log/slog"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/netlink"
)

type Watcher struct {
	Sources Sources

	OnChange func(Change)

	OnRecheck func()
}

func (w *Watcher) Start(ctx context.Context) error {
	if len(w.Sources) == 0 {
		return nil
	}
	if w.OnChange == nil || w.OnRecheck == nil {
		return errors.New("prefix watcher: both OnChange and OnRecheck are required")
	}

	chans := make([]chan netlink.Observation, len(w.Sources))
	needNetlink := false

	for i, s := range w.Sources {
		ch := make(chan netlink.Observation, 32)
		chans[i] = ch
		go s.start(ctx, ch, w.OnChange)
		if s.name() != config.SourceStatic {
			needNetlink = true
		}
		slog.Info("NPTv6 prefix source started", "uplink", s.uplink(), "source", s.name())
	}

	if !needNetlink {
		return nil
	}

	groups := uint32(netlink.RTMGRP_IPV6_ROUTE | netlink.RTMGRP_IPV6_PREFIX | netlink.RTMGRP_IPV6_IFADDR)
	err := netlink.Subscribe(ctx, "prefix watch", groups,
		func(msg []byte) { w.decode(ctx, chans, msg) }, w.OnRecheck)
	if err != nil {
		return err
	}

	slog.Info("Netlink socket open, subscribed to IPv6 route, prefix and address events")
	return nil
}

func (w *Watcher) decode(ctx context.Context, chans []chan netlink.Observation, msg []byte) {
	if obs, ok := netlink.ParseDiscardRoute(msg); ok {
		fanout(ctx, chans, obs)
		return
	}
	if obs, ok := netlink.ParseRAPrefix(msg); ok {
		fanout(ctx, chans, obs)
		return
	}
	if ch, ok := netlink.ParseAddrEvent(msg); ok {
		slog.Debug("IPv6 address change, resyncing leases",
			"ifindex", ch.IfIndex, "addr", ch.Addr, "withdraw", ch.Withdraw)
		w.OnRecheck()
	}
}

func fanout(ctx context.Context, chans []chan netlink.Observation, obs netlink.Observation) {
	for _, ch := range chans {
		select {
		case ch <- obs:
		case <-ctx.Done():
			return
		}
	}
}
