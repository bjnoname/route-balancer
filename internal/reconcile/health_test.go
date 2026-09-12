package reconcile

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/probe"
	"github.com/bjnoname/route-balancer/internal/route"
)

func (d *testReconciler) probed(t *testing.T, probeType string, ifNames ...string) {
	t.Helper()
	p := config.Probe{Type: probeType}
	if probeType == "exec" {
		p.Command = []string{"true"}
	}
	d.probedWith(t, config.Health{Probe: p}, ifNames...)
}

func (d *testReconciler) probedWith(t *testing.T, health config.Health, ifNames ...string) {
	t.Helper()
	gws := map[string]config.Gateway{}
	for _, name := range ifNames {
		h := health
		gws[name] = config.Gateway{Weight: 1, Health: &h}
	}
	d.useConfig(t, &config.Config{Gateways: gws})
}

func v4Of(ifName string) healthKey { return healthKey{IfName: ifName, Family: route.FamilyV4} }
func v6Of(ifName string) healthKey { return healthKey{IfName: ifName, Family: route.FamilyV6} }

func (d *testReconciler) runningMonitor(t *testing.T, key healthKey) uint64 {
	t.Helper()

	d.monSeq++
	d.state.Health.Monitors[key] = &monitor{
		id:    d.monSeq,
		child: d.self.NewChild("Health monitor "+key.String(), 0, func(<-chan time.Time) actor.Behaviour { return nil }),
	}

	return d.monSeq
}

func TestOneMonitorPerLinkAndFamily(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.probed(t, "exec", "eth1")
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3},
	)

	targets := probeTargets(d.cfg.Gateways, d.state.Routes)
	if len(targets) != 2 {
		t.Fatalf("probeTargets = %v, want one target per family on a dual-stack link", targets)
	}
	if got := targets[v4Of("eth1")].GatewayIP; !got.Equal(net.IPv4(10, 0, 1, 1)) {
		t.Errorf("v4 target = %v, want the v4 nexthop 10.0.1.1", got)
	}
	if got := targets[v6Of("eth1")].GatewayIP; !got.Equal(net.ParseIP("fe80::1")) {
		t.Errorf("v6 target = %v, want the v6 nexthop fe80::1", got)
	}
	for key, target := range targets {
		if target.Family != key.Family {
			t.Errorf("target %v carries family %d, which is not the one it is keyed under", target, target.Family)
		}
	}
}

func TestMonitorTargetsOnlyTheFamiliesWithARoute(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.probed(t, "exec", "eth1")
	d.useGateways(t, route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3})

	targets := probeTargets(d.cfg.Gateways, d.state.Routes)
	if len(targets) != 1 {
		t.Fatalf("probeTargets = %v, want only the family the link holds a route in", targets)
	}
	target := targets[v6Of("eth1")]
	if !target.GatewayIP.Equal(net.ParseIP("fe80::1")) {
		t.Errorf("target = %v, want the v6 nexthop", target.GatewayIP)
	}
	if target.Family != route.FamilyV6 {
		t.Errorf("target family = %d, want IPv6 to match the nexthop", target.Family)
	}
}

func TestPointToPointTargetStillCarriesItsFamily(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.probed(t, "exec", "ppp-ee")
	d.useGateways(t, route.Gateway{Family: route.FamilyV6, IfName: "ppp-ee", IfIndex: 7})

	target := probeTargets(d.cfg.Gateways, d.state.Routes)[v6Of("ppp-ee")]
	if len(target.GatewayIP) != 0 {
		t.Errorf("target address = %v, want none on a point-to-point link", target.GatewayIP)
	}
	if target.Family != route.FamilyV6 {
		t.Errorf("target family = %d, want the gateway's IPv6", target.Family)
	}
}

func TestUnstatedFamilyGetsNoTarget(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.probedWith(t, config.Health{Probe: config.Probe{Type: "tcp", Host: "192.0.2.1", Port: 443}}, "eth1")
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3},
	)

	targets := probeTargets(d.cfg.Gateways, d.state.Routes)
	if _, ok := targets[v4Of("eth1")]; !ok {
		t.Error("no v4 target for the family the configured probe names")
	}
	if _, ok := targets[v6Of("eth1")]; ok {
		t.Error("a v6 target was built from a probe aimed at an IPv4 destination")
	}
}

