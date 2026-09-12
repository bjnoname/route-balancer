package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"
)

const (
	SourceRoute  = "route"
	SourceRA     = "ra"
	SourceStatic = "static"
)

const (
	RuleFamilyIPv4 = "ipv4"
	RuleFamilyIPv6 = "ipv6"
	RuleFamilyBoth = "both"
)

const (
	defaultRouteProto       = 111
	defaultRouteTableOffset = 100
	defaultIptablesChain    = "ROUTE-BALANCER"
	defaultNftablesTable    = "route-balancer"
	defaultFirewall         = "iptables"
	defaultIPv6Metric       = 1
	defaultNPTv6Table       = "route-balancer-nptv6"
	defaultProxyNDPMaxHosts = 256
	defaultProxyNDPTimeout  = time.Hour
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"45s\": %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if parsed <= 0 {
		return fmt.Errorf("duration %q must be positive; omit the field to leave it unset", s)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return json.Marshal("")
	}
	return json.Marshal(time.Duration(d).String())
}

type Config struct {
	LogLevel string `json:"log_level"`

	IPv4ECMP *bool `json:"ipv4_ecmp,omitempty"`

	IPv6ECMP bool `json:"ipv6_ecmp,omitempty"`

	IPv6Metric int `json:"ipv6_metric,omitempty"`

	Firewall         string             `json:"firewall"`
	RouteProto       int                `json:"route_proto"`
	RouteTableOffset int                `json:"route_table_offset"`
	IptablesChain    string             `json:"iptables_chain"`
	NftablesTable    string             `json:"nftables_table"`
	Gateways         map[string]Gateway `json:"gateways"`
	Rules            []Rule             `json:"rules"`

	ReconcileInterval Duration `json:"reconcile_interval"`

	NPTv6 *NPTv6 `json:"nptv6,omitempty"`
}

type NPTv6 struct {
	Enable bool `json:"enable"`

	InternalPrefix string `json:"internal_prefix"`

	AllowNonULA bool `json:"allow_non_ula,omitempty"`

	NftablesTable string `json:"nftables_table,omitempty"`

	LeakProtection *bool `json:"leak_protection,omitempty"`

	ProxyNDPMaxHosts int `json:"proxy_ndp_max_hosts,omitempty"`

	ProxyNDPTimeout Duration `json:"proxy_ndp_timeout,omitempty"`

	Subnets map[string]NPTv6Subnet `json:"subnets"`
	Uplinks map[string]NPTv6Uplink `json:"uplinks"`
}

type NPTv6Subnet struct {
	Prefix      string `json:"prefix"`
	Description string `json:"description,omitempty"`
}

type NPTv6Uplink struct {
	PrefixSource string `json:"prefix_source,omitempty"`

	MatchPrefix string `json:"match_prefix,omitempty"`

	StaticPrefix string `json:"static_prefix,omitempty"`

	SubnetPriority []string `json:"subnet_priority"`

	ProxyNDP *bool `json:"proxy_ndp,omitempty"`

	AllowInboundConnections bool `json:"allow_inbound_connections,omitempty"`
}

type Gateway struct {
	Weight      int     `json:"weight"`
	Description string  `json:"description"`
	Health      *Health `json:"health,omitempty"`
}

type Health struct {
	UnhealthyThreshold int      `json:"unhealthy_threshold"`
	HealthyThreshold   int      `json:"healthy_threshold"`
	Interval           Duration `json:"interval"`
	Timeout            Duration `json:"timeout"`
	Probe              Probe    `json:"probe"`

	Probe6 *Probe `json:"probe6,omitempty"`
}

const ProbeNone = "none"

type Probe struct {
	Type string `json:"type"`

	PayloadSize int `json:"payload,omitempty"`

	URL            string `json:"url,omitempty"`
	Method         string `json:"method,omitempty"`
	ExpectedStatus []int  `json:"expected_status,omitempty"`
	ExpectedBody   string `json:"expected_body,omitempty"`

	Host string `json:"host,omitempty"`
	Port uint16 `json:"port,omitempty"`

	Resolver string `json:"resolver,omitempty"`
	Query    string `json:"query,omitempty"`

	Command []string `json:"command,omitempty"`
}

type Rule struct {
	Gateway       string `json:"gateway"`
	MatchDstPort  []int  `json:"match_dst_port,omitempty"`
	MatchProtocol string `json:"match_protocol,omitempty"`
	MatchFamily   string `json:"match_family,omitempty"`
}

func (r Rule) MatchFamilyName() string {
	if r.MatchFamily == "" {
		return RuleFamilyIPv4
	}
	return r.MatchFamily
}

