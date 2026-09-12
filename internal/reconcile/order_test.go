package reconcile

import (
	"net"
	"slices"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/route"
)

func TestRouteCleanupRunsTheInstallOrderBackwards(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	run := useRunner()
	d.useGatewayConfig(t, map[string]config.Gateway{"eth0": {Weight: 1}})
	d.useGateways(t, route.Gateway{
		Family: route.FamilyV4, IP: net.IPv4(10, 0, 0, 1), IfName: "eth0", IfIndex: 2,
		ConfigWeight: 1,
	})

	run.RunAll(d.cleanupRouteActions())

	want := []string{
		"ip -4 route del default proto 111",
		"ip -4 rule del priority 1102",
		"ip -4 route flush table 102",
	}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("the cleanup ran\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestGatewayTeardownDeletesTheRuleByPriority(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	run := useRunner()
	run.RunAll(d.teardownGatewayActions(route.Gateway{
		Family: route.FamilyV4, IP: net.IPv4(10, 0, 0, 1), IfName: "eth0", IfIndex: 2,
	}))

	want := []string{
		"ip -4 rule del priority 1102",
		"ip -4 route flush table 102",
	}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("the teardown ran\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestProxyEntriesAreWithdrawnBeforeTheirSuccessorsAreClaimed(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useAsker(map[string]string{
		"ip -6 neigh show proxy": "2001:db8:ff09::10 dev eth1 proxy proto 111\n",
	})
	run := useRunner()
	d.useNPTv6(t, tetherConfig())

	d.state.Prefixes.Leases["eth1"] = leaseFor(t, "eth1", "2001:db8:ff02::/64")
	d.state.Learn.Hosts = []ndp.LearnedHost{
		{Addr: net.ParseIP("fd00:dead:beef:1::10"), Uplink: "eth1"},
	}

	run.RunAll(d.proxyPass(Desired(d.r, &d.state)))

	want := []string{
		"ip -6 neigh del proxy 2001:db8:ff09::10 dev eth1",
		"ip -6 neigh add proxy 2001:db8:ff02::10 dev eth1 protocol 111",

		"probe-claim 2001:db8:ff02::10",
	}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("the proxy sync ran\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestShutdownIsOneBatchInDomainOrder(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	d.useAsker(map[string]string{
		"ip -6 neigh show proxy": "2001:db8:ff02::10 dev eth1 proxy proto 111\n",
	})
	run := useRunner()
	d.useNPTv6(t, tetherConfig())
	d.useGateways(t, route.Gateway{
		Family: route.FamilyV4, IP: net.IPv4(10, 0, 0, 1), IfName: "eth0", IfIndex: 2,
		ConfigWeight: 1,
	})

	run.RunAll(d.planShutdown(d))

	want := []string{
		"ip -4 route del default proto 111",
		"ip -4 rule del priority 1102",
		"ip -4 route flush table 102",
		"iptables -t mangle -D OUTPUT -j ROUTE-BALANCER",
		"iptables -t mangle -D PREROUTING -j ROUTE-BALANCER",
		"iptables -t mangle -D FORWARD -j ROUTE-BALANCER",
		"iptables -t mangle -F ROUTE-BALANCER",
		"iptables -t mangle -X ROUTE-BALANCER",
		"nft delete table ip route-balancer",
		"nft delete table inet route-balancer",
		"nft delete table ip6 route-balancer",
		"nft delete table ip6 route-balancer-nptv6",
		"ip -6 neigh del proxy 2001:db8:ff02::10 dev eth1",
		"nft delete table ip6 route-balancer-nptv6-ndp",
	}
	if got := run.Commands(); !slices.Equal(got, want) {
		t.Errorf("shutdown ran\n\t%v\nwant\n\t%v", got, want)
	}
}
