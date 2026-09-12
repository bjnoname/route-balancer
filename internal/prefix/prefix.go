package prefix

import (
	"fmt"
	"net"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
)

type Lease struct {
	Uplink string
	Prefix net.IP
	Length int
	Source string
}

func (l Lease) SameAs(o Lease) bool {
	return l.Length == o.Length && l.Prefix.Equal(o.Prefix)
}

func (l Lease) String() string {
	if l.Prefix == nil {
		return "<none>"
	}
	return (&net.IPNet{IP: l.Prefix, Mask: net.CIDRMask(l.Length, 128)}).String()
}

type Change struct {
	Lease Lease
	Gone  bool
}

func BuildSources(n *config.NPTv6) (Sources, error) {
	if n == nil {
		return nil, nil
	}

	var sources Sources
	for _, name := range command.SortedKeys(n.Uplinks) {
		u := n.Uplinks[name]

		var match *net.IPNet
		if u.MatchPrefix != "" {
			_, m, err := net.ParseCIDR(u.MatchPrefix)
			if err != nil {
				return nil, fmt.Errorf("uplink %q match_prefix %q: %w", name, u.MatchPrefix, err)
			}
			match = m
		}

		switch u.PrefixSourceName() {
		case config.SourceRoute:
			sources = append(sources, newRouteSource(name, match))
		case config.SourceRA:
			sources = append(sources, newRASource(name, match))
		case config.SourceStatic:
			_, p, err := net.ParseCIDR(u.StaticPrefix)
			if err != nil {
				return nil, fmt.Errorf("uplink %q static_prefix %q: %w", name, u.StaticPrefix, err)
			}
			sources = append(sources, &staticSource{iface: name, prefix: p})
		default:
			return nil, fmt.Errorf("uplink %q: unknown prefix_source %q", name, u.PrefixSource)
		}
	}
	return sources, nil
}
