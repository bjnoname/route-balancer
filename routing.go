package main

import (
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// applyECMP installs a multipath default route at metric 0 covering all
// known gateways whose EffectWeight > 0. It warns if other default routes at
// metric 0 already exist, then attempts "ip route add"; if a conflicting route
// prevents the add it logs an error and leaves the table untouched.
// If all gateways are unhealthy the existing ECMP route is retained to avoid
// a total loss of connectivity.
func applyECMP() {
	active := activeGateways()

	if len(active) == 0 {
		if ownECMPExists() {
			slog.Warn("All gateways unhealthy — retaining last ECMP route rather than removing default route. Check probe configuration.")
		} else {
			slog.Info("No active gateways — leaving routing table unchanged")
		}
		return
	}

	warnExistingDefaultRoutes()

	args := []string{"route", "add", "default", "metric", "0", "proto", cfg.routeProto()}
	for _, gw := range active {
		args = append(args, "nexthop", "via", gw.IP.String(), "dev", gw.IfName,
			"weight", strconv.Itoa(gw.EffectWeight))
	}

	slog.Info("Applying ECMP route", "command", "ip "+strings.Join(args, " "))
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil && ownECMPExists() {
		// Our own proto-tagged route occupies metric 0 — update it in-place.
		args[1] = "replace"
		out, err = exec.Command("ip", args...).CombinedOutput()
	}
	if err != nil {
		slog.Error("Failed to apply ECMP route", "error", err, "output", strings.TrimSpace(string(out)))
		return
	}
	slog.Info("ECMP default route configured", "nexthops", len(active))
}

// ownECMPExists reports whether a route-balancer-managed default route
// (identified by our proto number) already exists at metric 0.
func ownECMPExists() bool {
	out, err := exec.Command("ip", "-4", "route", "show", "default", "metric", "0", "proto", cfg.routeProto()).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// warnExistingDefaultRoutes logs a warning for every default route at metric 0
// that was not installed by this daemon.
func warnExistingDefaultRoutes() {
	out, err := exec.Command("ip", "-4", "route", "show", "default", "metric", "0").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "nexthop") || strings.Contains(line, "proto "+cfg.routeProto()) {
			continue
		}
		slog.Warn("Default route at metric 0 already exists", "route", line)
	}
}

// ecmpNeedsRestore reports whether the ECMP route installed by this daemon is
// missing or no longer matches the desired set of active nexthops. Returns
// false when all gateways are unhealthy (all-unhealthy retention case).
func ecmpNeedsRestore() bool {
	active := activeGateways()

	if len(active) == 0 {
		return false
	}

	out, err := exec.Command("ip", "-4", "route", "show", "default", "metric", "0", "proto", cfg.routeProto()).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return true
	}

	// Build expected set: "IP iface weight"
	expected := make(map[string]struct{}, len(active))
	for _, gw := range active {
		expected[gw.IP.String()+" "+gw.IfName+" "+strconv.Itoa(gw.EffectWeight)] = struct{}{}
	}

	// Parse actual nexthops. Multipath output uses "nexthop via IP dev IFACE weight W"
	// lines; single-nexthop output is "default via IP dev IFACE proto N".
	actual := make(map[string]struct{})
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		var ip, dev string
		weight := 1
		for i, f := range fields {
			switch f {
			case "via":
				if i+1 < len(fields) {
					ip = fields[i+1]
				}
			case "dev":
				if i+1 < len(fields) {
					dev = fields[i+1]
				}
			case "weight":
				if i+1 < len(fields) {
					if w, err2 := strconv.Atoi(fields[i+1]); err2 == nil {
						weight = w
					}
				}
			}
		}
		if ip != "" && dev != "" {
			actual[ip+" "+dev+" "+strconv.Itoa(weight)] = struct{}{}
		}
	}

	if len(actual) != len(expected) {
		return true
	}
	for k := range expected {
		if _, ok := actual[k]; !ok {
			return true
		}
	}
	return false
}

// gatewayTableNeedsRestore reports whether the per-gateway routing table or
// its associated ip rule for gw is missing or incomplete.
func gatewayTableNeedsRestore(gw Gateway) bool {
	ifAddr := primaryAddr(gw.IfIndex)
	if ifAddr == nil {
		return false
	}

	table := strconv.Itoa(tableForIface(gw.IfIndex))
	src := ifAddr.IP.String()

	// Check the split-access ip rule exists: "from <src> lookup <table>"
	ruleOut, err := exec.Command("ip", "-4", "rule", "show").Output()
	if err != nil {
		return true
	}
	ruleFound := false
	for _, line := range strings.Split(string(ruleOut), "\n") {
		if strings.Contains(line, "from "+src) && strings.Contains(line, "lookup "+table) {
			ruleFound = true
			break
		}
	}
	if !ruleFound {
		return true
	}

	// Check the table has a default route via the gateway.
	tableOut, err := exec.Command("ip", "-4", "route", "show", "table", table).Output()
	if err != nil || !strings.Contains(string(tableOut), "default via "+gw.IP.String()) {
		return true
	}

	return false
}

// reconcile checks whether the ECMP route, per-gateway routing tables, and ip
// rules still match the desired state, restoring any that were modified
// externally. Called periodically when reconcile_interval is configured.
func reconcile() {
	if ecmpNeedsRestore() {
		slog.Warn("ECMP route was modified externally — restoring")
		applyECMP()
	}

	for _, gw := range activeGateways() {
		if gatewayTableNeedsRestore(gw) {
			slog.Warn("Per-gateway routing table modified externally — restoring", "iface", gw.IfName)
			setupGatewayRoutes(gw)
		}
	}

	if fwmarkRulesNeedRestore() {
		slog.Warn("fwmark ip rules modified externally — restoring")
		applyFwmarkRules()
	}
}

