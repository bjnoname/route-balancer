package reconcile

import (
	"net"
	"slices"
	"sync"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/netlink"
	"github.com/bjnoname/route-balancer/internal/route"
)

func planned(acts []command.Action) []string {
	run := commandtest.NewRunner()
	run.RunAll(acts)
	return run.Commands()
}

func twoUplinks(t *testing.T) (*testReconciler, obs) {
	t.Helper()

	d := newTestReconciler(t)
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 3}, "eth1": {Weight: 1}})

	a := commandtest.New(map[string]string{
		"ip -4 route show default metric 0 proto 111": "default proto 111 metric 0 \n" +
			"\tnexthop via 10.0.0.1 dev eth0 weight 3 \n" +
			"\tnexthop via 10.0.1.1 dev eth1 weight 1 ",

		"ip -4 route show default": "default proto 111 metric 0 \n" +
			"\tnexthop via 10.0.0.1 dev eth0 weight 3 \n" +
			"\tnexthop via 10.0.1.1 dev eth1 weight 1 \n" +
			"default via 10.0.0.1 dev eth0 proto dhcp metric 100 \n" +
			"default via 10.0.1.1 dev eth1 proto dhcp metric 101 ",
		"ip -4 route show default metric 0": "default proto 111 metric 0 \n" +
			"\tnexthop via 10.0.0.1 dev eth0 weight 3 \n" +
			"\tnexthop via 10.0.1.1 dev eth1 weight 1 ",
		"ip -4 rule show": "0:\tfrom all lookup local\n" +
			"1102:\tfrom 10.0.0.5 lookup 102\n" +
			"1103:\tfrom 10.0.1.5 lookup 103\n" +
			"32766:\tfrom all lookup main\n",
		"ip -4 route show table 102": "default via 10.0.0.1 dev eth0 src 10.0.0.5 \n",
		"ip -4 route show table 103": "default via 10.0.1.1 dev eth1 src 10.0.1.5 \n",
	})
	a.Vals = map[string]any{
		netlink.LinksQuery().String(): []netlink.Link{
			{Index: 2, Name: "eth0"}, {Index: 3, Name: "eth1"},
		},
		netlink.AddrsQuery().String(): []netlink.Addr{
			{Index: 2, IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)},
			{Index: 3, IP: net.IPv4(10, 0, 1, 5), Mask: net.CIDRMask(24, 32)},
		},
	}
	d.obs = obs{a}
	return d, d.obs
}

func TestAConvergedPassPlansNothing(t *testing.T) {
	t.Parallel()

	d, _ := twoUplinks(t)

	for i := range 2 {
		acts := d.planReconcile(d)
		if len(acts) != 0 {
			t.Errorf("pass %d planned %v, want nothing", i+1, planned(acts))
		}
	}
}

func TestAPassReadsBothRoundsThroughTheEngine(t *testing.T) {
	t.Parallel()

	d, a := twoUplinks(t)

	before := d.Reads()
	d.planReconcile(d)
	reads := d.Reads() - before

	for _, q := range []command.Query{netlink.LinksQuery(), netlink.AddrsQuery()} {
		if n := a.Count(q.String()); n != 1 {
			t.Errorf("%s was read %d times, want exactly 1", q, n)
		}
	}
	if n := a.Count("ip -4 rule show"); n != 1 {
		t.Errorf("the policy database was read %d times, want 1", n)
	}
	if int(reads) != len(a.Asked) {
		t.Errorf("the pass issued %d reads but the stub recorded %d", reads, len(a.Asked))
	}
	t.Logf("a converged pass issued %d reads: %v", reads, a.Asked)
}

func TestAFlushedTableIsRepairedByTheNextPass(t *testing.T) {
	t.Parallel()

	d, a := twoUplinks(t)
	if acts := d.planReconcile(d); len(acts) != 0 {
		t.Fatalf("setup: the converged pass wanted work: %v", acts)
	}

	a.Out["ip -4 route show table 102"] = ""

	got := planned(d.planReconcile(d))
	want := []string{
		"ip -4 route replace 10.0.0.0/24 dev eth0 src 10.0.0.5 table 102",
		"ip -4 route replace default via 10.0.0.1 dev eth0 src 10.0.0.5 table 102",
		"ip -4 rule add from 10.0.0.5 lookup 102 priority 1102",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the pass planned\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestAnUplinkLeavingTheInterfaceTableLosesItsTable(t *testing.T) {
	t.Parallel()

	d, a := twoUplinks(t)
	if acts := d.planReconcile(d); len(acts) != 0 {
		t.Fatalf("setup: the converged pass wanted work: %v", acts)
	}

	a.Vals[netlink.LinksQuery().String()] = []netlink.Link{{Index: 2, Name: "eth0"}}
	a.Vals[netlink.AddrsQuery().String()] = []netlink.Addr{
		{Index: 2, IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)},
	}
	a.Out["ip -4 route show default metric 0 proto 111"] =
		"default via 10.0.0.1 dev eth0 proto 111 metric 0 weight 3 "

	got := planned(d.planReconcile(d))
	for _, w := range []string{"ip -4 rule del priority 1103", "ip -4 route flush table 103"} {
		if !slices.Contains(got, w) {
			t.Errorf("the pass planned\n\t%v\nwant it to contain %q", got, w)
		}
	}
	if slices.Contains(got, "ip -4 route flush table 102") {
		t.Errorf("the pass tore down a table it still wants: %v", got)
	}
}

func TestTheStubSurvivesAProbeSharingItWithThePass(t *testing.T) {
	t.Parallel()

	d, a := twoUplinks(t)

	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			d.Ask(ndp.NeighborsQuery("eth1").Query)
		}
	}()
	for range rounds {
		d.Observe([]command.Query{route.RulesQuery(route.FamilyV4).Query})
	}
	wg.Wait()

	if n := a.Count("ip -6 neigh show dev eth1"); n != rounds {
		t.Errorf("the probe's reads recorded %d times, want %d", n, rounds)
	}
	if n := a.Count("ip -4 rule show"); n != rounds {
		t.Errorf("the pass's reads recorded %d times, want %d", n, rounds)
	}
}
