package reconcile

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/probe"
	"github.com/bjnoname/route-balancer/internal/route"
)

type healthKey struct {
	IfName string
	Family int
}

func (k healthKey) String() string { return k.IfName + "/" + route.FamilyName(k.Family) }

type healthState int

const (
	stateHealthy healthState = iota
	stateFailed
)

type healthMonitor struct {
	id     uint64
	key    healthKey
	target probe.Target
	probe  probe.Checker
	cfg    config.Health

	out *actor.Mailbox[Event]

	tick <-chan time.Time

	st          healthState
	consecutive int

	sent healthState
}

func newHealthMonitor(
	id uint64,
	key healthKey,
	target probe.Target,
	hcfg config.Health,
	pcfg config.Probe,
	out *actor.Mailbox[Event],
	tick <-chan time.Time,
) *healthMonitor {
	return &healthMonitor{
		id:     id,
		key:    key,
		target: target,
		probe:  probe.New(pcfg),
		cfg:    hcfg,
		out:    out,
		tick:   tick,
	}
}

func probeInterval(hcfg config.Health) time.Duration {
	if hcfg.Interval == 0 {
		return 5 * time.Second
	}
	return time.Duration(hcfg.Interval)
}

func (m *healthMonitor) timeout() time.Duration {
	if m.cfg.Timeout == 0 {
		return 2 * time.Second
	}
	return time.Duration(m.cfg.Timeout)
}

func (m *healthMonitor) unhealthyThreshold() int {
	if m.cfg.UnhealthyThreshold <= 0 {
		return 3
	}
	return m.cfg.UnhealthyThreshold
}

func (m *healthMonitor) healthyThreshold() int {
	if m.cfg.HealthyThreshold <= 0 {
		return 5
	}
	return m.cfg.HealthyThreshold
}

func (m *healthMonitor) Step(ctx context.Context, look actor.Observer) ([]command.Action, bool) {
	select {
	case <-ctx.Done():
		slog.Info("Health monitor stopped", "gateway", m.key)
		return nil, false
	case <-m.tick:
	}

	q := probe.Query(ctx, m.probe, m.target, m.timeout())
	err := look.Observe([]command.Query{q}).Get(q).Err
	pass := err == nil

	slog.Debug("Health probe result",
		"gateway", m.key,
		"probe", m.probe.String(),
		"pass", pass,
		"error", err)

	m.fold(pass)

	if m.sent == m.st {
		return nil, true
	}
	return []command.Action{m.report()}, true
}

func (m *healthMonitor) fold(pass bool) {
	switch m.st {
	case stateHealthy:
		if pass {
			m.consecutive = 0
			return
		}
		m.consecutive++
		if m.consecutive >= m.unhealthyThreshold() {
			m.st = stateFailed
			m.consecutive = 0
			slog.Warn("Gateway marked unhealthy",
				"gateway", m.key,
				"consecutive_failures", m.unhealthyThreshold())
		}

	case stateFailed:
		if !pass {
			m.consecutive = 0
			return
		}
		m.consecutive++
		if m.consecutive >= m.healthyThreshold() {
			m.st = stateHealthy
			m.consecutive = 0
			slog.Info("Gateway marked healthy",
				"gateway", m.key,
				"consecutive_successes", m.healthyThreshold())
		}
	}
}

func (m *healthMonitor) report() command.Action {
	verdict := m.st
	healthy := verdict == stateHealthy

	outcome := "unhealthy"
	if healthy {
		outcome = "healthy"
	}

	a := actor.Send(m.out, Event(HealthChanged{
		ID:      m.id,
		IfName:  m.target.IfName,
		Family:  m.target.Family,
		Healthy: healthy,
	}), command.Action{
		Tool:  command.ToolHealthVerdict,
		Args:  []string{m.key.String(), outcome},
		Order: command.OrderMonitorStart,
	})

	send := a.Write
	a.Write = func() (string, error) {
		out, err := send()
		if err == nil {
			m.sent = verdict
		}
		return out, err
	}
	return a
}

func (d *reconciler) handleHealthEvent(e HealthChanged) {
	key := healthKey{IfName: e.IfName, Family: e.Family}

	if m, running := d.state.Health.Monitors[key]; !running || m.id != e.ID {
		slog.Debug("Discarding a health verdict from a monitor that is no longer running",
			"gateway", key, "healthy", e.Healthy)
		return
	}

	if e.Healthy {
		delete(d.state.Health.Unhealthy, key)
	} else {
		d.state.Health.Unhealthy[key] = true
	}
}

type monitor struct {
	id     uint64
	target probe.Target
	child  *actor.Child
}

