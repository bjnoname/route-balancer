package settings

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/nptv6"
	"github.com/bjnoname/route-balancer/internal/route"
)

func off() *bool { b := false; return &b }

func familyNames(families []int) []string {
	out := make([]string, 0, len(families))
	for _, f := range families {
		out = append(out, route.FamilyName(f))
	}
	return out
}

func TestRoutingResolvesTheFamilySets(t *testing.T) {
	t.Parallel()

	portRule := []config.Rule{{Gateway: "eth0", MatchDstPort: []int{443}}}

	npt := &config.NPTv6{
		Enable:         true,
		InternalPrefix: "fd00:1234::/48",
		Subnets:        map[string]config.NPTv6Subnet{"lan": {Prefix: "fd00:1234::/64"}},
		Uplinks:        map[string]config.NPTv6Uplink{"eth0": {SubnetPriority: []string{"lan"}}},
	}

	cases := []struct {
		name              string
		cfg               config.Config
		managed, observed []string
	}{
		{"by default only v4 is managed", config.Config{}, []string{"v4"}, []string{"v4"}},
		{"ipv6_ecmp adds v6 to both", config.Config{IPv6ECMP: true},
			[]string{"v4", "v6"}, []string{"v4", "v6"}},
		{"nptv6 observes v6 without managing it", config.Config{NPTv6: npt},
			[]string{"v4"}, []string{"v4", "v6"}},
		{"ipv4_ecmp off with nothing needing v4 drops it entirely",
			config.Config{IPv4ECMP: off(), IPv6ECMP: true},
			[]string{"v6"}, []string{"v6"}},
		{"ipv4_ecmp off still observes v4 for port rules",
			config.Config{IPv4ECMP: off(), IPv6ECMP: true, Rules: portRule},
			[]string{"v6"}, []string{"v4", "v6"}},
		{"a rule with no port does not hold v4 open",
			config.Config{IPv4ECMP: off(), IPv6ECMP: true, Rules: []config.Rule{{Gateway: "eth0"}}},
			[]string{"v6"}, []string{"v6"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.SetDefaults()
			opts, err := nptv6.Resolve(&cfg)
			if err != nil {
				t.Fatalf("nptv6.Resolve: %v", err)
			}
			rt := routing(&cfg, opts)

			if got := familyNames(rt.ManagedFamilies()); !slices.Equal(got, tc.managed) {
				t.Errorf("managed = %v, want %v", got, tc.managed)
			}
			if got := familyNames(rt.ObservedFamilies()); !slices.Equal(got, tc.observed) {
				t.Errorf("observed = %v, want %v", got, tc.observed)
			}
		})
	}
}

func TestAConfigThatManagesNothingIsRejected(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.IPv4ECMP = off()

	if _, err := Resolve(cfg); err == nil {
		t.Fatal("Resolve accepted a config with no family, no port rule and no nptv6")
	}

	cfg.Rules = []config.Rule{{Gateway: "eth0", MatchDstPort: []int{443}}}
	if _, err := Resolve(cfg); err != nil {
		t.Errorf("Resolve rejected a port-rules-only config: %v", err)
	}
}

func TestResolveRejectsATableOffsetWithNoRoomLeft(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		offset  int
		wantErr bool
	}{
		{offset: 1, wantErr: false},
		{offset: 100, wantErr: false},
		{offset: 251, wantErr: false},
		{offset: 252, wantErr: true},
		{offset: 300, wantErr: true},
		{offset: -1, wantErr: true},
	} {
		c := config.Default()
		c.RouteTableOffset = tc.offset
		c.Gateways = map[string]config.Gateway{"eth0": {Weight: 1}}

		_, err := Resolve(c)
		if (err != nil) != tc.wantErr {
			t.Errorf("Resolve(route_table_offset=%d) error = %v, want error: %v",
				tc.offset, err, tc.wantErr)
		}
	}
}