func (r Rule) KnownFamily() bool {
	switch r.MatchFamilyName() {
	case RuleFamilyIPv4, RuleFamilyIPv6, RuleFamilyBoth:
		return true
	}
	return false
}

func (r Rule) Matches(family int) bool {
	switch r.MatchFamilyName() {
	case RuleFamilyBoth:
		return family == syscall.AF_INET || family == syscall.AF_INET6
	case RuleFamilyIPv4:
		return family == syscall.AF_INET
	case RuleFamilyIPv6:
		return family == syscall.AF_INET6
	}
	return false
}

func Default() *Config {
	c := &Config{}
	c.SetDefaults()
	return c
}

func (c *Config) SetDefaults() {
	if c.RouteProto == 0 {
		c.RouteProto = defaultRouteProto
	}
	if c.RouteTableOffset == 0 {
		c.RouteTableOffset = defaultRouteTableOffset
	}
	if c.IptablesChain == "" {
		c.IptablesChain = defaultIptablesChain
	}
	if c.NftablesTable == "" {
		c.NftablesTable = defaultNftablesTable
	}
	if c.Firewall == "" {
		c.Firewall = defaultFirewall
	}
	if c.IPv6Metric <= 0 {
		c.IPv6Metric = defaultIPv6Metric
	}
}

func (c *Config) IPv4ECMPEnabled() bool {
	return c.IPv4ECMP == nil || *c.IPv4ECMP
}

func ParseFlags() string {
	configPath := flag.String("config", "", "path to JSON configuration file")
	flag.Parse()
	return *configPath
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()

	var c Config
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	c.SetDefaults()
	return &c, nil
}

type ProbeDecision int

const (
	ProbeRun ProbeDecision = iota
	ProbeOff
	ProbeUnstated
)

func (h Health) ProbeFor(family int) (Probe, ProbeDecision) {
	p := h.Probe
	if family == syscall.AF_INET6 {
		switch {
		case h.Probe6 != nil:
			p = *h.Probe6
		case namesADestination(h.Probe.Type):
			return Probe{}, ProbeUnstated
		}
	}
	if p.Type == ProbeNone {
		return Probe{}, ProbeOff
	}
	return p, ProbeRun
}

func namesADestination(probeType string) bool {
	switch probeType {
	case "http", "https", "tcp", "dns":
		return true
	default:
		return false
	}
}

func (c *Config) NPTv6Enabled() bool {
	return c.NPTv6 != nil && c.NPTv6.Enable
}

func (c *Config) NPTv6Table() string {
	if c.NPTv6 == nil || c.NPTv6.NftablesTable == "" {
		return defaultNPTv6Table
	}
	return c.NPTv6.NftablesTable
}

func (c *Config) NPTv6LeakProtection() bool {
	return c.NPTv6Enabled() && (c.NPTv6.LeakProtection == nil || *c.NPTv6.LeakProtection)
}

func (c *Config) NPTv6NdpTable() string {
	return c.NPTv6Table() + "-ndp"
}

func (u NPTv6Uplink) PrefixSourceName() string {
	if u.PrefixSource == "" {
		return SourceRoute
	}
	return u.PrefixSource
}

func (u NPTv6Uplink) ProxyNDPEnabled() bool {
	if u.ProxyNDP != nil {
		return *u.ProxyNDP
	}
	return u.PrefixSourceName() == SourceRA
}

func (c *Config) ProxyNDPMaxHosts() int {
	if c.NPTv6 == nil || c.NPTv6.ProxyNDPMaxHosts <= 0 {
		return defaultProxyNDPMaxHosts
	}
	return c.NPTv6.ProxyNDPMaxHosts
}

func (c *Config) ProxyNDPTimeout() time.Duration {
	if c.NPTv6 == nil || c.NPTv6.ProxyNDPTimeout <= 0 {
		return defaultProxyNDPTimeout
	}
	return time.Duration(c.NPTv6.ProxyNDPTimeout)
}

func (c *Config) PortRules() []Rule {
	var out []Rule
	for _, r := range c.Rules {
		if len(r.MatchDstPort) > 0 {
			out = append(out, r)
		}
	}
	return out
}

func (c *Config) PortRulesFor(family int) []Rule {
	var out []Rule
	for _, r := range c.PortRules() {
		if r.Matches(family) {
			out = append(out, r)
		}
	}
	return out
}

func (c *Config) ShouldInclude(ifName string) bool {
	if len(c.Gateways) == 0 {
		return true
	}
	gw, ok := c.Gateways[ifName]
	return ok && gw.Weight > 0
}

func (c *Config) Weight(ifName string) int {
	gw, ok := c.Gateways[ifName]
	if !ok || gw.Weight == 0 {
		return 1
	}
	return gw.Weight
}
