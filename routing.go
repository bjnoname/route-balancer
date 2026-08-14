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

	args := ecmpArgs(active, cfg.routeProto())

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

// ecmpArgs builds the ip(8) argument list that installs the multipath default
// route covering active. Gateways without a nexthop address get a bare
// "nexthop dev <if>" clause, which is what point-to-point links require.
func ecmpArgs(active []Gateway, proto string) []string {
	args := []string{"route", "add", "default", "metric", "0", "proto", proto}
	for _, gw := range active {
		args = append(args, "nexthop")
		if gw.hasNexthop() {
			args = append(args, "via", gw.IP.String())
		}
		args = append(args, "dev", gw.IfName, "weight", strconv.Itoa(gw.EffectWeight))
	}
	return args
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

	return !nexthopsMatch(desiredNexthops(active), parseNexthops(string(out)))
}

// nexthopKey identifies one nexthop of a default route. via is empty for
// point-to-point links, which have no nexthop address.
type nexthopKey struct {
	via string
	dev string
}

// weightUnknown marks a parsed nexthop whose weight the kernel did not report.
const weightUnknown = -1

// desiredNexthops maps each active gateway to the weight it should carry in
// the ECMP route.
func desiredNexthops(active []Gateway) map[nexthopKey]int {
	want := make(map[nexthopKey]int, len(active))
	for _, gw := range active {
		var via string
		if gw.hasNexthop() {
			via = gw.IP.String()
		}
		want[nexthopKey{via: via, dev: gw.IfName}] = gw.EffectWeight
	}
	return want
}

// parseNexthops extracts the nexthops from `ip route show` output. Multipath
// routes print one "nexthop [via IP] dev IFACE weight W" line per path; a
// route with a single path is stored by the kernel as a plain route and
// printed as "default [via IP] dev IFACE" with no weight token.
func parseNexthops(out string) map[nexthopKey]int {
	got := make(map[nexthopKey]int)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		var via, dev string
		weight := weightUnknown
		for i, f := range fields {
			switch f {
			case "via":
				if i+1 < len(fields) {
					via = fields[i+1]
				}
			case "dev":
				if i+1 < len(fields) {
					dev = fields[i+1]
				}
			case "weight":
				if i+1 < len(fields) {
					if w, err := strconv.Atoi(fields[i+1]); err == nil {
						weight = w
					}
				}
			}
		}
		// via is empty for point-to-point nexthops; the output interface is
		// the only field every nexthop is guaranteed to carry.
		if dev != "" {
			got[nexthopKey{via: via, dev: dev}] = weight
		}
	}
	return got
}

// nexthopsMatch reports whether the nexthops the kernel reports satisfy the
// desired set. A nexthop whose weight the kernel did not report is accepted at
// any desired weight: that only happens for a route with a single path, where
// the kernel stores a plain route and the weight is meaningless. Comparing it
// against a configured weight above 1 would declare drift on every reconcile
// tick and re-apply the route forever.
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

