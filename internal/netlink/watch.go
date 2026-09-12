package netlink

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/bjnoname/route-balancer/internal/route"
)

type Watcher struct {
	Families []int

	OwnProto int

	OnRoute func(msgType uint16, gw route.Gateway)

	OnOverrun func()
}

func (w *Watcher) Start(ctx context.Context) error {
	if w.OnRoute == nil || w.OnOverrun == nil {
		return errors.New("netlink watcher: both OnRoute and OnOverrun are required")
	}

	var groups uint32
	names := make([]string, 0, len(w.Families))
	for _, f := range w.Families {
		groups |= RouteGroup(f)
		names = append(names, route.FamilyName(f))
	}

	err := Subscribe(ctx, "route watch", groups, w.decode, w.OnOverrun)
	if err != nil {
		return err
	}

	slog.Info("Netlink socket open, subscribed to route events",
		"families", strings.Join(names, ","))
	return nil
}

func (w *Watcher) decode(msg []byte) {
	if msgType, gw, ok := ParseRouteEvent(msg, w.OwnProto); ok {
		w.OnRoute(msgType, gw)
	}
}
