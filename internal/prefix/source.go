package prefix

import (
	"context"
	"log/slog"
	"net"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/netlink"
)

type Source interface {
	name() string
	uplink() string
	attribute(o netlink.Observation) (Change, bool)
	standing() (Lease, bool)
	seedable(withdrawn bool) bool
	start(ctx context.Context, obs <-chan netlink.Observation, report func(Change))
}

func driveSourceLoop(ctx context.Context, s Source, obs <-chan netlink.Observation, report func(Change)) {
	for {
		select {
		case <-ctx.Done():
			return
		case o, ok := <-obs:
			if !ok {
				return
			}
			if c, mine := s.attribute(o); mine {
				report(c)
			}
		}
	}
}

type netlinkSource struct {
	iface string
	match *net.IPNet

	kind   string
	source string

	requireRoutable bool
	requireIface    bool

	seedAfterWithdraw bool
}

func newRouteSource(iface string, match *net.IPNet) *netlinkSource {
	return &netlinkSource{
		iface: iface, match: match,
		kind: netlink.ObsRoute, source: config.SourceRoute,
		requireRoutable:   true,
		seedAfterWithdraw: true,
	}
}

func newRASource(iface string, match *net.IPNet) *netlinkSource {
	return &netlinkSource{
		iface: iface, match: match,
		kind: netlink.ObsRA, source: config.SourceRA,
		requireIface: true,
	}
}

func (s *netlinkSource) name() string   { return s.source }
func (s *netlinkSource) uplink() string { return s.iface }

func (s *netlinkSource) standing() (Lease, bool) { return Lease{}, false }

func (s *netlinkSource) seedable(withdrawn bool) bool {
	return s.seedAfterWithdraw || !withdrawn
}

func (s *netlinkSource) start(ctx context.Context, obs <-chan netlink.Observation, report func(Change)) {
	driveSourceLoop(ctx, s, obs, report)
}

func globallyRoutable(ip net.IP) bool {
	v6 := ip.To16()
	return v6 != nil && ip.To4() == nil && v6[0]&0xe0 == 0x20
}

func (s *netlinkSource) attribute(o netlink.Observation) (Change, bool) {
	switch {
	case o.Kind != s.kind:
	case s.requireIface && o.IfName != s.iface:
	case s.requireRoutable && !globallyRoutable(o.Prefix):
	case s.match != nil && !s.match.Contains(o.Prefix):
	default:
		return Change{
			Lease: Lease{Uplink: s.iface, Prefix: o.Prefix, Length: o.Length, Source: s.source},
			Gone:  o.Withdraw,
		}, true
	}
	return Change{}, false
}

type staticSource struct {
	iface  string
	prefix *net.IPNet
}

func (s *staticSource) name() string   { return config.SourceStatic }
func (s *staticSource) uplink() string { return s.iface }

func (s *staticSource) standing() (Lease, bool) {
	ones, _ := s.prefix.Mask.Size()
	return Lease{
		Uplink: s.iface, Prefix: s.prefix.IP, Length: ones, Source: config.SourceStatic,
	}, true
}

func (s *staticSource) attribute(netlink.Observation) (Change, bool) {
	return Change{}, false
}

func (s *staticSource) seedable(bool) bool { return true }

func (s *staticSource) start(ctx context.Context, obs <-chan netlink.Observation, report func(Change)) {
	driveSourceLoop(ctx, s, obs, report)
}

type Sources []Source

func (ss Sources) Observed(a command.Answers, withdrawn map[string]bool) map[string]Lease {
	return ss.Attribute(ss.KernelObservations(a), withdrawn)
}

func (ss Sources) Attribute(obs []netlink.Observation, withdrawn map[string]bool) map[string]Lease {
	out := map[string]Lease{}
	for _, s := range ss {
		if l, ok := s.standing(); ok {
			out[s.uplink()] = l
			continue
		}
		if !s.seedable(withdrawn[s.uplink()]) {
			slog.Debug("Not seeding an uplink whose prefix was withdrawn",
				"uplink", s.uplink(), "source", s.name())
			continue
		}

		var chosen Lease
		var found bool
		for _, o := range obs {
			c, mine := s.attribute(o)
			if !mine || c.Gone {
				continue
			}
			if found && !chosen.SameAs(c.Lease) {
				slog.Warn("More than one observed prefix could be this uplink's delegation — "+
					"taking the last one seen, which may differ from pass to pass",
					"uplink", s.uplink(), "source", s.name(),
					"candidate", chosen.String(), "also", c.Lease.String(),
					"hint", "set match_prefix to the ISP's aggregate allocation so only the "+
						"delegation matches")
			}
			chosen, found = c.Lease, true
		}
		if found {
			out[s.uplink()] = chosen
		}
	}
	return out
}

func (ss Sources) Uplinks() []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.uplink())
	}
	return out
}
