// route-balancer — Reactive ECMP load-balanced default gateway daemon.
//
// Requires: root / CAP_NET_ADMIN
// Build:    go build -o route-balancer .
// Run:      sudo ./route-balancer [--config /path/to/config.json]

package main

import (
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// shutdown removes all state installed by this daemon: routes, routing tables,
// ip rules, and firewall chains. Called on SIGTERM/SIGINT.
func shutdown() {
	stopAllMonitors()
	cleanupRoutes()
	cleanupFwmarkRules()
	cleanupIptables()
	cleanupNftables()
	slog.Info("route-balancer cleanup complete")
}

func main() {
	cfgPath := parseFlags()

	if cfgPath != "" {
		if err := loadConfig(cfgPath); err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	level := slog.LevelInfo
	if cfg != nil {
		switch cfg.LogLevel {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})))

	if os.Geteuid() != 0 {
		log.Fatal("route-balancer must run as root (needs CAP_NET_ADMIN)")
	}

	slog.Info("route-balancer starting")

	sock, err := syscall.Socket(
		syscall.AF_NETLINK,
		syscall.SOCK_RAW,
		syscall.NETLINK_ROUTE,
	)
	if err != nil {
		log.Fatalf("socket: %v", err)
	}
	defer func() { _ = syscall.Close(sock) }()

	addr := syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Groups: RTMGRP_IPV4_ROUTE,
	}
	if err := syscall.Bind(sock, &addr); err != nil {
		log.Fatalf("bind: %v", err)
	}

	slog.Info("Netlink socket open, subscribed to IPv4 route events")

	// Always clean up both backends so a switch between them leaves no leftovers.
	cleanupIptables()
	cleanupNftables()

	switch cfg.firewallBackend() {
	case "iptables":
		applyIptables()
	case "nftables":
		applyNftables()
	default:
		log.Fatalf("unknown firewall backend %q: must be \"iptables\" or \"nftables\"", cfg.firewallBackend())
	}
	applyFwmarkRules()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		slog.Info("Received signal, shutting down", "signal", sig)
		shutdown()
		os.Exit(0)
	}()

	seedExistingRoutes()

	// Start health monitors for all seeded gateways.
	gatewaysMu.Lock()
	seeded := make([]Gateway, 0, len(gateways))
	for _, gw := range gateways {
		seeded = append(seeded, gw)
	}
	gatewaysMu.Unlock()
	for _, gw := range seeded {
		startMonitor(gw)
	}

	// Apply immediately for any number of seeded gateways: a host with a
	// single uplink still needs the metric-0 route installed, and waiting for
	// a second route event (or the first reconcile tick) would leave it
	// unmanaged until then.
	if len(seeded) > 0 {
		slog.Info("Gateways found on startup — applying ECMP immediately", "count", len(seeded))
		applyECMP()
	}

	// Periodic ECMP reconciliation: restore the route if it was changed externally.
	if interval := cfg.reconcileInterval(); interval > 0 {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for range ticker.C {
				reconcile()
			}
		}()
	}

	// Health event handler: update gateway EffectWeight and re-apply ECMP.
	go func() {
		for event := range healthCh {
			gatewaysMu.Lock()
			if gw, ok := gateways[event.GwKey]; ok {
				gw.Healthy = event.Healthy
				if event.Healthy {
					gw.EffectWeight = gw.ConfigWeight
				} else {
					gw.EffectWeight = 0
				}
				gateways[event.GwKey] = gw
			}
			gatewaysMu.Unlock()
			applyECMP()
		}
	}()

	buf := make([]byte, 4096)

	for {
		n, _, err := syscall.Recvfrom(sock, buf, 0)
		if err != nil {
			slog.Error("recvfrom error", "error", err)
			continue
		}

		msgType, gw, ok := parseRouteEvent(buf[:n])
		if !ok {
			continue
		}

		key := gwKey(gw.IP, gw.IfIndex)

		switch msgType {
		case RTM_NEWROUTE:
			if !cfg.shouldInclude(gw.IfName) {
				slog.Debug("Ignoring route for unconfigured interface", "iface", gw.IfName)
				continue
			}
			slog.Info("New default route detected", "gateway", gw)

			w := cfg.weight(gw.IfName)
			gw.ConfigWeight = w
			gw.EffectWeight = w
			gw.Healthy = true

			gatewaysMu.Lock()
			gateways[key] = gw
			gatewaysMu.Unlock()

			setupGatewayRoutes(gw)
			startMonitor(gw)
			applyECMP()

		case RTM_DELROUTE:
			// Tear down using the stored Gateway rather than the one parsed
			// from the event: it carries the source address cached at setup
			// time, which the interface itself may no longer have.
			gatewaysMu.Lock()
			stored, known := gateways[key]
			delete(gateways, key)
			gatewaysMu.Unlock()

			if !known {
				slog.Debug("Ignoring route removal for untracked gateway", "gateway", gw)
				continue
			}

			slog.Info("Default route removed", "gateway", stored)
			stopMonitor(key)
			teardownGatewayRoutes(stored)
			applyECMP()
		}
	}
}
