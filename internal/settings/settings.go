package settings

import (
	"errors"
	"fmt"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/firewall"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/nptv6"
	"github.com/bjnoname/route-balancer/internal/probe"
	"github.com/bjnoname/route-balancer/internal/route"
)

type Resolved struct {
	Cfg *config.Config

	Rt  route.Options
	Fw  firewall.Options
	Npt nptv6.Options
	Nd  ndp.Options
	Gws map[string]config.Gateway
}

func Resolve(c *config.Config) (Resolved, error) {
	npt, err := nptv6.Resolve(c)
	if err != nil {
		return Resolved{}, fmt.Errorf("nptv6 config: %w", err)
	}

	r := Resolved{
		Cfg: c,
		Rt:  routing(c, npt),
		Fw:  firewalls(c),
		Npt: npt,
		Nd:  ndpOpts(c, npt),
		Gws: c.Gateways,
	}
	if err := r.validate(); err != nil {
		return Resolved{}, err
	}
	return r, nil
}

const firewallNftables = "nftables"

func (r Resolved) validate() error {
	if err := r.managesSomething(); err != nil {
		return err
	}
	if b := r.Fw.Backend; b != "iptables" && b != firewallNftables {
		return fmt.Errorf("unknown firewall backend %q: must be \"iptables\" or \"nftables\"", b)
	}
	if err := r.validateTableOffset(); err != nil {
		return err
	}
	if err := r.validateProbes(); err != nil {
		return err
	}
	if err := r.validateNftTables(); err != nil {
		return err
	}
	return r.validateRules()
}

func (r Resolved) validateNftTables() error {
	claimed := map[string]string{}
	claim := func(setting, name string, used bool) error {
		if !used || name == "" {
			return nil
		}
		if prev, dup := claimed[name]; dup {
			return fmt.Errorf("%s and %s both name the nftables table %q: the two rulesets "+
				"are kept apart by name, so sharing one means each overwrites the other every "+
				"pass; give them different names", prev, setting, name)
		}
		claimed[name] = setting
		return nil
	}

	if err := claim("nftables_table", r.Fw.NftablesTable, r.Fw.Backend == firewallNftables); err != nil {
		return err
	}
	if err := claim("nptv6.nftables_table", r.Npt.Table, r.Npt.Enabled); err != nil {
		return err
	}
	return claim("the proxy NDP learning table", r.Nd.Table, r.Npt.Enabled && r.Nd.Enabled())
}

func (r Resolved) validateProbes() error {
	for _, name := range command.SortedKeys(r.Gws) {
		health := r.Gws[name].Health
		if health == nil {
			continue
		}
		for _, family := range r.Rt.Observed {
			pcfg, decision := health.ProbeFor(family)
			if decision != config.ProbeRun {
				continue
			}
			if err := probe.Validate(pcfg); err != nil {
				return fmt.Errorf("gateway %q %s health probe: %w",
					name, route.FamilyName(family), err)
			}
		}
	}
	return nil
}

const maxTableOffset = route.ReservedTable - 2

func (r Resolved) validateTableOffset() error {

	o := r.Rt.TableOffset
	if o < 0 {
		return fmt.Errorf("route_table_offset %d must not be negative", o)
	}
	if o > maxTableOffset {
		return fmt.Errorf("route_table_offset %d leaves no room for any interface: a gateway's "+
			"routing table is the offset plus its interface index, %d, %d and %d are the kernel's "+
			"own default, main and local tables, and the lowest index there is is 1, so the offset "+
			"can be at most %d", o, route.ReservedTable, route.ReservedTable+1,
			route.ReservedTable+2, maxTableOffset)
	}
	return nil
}

func (r Resolved) managesSomething() error {
	if len(r.Rt.ManagedFamilies()) > 0 || len(r.Fw.Rules)+len(r.Fw.Rules6) > 0 || r.Npt.Enabled {
		return nil
	}
	return errors.New("nothing to manage: ipv4_ecmp and ipv6_ecmp are both false, " +
		"no rules name a destination port, and nptv6 is not enabled")
}