func TestProbeTypeNoneGetsNoTarget(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	off := config.Probe{Type: config.ProbeNone}
	d.probedWith(t, config.Health{
		Probe:  config.Probe{Type: "exec", Command: []string{"true"}},
		Probe6: &off,
	}, "eth1")
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3},
	)

	targets := probeTargets(d.cfg.Gateways, d.state.Routes)
	if len(targets) != 1 {
		t.Fatalf("probeTargets = %v, want only the family that is probed", targets)
	}
	if _, ok := targets[v4Of("eth1")]; !ok {
		t.Errorf("probeTargets = %v, want the v4 target: only v6 was turned off", targets)
	}
}

func TestUnmonitoredInterfacesGetNoTarget(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, &config.Config{Gateways: map[string]config.Gateway{"eth1": {Weight: 1}}})
	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3})

	if got := probeTargets(d.cfg.Gateways, d.state.Routes); len(got) != 0 {
		t.Errorf("probeTargets = %v, want none for a gateway with no health config", got)
	}
}

func TestSyncMonitorsLeavesAnUnchangedTargetRunning(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.probed(t, "exec", "eth1")
	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3})

	d.syncMonitors()
	first, ok := d.state.Health.Monitors[v4Of("eth1")]
	if !ok {
		t.Fatal("no monitor started for a health-configured gateway")
	}

	d.useGateways(t,
		route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3},
	)
	d.syncMonitors()

	if d.state.Health.Monitors[v4Of("eth1")] != first {
		t.Error("monitor restarted for a link whose target did not change")
	}
	if _, running := d.state.Health.Monitors[v6Of("eth1")]; !running {
		t.Error("no monitor started for the second family the link now holds a route in")
	}
}

func TestSyncMonitorsRestartsOnAChangedNexthop(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.probed(t, "exec", "eth1")
	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3})

	d.syncMonitors()
	first := d.state.Health.Monitors[v4Of("eth1")]
	d.markUnhealthy(t, route.FamilyV4, "eth1")

	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 254), IfName: "eth1", IfIndex: 3})
	d.syncMonitors()

	if d.state.Health.Monitors[v4Of("eth1")] == first {
		t.Error("monitor kept running against a nexthop the link no longer has")
	}
	if !d.state.Health.healthy("eth1", route.FamilyV4) {
		t.Error("verdict from the old monitor outlived it, and nothing left can clear it")
	}
}

func TestSyncMonitorsRestartsOnlyTheFamilyThatChanged(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.probed(t, "exec", "eth1")
	d.useGateways(t,
		route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::1"), IfName: "eth1", IfIndex: 3},
	)

	d.syncMonitors()
	v4, v6 := d.state.Health.Monitors[v4Of("eth1")], d.state.Health.Monitors[v6Of("eth1")]

	d.useGateways(t,
		route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3},
		route.Gateway{Family: route.FamilyV6, IP: net.ParseIP("fe80::2"), IfName: "eth1", IfIndex: 3},
	)
	d.syncMonitors()

	if d.state.Health.Monitors[v4Of("eth1")] != v4 {
		t.Error("v4 monitor restarted by a v6 nexthop change it cannot see")
	}
	if d.state.Health.Monitors[v6Of("eth1")] == v6 {
		t.Error("v6 monitor kept running against a nexthop the link no longer has")
	}
}

func TestSyncMonitorsStopsWhenTheLinkGoesAway(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.probed(t, "exec", "eth1")
	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3})

	d.syncMonitors()
	d.markUnhealthy(t, route.FamilyV4, "eth1")

	d.useGateways(t)
	d.syncMonitors()

	if len(d.state.Health.Monitors) != 0 {
		t.Errorf("monitors = %v, want none once the link holds no default route", d.state.Health.Monitors)
	}
	if !d.state.Health.healthy("eth1", route.FamilyV4) {
		t.Error("a link with no probe running is still recorded as unhealthy")
	}
}