func TestResolveRefusesAProbeThatCouldNeverPass(t *testing.T) {
	t.Parallel()

	withProbe := func(h config.Health) *config.Config {
		c := config.Default()
		c.Gateways = map[string]config.Gateway{"eth0": {Weight: 1, Health: &h}}
		return c
	}
	none := config.Probe{Type: config.ProbeNone}

	for _, tc := range []struct {
		name    string
		health  config.Health
		wantErr bool
	}{
		{
			name:   "a complete probe is fine",
			health: config.Health{Probe: config.Probe{Type: "http", URL: "http://example.com/"}},
		},
		{
			name:    "an http probe with no url is not",
			health:  config.Health{Probe: config.Probe{Type: "http"}},
			wantErr: true,
		},
		{

			name:   "an unobserved family is not judged",
			health: config.Health{Probe: config.Probe{Type: "icmp"}, Probe6: &config.Probe{Type: "tcp"}},
		},
		{
			name:   "type none is exempt, it runs nothing on purpose",
			health: config.Health{Probe: config.Probe{Type: "icmp"}, Probe6: &none},
		},
	} {
		_, err := Resolve(withProbe(tc.health))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: Resolve error = %v, want error: %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestResolveJudgesTheIPv6ProbeOnceIPv6IsObserved(t *testing.T) {
	t.Parallel()

	c := config.Default()
	c.IPv6ECMP = true
	c.Gateways = map[string]config.Gateway{"eth0": {Weight: 1, Health: &config.Health{
		Probe:  config.Probe{Type: "icmp"},
		Probe6: &config.Probe{Type: "tcp", Host: "2001:db8::1"},
	}}}

	if _, err := Resolve(c); err == nil {
		t.Error("Resolve accepted a v6 probe with no port while ipv6_ecmp observes v6")
	}
}

func nptConfig() *config.Config {
	return &config.Config{
		Firewall: "nftables",
		Gateways: map[string]config.Gateway{"wan0": {Weight: 1}},
		NPTv6: &config.NPTv6{
			Enable:         true,
			InternalPrefix: "fd00:dead:beef::/48",
			Subnets:        map[string]config.NPTv6Subnet{"main": {Prefix: "fd00:dead:beef:1::/64"}},
			Uplinks: map[string]config.NPTv6Uplink{
				"wan0": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:a000::/56",
					SubnetPriority: []string{"main"}},
			},
		},
	}
}

func TestTwoSettingsMayNotNameTheSameNftablesTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		with func(*config.Config)
		want string
	}{
		{
			name: "the mangle table and the nptv6 table",
			with: func(c *config.Config) {
				c.NftablesTable = "shared"
				c.NPTv6.NftablesTable = "shared"
			},
			want: "nftables_table",
		},
		{
			name: "the mangle table and the proxy NDP learning table",
			with: func(c *config.Config) {
				on := true
				u := c.NPTv6.Uplinks["wan0"]
				u.ProxyNDP = &on
				c.NPTv6.Uplinks["wan0"] = u
				c.NPTv6.NftablesTable = "shared"
				c.NftablesTable = "shared-ndp"
			},
			want: "learning table",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := nptConfig()
			tc.with(c)
			c.SetDefaults()

			_, err := Resolve(c)
			if err == nil {
				t.Fatalf("Resolve accepted two settings naming one table")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Resolve said %q, which does not name %q", err, tc.want)
			}
		})
	}
}

func TestInboundDoesNotReachProxyNdp(t *testing.T) {
	t.Parallel()

	resolve := func(inbound bool) (nptv6.Options, ndp.Options) {
		t.Helper()
		c := nptConfig()
		on := true
		u := c.NPTv6.Uplinks["wan0"]
		u.ProxyNDP = &on
		u.AllowInboundConnections = inbound
		c.NPTv6.Uplinks["wan0"] = u
		c.SetDefaults()

		r, err := Resolve(c)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		return r.Npt, r.Nd
	}

	closedNpt, closedNd := resolve(false)
	openNpt, openNd := resolve(true)

	if !reflect.DeepEqual(closedNd, openNd) {
		t.Errorf("proxy NDP options follow the inbound setting:\nclosed %+v\nopen   %+v", closedNd, openNd)
	}
	if closedNpt.Uplinks[0].AllowInbound || !openNpt.Uplinks[0].AllowInbound {
		t.Errorf("the setting did not reach nptv6.Options at all: closed %+v open %+v",
			closedNpt.Uplinks[0], openNpt.Uplinks[0])
	}
}

func TestDistinctNftablesTablesAreAccepted(t *testing.T) {
	t.Parallel()

	c := nptConfig()
	c.SetDefaults()
	if _, err := Resolve(c); err != nil {
		t.Errorf("Resolve rejected the default table names: %v", err)
	}
}
