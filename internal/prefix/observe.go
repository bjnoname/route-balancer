package prefix

import (
	"math"
	"net"
	"strconv"
	"strings"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/netlink"
)

func (ss Sources) KernelObservations(a command.Answers) []netlink.Observation {
	obs := discardRouteObservations(a)
	return append(obs, ss.raAddressObservations(a)...)
}

func (ss Sources) Queries() []command.Query {
	var qs []command.Query
	for _, q := range discardRouteQueries() {
		qs = append(qs, q.Query)
	}
	for _, s := range ss {
		if s.name() == config.SourceRA {
			qs = append(qs, raAddrQuery(s.uplink()).Query)
		}
	}
	return qs
}

var rejectTypes = []string{"unreachable", "blackhole", "prohibit"}

func discardRouteQueries() []command.Typed[[]netlink.Observation] {
	qs := make([]command.Typed[[]netlink.Observation], 0, len(rejectTypes))
	for _, rejectType := range rejectTypes {
		qs = append(qs, command.NewTyped(
			command.Query{
				Tool: command.ToolIP,
				Args: []string{"-6", "route", "show", "type", rejectType},
			},
			func(out string) []netlink.Observation { return parseDiscardRoutes(rejectType, out) },
		))
	}
	return qs
}

func parseDiscardRoutes(rejectType, out string) []netlink.Observation {
	var obs []netlink.Observation
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != rejectType {
			continue
		}
		prefix, length, ok := parseObservedPrefix(fields[1])
		if !ok {
			continue
		}
		obs = append(obs, netlink.Observation{Kind: netlink.ObsRoute, Prefix: prefix, Length: length})
	}
	return obs
}

func raAddrQuery(name string) command.Typed[net.IP] {
	return command.NewTyped(
		command.Query{
			Tool: command.ToolIP,
			Args: []string{"-6", "-o", "addr", "show", "dev", name, "scope", "global", "dynamic"},
		},
		func(out string) net.IP {
			prefix, _ := freshestRAPrefix(out)
			return prefix
		},
	)
}

func discardRouteObservations(a command.Answers) []netlink.Observation {
	var obs []netlink.Observation
	for _, q := range discardRouteQueries() {
		obs = append(obs, q.ValueOr(a, nil)...)
	}
	return obs
}

func (ss Sources) raAddressObservations(a command.Answers) []netlink.Observation {
	var obs []netlink.Observation
	for _, s := range ss {
		if s.name() != config.SourceRA {
			continue
		}
		name := s.uplink()
		prefix := raAddrQuery(name).ValueOr(a, nil)
		if prefix == nil {
			continue
		}
		obs = append(obs, netlink.Observation{
			Kind: netlink.ObsRA, Prefix: prefix, Length: 64, IfName: name,
		})
	}
	return obs
}

func freshestRAPrefix(out string) (net.IP, bool) {
	var best net.IP
	bestValid := -1

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)

		addr := command.FieldAfter(fields, "inet6")
		if addr == "" {
			continue
		}
		valid := parseLifetime(command.FieldAfter(fields, "valid_lft"))
		preferred := parseLifetime(command.FieldAfter(fields, "preferred_lft"))

		ip := net.ParseIP(strings.SplitN(addr, "/", 2)[0])
		if ip == nil || ip.To4() != nil || ip.IsLinkLocalUnicast() {
			continue
		}

		if preferred <= 0 {
			continue
		}
		if valid <= bestValid {
			continue
		}
		best, bestValid = ip.Mask(net.CIDRMask(64, 128)), valid
	}

	return best, best != nil
}

func parseLifetime(s string) int {
	if s == "forever" {
		return math.MaxInt32
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "sec"))
	if err != nil {
		return 0
	}
	return n
}

func parseObservedPrefix(s string) (net.IP, int, bool) {
	_, n, err := net.ParseCIDR(s)
	if err != nil || n.IP.To4() != nil {
		return nil, 0, false
	}
	ones, bits := n.Mask.Size()
	if bits != 128 || ones == 0 || ones > 64 {
		return nil, 0, false
	}
	return n.IP, ones, true
}
