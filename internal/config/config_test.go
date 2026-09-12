package config

import (
	"encoding/json"
	"syscall"
	"testing"
	"time"
)

func TestWireFormat(t *testing.T) {
	const doc = `{
	  "route_proto": 222,
	  "route_table_offset": 200,
	  "iptables_chain": "CHAIN",
	  "nftables_table": "tbl",
	  "reconcile_interval": "45s",
	  "ipv6_metric": 100,
	  "ipv4_ecmp": false,
	  "ipv6_ecmp": true,
	  "firewall": "nftables",
	  "gateways": {
	    "eth0": {
	      "weight": 3,
	      "health": {
	        "interval": "5s",
	        "probe":  { "type": "http", "url": "http://example.com/" },
	        "probe6": { "type": "tcp", "host": "2001:db8::1", "port": 53 }
	      }
	    }
	  },
	  "nptv6": {
	    "enable": true,
	    "proxy_ndp_timeout": "10m",
	    "proxy_ndp_max_hosts": 64
	  }
	}`

	var c Config
	if err := json.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c.SetDefaults()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"RouteProto", c.RouteProto, 222},
		{"RouteTableOffset", c.RouteTableOffset, 200},
		{"IptablesChain", c.IptablesChain, "CHAIN"},
		{"NftablesTable", c.NftablesTable, "tbl"},
		{"ReconcileInterval", time.Duration(c.ReconcileInterval), 45 * time.Second},
		{"IPv6Metric", c.IPv6Metric, 100},
		{"IPv4ECMPEnabled", c.IPv4ECMPEnabled(), false},
		{"IPv6ECMP", c.IPv6ECMP, true},
		{"Firewall", c.Firewall, "nftables"},
		{"ProxyNDPTimeout", c.ProxyNDPTimeout(), 10 * time.Minute},
		{"ProxyNDPMaxHosts", c.ProxyNDPMaxHosts(), 64},

		{"HealthInterval", time.Duration(c.Gateways["eth0"].Health.Interval), 5 * time.Second},

		{"Probe", c.Gateways["eth0"].Health.Probe.URL, "http://example.com/"},
		{"Probe6", c.Gateways["eth0"].Health.Probe6.Host, "2001:db8::1"},
		{"Probe6Port", c.Gateways["eth0"].Health.Probe6.Port, uint16(53)},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
}

func TestDefaults(t *testing.T) {
	c := Default()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"RouteProto", c.RouteProto, 111},
		{"RouteTableOffset", c.RouteTableOffset, 100},
		{"IptablesChain", c.IptablesChain, "ROUTE-BALANCER"},
		{"NftablesTable", c.NftablesTable, "route-balancer"},
		{"ReconcileInterval", time.Duration(c.ReconcileInterval), time.Duration(0)},
		{"IPv6Metric", c.IPv6Metric, 1},
		{"IPv4ECMPEnabled", c.IPv4ECMPEnabled(), true},
		{"IPv6ECMP", c.IPv6ECMP, false},
		{"Firewall", c.Firewall, "iptables"},

		{"NPTv6Enabled", c.NPTv6Enabled(), false},
		{"NPTv6Table", c.NPTv6Table(), "route-balancer-nptv6"},
		{"NPTv6NdpTable", c.NPTv6NdpTable(), "route-balancer-nptv6-ndp"},
		{"NPTv6LeakProtection", c.NPTv6LeakProtection(), false},
		{"ProxyNDPMaxHosts", c.ProxyNDPMaxHosts(), 256},
		{"ProxyNDPTimeout", c.ProxyNDPTimeout(), time.Hour},

		{"ShouldInclude", c.ShouldInclude("eth0"), true},
		{"Weight", c.Weight("eth0"), 1},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
	if got := c.PortRules(); len(got) != 0 {
		t.Errorf("PortRules() = %v, want none", got)
	}
}

func TestSetDefaultsIdempotent(t *testing.T) {
	c := &Config{RouteProto: 222, Firewall: "nftables"}
	c.SetDefaults()
	first, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	c.SetDefaults()
	second, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("second SetDefaults changed the config:\n first  %s\n second %s", first, second)
	}
}

func TestIPv6MetricRejectsNonPositive(t *testing.T) {
	for _, in := range []int{0, -5} {
		c := &Config{IPv6Metric: in}
		c.SetDefaults()
		if c.IPv6Metric != 1 {
			t.Errorf("IPv6Metric %d resolved to %d, want the default 1", in, c.IPv6Metric)
		}
	}
}

