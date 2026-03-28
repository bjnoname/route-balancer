package main

import (
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ── Mark assignment ───────────────────────────────────────────────────────────
//
// Each gateway gets a stable 1-based fwmark derived from its alphabetical
// position in the configured gateway map. The same ordering is used by both
// the nftables mark rules and the ip fwmark rules, ensuring consistency.

func sortedGatewayNames() []string {
	if cfg == nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Gateways))
	for name := range cfg.Gateways {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// gatewayMark returns the 1-based fwmark for a gateway, or 0 if not found.
func gatewayMark(ifname string) int {
	for i, name := range sortedGatewayNames() {
		if name == ifname {
			return i + 1
		}
	}
	return 0
}

// ── nftables ruleset generation ───────────────────────────────────────────────

// generateNftRuleset builds the complete nftables table string for port-based
// routing. Returns "" when there are no port rules configured.
func generateNftRuleset() string {
	if cfg == nil {
		return ""
	}
	portRules := cfg.portRules()
	if len(portRules) == 0 {
		return ""
	}

	var lines []string
	for _, rule := range portRules {
		mark := gatewayMark(rule.Gateway)
		if mark == 0 {
			slog.Warn("no mark for gateway, skipping nftables rule", "gateway", rule.Gateway)
			continue
		}

		ports := make([]string, len(rule.MatchDstPort))
		for i, p := range rule.MatchDstPort {
			ports[i] = strconv.Itoa(p)
		}
		portSet := strings.Join(ports, ", ")

		switch rule.MatchProtocol {
		case "tcp":
			lines = append(lines, fmt.Sprintf("tcp dport { %s } meta mark set %d", portSet, mark))
		case "udp":
			lines = append(lines, fmt.Sprintf("udp dport { %s } meta mark set %d", portSet, mark))
		default: // "both" or ""
			lines = append(lines, fmt.Sprintf("tcp dport { %s } meta mark set %d", portSet, mark))
			lines = append(lines, fmt.Sprintf("udp dport { %s } meta mark set %d", portSet, mark))
		}
	}

	if len(lines) == 0 {
		return ""
	}

	body := strings.Join(lines, "\n    ")
	return fmt.Sprintf(`table inet %s {
  chain mangle_output {
    type filter hook output priority mangle; policy accept;
    %s
  }

  chain mangle_forward {
    type filter hook forward priority mangle; policy accept;
    %s
  }
}`, cfg.nftablesTable(), body, body)
}

// cleanupNftables removes the managed nftables table (no-op if absent).
func cleanupNftables() {
	exec.Command("nft", "delete", "table", "inet", cfg.nftablesTable()).Run() //nolint:errcheck
}

// applyNftables installs the port-routing nftables table, replacing any
// previous version. Cleans up the table when there are no port rules.
func applyNftables() {
	ruleset := generateNftRuleset()

	// Always delete the existing table first (ignore error when absent).
	cleanupNftables()

	if ruleset == "" {
		return
	}

	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("failed to apply nftables rules",
			"error", err, "output", strings.TrimSpace(string(out)))
		return
	}
	slog.Info("nftables port-routing rules applied", "port_rules", len(cfg.portRules()))
}

// cleanupFwmarkRules removes all fwmark ip rules installed by applyFwmarkRules.
func cleanupFwmarkRules() {
	if cfg == nil {
		return
	}
	for _, rule := range cfg.portRules() {
		mark := gatewayMark(rule.Gateway)
		if mark == 0 {
			continue
		}

		iface, err := net.InterfaceByName(rule.Gateway)
		if err != nil {
			continue
		}

		table := strconv.Itoa(tableForIface(iface.Index))
		prio := strconv.Itoa(500 + mark)
		runIP("rule", "del", "fwmark", strconv.Itoa(mark), "lookup", table, "priority", prio)
	}
}

// fwmarkRulesNeedRestore reports whether any fwmark ip rule installed by
// applyFwmarkRules is missing from the routing policy database.
func fwmarkRulesNeedRestore() bool {
	if cfg == nil || len(cfg.portRules()) == 0 {
		return false
	}

	ruleOut, err := exec.Command("ip", "-4", "rule", "show").Output()
	if err != nil {
		return true
	}
	ruleLines := strings.Split(string(ruleOut), "\n")

	for _, rule := range cfg.portRules() {
		mark := gatewayMark(rule.Gateway)
		if mark == 0 {
			continue
		}

		iface, err := net.InterfaceByName(rule.Gateway)
		if err != nil {
			continue
		}

		table := strconv.Itoa(tableForIface(iface.Index))
		// ip rule show displays fwmark values in hex (e.g. "fwmark 0x1").
		markHex := "0x" + strconv.FormatInt(int64(mark), 16)

		found := false
		for _, line := range ruleLines {
			if strings.Contains(line, "fwmark "+markHex) && strings.Contains(line, "lookup "+table) {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

// ── fwmark ip rules ───────────────────────────────────────────────────────────

// applyFwmarkRules installs ip rules that steer fwmark-stamped packets into
// the per-gateway routing table set up by setupGatewayRoutes.
// Safe to call multiple times — runIP tolerates duplicate rules.
func applyFwmarkRules() {
	if cfg == nil {
		return
	}
	for _, rule := range cfg.portRules() {
		mark := gatewayMark(rule.Gateway)
		if mark == 0 {
			slog.Warn("no mark for gateway, skipping fwmark rule", "gateway", rule.Gateway)
			continue
		}

		iface, err := net.InterfaceByName(rule.Gateway)
		if err != nil {
			slog.Warn("fwmark gateway interface not found",
				"iface", rule.Gateway, "error", err)
			continue
		}

		table := strconv.Itoa(tableForIface(iface.Index))
		// Priority 500+mark keeps fwmark rules below split-access rules (1100+).
		prio := strconv.Itoa(500 + mark)
		slog.Info("Installing fwmark ip rule",
			"mark", mark, "gateway", rule.Gateway, "table", table)
		runIP("rule", "add", "fwmark", strconv.Itoa(mark), "lookup", table, "priority", prio)
	}
}