func (d *reconciler) syncMonitors() []command.Action {
	want := probeTargets(d.cfg.Gateways, d.state.Routes)

	var acts []command.Action

	for _, key := range sortedHealthKeys(d.state.Health.Monitors) {
		m := d.state.Health.Monitors[key]
		if target, still := want[key]; still && target.SameAs(m.target) {
			continue
		}
		acts = append(acts, d.stopMonitor(key)...)
	}

	for _, key := range sortedHealthKeys(want) {
		if _, running := d.state.Health.Monitors[key]; running {
			continue
		}
		acts = append(acts, d.startMonitor(key, want[key])...)
	}

	return acts
}

func sortedHealthKeys[V any](m map[healthKey]V) []healthKey {
	return command.SortedKeysFunc(m, func(a, b healthKey) int {
		return cmp.Compare(a.String(), b.String())
	})
}

func probeTargets(gws map[string]config.Gateway, routes routeState) map[healthKey]probe.Target {
	targets := map[healthKey]probe.Target{}
	for _, gw := range routes.Gateways {
		gwCfg, ok := gws[gw.IfName]
		if !ok || gwCfg.Health == nil {
			continue
		}
		key := healthKey{IfName: gw.IfName, Family: gw.AddrFamily()}
		if _, decision := gwCfg.Health.ProbeFor(key.Family); decision != config.ProbeRun {
			continue
		}
		candidate := probe.Target{
			Family:    key.Family,
			GatewayIP: gw.IP,
			IfName:    gw.IfName,
			IfIndex:   gw.IfIndex,
		}
		if best, seen := targets[key]; seen && !preferTarget(candidate, best) {
			continue
		}
		targets[key] = candidate
	}
	return targets
}

func preferTarget(a, b probe.Target) bool {
	if ha, hb := len(a.GatewayIP) > 0, len(b.GatewayIP) > 0; ha != hb {
		return ha
	}
	return a.GatewayIP.String() < b.GatewayIP.String()
}

func (d *reconciler) startMonitor(key healthKey, target probe.Target) []command.Action {
	gwCfg, ok := d.cfg.Gateways[key.IfName]
	if !ok || gwCfg.Health == nil {
		return nil
	}
	pcfg, decision := gwCfg.Health.ProbeFor(key.Family)
	if decision != config.ProbeRun {
		return nil
	}

	if probe.IsICMP(pcfg) && len(target.GatewayIP) == 0 {
		slog.Warn("ICMP probe configured on a point-to-point link, which has no nexthop address to ping — "+
			"the probe will always fail; use an http, tcp, dns, or exec probe instead", "gateway", key)
	}

	d.monSeq++

	var body *healthMonitor
	child := d.self.NewChild("Health monitor "+key.String(), probeInterval(*gwCfg.Health),
		func(tick <-chan time.Time) actor.Behaviour {
			body = newHealthMonitor(d.monSeq, key, target, *gwCfg.Health, pcfg, d.events, tick)
			return body
		})

	d.state.Health.Monitors[key] = &monitor{id: d.monSeq, target: target, child: child}

	return []command.Action{startMonitorAction(d.self, child, key, body.probe.String())}
}

func (d *reconciler) stopMonitor(key healthKey) []command.Action {
	m, running := d.state.Health.Monitors[key]

	delete(d.state.Health.Monitors, key)
	delete(d.state.Health.Unhealthy, key)

	if !running {
		return nil
	}
	return []command.Action{stopMonitorAction(m.child, key)}
}

func startMonitorAction(parent *actor.Actor[Event], child *actor.Child, key healthKey, probeName string) command.Action {
	return command.Action{
		Tool:  command.ToolMonitor,
		Args:  []string{"start", key.String()},
		Order: command.OrderMonitorStart,
		Write: func() (string, error) {
			parent.Adopt(child)
			slog.Info("Health monitor started", "gateway", key, "probe", probeName)
			return "", nil
		},
	}
}

func stopMonitorAction(child *actor.Child, key healthKey) command.Action {
	return command.Action{
		Tool:  command.ToolMonitor,
		Args:  []string{"stop", key.String()},
		Order: command.OrderMonitorStop,
		Write: func() (string, error) {
			child.Stop()
			return "", nil
		},
	}
}

func (d *reconciler) checkUnprobedFamilies() {
	if !d.r.Rt.Observes(route.FamilyV6) {
		return
	}
	for _, name := range command.SortedKeys(d.cfg.Gateways) {
		health := d.cfg.Gateways[name].Health
		if health == nil {
			continue
		}
		if _, decision := health.ProbeFor(route.FamilyV6); decision != config.ProbeUnstated {
			continue
		}
		slog.Warn("IPv6 will not be health-checked on this uplink: its probe names an IPv4 destination, "+
			"and no probe6 is configured — set health.probe6, or health.probe6.type=\"none\" to say so deliberately",
			"iface", name, "probe", health.Probe.Type)
	}
}
