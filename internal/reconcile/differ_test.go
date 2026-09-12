package reconcile

import (
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/route"
)

func (d *testReconciler) pass(resources []resource) *commandtest.Runner {
	d.absorbInstalled(d.Observe(queries(resources)))

	run := commandtest.NewRunner()
	run.RunAll(plan(resources))
	return run
}

func TestTableActionsPutContentsBeforeSelector(t *testing.T) {
	t.Parallel()
	acts := route.TableActions(route.TableSpec{
		IfIndex: 3, IfName: "eth1", Table: "103",
		Src: "10.0.1.2", Subnet: "10.0.1.0/24", Via: "10.0.1.1", Prio: "1103",
	})

	last := -1
	for _, a := range acts {
		if a.Order < last {
			t.Fatalf("actions came back out of order: %v", argsOf(acts))
		}
		last = a.Order
	}
	final := acts[len(acts)-1]
	if final.Args[1] != "rule" {
		t.Errorf("last action is %v, want the ip rule that selects into the table", final.Args)
	}
	if final.Order <= acts[0].Order {
		t.Errorf("the selector does not outrank the table contents: %v", argsOf(acts))
	}
}

func TestInstallRunsInDependencyOrderNotEmissionOrder(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	run := d.pass([]resource{{
		ID:      "test/order",
		Verdict: Want,
		InSync:  func() bool { return false },
		Install: func() []command.Action {
			return []command.Action{
				{Tool: "true", Args: []string{"selector"}, Order: command.OrderFwmarkRule},
				{Tool: "true", Args: []string{"table"}, Order: command.OrderTableRoutes},
			}
		},
	}})

	want := []string{"true table", "true selector"}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("ran %v, want %v — the ranks did not decide the order", got, want)
	}
}

func argsOf(acts []command.Action) []string {
	out := make([]string, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Args[0])
	}
	return out
}

func harmless() []command.Action { return []command.Action{{Tool: "true"}} }

func TestReconcileLeavesUnmanagedAlone(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	installed, removed, explained := false, false, false

	d.pass([]resource{{
		ID:        "test/unmanaged",
		Verdict:   Unmanaged,
		Reason:    retentionReason,
		InSync:    func() bool { return false },
		Install:   func() []command.Action { installed = true; return harmless() },
		Remove:    func() []command.Action { removed = true; return harmless() },
		Unmanaged: func() { explained = true },
	}})

	if installed || removed {
		t.Errorf("unmanaged resource was touched (installed=%v removed=%v)", installed, removed)
	}
	if !explained {
		t.Error("unmanaged resource did not get to report its refusal")
	}
}

func TestReconcileSkipsWhatIsAlreadyInSync(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	installed := false

	run := d.pass([]resource{{
		ID:      "test/insync",
		Verdict: Want,
		InSync:  func() bool { return true },
		Install: func() []command.Action { installed = true; return harmless() },
	}})

	if installed {
		t.Error("an in-sync resource was reinstalled")
	}
	if got := run.Commands(); len(got) != 0 {
		t.Errorf("ran %v, want nothing", got)
	}
}

func TestReconcileInstallsWhatHasDrifted(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	installs := 0

	inSync := false
	r := resource{
		ID:      "test/drifted",
		Verdict: Want,
		InSync:  func() bool { return inSync },
		Install: func() []command.Action { installs++; return harmless() },
	}

	d.pass([]resource{r})
	d.pass([]resource{r})
	if installs != 2 {
		t.Errorf("installed %d times, want one per pass for as long as the kernel disagrees", installs)
	}

	inSync = true
	d.pass([]resource{r})
	if installs != 2 {
		t.Errorf("installed %d times, want the install to stop once the kernel agrees", installs)
	}
}

func TestReconcileRemovesAbsent(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	installed, removed := false, false

	d.pass([]resource{{
		ID:      "test/absent",
		Verdict: Absent,
		InSync:  func() bool { return true },
		Install: func() []command.Action { installed = true; return harmless() },
		Remove:  func() []command.Action { removed = true; return harmless() },
	}})

	if installed {
		t.Error("an absent resource was installed")
	}
	if !removed {
		t.Error("an absent resource was not removed")
	}
}

