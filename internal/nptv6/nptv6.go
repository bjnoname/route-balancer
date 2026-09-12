package nptv6

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/sysctl"
)

type Options struct {
	Enabled bool

	Table string

	Internal *net.IPNet

	Subnets map[string]*net.IPNet

	Uplinks []Uplink

	LeakProtection bool
}

type Uplink struct {
	Name           string
	SubnetPriority []string

	AllowInbound bool
}

func (o Options) HasUplink(name string) bool {
	_, ok := o.uplink(name)
	return ok
}

func (o Options) uplink(name string) (Uplink, bool) {
	for _, u := range o.Uplinks {
		if u.Name == name {
			return u, true
		}
	}
	return Uplink{}, false
}

func Resolve(c *config.Config) (Options, error) {
	opts := Options{Table: c.NPTv6Table()}
	if !c.NPTv6Enabled() {
		return opts, nil
	}
	n := c.NPTv6

	if n.InternalPrefix == "" {
		return opts, errors.New("internal_prefix is required")
	}
	_, internal, err := net.ParseCIDR(n.InternalPrefix)
	if err != nil {
		return opts, fmt.Errorf("internal_prefix %q: %w", n.InternalPrefix, err)
	}
	if internal.IP.To4() != nil {
		return opts, fmt.Errorf("internal_prefix %s must be an IPv6 prefix", internal)
	}
	if ones, _ := internal.Mask.Size(); ones > 64 {
		return opts, fmt.Errorf("internal_prefix %s is longer than a /64 and cannot hold a subnet", internal)
	}
	if !n.AllowNonULA && !isULA(internal.IP) {
		return opts, fmt.Errorf("internal_prefix %s is not a ULA (fd00::/8); "+
			"set allow_non_ula to translate a globally routable prefix anyway", internal)
	}

	if len(n.Subnets) == 0 {
		return opts, errors.New("at least one subnet must be defined")
	}
	parsed := make(map[string]*net.IPNet, len(n.Subnets))
	for _, name := range command.SortedKeys(n.Subnets) {
		s := n.Subnets[name]
		if s.Prefix == "" {
			return opts, fmt.Errorf("subnet %q: prefix is required", name)
		}
		_, sn, err := net.ParseCIDR(s.Prefix)
		if err != nil {
			return opts, fmt.Errorf("subnet %q prefix %q: %w", name, s.Prefix, err)
		}
		if ones, bits := sn.Mask.Size(); ones != 64 || bits != 128 {
			return opts, fmt.Errorf("subnet %q prefix %s must be a /64: NPTv6 maps whole /64s "+
				"so that the host identifier is preserved unchanged", name, sn)
		}
		if !internal.Contains(sn.IP) {
			return opts, fmt.Errorf("subnet %q prefix %s is outside internal_prefix %s", name, sn, internal)
		}
		parsed[name] = sn
	}

	if len(n.Uplinks) == 0 {
		return opts, errors.New("at least one uplink must be defined")
	}
	uplinks := command.SortedKeys(n.Uplinks)
	routeSources := 0
	for _, name := range uplinks {
		u := n.Uplinks[name]

		switch u.PrefixSourceName() {
		case config.SourceRoute:
			routeSources++
		case config.SourceRA:
		case config.SourceStatic:
			if u.StaticPrefix == "" {
				return opts, fmt.Errorf("uplink %q: prefix_source %q requires static_prefix", name, config.SourceStatic)
			}
			_, p, err := net.ParseCIDR(u.StaticPrefix)
			if err != nil {
				return opts, fmt.Errorf("uplink %q static_prefix %q: %w", name, u.StaticPrefix, err)
			}
			if ones, _ := p.Mask.Size(); ones > 64 {
				return opts, fmt.Errorf("uplink %q static_prefix %s is longer than a /64 "+
					"and cannot hold a subnet", name, p)
			}
		default:
			return opts, fmt.Errorf("uplink %q: unknown prefix_source %q (want %q, %q or %q)",
				name, u.PrefixSource, config.SourceRoute, config.SourceRA, config.SourceStatic)
		}

		if u.MatchPrefix != "" {
			if _, _, err := net.ParseCIDR(u.MatchPrefix); err != nil {
				return opts, fmt.Errorf("uplink %q match_prefix %q: %w", name, u.MatchPrefix, err)
			}
		}

		if len(u.SubnetPriority) == 0 {
			return opts, fmt.Errorf("uplink %q: subnet_priority must name at least one subnet", name)
		}
		seen := make(map[string]bool, len(u.SubnetPriority))
		for _, sub := range u.SubnetPriority {
			if _, ok := parsed[sub]; !ok {
				return opts, fmt.Errorf("uplink %q: subnet_priority references undefined subnet %q", name, sub)
			}
			if seen[sub] {
				return opts, fmt.Errorf("uplink %q: subnet %q appears twice in subnet_priority", name, sub)
			}
			seen[sub] = true
		}
	}

	if n.ProxyNDPMaxHosts < 0 {
		return opts, fmt.Errorf("proxy_ndp_max_hosts %d must not be negative", n.ProxyNDPMaxHosts)
	}

	if routeSources > 1 {
		for _, name := range uplinks {
			u := n.Uplinks[name]
			if u.PrefixSourceName() == config.SourceRoute && u.MatchPrefix == "" {
				return opts, fmt.Errorf("uplink %q: match_prefix is required when more than one uplink "+
					"uses prefix_source %q, because a discard route does not identify its uplink; "+
					"set it to the ISP's aggregate allocation (e.g. \"2001:db8::/32\")", name, config.SourceRoute)
			}
		}
	}

	opts.Enabled = true
	opts.Internal = internal
	opts.Subnets = parsed
	for _, name := range uplinks {
		opts.Uplinks = append(opts.Uplinks, Uplink{
			Name:           name,
			SubnetPriority: slices.Clone(n.Uplinks[name].SubnetPriority),
			AllowInbound:   n.Uplinks[name].AllowInboundConnections,
		})
	}
	opts.LeakProtection = c.NPTv6LeakProtection()
	return opts, nil
}

func isULA(ip net.IP) bool {
	ip16 := ip.To16()
	return ip16 != nil && ip16[0] == 0xfd
}

func CheckForwarding() {
	if sysctl.Read("/proc/sys/net/ipv6/conf/all/forwarding") != "1" {
		slog.Warn("NPTv6 is enabled but net.ipv6.conf.all.forwarding is 0 — " +
			"translated traffic will not be forwarded")
	}
}
