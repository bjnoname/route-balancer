package reconcile

import (
	"net"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/prefix"
	"github.com/bjnoname/route-balancer/internal/route"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

func leaseFor(t *testing.T, uplink, cidr string) prefix.Lease {
	t.Helper()
	n := mustCIDR(t, cidr)
	ones, _ := n.Mask.Size()
	return prefix.Lease{Uplink: uplink, Prefix: n.IP, Length: ones, Source: config.SourceStatic}
}

func TestDesiredAssignmentsAsymmetric(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets: map[string]config.NPTv6Subnet{
			"main":  {Prefix: "fd00:dead:beef:1::/64"},
			"guest": {Prefix: "fd00:dead:beef:2::/64"},
			"iot":   {Prefix: "fd00:dead:beef:3::/64"},
		},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRoute, MatchPrefix: "2001:db8::/32",
				SubnetPriority: []string{"main", "guest", "iot"}},
			"wan1": {PrefixSource: config.SourceRA, SubnetPriority: []string{"main", "guest"}},
		},
	})

	d.state.Prefixes.Leases["wan0"] = leaseFor(t, "wan0", "2001:db8:abcd::/56")
	d.state.Prefixes.Leases["wan1"] = leaseFor(t, "wan1", "2a02:1234:5678:9abc::/64")

	assigned, dropped := assignmentsFor(d.r.Npt, d.cfg.Gateways, &d.state)

	if len(assigned) != 4 {
		t.Fatalf("assigned %d mappings, want 4 (3 on wan0 + 1 on wan1)", len(assigned))
	}
	if got := strings.Join(dropped["wan1"], ","); got != "guest" {
		t.Errorf("wan1 dropped = %v, want [guest]", dropped["wan1"])
	}
	if len(dropped["wan0"]) != 0 {
		t.Errorf("wan0 dropped = %v, want none", dropped["wan0"])
	}

	var mainExternals []string
	for _, a := range assigned {
		if a.Subnet == "main" {
			mainExternals = append(mainExternals, a.Uplink+"="+a.External.String())
		}
	}
	want := "wan0=2001:db8:abcd::/64,wan1=2a02:1234:5678:9abc::/64"
	if got := strings.Join(mainExternals, ","); got != want {
		t.Errorf("main subnet externals = %s, want %s", got, want)
	}
}

func TestDesiredAssignmentsSkipsUnhealthyUplink(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:abcd::/56",
				SubnetPriority: []string{"main"}},
		},
	})

	d.useGatewayConfig(t, map[string]config.Gateway{"wan0": {Weight: 1}})
	d.state.Prefixes.Leases["wan0"] = leaseFor(t, "wan0", "2001:db8:abcd::/56")

	if assigned, _ := assignmentsFor(d.r.Npt, d.cfg.Gateways, &d.state); len(assigned) != 0 {
		t.Fatalf("assigned %d mappings for a down uplink, want none", len(assigned))
	}

	d.state.Routes.Gateways["2001:db8::1@9001"] = route.Gateway{IfName: "wan0", IfIndex: 9001, ConfigWeight: 1}

	if assigned, _ := assignmentsFor(d.r.Npt, d.cfg.Gateways, &d.state); len(assigned) != 1 {
		t.Fatalf("assigned %d mappings once the uplink is healthy, want 1", len(assigned))
	}
}

func TestDesiredAssignmentsUngatedUplink(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)
	d.useNPTv6(t, &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:dead:beef::/48",
		Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
		Uplinks: map[string]config.NPTv6Uplink{
			"tun0": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:abcd::/56",
				SubnetPriority: []string{"main"}},
		},
	})

	d.useGatewayConfig(t, map[string]config.Gateway{"wan0": {Weight: 1}})
	d.state.Prefixes.Leases["tun0"] = leaseFor(t, "tun0", "2001:db8:abcd::/56")

	if assigned, _ := assignmentsFor(d.r.Npt, d.cfg.Gateways, &d.state); len(assigned) != 1 {
		t.Fatalf("assigned %d mappings for an ungated uplink, want 1", len(assigned))
	}
}