func (r Resolved) validateRules() error {
	for _, rule := range r.Cfg.Rules {
		if !rule.KnownFamily() {
			return fmt.Errorf("rule for gateway %q: unknown match_family %q (want %q, %q or %q)",
				rule.Gateway, rule.MatchFamily,
				config.RuleFamilyIPv4, config.RuleFamilyIPv6, config.RuleFamilyBoth)
		}
	}

	if len(r.Fw.Rules6) == 0 {
		return nil
	}

	if !r.Npt.Enabled {
		return errors.New("port rules for IPv6 require nptv6: with nothing rewriting the " +
			"source per uplink, a forwarded flow pinned to a second uplink leaves carrying " +
			"the first one's prefix, and that ISP drops it. Enable nptv6, or set " +
			"match_family to \"" + config.RuleFamilyIPv4 + "\"")
	}
	if r.Fw.Backend != firewallNftables {
		return fmt.Errorf("port rules for IPv6 require firewall %q, not %q: the IPv6 marks "+
			"have no ip6tables backend, and nptv6 already requires nftables",
			firewallNftables, r.Fw.Backend)
	}
	for _, rule := range r.Fw.Rules6 {
		if !r.Npt.HasUplink(rule.Gateway) {
			return fmt.Errorf("rule for gateway %q steers IPv6, but %q is not an nptv6 uplink, "+
				"so nothing would translate what it steers there", rule.Gateway, rule.Gateway)
		}
	}
	return nil
}

func routing(cfg *config.Config, npt nptv6.Options) route.Options {
	opts := route.Options{
		Proto:       cfg.RouteProto,
		TableOffset: cfg.RouteTableOffset,
		IPv6Metric:  cfg.IPv6Metric,
	}

	v4, v6 := cfg.IPv4ECMPEnabled(), cfg.IPv6ECMP
	rules4 := cfg.PortRulesFor(route.FamilyV4)
	rules6 := cfg.PortRulesFor(route.FamilyV6)

	if v4 {
		opts.Managed = append(opts.Managed, route.FamilyV4)
	}
	if v6 {
		opts.Managed = append(opts.Managed, route.FamilyV6)
	}

	if len(rules4) > 0 {
		opts.Marked = append(opts.Marked, route.FamilyV4)
	}
	if len(rules6) > 0 {
		opts.Marked = append(opts.Marked, route.FamilyV6)
	}

	if v4 || len(rules4) > 0 {
		opts.Observed = append(opts.Observed, route.FamilyV4)
	}
	if v6 || npt.Enabled || len(rules6) > 0 {
		opts.Observed = append(opts.Observed, route.FamilyV6)
	}

	return opts
}

func firewalls(cfg *config.Config) firewall.Options {
	return firewall.Options{
		Backend:       cfg.Firewall,
		IptablesChain: cfg.IptablesChain,
		NftablesTable: cfg.NftablesTable,
		Gateways:      command.SortedKeys(cfg.Gateways),
		Rules:         cfg.PortRulesFor(route.FamilyV4),
		Rules6:        cfg.PortRulesFor(route.FamilyV6),
	}
}

func ndpOpts(cfg *config.Config, npt nptv6.Options) ndp.Options {
	o := ndp.Options{
		Table:    cfg.NPTv6NdpTable(),
		Proto:    routing(cfg, npt).ProtoString(),
		MaxHosts: cfg.ProxyNDPMaxHosts(),
		Timeout:  cfg.ProxyNDPTimeout(),
	}
	if !npt.Enabled {
		return o
	}

	for _, u := range npt.Uplinks {
		if !cfg.NPTv6.Uplinks[u.Name].ProxyNDPEnabled() {
			continue
		}
		up := ndp.Uplink{Name: u.Name}
		for _, sub := range u.SubnetPriority {
			if p, ok := npt.Subnets[sub]; ok {
				up.Subnets = append(up.Subnets, ndp.Subnet{Name: sub, Prefix: p})
			}
		}
		o.Uplinks = append(o.Uplinks, up)
	}
	return o
}