func markRuleConfig(backend string) *config.Config {
	return &config.Config{
		Firewall: backend,
		Gateways: map[string]config.Gateway{"eth0": {Weight: 1}, "eth1": {Weight: 1}},
		Rules: []config.Rule{
			{MatchDstPort: []int{443}, MatchProtocol: "tcp", Gateway: "eth1"},
		},
	}
}

func TestMangleResourceWantsWhatTheCalculatorAsked(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	for _, backend := range []string{"iptables", "nftables"} {
		t.Run(backend, func(t *testing.T) {
			d.useConfig(t, markRuleConfig(backend))

			inv := Desired(d.r, &d.state)
			if len(inv.MarkRules) == 0 {
				t.Fatal("the calculator produced no mark rules for a configured port rule")
			}

			rs := mangleResource(d.r.Fw, inv, d.look())
			if len(rs) != 2 {
				t.Fatalf("mangleResource returned %d resources, want 2", len(rs))
			}
			if rs[1].Verdict != Absent || rs[1].Remove == nil {
				t.Errorf("the unused backend is %v with Remove=%v, want an absent resource that can remove itself",
					rs[1].Verdict, rs[1].Remove != nil)
			}
			r := rs[0]
			if r.Verdict != Want {
				t.Errorf("verdict = %v, want %v", r.Verdict, Want)
			}
			if r.InSync == nil || r.Install == nil || r.Remove == nil {
				t.Error("a mangle resource is missing one of InSync/Install/Remove")
			}
		})
	}
}

func TestMangleRulesetReachesTheInstall(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, markRuleConfig("nftables"))

	inv := Desired(d.r, &d.state)
	ruleset := inv.Nft[d.cfg.NftablesTable]
	if !strings.Contains(ruleset, "tcp dport { 443 } meta mark set 2") {
		t.Fatalf("inv.Nft[%q] = %q, want the rendered mangle table", d.cfg.NftablesTable, ruleset)
	}

	var stdin string
	for _, a := range mangleResource(d.r.Fw, inv, d.look())[0].Install() {
		if a.Stdin != "" {
			stdin = a.Stdin
		}
	}
	if stdin != ruleset {
		t.Errorf("install piped %q, want the calculator's ruleset", stdin)
	}
}

func TestMangleResourceIsAbsentWithoutPortRules(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	for _, backend := range []string{"iptables", "nftables"} {
		t.Run(backend, func(t *testing.T) {
			d.useConfig(t, &config.Config{
				Firewall: backend,
				Gateways: map[string]config.Gateway{"eth0": {Weight: 1}},
			})

			r := mangleResource(d.r.Fw, Desired(d.r, &d.state), d.look())[0]
			if r.Verdict != Absent {
				t.Errorf("verdict = %v, want %v when nothing should be marked", r.Verdict, Absent)
			}
		})
	}
}

func TestLoneGatewayKeepsItsConfiguredWeight(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useConfig(t, nil)

	cases := []struct {
		name string
		gw   route.Gateway
		want route.NexthopSpec
	}{
		{
			"with nexthop",
			route.Gateway{IP: net.IPv4(172, 16, 19, 91), IfName: "ppp-ee", IfIndex: 7, ConfigWeight: 10},
			route.NexthopSpec{Via: "172.16.19.91", Dev: "ppp-ee", Weight: 10},
		},
		{
			"point-to-point",
			route.Gateway{IfName: "ppp-ee", IfIndex: 7, ConfigWeight: 10},
			route.NexthopSpec{Dev: "ppp-ee", Weight: 10},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d.useGateways(t, tc.gw)
			got := Desired(d.r, &d.state).ECMP[route.FamilyV4].Spec.Nexthops
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("nexthops = %v, want exactly %v", got, tc.want)
			}
		})
	}
}