func testMonitor(out *actor.Mailbox[Event], unhealthy int) (*healthMonitor, command.Query) {
	target := probe.Target{
		Family:    route.FamilyV4,
		GatewayIP: net.IPv4(10, 0, 1, 1),
		IfName:    "eth1",
		IfIndex:   3,
	}
	m := newHealthMonitor(7, v4Of("eth1"), target, config.Health{
		Interval:           config.Duration(time.Millisecond),
		UnhealthyThreshold: unhealthy,
		HealthyThreshold:   1,
	}, config.Probe{Type: "exec", Command: []string{"true"}}, out, time.Tick(time.Millisecond))

	return m, probe.Query(context.Background(), m.probe, target, m.timeout())
}

func TestAMonitorAsksForItsProbeThroughTheSeam(t *testing.T) {
	t.Parallel()

	m, q := testMonitor(actor.NewMailbox[Event](4), 1)

	look := newObs(map[string]string{})

	acts, more := m.Step(context.Background(), look)

	if !more {
		t.Error("a monitor stopped on a failing probe, want it to keep watching")
	}
	if got := look.Reads(); got != 1 {
		t.Errorf("the monitor made %d reads through the seam, want exactly its probe", got)
	}
	if got := look.Count(q.String()); got != 1 {
		t.Errorf("the monitor asked %q %d times, want once", q, got)
	}
	if got, want := commands(acts), []string{"health-verdict eth1/v4 unhealthy"}; !slices.Equal(got, want) {
		t.Errorf("one failure past the threshold planned %v, want %v", got, want)
	}
}

func TestAMonitorSaysNothingUntilTheThresholdIsReached(t *testing.T) {
	t.Parallel()

	m, _ := testMonitor(actor.NewMailbox[Event](4), 3)
	look := newObs(map[string]string{})

	for i := 1; i < 3; i++ {
		if acts, _ := m.Step(context.Background(), look); len(acts) != 0 {
			t.Fatalf("failure %d of 3 planned %v, want nothing", i, commands(acts))
		}
	}
	if acts, _ := m.Step(context.Background(), look); len(commands(acts)) != 1 {
		t.Errorf("the third failure planned %v, want one verdict", commands(acts))
	}
}

func TestAMonitorRepeatsAVerdictThatNeverLanded(t *testing.T) {
	t.Parallel()

	full := actor.NewMailbox[Event](0)
	m, _ := testMonitor(full, 1)
	look := newObs(map[string]string{})

	run := commandtest.NewRunner()

	acts, _ := m.Step(context.Background(), look)
	run.RunAll(acts)

	acts, _ = m.Step(context.Background(), look)
	if got, want := commands(acts), []string{"health-verdict eth1/v4 unhealthy"}; !slices.Equal(got, want) {
		t.Fatalf("after a verdict that did not land the monitor planned %v, want %v", got, want)
	}

	landed := actor.NewMailbox[Event](4)
	m.out = landed
	acts, _ = m.Step(context.Background(), look)
	for _, a := range acts {
		if _, err := a.Write(); err != nil {
			t.Fatalf("delivering the verdict: %v", err)
		}
	}
	if landed.Len() != 1 {
		t.Fatalf("%d verdicts landed, want 1", landed.Len())
	}

	if acts, _ := m.Step(context.Background(), look); len(acts) != 0 {
		t.Errorf("the monitor repeated a verdict that had landed: %v", commands(acts))
	}
}

func TestSyncMonitorsPlansTheStartAndTheStop(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.probed(t, "exec", "eth1")
	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3})

	got := commands(d.syncMonitors())
	if want := []string{"monitor start eth1/v4"}; !slices.Equal(got, want) {
		t.Errorf("a newly routed gateway planned %v, want %v", got, want)
	}

	d.useGateways(t)
	got = commands(d.syncMonitors())
	if want := []string{"monitor stop eth1/v4"}; !slices.Equal(got, want) {
		t.Errorf("a link that went away planned %v, want %v", got, want)
	}
}

func TestARestartStopsBeforeItStarts(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.probed(t, "exec", "eth1")
	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3})
	d.syncMonitors()

	d.useGateways(t, route.Gateway{IP: net.IPv4(10, 0, 1, 254), IfName: "eth1", IfIndex: 3})

	got := commands(d.syncMonitors())
	want := []string{"monitor stop eth1/v4", "monitor start eth1/v4"}
	if !slices.Equal(got, want) {
		t.Errorf("a changed nexthop planned %v, want %v", got, want)
	}
}
