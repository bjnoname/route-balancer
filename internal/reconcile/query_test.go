package reconcile

import (
	"net"
	"slices"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/route"
)

func TestDriftChecksReadTheInventoryTheCalculatorProduced(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useAsker(map[string]string{
		"ip -4 route show default metric 0 proto 111": "default proto 111 metric 0 \n" +
			"\tnexthop via 10.0.0.1 dev eth0 weight 3 \n" +
			"\tnexthop via 10.0.1.1 dev eth1 weight 1 ",
		"ip -4 rule show": "0:\tfrom all lookup local\n" +
			"1102:\tfrom 10.0.0.5 lookup 102\n" +
			"1103:\tfrom 10.0.1.5 lookup 103\n" +
			"32766:\tfrom all lookup main\n",
		"ip -4 route show table 102": "default via 10.0.0.1 dev eth0 src 10.0.0.5 \n",
		"ip -4 route show table 103": "default via 10.0.1.1 dev eth1 src 10.0.1.5 \n",
	})
	d.useGatewayConfig(t, map[string]config.Gateway{
		"eth0": {Weight: 3},
		"eth1": {Weight: 1},
	})
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 0, 1), IfName: "eth0", IfIndex: 2, ConfigWeight: 3},
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 1},
	)
	d.state.Links.Ifaces = map[string]int{"eth0": 2, "eth1": 3}
	d.state.Links.Addrs = map[int]*net.IPNet{
		2: {IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)},
		3: {IP: net.IPv4(10, 0, 1, 5), Mask: net.CIDRMask(24, 32)},
	}

	inv := Desired(d.r, &d.state)
	inst := d.look()

	for _, rs := range [][]resource{ecmpResources(d.r.Rt, inv, inst), d.tableResources(inv, inst)} {
		for _, r := range rs {
			if !r.InSync() {
				t.Errorf("%s reported drift against what the kernel is holding", r.ID)
			}
		}
	}
}

func TestBothHalvesOfTheFwmarkMechanismAreJudged(t *testing.T) {
	t.Parallel()

	gateways := map[string]config.Gateway{"eth0": {Weight: 1}, "eth1": {Weight: 1}}
	rules := []config.Rule{{MatchDstPort: []int{443}, MatchProtocol: "tcp", Gateway: "eth1"}}

	chainPresent := "-N ROUTE-BALANCER\n" +
		"-A ROUTE-BALANCER -p tcp -m tcp --dport 443 -j MARK --set-xmark 0x2/0xffffffff"
	rulePresent := "502:\tfrom all fwmark 0x2 lookup 103\n"

	newReconcilerWith := func(t *testing.T, out map[string]string) (*testReconciler, Inventory) {
		t.Helper()
		d := newTestReconciler(t)
		d.useAsker(out)
		c := config.Config{Gateways: gateways, Rules: rules}
		d.useConfig(t, &c)
		d.state.Links.Ifaces = map[string]int{"eth0": 2, "eth1": 3}
		return d, Desired(d.r, &d.state)
	}

	t.Run("both halves present", func(t *testing.T) {
		t.Parallel()
		d, inv := newReconcilerWith(t, map[string]string{
			"iptables -t mangle -S ROUTE-BALANCER":               chainPresent,
			"iptables -t mangle -C OUTPUT -j ROUTE-BALANCER":     "",
			"iptables -t mangle -C PREROUTING -j ROUTE-BALANCER": "",
			"ip -4 rule show": rulePresent,
		})
		inst := d.look()
		rs := append(mangleResource(d.r.Fw, inv, inst), fwmarkResources(inv, inst)...)

		for _, r := range rs {
			if r.Verdict == Absent {
				if acts := r.Remove(); len(acts) > 0 {
					t.Errorf("%s wants to remove %d things from a backend that was never installed", r.ID, len(acts))
				}
				continue
			}
			if !r.InSync() {
				t.Errorf("%s reported drift against a mechanism that is fully installed", r.ID)
			}
		}
	})

	t.Run("the mangle chain was flushed and the ip rule survived", func(t *testing.T) {
		t.Parallel()
		d, inv := newReconcilerWith(t, map[string]string{
			"iptables -t mangle -S ROUTE-BALANCER":               "-N ROUTE-BALANCER",
			"iptables -t mangle -C OUTPUT -j ROUTE-BALANCER":     "",
			"iptables -t mangle -C PREROUTING -j ROUTE-BALANCER": "",
			"ip -4 rule show": rulePresent,
		})
		inst := d.look()
		mangle := mangleResource(d.r.Fw, inv, inst)
		if len(mangle) != 2 || mangle[0].InSync() {
			t.Errorf("the mangle chain was flushed and reported in sync")
		}
		fwmark := fwmarkResources(inv, inst)

		for _, r := range fwmark {
			if !r.InSync() {
				t.Errorf("%s reported drift on a rule that is still installed", r.ID)
			}
		}
	})
}