func TestDurationDecoding(t *testing.T) {
	var c Config
	err := json.Unmarshal([]byte(`{"reconcile_interval": "not-a-duration"}`), &c)
	if err == nil {
		t.Fatal("decoding an unparseable duration succeeded, want an error")
	}
	if !contains(err.Error(), "not-a-duration") {
		t.Errorf("error %q does not name the offending value", err)
	}

	for _, doc := range []string{`{}`, `{"reconcile_interval": ""}`, `{"reconcile_interval": null}`} {
		var c Config
		if err := json.Unmarshal([]byte(doc), &c); err != nil {
			t.Errorf("decode %s: %v", doc, err)
		}
		if c.ReconcileInterval != 0 {
			t.Errorf("decode %s: ReconcileInterval = %v, want 0", doc, c.ReconcileInterval)
		}
	}
}

func TestHealthDurationsAreValidated(t *testing.T) {
	for _, bad := range []string{"5", "5 s", "every 5s"} {
		doc := `{"gateways": {"eth0": {"weight": 1, "health": {"interval": "` + bad + `"}}}}`
		var c Config
		err := json.Unmarshal([]byte(doc), &c)
		if err == nil {
			t.Errorf("health interval %q decoded without complaint", bad)
			continue
		}
		if !contains(err.Error(), bad) {
			t.Errorf("health interval %q: error %q does not name the offending value", bad, err)
		}
	}

	var c Config
	doc := `{"gateways": {"eth0": {"weight": 1, "health": {"interval": "1s", "timeout": "800ms"}}}}`
	if err := json.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	h := c.Gateways["eth0"].Health
	if time.Duration(h.Interval) != time.Second {
		t.Errorf("Interval = %v, want 1s", time.Duration(h.Interval))
	}
	if time.Duration(h.Timeout) != 800*time.Millisecond {
		t.Errorf("Timeout = %v, want 800ms", time.Duration(h.Timeout))
	}
}

func TestNonPositiveDurationRejected(t *testing.T) {
	for _, doc := range []string{
		`{"reconcile_interval": "0s"}`,
		`{"reconcile_interval": "-5s"}`,
		`{"nptv6": {"proxy_ndp_timeout": "0s"}}`,
		`{"gateways": {"eth0": {"health": {"interval": "0s"}}}}`,
		`{"gateways": {"eth0": {"health": {"timeout": "-2s"}}}}`,
	} {
		var c Config
		if err := json.Unmarshal([]byte(doc), &c); err == nil {
			t.Errorf("decode %s succeeded, want an error", doc)
		} else if !contains(err.Error(), "must be positive") {
			t.Errorf("decode %s: error %q does not explain the rule", doc, err)
		}
	}
}