// ── Per-gateway routing tables (LARTC 4.2.1 split-access) ───────────────────
//
// Each active gateway gets a dedicated routing table (ID = 100 + ifIndex)
// containing:
//   - a subnet route for the directly-connected network (with src hint)
//   - a default route via the gateway (with src hint)
//
// An ip rule "from <ifaceIP> lookup <table>" steers reply traffic back out
// through the interface it arrived on, preventing asymmetric routing.

// tableForIface returns the dedicated routing table ID for an interface.
// Interface indices are small integers (lo=1, eth0=2, …), so 100+ifIndex
// stays well within the valid range of 1–252.
func tableForIface(ifIndex int) int {
	return cfg.routeTableOffset() + ifIndex
}

// gatewayPriority returns the ip rule priority for a gateway's split-access
// rule as a string, ready for use as an ip(8) argument.
func gatewayPriority(ifIndex int) string {
	return strconv.Itoa(1000 + tableForIface(ifIndex))
}

// primaryAddr returns the first IPv4 address (with subnet mask) on the
// interface, or nil when none is assigned yet.
func primaryAddr(ifIndex int) *net.IPNet {
	iface, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			return &net.IPNet{IP: ip4, Mask: ipNet.Mask}
		}
	}
	return nil
}

// runIP runs `ip <args>`, logging any error without aborting.
// "File exists" (EEXIST from duplicate ip rule add) is demoted to Debug
// because setupGatewayRoutes is called idempotently on every RTM_NEWROUTE,
// including the ones the daemon itself generates when replacing a route.
func runIP(args ...string) {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "File exists") {
			slog.Debug("ip rule already present, skipping", "args", args)
			return
		}
		slog.Warn("ip command failed", "args", args, "output", msg, "error", err)
	}
}

// setupGatewayRoutes establishes the per-gateway routing table and ip rule
// for the LARTC split-access pattern. Called whenever a new gateway appears.
func setupGatewayRoutes(gw Gateway) {
	ifAddr := primaryAddr(gw.IfIndex)
	if ifAddr == nil {
		slog.Warn("no IPv4 address on interface, skipping table setup", "iface", gw.IfName)
		return
	}

	table := strconv.Itoa(tableForIface(gw.IfIndex))
	src := ifAddr.IP.String()
	subnet := (&net.IPNet{IP: ifAddr.IP.Mask(ifAddr.Mask), Mask: ifAddr.Mask}).String()
	prio := gatewayPriority(gw.IfIndex)

	slog.Info("Setting up per-gateway routing table",
		"iface", gw.IfName, "table", table, "src", src, "gw", gw.IP)

	runIP("route", "replace", subnet, "dev", gw.IfName, "src", src, "table", table)
	runIP("route", "replace", "default", "via", gw.IP.String(), "dev", gw.IfName, "src", src, "table", table)
	runIP("rule", "add", "from", src, "lookup", table, "priority", prio)
}

// teardownGatewayRoutes removes the per-gateway routing table and ip rule.
// Called when a gateway disappears.
func teardownGatewayRoutes(gw Gateway) {
	ifAddr := primaryAddr(gw.IfIndex)
	if ifAddr == nil {
		return
	}

	table := strconv.Itoa(tableForIface(gw.IfIndex))
	src := ifAddr.IP.String()
	prio := gatewayPriority(gw.IfIndex)

	slog.Info("Tearing down per-gateway routing table", "iface", gw.IfName, "table", table)
	runIP("rule", "del", "from", src, "lookup", table, "priority", prio)
	runIP("route", "flush", "table", table)
}

// cleanupRoutes removes the ECMP default route installed by this daemon and
// tears down all per-gateway routing tables and ip rules for every gateway
// currently tracked in memory.
func cleanupRoutes() {
	runIP("route", "del", "default", "proto", cfg.routeProto())

	gatewaysMu.Lock()
	snapshot := make([]Gateway, 0, len(gateways))
	for _, gw := range gateways {
		snapshot = append(snapshot, gw)
	}
	gatewaysMu.Unlock()

	for _, gw := range snapshot {
		teardownGatewayRoutes(gw)
	}
}

// seedExistingRoutes reads the current default routes from the kernel and
// populates the gateways map, so the daemon starts from a correct state
// rather than waiting for the first route event.
func seedExistingRoutes() {
	out, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		slog.Warn("Could not read existing default routes", "error", err)
		return
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		var gwIP net.IP
		var ifName string
		for i, f := range fields {
			if f == "via" && i+1 < len(fields) {
				gwIP = net.ParseIP(fields[i+1]).To4()
			}
			if f == "dev" && i+1 < len(fields) {
				ifName = fields[i+1]
			}
		}

		if gwIP == nil || ifName == "" {
			continue
		}
		if !cfg.shouldInclude(ifName) {
			slog.Debug("Skipping unconfigured interface during seed", "iface", ifName)
			continue
		}
		iface, err := net.InterfaceByName(ifName)
		if err != nil {
			continue
		}

		w := cfg.weight(ifName)
		gw := Gateway{
			IP:           gwIP,
			IfIndex:      iface.Index,
			IfName:       ifName,
			ConfigWeight: w,
			EffectWeight: w,
			Healthy:      true,
		}
		key := gwKey(gwIP, iface.Index)

		gatewaysMu.Lock()
		gateways[key] = gw
		gatewaysMu.Unlock()
		setupGatewayRoutes(gw)
		slog.Info("Seeded existing default gateway", "gateway", gw)
	}
}