func TestARepairInstallsTheWholeMechanism(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useAsker(map[string]string{
		"iptables -t mangle -S ROUTE-BALANCER":               "-N ROUTE-BALANCER",
		"iptables -t mangle -C OUTPUT -j ROUTE-BALANCER":     "",
		"iptables -t mangle -C PREROUTING -j ROUTE-BALANCER": "",
	})
	d.useConfig(t, &config.Config{
		Gateways: map[string]config.Gateway{"eth0": {Weight: 1}, "eth1": {Weight: 1}},
		Rules: []config.Rule{
			{MatchDstPort: []int{443}, MatchProtocol: "tcp", Gateway: "eth1"},
		},
	})
	d.state.Links.Ifaces = map[string]int{"eth0": 2, "eth1": 3}

	run := d.pass(mangleResource(d.r.Fw, Desired(d.r, &d.state), d.look()))

	want := []string{
		"iptables -t mangle -D OUTPUT -j ROUTE-BALANCER",
		"iptables -t mangle -D PREROUTING -j ROUTE-BALANCER",
		"iptables -t mangle -D FORWARD -j ROUTE-BALANCER",
		"iptables -t mangle -F ROUTE-BALANCER",
		"iptables -t mangle -X ROUTE-BALANCER",
		"iptables -t mangle -N ROUTE-BALANCER",
		"iptables -t mangle -A ROUTE-BALANCER -p tcp --dport 443 -j MARK --set-mark 2",
		"iptables -t mangle -I OUTPUT -j ROUTE-BALANCER",
		"iptables -t mangle -I PREROUTING -j ROUTE-BALANCER",
	}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("the repair ran\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestATableIsBuiltBeforeAnythingSelectsIntoIt(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useAsker(map[string]string{
		"ip -4 rule show":            "0:\tfrom all lookup local\n32766:\tfrom all lookup main\n",
		"ip -4 route show table 102": "",
	})
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 1}})
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 0, 1), IfName: "eth0", IfIndex: 2, ConfigWeight: 1},
	)
	d.state.Links.Ifaces = map[string]int{"eth0": 2}
	d.state.Links.Addrs = map[int]*net.IPNet{
		2: {IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)},
	}

	run := d.pass(d.tableResources(Desired(d.r, &d.state), d.look()))

	want := []string{
		"ip -4 route replace 10.0.0.0/24 dev eth0 src 10.0.0.5 table 102",
		"ip -4 route replace default via 10.0.0.1 dev eth0 src 10.0.0.5 table 102",
		"ip -4 rule add from 10.0.0.5 lookup 102 priority 1102",
	}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("the table was installed as\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestOneWalkReadsThePolicyDatabaseOnce(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	a := d.useAsker(map[string]string{
		"ip -4 rule show": "0:\tfrom all lookup local\n" +
			"502:\tfrom all fwmark 0x2 lookup 103\n" +
			"1102:\tfrom 10.0.0.5 lookup 102\n" +
			"1103:\tfrom 10.0.1.5 lookup 103\n" +
			"32766:\tfrom all lookup main\n",
		"ip -4 route show table 102": "default via 10.0.0.1 dev eth0 src 10.0.0.5 \n",
		"ip -4 route show table 103": "default via 10.0.1.1 dev eth1 src 10.0.1.5 \n",
	})
	d.useConfig(t, &config.Config{
		Gateways: map[string]config.Gateway{"eth0": {Weight: 1}, "eth1": {Weight: 1}},
		Rules: []config.Rule{
			{MatchDstPort: []int{443}, MatchProtocol: "tcp", Gateway: "eth1"},
		},
	})
	d.useGateways(t,
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 0, 1), IfName: "eth0", IfIndex: 2, ConfigWeight: 1},
		route.Gateway{Family: route.FamilyV4, IP: net.IPv4(10, 0, 1, 1), IfName: "eth1", IfIndex: 3, ConfigWeight: 1},
	)
	d.state.Links.Ifaces = map[string]int{"eth0": 2, "eth1": 3}
	d.state.Links.Addrs = map[int]*net.IPNet{
		2: {IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)},
		3: {IP: net.IPv4(10, 0, 1, 5), Mask: net.CIDRMask(24, 32)},
	}

	inv := Desired(d.r, &d.state)
	inst := d.look()
	var all []resource
	all = append(all, d.tableResources(inv, inst)...)
	all = append(all, fwmarkResources(inv, inst)...)
	if len(all) != 3 {
		t.Fatalf("setup: built %d resources, want two tables and the fwmark rules", len(all))
	}

	if acts := plan(all); len(acts) != 0 {
		t.Fatalf("setup: the walk wanted work: %v", acts)
	}
	if n := a.Count("ip -4 rule show"); n != 1 {
		t.Errorf("the pass read the policy database %d times, want 1: %v", n, a.Asked)
	}
}

