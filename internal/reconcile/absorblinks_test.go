package reconcile

import (
	"net"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/netlink"
	"github.com/bjnoname/route-balancer/internal/route"
)

func (d *testReconciler) useLinks(links []netlink.Link, addrs []netlink.Addr) *commandtest.Asker {
	a := commandtest.New(map[string]string{})
	a.Vals = map[string]any{
		netlink.LinksQuery().String(): links,
		netlink.AddrsQuery().String(): addrs,
	}
	d.obs = obs{a}
	return a
}

func (d *testReconciler) linkRound() {
	d.absorbLinks(d.Observe(d.linkPlan()))
}

func TestTheInterfaceTableIsReadThroughTheEngine(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 1}, "eth1": {Weight: 1}})
	a := d.useLinks(
		[]netlink.Link{{Index: 2, Name: "eth0"}, {Index: 3, Name: "eth1"}, {Index: 9, Name: "eth9"}},
		[]netlink.Addr{
			{Index: 2, IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)},
			{Index: 3, IP: net.IPv4(10, 0, 1, 5), Mask: net.CIDRMask(24, 32)},
			{Index: 9, IP: net.IPv4(10, 0, 9, 5), Mask: net.CIDRMask(24, 32)},
		},
	)

	before := d.Reads()
	d.linkRound()

	if n := d.Reads() - before; n != 2 {
		t.Errorf("the interface picture cost %d reads, want 2: %v", n, a.Asked)
	}
	if got := d.state.Links.Ifaces; len(got) != 2 || got["eth0"] != 2 || got["eth1"] != 3 {
		t.Errorf("Ifaces = %v, want only the two configured gateways", got)
	}
	if got := d.state.Links.Addrs[2]; got == nil || got.String() != "10.0.0.5/24" {
		t.Errorf("Addrs[2] = %v, want 10.0.0.5/24", got)
	}
	if _, unconfigured := d.state.Links.Addrs[9]; unconfigured {
		t.Error("an unconfigured interface made it into the pass's picture")
	}
}

func TestAFailedInterfaceReadKeepsTheLinksAlreadyKnown(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 1}})
	d.useLinks(
		[]netlink.Link{{Index: 2, Name: "eth0"}},
		[]netlink.Addr{{Index: 2, IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)}},
	)
	d.linkRound()
	if len(d.state.Links.Ifaces) != 1 {
		t.Fatalf("setup: the first pass saw %v", d.state.Links.Ifaces)
	}

	d.obs = newObs(map[string]string{})
	d.linkRound()

	if got := d.state.Links.Ifaces; len(got) != 1 || got["eth0"] != 2 {
		t.Errorf("Ifaces = %v after a failed dump, want the links already known — "+
			"an unreadable interface table is not an uplink going away", got)
	}
	if d.state.Links.Addrs[2] == nil {
		t.Error("a failed dump dropped the address behind a live uplink, which tears down its table")
	}
}

func TestAnInterfaceThatIsGoneLeavesThePicture(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 1}, "eth1": {Weight: 1}})
	d.useLinks(
		[]netlink.Link{{Index: 2, Name: "eth0"}, {Index: 3, Name: "eth1"}},
		[]netlink.Addr{
			{Index: 2, IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)},
			{Index: 3, IP: net.IPv4(10, 0, 1, 5), Mask: net.CIDRMask(24, 32)},
		},
	)
	d.linkRound()

	d.useLinks(
		[]netlink.Link{{Index: 2, Name: "eth0"}},
		[]netlink.Addr{{Index: 2, IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(24, 32)}},
	)
	d.linkRound()

	if _, still := d.state.Links.Ifaces["eth1"]; still {
		t.Error("a successful dump that no longer lists eth1 left it in the picture")
	}
	if d.state.Links.Ifaces["eth0"] != 2 {
		t.Error("the surviving uplink was dropped too")
	}
}

func TestADegradedFirstPassDoesNotFlushTheTablesItCannotSee(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 1}, "eth1": {Weight: 1}})

	a := commandtest.New(map[string]string{
		"ip -4 rule show": "0:\tfrom all lookup local\n" +
			"1102:\tfrom 10.0.0.5 lookup 102\n" +
			"1103:\tfrom 10.0.1.5 lookup 103\n" +
			"32766:\tfrom all lookup main\n",
	})
	d.obs = obs{a}

	d.linkRound()
	if !d.state.Links.Stale {
		t.Fatal("setup: a failed interface dump did not mark the picture stale")
	}
	d.state.Installed.Rules = map[int]route.Rules{route.FamilyV4: route.ReadRules(route.FamilyV4, d.Observe([]command.Query{route.RulesQuery(route.FamilyV4).Query}))}

	rs := d.staleTableResources(Desired(d.r, &d.state))
	if len(rs) != 2 {
		t.Fatalf("setup: judged %d installed tables, want the two left behind", len(rs))
	}

	run := useRunner()
	run.RunAll(plan(rs))

	if got := run.Commands(); len(got) != 0 {
		t.Errorf("a pass that could not read the interface table ran\n\t%v\nwant nothing — "+
			"an unreadable dump is not every uplink going away", got)
	}
	for _, r := range rs {
		if r.Verdict != Unmanaged {
			t.Errorf("%s was judged %v, want unmanaged", r.ID, r.Verdict)
		}
		if r.Reason != unreadableLinksReason {
			t.Errorf("%s gave reason %q, want the unreadable-links reason", r.ID, r.Reason)
		}
	}
}

func TestATableWhoseUplinkIsGoneIsStillTornDown(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 1}, "eth1": {Weight: 1}})
	a := d.useLinks(nil, nil)
	a.Out["ip -4 rule show"] = "0:\tfrom all lookup local\n" +
		"1102:\tfrom 10.0.0.5 lookup 102\n" +
		"32766:\tfrom all lookup main\n"

	d.linkRound()
	if d.state.Links.Stale {
		t.Fatal("setup: a dump that succeeded and listed nothing was treated as unreadable")
	}
	d.state.Installed.Rules = map[int]route.Rules{route.FamilyV4: route.ReadRules(route.FamilyV4, d.Observe([]command.Query{route.RulesQuery(route.FamilyV4).Query}))}

	run := useRunner()
	run.RunAll(plan(d.staleTableResources(Desired(d.r, &d.state))))

	if len(run.Commands()) == 0 {
		t.Error("a table whose uplink is genuinely gone was left installed")
	}
}
