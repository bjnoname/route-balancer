package main

import (
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

// applyIptables installs port-routing mark rules in the mangle table using
// iptables, replacing any previous version. Cleans up when there are no port
// rules.
func applyIptables() {
	// Always start clean.
	cleanupIptables()

	if cfg == nil {
		return
	}
	portRules := cfg.portRules()
	if len(portRules) == 0 {
		return
	}

	chain := cfg.iptablesChain()
	out, err := exec.Command("iptables", "-t", "mangle", "-N", chain).CombinedOutput()
	if err != nil {
		slog.Error("failed to create iptables chain",
			"chain", chain, "error", err, "output", strings.TrimSpace(string(out)))
		return
	}

	for _, rule := range portRules {
		mark := gatewayMark(rule.Gateway)
		if mark == 0 {
			slog.Warn("no mark for gateway, skipping iptables rule", "gateway", rule.Gateway)
			continue
		}

		ports := make([]string, len(rule.MatchDstPort))
		for i, p := range rule.MatchDstPort {
			ports[i] = strconv.Itoa(p)
		}
		portSet := strings.Join(ports, ",")
		markStr := strconv.Itoa(mark)

		switch rule.MatchProtocol {
		case "tcp":
			addIPTMarkRule(chain, "tcp", portSet, markStr)
		case "udp":
			addIPTMarkRule(chain, "udp", portSet, markStr)
		default: // "both" or ""
			addIPTMarkRule(chain, "tcp", portSet, markStr)
			addIPTMarkRule(chain, "udp", portSet, markStr)
		}
	}

	// Ensure jump rules from OUTPUT and FORWARD into our chain.
	for _, hook := range []string{"OUTPUT", "FORWARD"} {
		check := exec.Command("iptables", "-t", "mangle", "-C", hook, "-j", chain)
		if check.Run() != nil {
			runIPT("-t", "mangle", "-I", hook, "-j", chain)
		}
	}

	slog.Info("iptables port-routing rules applied", "port_rules", len(portRules))
}

// cleanupIptables removes the managed chain and its jump references.
func cleanupIptables() {
	chain := cfg.iptablesChain()
	for _, hook := range []string{"OUTPUT", "FORWARD"} {
		exec.Command("iptables", "-t", "mangle", "-D", hook, "-j", chain).Run() //nolint:errcheck
	}
	exec.Command("iptables", "-t", "mangle", "-F", chain).Run() //nolint:errcheck
	exec.Command("iptables", "-t", "mangle", "-X", chain).Run() //nolint:errcheck
}

// addIPTMarkRule appends a MARK rule for the given protocol and port set to
// the chain. Uses -m multiport when more than one port is specified.
func addIPTMarkRule(chain, proto, portSet, markStr string) {
	args := []string{"-t", "mangle", "-A", chain, "-p", proto}
	if strings.Contains(portSet, ",") {
		args = append(args, "-m", "multiport", "--dports", portSet)
	} else {
		args = append(args, "--dport", portSet)
	}
	args = append(args, "-j", "MARK", "--set-mark", markStr)
	runIPT(args...)
}

// runIPT runs iptables with the given args, logging any failure.
func runIPT(args ...string) {
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err != nil {
		slog.Warn("iptables command failed",
			"args", args, "output", strings.TrimSpace(string(out)), "error", err)
	}
}