func TestLeakProtectionDefaultsOn(t *testing.T) {
	off := false
	on := true
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"absent", nil, true},
		{"explicit true", &on, true},
		{"explicit false", &off, false},
	}
	for _, tc := range cases {
		c := &Config{NPTv6: &NPTv6{Enable: true, LeakProtection: tc.in}}
		if got := c.NPTv6LeakProtection(); got != tc.want {
			t.Errorf("%s: NPTv6LeakProtection() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestProbeForResolvesEachFamily(t *testing.T) {
	icmp := Probe{Type: "icmp"}
	exec := Probe{Type: "exec", Command: []string{"true"}}
	web := Probe{Type: "http", URL: "http://198.51.100.1/"}
	tcp6 := Probe{Type: "tcp", Host: "2001:db8::1", Port: 53}
	off := Probe{Type: ProbeNone}

	for _, tc := range []struct {
		name     string
		health   Health
		family   int
		want     ProbeDecision
		wantType string
	}{
		{
			name:     "icmp serves both, aiming at whatever nexthop the target has",
			health:   Health{Probe: icmp},
			family:   syscall.AF_INET6,
			want:     ProbeRun,
			wantType: "icmp",
		},
		{
			name:     "exec serves both, having no destination to be wrong about",
			health:   Health{Probe: exec},
			family:   syscall.AF_INET6,
			want:     ProbeRun,
			wantType: "exec",
		},
		{
			name:   "an http URL is not reused for v6",
			health: Health{Probe: web},
			family: syscall.AF_INET6,
			want:   ProbeUnstated,
		},
		{
			name:     "probe6 answers where the base probe cannot",
			health:   Health{Probe: web, Probe6: &tcp6},
			family:   syscall.AF_INET6,
			want:     ProbeRun,
			wantType: "tcp",
		},
		{
			name:     "v4 keeps the base probe whatever probe6 says",
			health:   Health{Probe: web, Probe6: &tcp6},
			family:   syscall.AF_INET,
			want:     ProbeRun,
			wantType: "http",
		},
		{
			name:   "none declines, and is an answer rather than a gap",
			health: Health{Probe: icmp, Probe6: &off},
			family: syscall.AF_INET6,
			want:   ProbeOff,
		},
		{
			name:   "none declines v4 too",
			health: Health{Probe: off, Probe6: &tcp6},
			family: syscall.AF_INET,
			want:   ProbeOff,
		},
		{
			name:     "and the v6 half of that uplink still runs",
			health:   Health{Probe: off, Probe6: &tcp6},
			family:   syscall.AF_INET6,
			want:     ProbeRun,
			wantType: "tcp",
		},
		{
			name:     "an unset probe is icmp, and serves either family",
			health:   Health{},
			family:   syscall.AF_INET6,
			want:     ProbeRun,
			wantType: "",
		},
	} {
		got, decision := tc.health.ProbeFor(tc.family)
		if decision != tc.want {
			t.Errorf("%s: decision = %v, want %v", tc.name, decision, tc.want)
			continue
		}
		if decision == ProbeRun && got.Type != tc.wantType {
			t.Errorf("%s: probe type = %q, want %q", tc.name, got.Type, tc.wantType)
		}
	}
}

func TestRuleMatchFamilyDefaultsToIPv4(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rule   Rule
		wantV4 bool
		wantV6 bool
	}{
		{
			name:   "absent, as every rule written before the field existed",
			rule:   Rule{Gateway: "eth1", MatchDstPort: []int{443}},
			wantV4: true,
		},
		{
			name:   "explicit ipv4",
			rule:   Rule{Gateway: "eth1", MatchDstPort: []int{443}, MatchFamily: RuleFamilyIPv4},
			wantV4: true,
		},
		{
			name:   "ipv6 alone does not steer IPv4",
			rule:   Rule{Gateway: "eth1", MatchDstPort: []int{443}, MatchFamily: RuleFamilyIPv6},
			wantV6: true,
		},
		{
			name:   "both",
			rule:   Rule{Gateway: "eth1", MatchDstPort: []int{443}, MatchFamily: RuleFamilyBoth},
			wantV4: true,
			wantV6: true,
		},
		{
			name: "a selector nobody understands steers nothing",
			rule: Rule{Gateway: "eth1", MatchDstPort: []int{443}, MatchFamily: "inet"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Matches(syscall.AF_INET); got != tc.wantV4 {
				t.Errorf("Matches(v4) = %v, want %v", got, tc.wantV4)
			}
			if got := tc.rule.Matches(syscall.AF_INET6); got != tc.wantV6 {
				t.Errorf("Matches(v6) = %v, want %v", got, tc.wantV6)
			}
		})
	}
}

func TestRuleKnownFamily(t *testing.T) {
	for _, family := range []string{"", RuleFamilyIPv4, RuleFamilyIPv6, RuleFamilyBoth} {
		if !(Rule{MatchFamily: family}).KnownFamily() {
			t.Errorf("match_family %q was rejected", family)
		}
	}
	for _, family := range []string{"inet", "v6", "IPv6", "6"} {
		if (Rule{MatchFamily: family}).KnownFamily() {
			t.Errorf("match_family %q was accepted", family)
		}
	}
}

func TestPortRulesForSplitsByFamily(t *testing.T) {
	c := &Config{Rules: []Rule{
		{Gateway: "eth0", MatchDstPort: []int{25}},
		{Gateway: "eth1", MatchDstPort: []int{443}, MatchFamily: RuleFamilyIPv6},
		{Gateway: "eth1", MatchDstPort: []int{53}, MatchFamily: RuleFamilyBoth},

		{Gateway: "eth0", MatchFamily: RuleFamilyBoth},
	}}

	v4 := c.PortRulesFor(syscall.AF_INET)
	if len(v4) != 2 || v4[0].MatchDstPort[0] != 25 || v4[1].MatchDstPort[0] != 53 {
		t.Errorf("PortRulesFor(v4) = %v, want the port-25 and port-53 rules", v4)
	}

	v6 := c.PortRulesFor(syscall.AF_INET6)
	if len(v6) != 2 || v6[0].MatchDstPort[0] != 443 || v6[1].MatchDstPort[0] != 53 {
		t.Errorf("PortRulesFor(v6) = %v, want the port-443 and port-53 rules", v6)
	}

	if len(c.PortRules()) != 3 {
		t.Errorf("PortRules() = %v, want every port rule regardless of family", c.PortRules())
	}
}

func TestMatchFamilyWireFormat(t *testing.T) {
	var c Config
	const doc = `{"rules": [{"gateway": "eth1", "match_dst_port": [443], "match_family": "ipv6"}]}`
	if err := json.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := c.Rules[0].MatchFamilyName(); got != RuleFamilyIPv6 {
		t.Errorf("MatchFamilyName() = %q, want %q", got, RuleFamilyIPv6)
	}

	out, err := json.Marshal(Rule{Gateway: "eth1", MatchDstPort: []int{443}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if contains(string(out), "match_family") {
		t.Errorf("a rule that named no family round-tripped as %s, want the key omitted", out)
	}
}
