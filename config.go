package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the daemon configuration loaded from the JSON file passed via --config.
type Config struct {
	LogLevel          string                   `json:"log_level"`
	HashPolicy        int                      `json:"hash_policy"`
	Firewall          string                   `json:"firewall"` // "iptables" (default) or "nftables"
	RouteProto        int                      `json:"route_proto"`
	RouteTableOffset  int                      `json:"route_table_offset"`
	IptablesChain     string                   `json:"iptables_chain"`
	NftablesTable     string                   `json:"nftables_table"`
	Gateways          map[string]GatewayConfig `json:"gateways"`
	Rules             []RuleConfig             `json:"rules"`
	ReconcileInterval string                   `json:"reconcile_interval"`
}

// firewallBackend returns the configured firewall backend, defaulting to "iptables".
func (c *Config) firewallBackend() string {
	if c == nil || c.Firewall == "" {
		return "iptables"
	}
	return c.Firewall
}

// GatewayConfig holds per-interface ECMP settings.
type GatewayConfig struct {
	Weight      int           `json:"weight"`
	Description string        `json:"description"`
	Health      *HealthConfig `json:"health,omitempty"`
}

// HealthConfig describes the health-monitoring parameters for one gateway.
type HealthConfig struct {
	UnhealthyThreshold int         `json:"unhealthy_threshold"` // consecutive failures before removing
	HealthyThreshold   int         `json:"healthy_threshold"`   // consecutive successes before re-adding
	Interval           string      `json:"interval"`            // probe interval, e.g. "5s"
	Timeout            string      `json:"timeout"`             // per-probe timeout, e.g. "2s"
	Probe              ProbeConfig `json:"probe"`
}

// ProbeConfig selects a probe type and its parameters.
// Only the fields relevant to the chosen Type are used.
type ProbeConfig struct {
	Type string `json:"type"` // "icmp" (default), "http", "tcp", "dns", "exec"

	// icmp
	PayloadSize int `json:"payload,omitempty"`

	// http / https
	URL            string `json:"url,omitempty"`
	Method         string `json:"method,omitempty"`
	ExpectedStatus []int  `json:"expected_status,omitempty"`
	ExpectedBody   string `json:"expected_body,omitempty"`

	// tcp
	Host string `json:"host,omitempty"`
	Port uint16 `json:"port,omitempty"`

	// dns
	Resolver string `json:"resolver,omitempty"`
	Query    string `json:"query,omitempty"`

	// exec
	Command []string `json:"command,omitempty"`
}

// RuleConfig describes a single policy routing rule.
type RuleConfig struct {
	Priority      int    `json:"priority"`
	Gateway       string `json:"gateway"`
	MatchDstIP    string `json:"match_dst_ip,omitempty"`
	MatchDstPort  []int  `json:"match_dst_port,omitempty"`
	MatchProtocol string `json:"match_protocol,omitempty"`
}

// portRules returns only rules that have matchDstPort set.
func (c *Config) portRules() []RuleConfig {
	var out []RuleConfig
	for _, r := range c.Rules {
		if len(r.MatchDstPort) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// iptablesChain returns the mangle chain name used by the iptables backend,
// defaulting to "ROUTE-BALANCER".
func (c *Config) iptablesChain() string {
	if c == nil || c.IptablesChain == "" {
		return "ROUTE-BALANCER"
	}
	return c.IptablesChain
}

// nftablesTable returns the nftables table name, defaulting to "route-balancer".
func (c *Config) nftablesTable() string {
	if c == nil || c.NftablesTable == "" {
		return "route-balancer"
	}
	return c.NftablesTable
}

// routeTableOffset returns the base added to an interface index to derive its
// per-gateway routing table ID, defaulting to 100.
func (c *Config) routeTableOffset() int {
	if c == nil || c.RouteTableOffset == 0 {
		return 100
	}
	return c.RouteTableOffset
}

// reconcileInterval parses the configured reconcile_interval duration.
// Returns 0 when not set, which disables periodic reconciliation.
func (c *Config) reconcileInterval() time.Duration {
	if c == nil || c.ReconcileInterval == "" {
		return 0
	}
	d, err := time.ParseDuration(c.ReconcileInterval)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// routeProtoInt returns the configured routing protocol number as an int,
// defaulting to 111.
func (c *Config) routeProtoInt() int {
	if c == nil || c.RouteProto == 0 {
		return 111
	}
	return c.RouteProto
}

// routeProto returns the configured routing protocol number as a string,
// defaulting to 111.
func (c *Config) routeProto() string {
	return strconv.Itoa(c.routeProtoInt())
}

// shouldInclude reports whether an interface should participate in ECMP routing.
// When no config is loaded all interfaces are included. When a config is present
// only interfaces listed in the gateways map with a non-zero weight are included.
func (c *Config) shouldInclude(ifName string) bool {
	if c == nil || len(c.Gateways) == 0 {
		return true
	}
	gw, ok := c.Gateways[ifName]
	return ok && gw.Weight > 0
}

// weight returns the configured ECMP weight for an interface, defaulting to 1.
func (c *Config) weight(ifName string) int {
	if c == nil {
		return 1
	}
	gw, ok := c.Gateways[ifName]
	if !ok || gw.Weight == 0 {
		return 1
	}
	return gw.Weight
}

// cfg is the loaded daemon configuration; nil when no --config flag is given.
var cfg *Config

// parseFlags registers and parses CLI flags, returning the config file path.
func parseFlags() string {
	configPath := flag.String("config", "", "path to JSON configuration file")
	flag.Parse()
	return *configPath
}

// loadConfig reads and JSON-decodes the config file at path into the global cfg.
func loadConfig(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()

	var c Config
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	cfg = &c
	return nil
}