// gatewayTableNeedsRestore reports whether the per-gateway routing table or
// its associated ip rule for gw is missing or incomplete.
func gatewayTableNeedsRestore(gw Gateway) bool {
	ifAddr := primaryAddr(gw.IfIndex)
	if ifAddr == nil {
		// Nothing to restore until the interface has an address again;
		// setupGatewayRoutes would bail out for the same reason.
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

	// Check the table has a default route for the gateway. Point-to-point
	// links have no nexthop address, so their route reads "default dev <if>".
	want := "default dev " + gw.IfName
	if gw.hasNexthop() {
		want = "default via " + gw.IP.String()
	}
	tableOut, err := exec.Command("ip", "-4", "route", "show", "table", table).Output()
	if err != nil || !strings.Contains(string(tableOut), want) {
		return true
	}

	return false
}

// pruneStaleGateways drops any tracked gateway whose default route is no
// longer present in the kernel routing table.
//
// Route events alone are not enough to notice this. When an interface loses
// its address — a lost DHCP lease, a PPP link going down — the kernel removes
// the routes that depended on it without emitting RTM_DELROUTE for each one,
// so the daemon never learns the gateway is gone and its routing table and
// split-access ip rule leak until the next restart.
func pruneStaleGateways() {
	routes, err := foreignDefaultRoutes()
	if err != nil {
		return
	}

	live := make(map[string]struct{}, len(routes))
	for _, r := range routes {
		iface, err := net.InterfaceByName(r.ifName)
		if err != nil {
			continue
		}
		live[gwKey(r.gwIP, iface.Index)] = struct{}{}
	}

	gatewaysMu.Lock()
	stale := staleGateways(gateways, live, staleMisses)
	for _, gw := range stale {
		delete(gateways, gwKey(gw.IP, gw.IfIndex))
	}
	gatewaysMu.Unlock()

	if len(stale) == 0 {
		return
	}

	for _, gw := range stale {
		slog.Warn("Gateway route disappeared without a delete event — tearing down", "gateway", gw)
		stopMonitor(gwKey(gw.IP, gw.IfIndex))
		teardownGatewayRoutes(gw)
	}
	applyECMP()
}

// stalePruneThreshold is how many consecutive liveness checks a gateway must
// fail before it is pruned. It is deliberately above 1: the routing table is
// sampled before the gateway map is read, so a gateway registered in between
// is absent from the sample through no fault of its own. Waiting for a second
// confirmation costs one reconcile interval and makes that race harmless,
// whereas pruning on the first miss tears down a gateway that has just been
// installed.
const stalePruneThreshold = 2

// staleMisses counts consecutive liveness-check failures per gateway key.
// Only ever touched from pruneStaleGateways, which runs on the single
// reconcile goroutine.
var staleMisses = map[string]int{}

// staleGateways returns the tracked gateways that have now missed
// stalePruneThreshold consecutive liveness checks, updating misses in place.
// Callers must hold gatewaysMu.
func staleGateways(tracked map[string]Gateway, live map[string]struct{}, misses map[string]int) []Gateway {
	var stale []Gateway
	for key, gw := range tracked {
		if _, ok := live[key]; ok {
			delete(misses, key)
			continue
		}
		misses[key]++
		if misses[key] < stalePruneThreshold {
			continue
		}
		delete(misses, key)
		stale = append(stale, gw)
	}

	// Forget counters for gateways that are no longer tracked at all, e.g.
	// because a route delete event removed them in the meantime.
	for key := range misses {
		if _, ok := tracked[key]; !ok {
			delete(misses, key)
		}
	}
	return stale
}

// reconcile checks whether the ECMP route, per-gateway routing tables, and ip
// rules still match the desired state, restoring any that were modified
// externally. Called periodically when reconcile_interval is configured.
func reconcile() {
	// Drop vanished gateways first, so the checks below work against the set
	// of gateways that actually still exist.
	pruneStaleGateways()

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
	prio := gatewayPriority(gw.IfIndex)

	slog.Info("Setting up per-gateway routing table",
		"iface", gw.IfName, "table", table, "src", src, "gw", gw.IP)

	// A /32 address (typical on point-to-point links) has no subnet beyond the
	// host itself, so the derived "subnet route" would just duplicate the local
	// address. Skip it.
	if ones, bits := ifAddr.Mask.Size(); ones != bits {
		subnet := (&net.IPNet{IP: ifAddr.IP.Mask(ifAddr.Mask), Mask: ifAddr.Mask}).String()
		runIP("route", "replace", subnet, "dev", gw.IfName, "src", src, "table", table)
	}

	args := []string{"route", "replace", "default"}
	if gw.hasNexthop() {
		args = append(args, "via", gw.IP.String())
	}
	args = append(args, "dev", gw.IfName, "src", src, "table", table)
	runIP(args...)

	runIP("rule", "add", "from", src, "lookup", table, "priority", prio)

	rememberGatewaySrc(gwKey(gw.IP, gw.IfIndex), ifAddr.IP)
}

// teardownGatewayRoutes removes the per-gateway routing table and ip rule.
// Called when a gateway disappears.
func teardownGatewayRoutes(gw Gateway) {
	table := strconv.Itoa(tableForIface(gw.IfIndex))
	prio := gatewayPriority(gw.IfIndex)

	slog.Info("Tearing down per-gateway routing table", "iface", gw.IfName, "table", table)

	if src := gw.srcAddr(); src != nil {
		runIP("rule", "del", "from", src.String(), "lookup", table, "priority", prio)
	} else {
		// The address is already gone — the normal state after a lost DHCP
		// lease, which is exactly when teardown runs — so the rule's selector
		// cannot be reconstructed. Delete by priority instead, which is
		// derived from the interface index and therefore unique to this
		// gateway. Without this the rule and its table leak forever.
		slog.Debug("no source address known, deleting ip rule by priority",
			"iface", gw.IfName, "priority", prio)
		runIP("rule", "del", "priority", prio)
	}

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

// seededRoute is one default route recovered from `ip route show default`.
// gwIP is nil for point-to-point links, which have no nexthop address.
type seededRoute struct {
	gwIP   net.IP
	ifName string
}

// key identifies the route's gateway independently of interface index, for
// multiset comparison between two `ip route show` snapshots.
func (r seededRoute) key() string {
	if len(r.gwIP) == 0 {
		return "dev@" + r.ifName
	}
	return r.gwIP.String() + "@" + r.ifName
}

// subtractRoutes returns all with one occurrence removed for each entry in
// drop. A gateway that appears both in its own default route and as a nexthop
// of ours must survive the subtraction, hence multiset rather than set
// semantics.
func subtractRoutes(all, drop []seededRoute) []seededRoute {
	counts := make(map[string]int, len(drop))
	for _, r := range drop {
		counts[r.key()]++
	}
	out := make([]seededRoute, 0, len(all))
	for _, r := range all {
		if counts[r.key()] > 0 {
			counts[r.key()]--
			continue
		}
		out = append(out, r)
	}
	return out
}

// foreignDefaultRoutes returns the default routes in the main table that this
// daemon did not install, i.e. the ones that describe real uplinks rather than
// our own ECMP route.
func foreignDefaultRoutes() ([]seededRoute, error) {
	// Read our own route first: if the ECMP route changes between the two
	// calls the mismatch can only make a gateway look alive, never dead, so a
	// race cannot cause a live gateway to be pruned.
	ownOut, err := exec.Command("ip", "-4", "route", "show", "default", "metric", "0", "proto", cfg.routeProto()).Output()
	if err != nil {
		ownOut = nil
	}

	allOut, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return nil, err
	}

	return subtractRoutes(parseDefaultRoutes(string(allOut)), parseDefaultRoutes(string(ownOut))), nil
}

// parseDefaultRoutes extracts the usable default routes from
// `ip -4 route show default` output.
func parseDefaultRoutes(out string) []seededRoute {
	var routes []seededRoute
	for _, line := range strings.Split(out, "\n") {
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
		// gwIP stays nil for point-to-point routes ("default dev ppp-ee"),
		// which must be recovered on restart just like any other.
		if ifName == "" {
			continue
		}
		routes = append(routes, seededRoute{gwIP: gwIP, ifName: ifName})
	}
	return routes
}

// seedExistingRoutes reads the current default routes from the kernel and
// populates the gateways map, so the daemon starts from a correct state
// rather than waiting for the first route event.
func seedExistingRoutes() {
	routes, err := foreignDefaultRoutes()
	if err != nil {
		slog.Warn("Could not read existing default routes", "error", err)
		return
	}

	for _, r := range routes {
		if !cfg.shouldInclude(r.ifName) {
			slog.Debug("Skipping unconfigured interface during seed", "iface", r.ifName)
			continue
		}
		iface, err := net.InterfaceByName(r.ifName)
		if err != nil {
			continue
		}

		w := cfg.weight(r.ifName)
		gw := Gateway{
			IP:           r.gwIP,
			IfIndex:      iface.Index,
			IfName:       r.ifName,
			ConfigWeight: w,
			EffectWeight: w,
			Healthy:      true,
		}
		key := gwKey(r.gwIP, iface.Index)

		gatewaysMu.Lock()
		gateways[key] = gw
		gatewaysMu.Unlock()
		setupGatewayRoutes(gw)
		slog.Info("Seeded existing default gateway", "gateway", gw)
	}
}
