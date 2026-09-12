package route

import (
	"net"
	"strconv"
	"strings"

	"github.com/bjnoname/route-balancer/internal/command"
)

type nexthopKey struct {
	via string
	dev string
}

const weightUnknown = -1

func parseNexthops(out string) map[nexthopKey]int {
	got := make(map[nexthopKey]int)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)

		dev := command.FieldAfter(fields, "dev")
		if dev == "" {
			continue
		}

		weight := weightUnknown
		if w, err := strconv.Atoi(command.FieldAfter(fields, "weight")); err == nil {
			weight = w
		}
		got[nexthopKey{via: command.FieldAfter(fields, "via"), dev: dev}] = weight
	}
	return got
}

func nexthopsMatch(want, got map[nexthopKey]int) bool {
	if len(want) != len(got) {
		return false
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			return false
		}
		if g != weightUnknown && g != w {
			return false
		}
	}
	return true
}

func DefaultRoutesQuery(family int, selectors ...string) command.Typed[[]ObservedRoute] {
	return routesView(family, command.Query{
		Tool: command.ToolIP,
		Args: append([]string{FamilyFlag(family), "route", "show", "default"}, selectors...),
	})
}

func routesView(family int, q command.Query) command.Typed[[]ObservedRoute] {
	return command.NewTyped(q, func(out string) []ObservedRoute {
		return parseDefaultRoutes(family, out)
	})
}

func ownRouteQuery(family int, metric, proto string) command.Typed[map[nexthopKey]int] {
	return command.NewTyped(
		DefaultRoutesQuery(family, "metric", metric, "proto", proto).Query,
		parseNexthops,
	)
}

type ObservedRoute struct {
	GwIP   net.IP
	IfName string

	Metric int
}

func (r ObservedRoute) key() string {
	if len(r.GwIP) == 0 {
		return "dev@" + r.IfName
	}
	return r.GwIP.String() + "@" + r.IfName
}

func subtractRoutes(all, drop []ObservedRoute) []ObservedRoute {
	counts := make(map[string]int, len(drop))
	for _, r := range drop {
		counts[r.key()]++
	}
	out := make([]ObservedRoute, 0, len(all))
	for _, r := range all {
		if counts[r.key()] > 0 {
			counts[r.key()]--
			continue
		}
		out = append(out, r)
	}
	return out
}

func parseDefaultRoutes(family int, out string) []ObservedRoute {
	var routes []ObservedRoute
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "unreachable", "blackhole", "prohibit", "throw":
			continue
		}

		ifName := command.FieldAfter(fields, "dev")
		if ifName == "" || ifName == "lo" {
			continue
		}
		metric, _ := strconv.Atoi(command.FieldAfter(fields, "metric"))
		routes = append(routes, ObservedRoute{
			GwIP:   parseGatewayAddr(family, command.FieldAfter(fields, "via")),
			IfName: ifName,
			Metric: metric,
		})
	}
	return routes
}

func parseGatewayAddr(family int, s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil
	}
	if family == FamilyV6 {
		if ip.To4() != nil {
			return nil
		}
		return ip.To16()
	}
	return ip.To4()
}
