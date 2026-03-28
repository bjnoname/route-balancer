package main

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"
)

// bindToDeviceDialer returns a net.Dialer whose Control function sets
// SO_BINDTODEVICE on the socket, forcing connections through ifName.
func bindToDeviceDialer(ifName string) *net.Dialer {
	return &net.Dialer{
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifName)
			})
		},
	}
}

// Probe is the interface satisfied by all health check implementations.
// Check returns nil when the gateway is reachable, non-nil otherwise.
// It must honour ctx cancellation for timeout handling.
type Probe interface {
	Check(ctx context.Context, target Target) error
	String() string
}

// Target describes the gateway being probed.
type Target struct {
	GatewayIP net.IP
	IfName    string
	IfIndex   int
}

// HealthEvent is sent on healthCh when a gateway's health state changes.
type HealthEvent struct {
	GwKey   string
	IfName  string
	Healthy bool
}

// healthCh carries state-change events from monitor goroutines to the main loop.
var healthCh = make(chan HealthEvent, 32)

type healthState int

const (
	stateHealthy healthState = iota
	stateFailed
)

// HealthMonitor runs a single probe for one gateway and emits HealthEvents on
// state transitions. It uses asymmetric thresholds for flap dampening: fast to
// remove a failing gateway, slow to re-add a recovering one.
type HealthMonitor struct {
	gwKey  string
	target Target
	probe  Probe
	cfg    HealthConfig
}

func newHealthMonitor(gw Gateway, hcfg HealthConfig) *HealthMonitor {
	return &HealthMonitor{
		gwKey:  gwKey(gw.IP, gw.IfIndex),
		target: Target{GatewayIP: gw.IP, IfName: gw.IfName, IfIndex: gw.IfIndex},
		probe:  newProbe(hcfg.Probe),
		cfg:    hcfg,
	}
}

func (m *HealthMonitor) interval() time.Duration {
	d, err := time.ParseDuration(m.cfg.Interval)
	if err != nil || d <= 0 {
		return 5 * time.Second
	}
	return d
}

func (m *HealthMonitor) timeout() time.Duration {
	d, err := time.ParseDuration(m.cfg.Timeout)
	if err != nil || d <= 0 {
		return 2 * time.Second
	}
	return d
}

func (m *HealthMonitor) unhealthyThreshold() int {
	if m.cfg.UnhealthyThreshold <= 0 {
		return 3
	}
	return m.cfg.UnhealthyThreshold
}

func (m *HealthMonitor) healthyThreshold() int {
	if m.cfg.HealthyThreshold <= 0 {
		return 5
	}
	return m.cfg.HealthyThreshold
}

// Run executes the health check loop until ctx is cancelled.
// Starts in the HEALTHY state (optimistic) so that ECMP is not disrupted on
// daemon restart — the probe cycle will correct the state if a gateway is down.
func (m *HealthMonitor) Run(ctx context.Context) {
	state := stateHealthy
	consecutive := 0
	ticker := time.NewTicker(m.interval())
	defer ticker.Stop()

	slog.Info("Health monitor started", "gateway", m.target.IfName, "probe", m.probe.String())

	for {
		select {
		case <-ctx.Done():
			slog.Info("Health monitor stopped", "gateway", m.target.IfName)
			return
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, m.timeout())
			err := m.probe.Check(probeCtx, m.target)
			cancel()

			pass := err == nil
			slog.Debug("Health probe result",
				"gateway", m.target.IfName,
				"probe", m.probe.String(),
				"pass", pass,
				"error", err)

			switch state {
			case stateHealthy:
				if !pass {
					consecutive++
					if consecutive >= m.unhealthyThreshold() {
						state = stateFailed
						consecutive = 0
						slog.Warn("Gateway marked unhealthy",
							"gateway", m.target.IfName,
							"consecutive_failures", m.unhealthyThreshold())
						healthCh <- HealthEvent{GwKey: m.gwKey, IfName: m.target.IfName, Healthy: false}
					}
				} else {
					consecutive = 0
				}

			case stateFailed:
				if pass {
					consecutive++
					if consecutive >= m.healthyThreshold() {
						state = stateHealthy
						consecutive = 0
						slog.Info("Gateway marked healthy",
							"gateway", m.target.IfName,
							"consecutive_successes", m.healthyThreshold())
						healthCh <- HealthEvent{GwKey: m.gwKey, IfName: m.target.IfName, Healthy: true}
					}
				} else {
					consecutive = 0
				}
			}
		}
	}
}

// monitors tracks cancel functions for running health monitor goroutines,
// keyed by gwKey.
var (
	monitorsMu sync.Mutex
	monitors   = map[string]context.CancelFunc{}
)

// startMonitor starts a health monitor goroutine for gw if the gateway has a
// health config entry. Any existing monitor for the same key is stopped first.
func startMonitor(gw Gateway) {
	if cfg == nil {
		return
	}
	gwCfg, ok := cfg.Gateways[gw.IfName]
	if !ok || gwCfg.Health == nil {
		return
	}

	key := gwKey(gw.IP, gw.IfIndex)
	ctx, cancel := context.WithCancel(context.Background())

	monitorsMu.Lock()
	if existing, exists := monitors[key]; exists {
		existing()
	}
	monitors[key] = cancel
	monitorsMu.Unlock()

	m := newHealthMonitor(gw, *gwCfg.Health)
	go m.Run(ctx)
}

// stopMonitor cancels the health monitor goroutine for the given gateway key.
func stopMonitor(key string) {
	monitorsMu.Lock()
	defer monitorsMu.Unlock()
	if cancel, ok := monitors[key]; ok {
		cancel()
		delete(monitors, key)
	}
}

// stopAllMonitors cancels all running health monitor goroutines.
func stopAllMonitors() {
	monitorsMu.Lock()
	defer monitorsMu.Unlock()
	for key, cancel := range monitors {
		cancel()
		delete(monitors, key)
	}
}

// newProbe constructs the Probe implementation described by pcfg.
func newProbe(pcfg ProbeConfig) Probe {
	switch pcfg.Type {
	case "http", "https":
		method := pcfg.Method
		if method == "" {
			method = "GET"
		}
		return &HTTPProbe{
			URL:            pcfg.URL,
			Method:         method,
			ExpectedStatus: pcfg.ExpectedStatus,
			ExpectedBody:   pcfg.ExpectedBody,
		}
	case "tcp":
		return &TCPProbe{Host: pcfg.Host, Port: pcfg.Port}
	case "dns":
		return &DNSProbe{Resolver: pcfg.Resolver, Query: pcfg.Query}
	case "exec":
		return &ExecProbe{Command: pcfg.Command}
	default: // "icmp" or unset
		size := pcfg.PayloadSize
		if size <= 0 {
			size = 56
		}
		return &ICMPProbe{PayloadSize: size}
	}
}