func TestTheReadBackTakesAFreshPicture(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	a := d.useAsker(map[string]string{"ip -4 rule show": ""})

	answers := []bool{false, true}
	i := 0
	r := resource{
		ID:      "test/readback",
		Verdict: Want,
		Queries: []command.Query{route.RulesQuery(route.FamilyV4).Query},
		InSync: func() bool {
			_ = d.state.Installed.Rules
			_ = d.state.Installed.Rules
			v := answers[min(i, len(answers)-1)]
			i++
			return v
		},
		Install: func() []command.Action { return harmless() },
	}

	d.pass([]resource{r})
	if n := a.Count("ip -4 rule show"); n != 1 {
		t.Errorf("a pass that installs took %d pictures of the policy database, want 1", n)
	}

	d.pass([]resource{r})
	if n := a.Count("ip -4 rule show"); n != 2 {
		t.Errorf("the read-back reused a picture taken before the install (%d reads, want 2)", n)
	}
}

func TestReadsAreCounted(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	a := d.useAsker(map[string]string{"ip -4 rule show": ""})

	before := d.Reads()
	d.Observe([]command.Query{route.RulesQuery(route.FamilyV4).Query})
	if d.Reads() != before+1 {
		t.Errorf("Reads() = %d, want %d — every read goes through the reconciler's one asker",
			d.Reads(), before+1)
	}
	if len(a.Asked) != 1 {
		t.Errorf("the stub recorded %v, want the one query", a.Asked)
	}
}

func TestQueryPlanCoversWhatEveryResourceDeclares(t *testing.T) {
	t.Parallel()

	d, _ := twoUplinks(t)
	d.useConfig(t, &config.Config{
		Gateways: map[string]config.Gateway{"eth0": {Weight: 3}, "eth1": {Weight: 1}},
		Rules: []config.Rule{
			{MatchDstPort: []int{443}, MatchProtocol: "tcp", Gateway: "eth1"},
		},
		NPTv6: tetherConfig(),
	})
	d.absorbLinks(d.Observe(d.linkPlan()))

	planned := map[string]bool{}
	for _, q := range d.queryPlan() {
		planned[q.String()] = true
	}

	d.absorbInstalled(d.Observe(d.queryPlan()))
	inv := Desired(d.r, &d.state)
	rs := d.resources(inv, time.Now())
	if len(rs) == 0 {
		t.Fatal("setup: this reconciler manages nothing, so the check is vacuous")
	}
	for _, r := range rs {
		for _, q := range r.Queries {
			if !planned[q.String()] {
				t.Errorf("%s consults %q, which the pass never declared", r.ID, q)
			}
		}
	}
}
