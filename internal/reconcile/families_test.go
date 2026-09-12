package reconcile

import (
	"slices"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
)

func TestIPv4OffWithdrawsWhatIPv4WasManaging(t *testing.T) {
	t.Parallel()

	d, a := twoUplinks(t)

	a.Out["ip -4 route show default proto 111"] = "default proto 111 metric 0 \n" +
		"\tnexthop via 10.0.0.1 dev eth0 weight 3 \n" +
		"\tnexthop via 10.0.1.1 dev eth1 weight 1 "

	c := *d.cfg
	c.IPv4ECMP, c.IPv6ECMP = off(), true
	d.useConfig(t, &c)

	got := planned(d.planReconcile(d))

	want := []string{
		"ip -4 route del default proto 111",
		"ip -4 rule del priority 1102",
		"ip -4 rule del priority 1103",
		"ip -4 route flush table 102",
		"ip -4 route flush table 103",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the pass planned\n\t%v\nwant\n\t%v", got, want)
	}

	a.Out["ip -4 route show default proto 111"] = ""
	a.Out["ip -4 rule show"] = "0:\tfrom all lookup local\n32766:\tfrom all lookup main\n"
	if acts := d.planReconcile(d); len(acts) != 0 {
		t.Errorf("the pass after the withdrawal planned %v, want nothing", planned(acts))
	}
}

func TestIPv4OffKeepsThePortRulePlumbing(t *testing.T) {
	t.Parallel()

	d, a := twoUplinks(t)
	a.Out["ip -4 route show default proto 111"] = "default proto 111 metric 0 \n" +
		"\tnexthop via 10.0.0.1 dev eth0 weight 3 "

	c := *d.cfg
	c.IPv4ECMP, c.IPv6ECMP = off(), true
	c.Rules = []config.Rule{{Gateway: "eth1", MatchDstPort: []int{443}, MatchProtocol: "tcp"}}
	d.useConfig(t, &c)

	got := planned(d.planReconcile(d))
	joined := strings.Join(got, "\n")

	for _, want := range []string{
		"ip -4 rule add fwmark 2 lookup 103 priority 502",
		"ip -4 route del default proto 111",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("the pass did not plan %q; it planned\n\t%v", want, got)
		}
	}
	if strings.Contains(joined, "route add default") || strings.Contains(joined, "route replace default metric") {
		t.Errorf("the pass installed an IPv4 default route with ipv4_ecmp off:\n\t%v", got)
	}
	for _, table := range []string{"102", "103"} {
		if strings.Contains(joined, "route flush table "+table) {
			t.Errorf("the pass tore down table %s that the port rules still need:\n\t%v", table, got)
		}
	}
}

func off() *bool { b := false; return &b }
