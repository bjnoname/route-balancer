{ config, lib, pkgs, ... }:

with lib;

let
  cfg = config.services.route-balancer;

  # A Go duration string the daemon will accept. config.Duration rejects any
  # non-positive value outright, so "0s" has to fail here rather than at
  # startup: every component is unsigned, so the total is positive exactly
  # when some digit in the string is not a zero.
  goDuration =
    let shape = types.strMatching "^([0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$";
    in types.addCheck shape (s: builtins.match ".*[1-9].*" s != null) // {
      description = "positive Go duration string";
    };

  probeModule = types.submodule {
    options = {
      type = mkOption {
        type = types.enum [ "icmp" "http" "https" "tcp" "dns" "exec" "none" ];
        default = "icmp";
        description = ''
          Health probe type:
            icmp  — ICMP echo (layer 3 reachability)
            http  — HTTP/HTTPS request (layer 7, captive portal detection)
            tcp   — TCP connect (lightweight layer 4 check)
            dns   — DNS query via a specific resolver
            exec  — arbitrary command; exit 0 = healthy
            none  — do not probe this address family at all

          "none" runs no probe and starts no monitor, so the family is treated
          as healthy and keeps whatever route it has. It is how an uplink says
          it carries a family untested on purpose — probe6.type = "none" beside
          a real probe — or that it has only one family to test, with
          probe.type = "none" beside a real probe6.
        '';
      };

      payload = mkOption {
        type = types.ints.between 0 65535;
        default = 56;
        description = "ICMP echo payload size in bytes (icmp probe).";
      };

      url = mkOption {
        type = types.str;
        default = "";
        example = "http://detectportal.firefox.com/success.txt";
        description = "URL to request (http/https probe).";
      };

      method = mkOption {
        type = types.str;
        default = "GET";
        description = "HTTP method (http/https probe).";
      };

      expectedStatus = mkOption {
        type = types.listOf types.int;
        default = [ 200 ];
        description = "Acceptable HTTP response status codes (http/https probe).";
      };

      expectedBody = mkOption {
        type = types.str;
        default = "";
        description = "Required response body substring; empty = skip check (http/https probe).";
      };

      host = mkOption {
        type = types.str;
        default = "";
        example = "8.8.8.8";
        description = "Host to connect to (tcp probe).";
      };

      port = mkOption {
        type = types.ints.between 0 65535;
        default = 0;
        example = 80;
        description = "TCP port to connect to (tcp probe).";
      };

      resolver = mkOption {
        type = types.str;
        default = "";
        example = "8.8.8.8:53";
        description = ''
          DNS resolver address to query (dns probe): host, or host:port when it
          is not 53. An IPv6 literal may be written bare or bracketed —
          "2001:4860:4860::8888" and "[2001:4860:4860::8888]:53" are both
          accepted.
        '';
      };

      query = mkOption {
        type = types.str;
        default = "example.com";
        description = "Domain name to resolve (dns probe).";
      };

      command = mkOption {
        type = types.listOf types.str;
        default = [ ];
        example = [ "curl" "--interface" "eth0" "-sf" "https://example.com/" ];
        description = "Command to run; exit 0 = healthy (exec probe).";
      };
    };
  };

  healthModule = types.submodule {
    options = {
      unhealthyThreshold = mkOption {
        type = types.ints.positive;
        default = 3;
        description = "Consecutive probe failures before removing the gateway from ECMP.";
      };

      healthyThreshold = mkOption {
        type = types.ints.positive;
        default = 5;
        description = "Consecutive probe successes before re-adding the gateway to ECMP.";
      };

      interval = mkOption {
        type = goDuration;
        default = "5s";
        example = "10s";
        description = "How often to run the probe (Go duration string).";
      };

      timeout = mkOption {
        type = goDuration;
        default = "2s";
        example = "3s";
        description = "Per-probe deadline (Go duration string).";
      };

      probe = mkOption {
        type = probeModule;
        default = { };
        description = ''
          Probe configuration for IPv4. Defaults to an ICMP echo probe.

          Also serves IPv6 when probe6 is unset and this probe names no
          destination of its own — an icmp probe aims at whatever nexthop it is
          handed and an exec probe ignores the target entirely, so either
          measures either family. An http, tcp or dns probe carries an address
          or URL belonging to exactly one, and is not reused for the other.
        '';
      };

      probe6 = mkOption {
        type = types.nullOr probeModule;
        default = null;
        example = { type = "tcp"; host = "2001:4860:4860::8888"; port = 53; };
        description = ''
          Probe configuration for IPv6, when it differs from probe.

          Each family is measured separately and the verdicts are applied
          separately: the IPv4 verdict decides the IPv4 default route, the IPv6
          verdict decides the IPv6 default route and whether NPTv6 keeps
          translating through this uplink.

          When null, IPv6 falls back to probe if probe can serve it. Where it
          cannot — an http, tcp or dns probe, whose destination names IPv4 —
          IPv6 is left unmeasured rather than measured wrongly, and the daemon
          warns at startup. Set this, or set probe6.type = "none" to say the
          omission is deliberate.
        '';
      };
    };
  };

  gatewayModule = types.submodule {
    options = {
      weight = mkOption {
        type = types.ints.between 0 100;
        default = 1;
        description = ''
          ECMP weight for this gateway. Higher values receive proportionally
          more traffic. Set to 0 to suppress this gateway from ECMP even if
          its route is present in the kernel routing table.
        '';
      };

      description = mkOption {
        type = types.str;
        default = "";
        description = "Human-readable description (e.g. 'Primary ISP fibre').";
      };

      health = mkOption {
        type = types.nullOr healthModule;
        default = null;
        description = ''
          Health monitoring configuration.  When set, the daemon probes the
          gateway on every interval and removes it from ECMP after
          unhealthyThreshold consecutive failures, re-adding it after
          healthyThreshold consecutive successes.

          All probe types bind their socket to the gateway's interface via
          SO_BINDTODEVICE, ensuring probes travel through the gateway under
          test and not the current default route.

          When null (the default) the gateway is always considered healthy and
          its inclusion in ECMP is driven solely by the kernel routing table.
        '';
      };
    };
  };

  ruleModule = types.submodule {
    options = {
      matchDstPort = mkOption {
        type = types.nullOr (types.listOf types.port);
        default = null;
        example = [ 443 8443 ];
        description = ''
          Match packets destined for these TCP/UDP ports.
          Implemented via nftables fwmark + ip rule. Requires
          networking.nftables.enable = true on the host.
        '';
      };

      matchProtocol = mkOption {
        type = types.nullOr (types.enum [ "tcp" "udp" "both" ]);
        default = "both";
        description = "Protocol to match for port-based rules.";
      };

      matchFamily = mkOption {
        type = types.nullOr (types.enum [ "ipv4" "ipv6" "both" ]);
        default = null;
        description = ''
          Address family the rule steers. Null means ipv4, which is what
          every rule written before this option existed meant: reading an
          absent family as "both" would start steering IPv6 on hosts whose
          configuration never mentioned it.

          ipv6 (and the IPv6 half of both) requires nptv6, and steers only
          the internal subnets the rule's gateway is currently translating —
          see services.route-balancer.rules for why.
        '';
      };

      gateway = mkOption {
        type = types.str;
        description = ''
          Interface name of the gateway to route matched traffic through.
          Must match a key in services.route-balancer.gateways.
        '';
      };
    };
  };

  nptv6SubnetModule = types.submodule {
    options = {
      prefix = mkOption {
        type = types.str;
        example = "fd00:dead:beef:1::/64";
        description = ''
          The internal /64 this subnet uses. Must sit inside internalPrefix.
          Both halves of an NPTv6 mapping are /64s so that the host identifier
          is carried across the translation unchanged.
        '';
      };

      description = mkOption {
        type = types.str;
        default = "";
        example = "Trusted LAN";
        description = "Human-readable label for this subnet.";
      };
    };
  };

  nptv6UplinkModule = types.submodule {
    options = {
      prefixSource = mkOption {
        type = types.enum [ "route" "ra" "static" ];
        default = "route";
        description = ''
          How this uplink's external prefix is discovered:
            route  — the RFC 7084 discard route a DHCPv6-PD client installs
                     for the delegation ("unreachable 2001:db8::/56 dev lo").
                     The only netlink-visible object carrying the delegation's
                     true length, and the default.
            ra     — the Prefix Information Option of a Router Advertisement.
                     Always a /64, for uplinks with no delegation at all.
                     Requires kernel RA processing (accept_ra = 2, which this
                     module sets by default for such uplinks).
            static — a fixed prefix from staticPrefix, for static allocations
                     and 6in4 tunnels.
        '';
      };

      matchPrefix = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "2001:db8::/32";
        description = ''
          The ISP's aggregate allocation, used to attribute an observed prefix
          to this uplink. A discard route is parked on loopback and carries
          nothing identifying its uplink, so with two delegations arriving at
          once there is otherwise no way to tell which is whose — and sourcing
          packets out of one ISP bearing another ISP's prefix is silently
          dropped by transit providers as spoofing.

          Required when more than one uplink uses prefixSource = "route".
          An RA arrives on the link it describes, so "ra" never needs it.
        '';
      };

      staticPrefix = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "2001:db8:abcd::/56";
        description = ''
          The external prefix for prefixSource = "static". Ignored by the
          other sources.
        '';
      };

      subnetPriority = mkOption {
        type = types.listOf types.str;
        default = [ ];
        example = [ "main" "guest" "iot" ];
        description = ''
          Internal subnets to map through this uplink, most important first.
          A delegation holds 2^(64 - length) /64s — 256 for a /56, 16 for a
          /60, exactly one for a /64 — and maps that many subnets from the
          head of this list, dropping the rest with a warning. Ordering is
          how the operator, rather than the daemon, decides what keeps working
          when an ISP shortens the delegation.
        '';
      };

      proxyNdp = mkOption {
        type = types.nullOr types.bool;
        default = null;
        example = true;
        description = ''
          Answer neighbour solicitations for this uplink's translated
          addresses. A translated address exists only inside nftables, so
          nothing on the link owns it — invisible when the upstream router
          routes the external prefix to you, and fatal when it treats the
          prefix as on-link, which is what a tethering phone does.

          The daemon learns which host identifiers are live from the forwarded
          traffic itself and installs one kernel proxy entry per identifier,
          which also means an inbound-first connection to a host that has been
          silent for proxyNdpTimeout has to wait for that host to speak again.

          Null selects the default: on for prefixSource = "ra", off otherwise,
          since a delegation is routed to the customer edge by definition.
        '';
      };

      allowInboundConnections = mkOption {
        type = types.bool;
        default = false;
        example = true;
        description = ''
          Let the internet start connections to the hosts this uplink maps.

          Translation is symmetric by construction: the prerouting rule that
          rewrites a reply back to its internal address is the same rule that
          would deliver an unsolicited packet, so a mapping either offers
          inbound-initiated reachability for the whole external /64 or it
          offers none. That is the RFC 6296 property NAT66 does not have, and
          it is a real thing to want — but a LAN becoming reachable is not
          something that should happen because translation was switched on.

          Off, no prerouting rule is emitted for this uplink and a counted drop
          is installed where it would have been, at prerouting priority
          dstnat - 10, matching the external prefix on ct state new. Traffic
          the LAN starts is unaffected in both directions: its replies are
          reverse-translated from the conntrack entry without the nat chain
          being consulted again, and they arrive as ct state established, so
          the drop does not see them. Inbound ICMPv6 errors — the Packet Too
          Big that path MTU discovery depends on — arrive as ct state related
          and are likewise untouched.

          On, this uplink behaves as it always did, and deciding which hosts
          and which ports may be reached is the host firewall's. Note that
          networking.firewall.filterForward does not do it: its forward-allow
          chain accepts anything conntrack has flagged as DNAT before it
          reaches extraForwardRules. The defence has to be its own base chain
          at a lower priority; README.md carries the recipe.

          Proxy NDP is deliberately not tied to this. On an ra uplink the
          upstream solicits for a translated address whenever a reply comes
          back, so switching inbound off while switching proxy NDP off with it
          would break the return path this option promises to leave alone.

          One consequence to know on an ra uplink, where the external prefix is
          the upstream's own on-link /64: it contains this host's WAN address
          too, so with inbound off, connections started to the host at that
          address are dropped as well. Enabling nptv6 already made that address
          unusable — the prerouting rule rewrote its prefix like any other —
          so this changes which mechanism refuses, not whether it works.
        '';
      };
    };
  };

  probeToJson = p:
    { type = p.type; }
    // optionalAttrs (p.type == "icmp") (
      optionalAttrs (p.payload != 56) { payload = p.payload; }
    )
    // optionalAttrs (p.type == "http" || p.type == "https") ({
      url = p.url;
      method = p.method;
      expected_status = p.expectedStatus;
    } // optionalAttrs (p.expectedBody != "") { expected_body = p.expectedBody; })
    // optionalAttrs (p.type == "tcp") {
      host = p.host;
      port = p.port;
    }
    // optionalAttrs (p.type == "dns") {
      resolver = p.resolver;
      query = p.query;
    }
    // optionalAttrs (p.type == "exec") {
      command = p.command;
    };

  healthToJson = h: {
    unhealthy_threshold = h.unhealthyThreshold;
    healthy_threshold = h.healthyThreshold;
    interval = h.interval;
    timeout = h.timeout;
    probe = probeToJson h.probe;
  } // optionalAttrs (h.probe6 != null) { probe6 = probeToJson h.probe6; };

  nptv6ToJson = n: {
    enable = true;
    internal_prefix = n.internalPrefix;
    nftables_table = n.nftablesTable;
    subnets = mapAttrs
      (_: s:
        { inherit (s) prefix; }
          // optionalAttrs (s.description != "") { inherit (s) description; }
      )
      n.subnets;
    uplinks = mapAttrs
      (_: u:
        {
          prefix_source = u.prefixSource;
          subnet_priority = u.subnetPriority;
        }
        // optionalAttrs (u.matchPrefix != null) { match_prefix = u.matchPrefix; }
        // optionalAttrs (u.staticPrefix != null) { static_prefix = u.staticPrefix; }
        // optionalAttrs (u.proxyNdp != null) { proxy_ndp = u.proxyNdp; }
        // optionalAttrs u.allowInboundConnections { allow_inbound_connections = true; }
      )
      n.uplinks;
  } // optionalAttrs n.allowNonUla { allow_non_ula = true; }
  // optionalAttrs (!n.leakProtection) { leak_protection = false; }
  // optionalAttrs (n.proxyNdpMaxHosts != null) { proxy_ndp_max_hosts = n.proxyNdpMaxHosts; }
  // optionalAttrs (n.proxyNdpTimeout != null) { proxy_ndp_timeout = n.proxyNdpTimeout; };

  configFile = pkgs.writeText "route-balancer.json" (builtins.toJSON (
    {
      log_level = cfg.logLevel;
      firewall = cfg.firewall;
      route_proto = cfg.routeProto;
      route_table_offset = cfg.routeTableOffset;
      iptables_chain = cfg.iptablesChain;
      nftables_table = cfg.nftablesTable;
      ipv4_ecmp = cfg.ipv4Ecmp;
      ipv6_ecmp = cfg.ipv6Ecmp;
      ipv6_metric = cfg.ipv6Metric;
      gateways = mapAttrs
        (_: gw:
          { inherit (gw) weight description; }
          // optionalAttrs (gw.health != null) { health = healthToJson gw.health; }
        )
        cfg.gateways;
      rules = map
        (r:
          { inherit (r) gateway; }
          // optionalAttrs (r.matchDstPort != null) { match_dst_port = r.matchDstPort; }
          // optionalAttrs (r.matchProtocol != null) { match_protocol = r.matchProtocol; }
          // optionalAttrs (r.matchFamily != null) { match_family = r.matchFamily; }
        )
        cfg.rules;
    }
    // optionalAttrs (cfg.reconcileInterval != null) {
      reconcile_interval = cfg.reconcileInterval;
    }
    // optionalAttrs cfg.nptv6.enable {
      nptv6 = nptv6ToJson cfg.nptv6;
    }
  ));

  # Agrees with config.PortRules: an empty list is not a port rule to the
  # daemon either, so it must not count as work here.
  portRules = filter (r: r.matchDstPort != null && r.matchDstPort != [ ]) cfg.rules;

  ruleFamily = r: if r.matchFamily == null then "ipv4" else r.matchFamily;

  ipv6PortRules = filter (r: elem (ruleFamily r) [ "ipv6" "both" ]) portRules;

  ipv6RulesOffUplink = filter
    (r: !hasAttr r.gateway cfg.nptv6.uplinks)
    ipv6PortRules;

  ipv4PortRules = filter (r: elem (ruleFamily r) [ "ipv4" "both" ]) portRules;

  ipv4InPlay = cfg.ipv4Ecmp || ipv4PortRules != [ ];

  ipv6InPlay = cfg.ipv6Ecmp || cfg.nptv6.enable;

  unprobedIpv6Gateways = filter
    (n:
      let health = cfg.gateways.${n}.health; in
      health != null
      && health.probe6 == null
      && elem health.probe.type [ "http" "https" "tcp" "dns" ])
    (attrNames cfg.gateways);

  # Mirrors probe.Validate: a probe missing the one thing its type needs
  # fails every check it runs, and a failing probe withdraws the route and
  # stops nptv6 translating, so it is caught here rather than at startup.
  probeComplaint = p:
    if elem p.type [ "http" "https" ] && p.url == "" then "needs a url"
    else if p.type == "tcp" && p.host == "" then "needs a host"
    else if p.type == "tcp" && p.port == 0 then "needs a port"
    else if p.type == "dns" && p.resolver == "" then "needs a resolver"
    else if p.type == "exec" && p.command == [ ] then "needs a command"
    else null;

  # Mirrors config.ProbeFor: IPv6 falls back to `probe` unless probe6 is set,
  # or the type names a destination an IPv6 family cannot inherit.
  probeServesIpv6 = health:
    health.probe6 == null
    && !elem health.probe.type [ "http" "https" "tcp" "dns" ];

  # Gated the way settings.validateProbes gates, on the families route.Options
  # actually observes: a probe for a family nobody looks at is never built, so
  # it is not judged either. `probe` is judged whenever IPv6 would fall back to
  # it as well, because settings.Resolve judges it there — and that failure is
  # a log.Fatalf, so an IPv6-only gateway with an incomplete icmp or exec probe
  # would evaluate here and then restart-loop the unit instead.
  incompleteProbes = concatMap
    (n:
      let
        health = cfg.gateways.${n}.health;
        check = attr: p:
          let c = if p == null then null else probeComplaint p; in
          optional (c != null) "${n}.health.${attr} (${p.type}) ${c}";
        probeInPlay = ipv4InPlay || (ipv6InPlay && probeServesIpv6 health);
      in
      optionals (health != null)
        (optionals probeInPlay (check "probe" health.probe)
          ++ optionals ipv6InPlay (check "probe6" health.probe6)))
    (attrNames cfg.gateways);

  nptv6UplinkNames = attrNames cfg.nptv6.uplinks;

  nptv6UplinksWith = source:
    filter (n: cfg.nptv6.uplinks.${n}.prefixSource == source) nptv6UplinkNames;

  nptv6UndefinedSubnetRefs = concatMap
    (n: map (s: "${n} → ${s}")
      (filter (s: !hasAttr s cfg.nptv6.subnets) cfg.nptv6.uplinks.${n}.subnetPriority))
    nptv6UplinkNames;

  nptv6AmbiguousRouteUplinks =
    let routeUplinks = nptv6UplinksWith "route"; in
    optionals (length routeUplinks > 1)
      (filter (n: cfg.nptv6.uplinks.${n}.matchPrefix == null) routeUplinks);

  # nptv6.Resolve's ULA test is the first byte being 0xfd, which in text is a
  # first group of four hex digits beginning "fd".
  nptv6InternalIsULA =
    builtins.match "[fF][dD][0-9a-fA-F][0-9a-fA-F]:.*" cfg.nptv6.internalPrefix != null;

  nptv6BadSubnetLengths = map (n: "${n} (${cfg.nptv6.subnets.${n}.prefix})")
    (filter (n: builtins.match ".*/64" cfg.nptv6.subnets.${n}.prefix == null)
      (attrNames cfg.nptv6.subnets));

  nptv6DuplicateSubnetRefs = concatMap
    (n:
      let prio = cfg.nptv6.uplinks.${n}.subnetPriority; in
      map (s: "${n} → ${s}")
        (unique (filter (s: count (x: x == s) prio > 1) prio)))
    nptv6UplinkNames;

  nptv6BadAcceptRa = filter
    (n: toString (attrByPath [ "net.ipv6.conf.${n}.accept_ra" ] 2 config.boot.kernel.sysctl) != "2")
    (nptv6UplinksWith "ra");

  nptv6ProxyNdpUplinks = filter
    (n:
      let u = cfg.nptv6.uplinks.${n}; in
      if u.proxyNdp != null then u.proxyNdp else u.prefixSource == "ra")
    nptv6UplinkNames;

  nptv6BadProxyNdp = filter
    (n: toString (attrByPath [ "net.ipv6.conf.${n}.proxy_ndp" ] 1 config.boot.kernel.sysctl) != "1")
    nptv6ProxyNdpUplinks;

  nptv6InboundUplinks = filter (n: cfg.nptv6.uplinks.${n}.allowInboundConnections) nptv6UplinkNames;

  # Whether anything filters what these uplinks now let in cannot be decided
  # from here. networking.firewall.filterForward is not the answer — it accepts
  # every DNATted flow ahead of extraForwardRules — so the defence is a base
  # chain of the operator's own, which reaches us as opaque text. A table whose
  # content mentions the internal prefix is the cheapest evidence that someone
  # wrote one; it misses a guard that names a set or a variable, which is
  # exactly why what follows is a warning and not an assertion.
  nptv6ForwardGuardWritten = cfg.nptv6.internalPrefix != "" && any
    (t: hasInfix cfg.nptv6.internalPrefix t.content)
    (attrValues config.networking.nftables.tables);

in
{
  options.services.route-balancer = {
    enable = mkEnableOption "route-balancer ECMP gateway daemon";

    package = mkOption {
      type = types.package;
      description = "The route-balancer package to use.";
    };

    logLevel = mkOption {
      type = types.enum [ "debug" "info" "warn" "error" ];
      default = "info";
      description = "Log verbosity level.";
    };

    hashPolicy = mkOption {
      type = types.ints.between 0 2;
      default = 1;
      description = ''
        Linux kernel ECMP hash policy:
          0 = L3 only (src+dst IP)
          1 = L4 (src+dst IP + ports) — recommended
          2 = L3+L4+inner (for tunnels)

        A boot-time kernel setting rather than a daemon one: it is written to
        net.ipv4.fib_multipath_hash_policy and net.ipv6.fib_multipath_hash_policy
        through boot.kernel.sysctl, and the daemon never reads it. Each family's
        knob is set only when this module manages that family's route (ipv4Ecmp,
        ipv6Ecmp), and both are mkDefault, so a host that sets either itself
        keeps its own value.
      '';
    };

    ipv4Ecmp = mkOption {
      type = types.bool;
      default = true;
      description = ''
        Manage the IPv4 default route: observe it on netlink, weight it, gate it
        on health, and reconcile it. On by default, which is the one asymmetry
        with ipv6Ecmp — IPv4 balancing is what this daemon was for, and turning
        it off is the deliberate act.

        What it does not turn off is everything IPv4: destination-port rules
        keep working, because their fwmark rules point at the per-gateway
        routing tables and those are built from the same IPv4 default routes.
        With rules set the daemon still watches IPv4 and still maintains the
        tables and their ip rules — it just stops installing an ECMP route of
        its own. With no rules set, IPv4 is left entirely alone.

        Turning it off on a host where it was on withdraws what it installed:
        the next start finds the metric-0 route carrying routeProto and removes
        it, along with the per-gateway tables, rather than leaving nexthops
        behind that nothing is health-checking any more.

        Useful with ipv6Ecmp = true on a host whose IPv4 is a single uplink, or
        with nptv6 alone on one that only translates prefixes.
      '';
    };

    ipv6Ecmp = mkOption {
      type = types.bool;
      default = false;
      description = ''
        Manage the IPv6 default route the same way the IPv4 one is managed:
        observe it on netlink, weight it, gate it on health, and reconcile it.
        The gateways option describes both families at once — an interface
        carrying a default route in each gets one nexthop in each managed route.

        Off by default because it is not a passive addition. Switching it on
        makes the daemon install an IPv6 default route on a host where it has
        never written one, and whatever ordering of RA or DHCPv6 route metrics
        currently decides which uplink IPv6 uses stops deciding it.

        Two prerequisites that IPv4 does not have:

        - the uplinks need IPv6 default routes for the daemon to observe. On a
          forwarding host the kernel ignores Router Advertisements unless
          net.ipv6.conf.<iface>.accept_ra is 2 — and it is 0 on any link a
          userspace RA client (systemd-networkd, dhcpcd) manages, which installs
          the route itself instead. The daemon warns at startup when a
          configured gateway has no IPv6 route and nothing kernel-side is in a
          position to give it one.
        - every uplink needs a probe that can measure IPv6, because each family
          is now judged by its own. The icmp probe serves either, sending an
          ICMPv6 echo to a v6 nexthop and an ICMP one to a v4 nexthop, and exec
          ignores the target entirely. An http, tcp or dns probe names a
          destination in one family and is not reused for the other: set
          health.probe6 for those uplinks, or health.probe6.type = "none" to
          leave IPv6 untested deliberately. An unmeasured family counts as
          healthy, so the omission costs the route nothing — but it does mean an
          IPv6 outage on that uplink goes unnoticed.

        Split-access routing tables stay IPv4-only. The IPv6 equivalent would
        need one rule per global address per interface, and privacy extensions
        keep that set churning.
      '';
    };

    ipv6Metric = mkOption {
      type = types.ints.between 1 1023;
      default = 1;
      description = ''
        Metric the managed IPv6 default route is installed at.

        It cannot be 0 the way the IPv4 route's is: the kernel rewrites a
        requested metric of 0 on an IPv6 route to IP6_RT_PRIO_USER (1024), which
        is exactly where kernel-RA and DHCPv6 default routes live, so the managed
        route would collide with the uplink routes it is derived from. Anything
        below 1024 wins route selection while leaving those routes in place.
      '';
    };

    firewall = mkOption {
      type = types.enum [ "iptables" "nftables" ];
      default = "iptables";
      description = ''
        Firewall backend used for port-based routing marks.
          iptables  — uses iptables mangle rules (default, no extra config needed)
          nftables  — uses nftables fwmark rules (requires networking.nftables.enable = true)
      '';
    };

    routeProto = mkOption {
      type = types.ints.between 1 255;
      default = 111;
      description = ''
        Routing protocol number stamped on all routes installed by route-balancer
        (the ECMP default route). Used to identify and clean up managed routes.
        Must not conflict with other routing daemons on the same host.
        Register a name for it in /etc/iproute2/rt_protos.d/ for human-readable output.
      '';
    };

    routeTableOffset = mkOption {
      type = types.ints.between 1 251;
      default = 100;
      description = ''
        Base value added to an interface's kernel index to derive its
        per-gateway routing table ID (tableID = offset + ifIndex).

        253, 254 and 255 are the kernel's own default, main and local tables,
        so a gateway whose index carries it that high is one the daemon
        declines to manage: it drops that gateway's table, ip rule and fwmark
        from the plan and logs which interface and which ID, rather than
        writing per-gateway routes into main. Interface indexes climb as links
        are created and destroyed, so leave room — the default of 100 is far
        more than a router with a handful of physical uplinks will ever need.
      '';
    };

    iptablesChain = mkOption {
      type = types.str;
      default = "ROUTE-BALANCER";
      description = ''
        Name of the iptables mangle chain created by route-balancer.
        Only relevant when firewall = "iptables".
      '';
    };

    nftablesTable = mkOption {
      type = types.str;
      default = "route-balancer";
      description = ''
        Name of the nftables inet table created by route-balancer.
        Only relevant when firewall = "nftables".
      '';
    };

    gateways = mkOption {
      type = types.attrsOf gatewayModule;
      default = { };
      example = literalExpression ''
        {
          eth0 = { weight = 3; description = "Primary ISP fibre"; };
          eth1 = {
            weight = 1;
            description = "LTE backup";
            health = {
              unhealthyThreshold = 3;
              healthyThreshold   = 5;
              interval           = "5s";
              timeout            = "2s";
              probe.type         = "icmp";
            };
          };
        }
      '';
      description = ''
        Gateway configuration keyed by network interface name.
        The daemon monitors for default routes on these interfaces
        and includes them in ECMP according to their weights.
        When a health config is present the gateway is only included
        in ECMP while probes are passing.
      '';
    };

    reconcileInterval = mkOption {
      type = types.nullOr goDuration;
      default = "30s";
      description = ''
        How often route-balancer wakes up with nothing to report.

        Every pass the daemon makes — whether woken by a netlink message, a
        health verdict or this timer — re-reads the whole kernel state it cares
        about and repairs whatever does not match: the ECMP route, the
        per-gateway routing tables and ip rules, the firewall chains, and, with
        nptv6, the translation rules and which prefix each uplink holds. Events
        decide how *soon* a pass happens; this decides how long one can be
        delayed.

        So the interval bounds two things. A change made behind the daemon's
        back — somebody flushing a routing table, a chain or an ip rule by hand
        — announces nothing, so nothing but this timer will notice it. And a
        command of the daemon's own that failed is retried on the next pass,
        which in a quiet moment is this one.

        Accepts Go duration strings ("30s", "1m"). Setting it to null does not
        turn reconciliation off — there is no mode in which this daemon does not
        reconcile, since a pass is the only thing that repairs anything — it
        falls back to the 30s default. Lengthen it if the wakeups are
        unwelcome; the cost of a pass is a few dozen ip/nft invocations.
      '';
    };

    rules = mkOption {
      type = types.listOf ruleModule;
      default = [ ];
      example = literalExpression ''
        [
          { matchDstPort = [ 443 8443 ]; gateway = "eth1"; }
          { matchDstPort = [ 25 ];       gateway = "eth0"; }
        ]
      '';
      description = ''
        Destination-port policy routing rules. Each rule stamps an fwmark on
        traffic to the named ports and an ip rule sends that mark to the
        gateway's own routing table; traffic that matches no rule uses ECMP.
        Marking needs a mangle backend, which `firewall` selects.

        A rule with no matchDstPort is ignored: port matching is the only
        selector implemented.

        Rules are emitted into the mangle chain in the order listed, and
        stamping a mark does not stop evaluation — so if two rules name
        overlapping ports, the later one wins.

        Marks are stamped in two hooks: output, for traffic this host
        originates, where the mark triggers a second route lookup; and
        prerouting, for traffic it forwards, which is routed on the way out
        of prerouting and so cannot be steered by a mark stamped any later.

        An IPv6 rule (matchFamily) needs nptv6, and steers less than it
        looks like it does: it marks only traffic whose source is one of the
        internal subnets its gateway is currently translating. A subnet the
        gateway has no mapping for is left alone and keeps using ECMP, which
        is the only sound answer — steering it would put an untranslated
        internal source on the wire. So an uplink whose lease has gone, or
        whose health probe has withdrawn it, stops being steered to without
        the rule changing, and a rule can never disagree with the uplink's
        subnetPriority.
      '';
    };

    nptv6 = {
      enable = mkEnableOption ''
        stateless IPv6 prefix translation (NPTv6, RFC 6296).

        Internal subnets keep a stable ULA address plan while each uplink
        rewrites them into whatever external prefix it currently holds, so a
        DHCPv6-PD lease change renumbers nothing inside the network.

        Requires networking.nftables.enable — unlike the port-mark rules there
        is no iptables fallback.

        Note that translation is symmetric: the prerouting half is what brings
        replies back, and it covers the whole external /64, so every host in a
        mapped subnet becomes reachable from the internet at its translated
        address. networking.firewall.filterForward does not stop that — its
        forward-allow chain accepts anything carrying ct status dnat. Filtering
        inbound traffic is left to your own nftables rules; see the README
        section "Inbound reachability, and whose job it is" for the guard chain
        to write
      '';

      internalPrefix = mkOption {
        type = types.str;
        default = "";
        example = "fd00:dead:beef::/48";
        description = ''
          The stable internal address plan every subnet is carved out of,
          normally a ULA /48. Nothing outside this prefix is ever translated.
        '';
      };

      allowNonUla = mkOption {
        type = types.bool;
        default = false;
        description = ''
          Permit an internalPrefix outside fd00::/8. Translating a globally
          routable prefix is almost always a misconfiguration, so it has to be
          asked for explicitly.
        '';
      };

      leakProtection = mkOption {
        type = types.bool;
        default = true;
        description = ''
          Drop packets that still bear an internal source address as they leave
          an uplink that is not translating at all — because it holds no lease
          yet, or because its health probe has withdrawn it.

          Without this such a packet leaves with a ULA source and is discarded
          by the first upstream router with no route back: silently, several
          hops away, and indistinguishable from the uplink being broken. With
          it the drop happens here, where nft counts it and the daemon logs the
          uplink it applies to.

          Only uplinks carrying no mappings whatsoever are guarded. An uplink
          that maps some subnets but not others is a subnetPriority decision the
          operator made deliberately — a delegation too short to hold every
          subnet — and traffic from the subnets it could not map keeps leaving
          untranslated, which is the documented behaviour.

          The chain hooks at postrouting priority srcnat + 10, so translation
          has already had its chance at the packet; anything it still sees is
          traffic no rule claimed.
        '';
      };

      nftablesTable = mkOption {
        type = types.str;
        default = "route-balancer-nptv6";
        description = ''
          Name of the ip6 nftables table holding the translation rules. Kept
          separate from nftablesTable (the ip table used for fwmark rules)
          so the two can be replaced independently.

          The proxy-NDP learning rules live in "''${nftablesTable}-ndp", a
          second table that is never rebuilt on a lease change so that the
          host identifiers learned from the data plane survive a renumber.
        '';
      };

      proxyNdpMaxHosts = mkOption {
        type = types.nullOr types.ints.positive;
        default = null;
        example = 512;
        description = ''
          Cap on the number of host identifiers learned per proxy-NDP uplink
          set. Each becomes a kernel proxy neighbour entry, which counts
          against net.ipv6.neigh.default.gc_thresh1/2/3 (128/512/1024), so an
          uncapped set would let a LAN host spraying source addresses evict
          genuine neighbours. Null uses the daemon's default of 256.
        '';
      };

      proxyNdpTimeout = mkOption {
        type = types.nullOr goDuration;
        default = null;
        example = "4h";
        description = ''
          How long a learned host identifier survives without being seen
          again. Every forwarded packet refreshes it, so this is an idle
          timeout: it bounds how long a host can stay silent and still accept
          an inbound-first connection. Null uses the daemon's default of 1h.
        '';
      };

      subnets = mkOption {
        type = types.attrsOf nptv6SubnetModule;
        default = { };
        example = literalExpression ''
          {
            main  = { prefix = "fd00:dead:beef:1::/64"; description = "Trusted LAN"; };
            guest = { prefix = "fd00:dead:beef:2::/64"; };
            iot   = { prefix = "fd00:dead:beef:3::/64"; };
          }
        '';
        description = ''
          Named internal /64s, referenced by name from each uplink's
          subnetPriority list.
        '';
      };

      uplinks = mkOption {
        type = types.attrsOf nptv6UplinkModule;
        default = { };
        example = literalExpression ''
          {
            wan0 = {
              prefixSource   = "route";           # DHCPv6-PD delegation
              matchPrefix    = "2001:db8::/32";   # the ISP's aggregate
              subnetPriority = [ "main" "guest" "iot" ];
            };
            wan1 = {
              prefixSource   = "ra";              # bare /64 from an RA
              subnetPriority = [ "main" ];
            };
          }
        '';
        description = ''
          Translation configuration keyed by uplink interface name. An uplink
          that is also declared in services.route-balancer.gateways only
          carries translation rules while its health probes pass, exactly as
          it only carries ECMP traffic while they pass.
        '';
      };
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.gateways != { };
        message = "services.route-balancer.gateways must not be empty.";
      }
      {
        assertion = cfg.ipv4Ecmp || cfg.ipv6Ecmp || portRules != [ ] || cfg.nptv6.enable;
        message = ''
          services.route-balancer has ipv4Ecmp = false and ipv6Ecmp = false with no
          port-based rules and nptv6 disabled, which leaves the daemon nothing to do.
          Enable a family, add a rule with matchDstPort, or enable nptv6.
        '';
      }
      {
        assertion = all (r: hasAttr r.gateway cfg.gateways) cfg.rules;
        message = ''
          services.route-balancer.rules references a gateway interface that is
          not declared in services.route-balancer.gateways. Check rule.gateway values.
        '';
      }
      {
        assertion = portRules == [ ] || cfg.firewall != "nftables" || config.networking.nftables.enable;
        message = ''
          services.route-balancer has port-based rules (matchDstPort) with firewall = "nftables"
          but networking.nftables.enable is false.
          Either add: networking.nftables.enable = true;
          Or switch to: services.route-balancer.firewall = "iptables";
        '';
      }

      {
        assertion = incompleteProbes == [ ];
        message = ''
          services.route-balancer has health probes that are missing what their type
          needs, so they would fail every check they ever run:
            ${concatStringsSep "\n    " incompleteProbes}

          A probe that cannot pass does not merely leave its gateway unmeasured — it
          withdraws the gateway from ECMP and stops nptv6 translating through it. Set
          the missing field, or set the probe's type to "none" to say the family rides
          along untested on purpose.
        '';
      }
      {
        assertion = ipv6PortRules == [ ] || cfg.nptv6.enable;
        message = ''
          services.route-balancer.rules has a rule with matchFamily = "ipv6" or "both",
          which requires services.route-balancer.nptv6.enable = true.

          With nothing rewriting the source address per uplink, a forwarded flow pinned
          to a second uplink leaves carrying the first uplink's delegated prefix, and
          that ISP drops it. IPv4 never meets this because something NATs at the border;
          nptv6 is the equivalent, and its translation follows the routing decision the
          rule made.
        '';
      }
      {
        assertion = ipv6PortRules == [ ] || cfg.firewall == "nftables";
        message = ''
          services.route-balancer.rules has a rule with matchFamily = "ipv6" or "both",
          which requires services.route-balancer.firewall = "nftables".

          The IPv6 marks have no ip6tables backend. They only exist alongside nptv6,
          which already requires nftables, so there is nowhere for one to be useful.
        '';
      }
      {
        assertion = ipv6RulesOffUplink == [ ];
        message = ''
          services.route-balancer.rules steers IPv6 to ${
            concatStringsSep ", " (map (r: r.gateway) ipv6RulesOffUplink)
          }, which is not declared in services.route-balancer.nptv6.uplinks.

          Nothing would translate the traffic the rule sends there, so it would leave
          with an internal source address and be dropped upstream — silently, since a
          drop at the far side is not visible here.
        '';
      }
      {
        assertion = !cfg.nptv6.enable || config.networking.nftables.enable;
        message = ''
          services.route-balancer.nptv6.enable requires networking.nftables.enable = true.
          NPTv6 translation is implemented with nftables NETMAP rules and has no
          iptables fallback, unlike the port-based fwmark rules.
        '';
      }
      {
        assertion = !cfg.nptv6.enable || cfg.nptv6.internalPrefix != "";
        message = "services.route-balancer.nptv6.internalPrefix must be set when nptv6 is enabled.";
      }
      {
        assertion = !cfg.nptv6.enable || cfg.nptv6.internalPrefix == ""
          || cfg.nptv6.allowNonUla || nptv6InternalIsULA;
        message = ''
          services.route-balancer.nptv6.internalPrefix is ${cfg.nptv6.internalPrefix}, which is
          not a ULA in fd00::/8.

          The internal plan is the half of every mapping that never changes and never leaves,
          so a globally routable prefix there is almost always a misconfiguration. Set
          services.route-balancer.nptv6.allowNonUla = true to translate one anyway.
        '';
      }
      {
        assertion = !cfg.nptv6.enable || cfg.nptv6.subnets != { };
        message = "services.route-balancer.nptv6.subnets must define at least one internal /64.";
      }
      {
        assertion = !cfg.nptv6.enable || nptv6BadSubnetLengths == [ ];
        message = ''
          services.route-balancer.nptv6.subnets has prefixes that are not /64s:
            ${concatStringsSep "\n    " nptv6BadSubnetLengths}

          NPTv6 maps whole /64s so that the host identifier is carried across the
          translation unchanged, so both halves of a mapping have to be /64s.
        '';
      }
      {
        assertion = !cfg.nptv6.enable || cfg.nptv6.uplinks != { };
        message = "services.route-balancer.nptv6.uplinks must define at least one uplink.";
      }
      {
        assertion = !cfg.nptv6.enable || nptv6UndefinedSubnetRefs == [ ];
        message = ''
          services.route-balancer.nptv6 has uplinks whose subnetPriority names a subnet
          that is not defined in nptv6.subnets:
            ${concatStringsSep "\n    " nptv6UndefinedSubnetRefs}
        '';
      }
      {
        assertion = !cfg.nptv6.enable || nptv6DuplicateSubnetRefs == [ ];
        message = ''
          services.route-balancer.nptv6 has uplinks whose subnetPriority names the same
          subnet twice:
            ${concatStringsSep "\n    " nptv6DuplicateSubnetRefs}

          The list is an order of preference over distinct subnets, so a repeat says
          nothing the first mention did not, and is usually a typo for the subnet that
          was meant to be named there.
        '';
      }
      {
        assertion = !cfg.nptv6.enable
          || all (n: cfg.nptv6.uplinks.${n}.subnetPriority != [ ]) nptv6UplinkNames;
        message = ''
          Every services.route-balancer.nptv6.uplinks.<name>.subnetPriority must name at
          least one subnet — an uplink with an empty list would translate nothing.
        '';
      }
      {
        assertion = !cfg.nptv6.enable
          || all (n: cfg.nptv6.uplinks.${n}.staticPrefix != null) (nptv6UplinksWith "static");
        message = ''
          services.route-balancer.nptv6 has an uplink with prefixSource = "static" but no
          staticPrefix. There is nothing to observe for a static source, so the prefix has
          to be declared.
        '';
      }
      {
        assertion = !cfg.nptv6.enable || nptv6AmbiguousRouteUplinks == [ ];
        message = ''
          services.route-balancer.nptv6 has more than one uplink with prefixSource = "route"
          but no matchPrefix on ${concatStringsSep ", " nptv6AmbiguousRouteUplinks}.

          A DHCPv6-PD discard route is parked on loopback and carries nothing identifying
          its uplink, so two simultaneous delegations are indistinguishable. Attributing
          one to the wrong uplink sources packets out of one ISP bearing another ISP's
          prefix, which transit providers drop silently as spoofing.

          Set matchPrefix to each ISP's aggregate allocation (e.g. "2001:db8::/32"), which
          stays put while the delegation itself changes on renewal.
        '';
      }
      {
        assertion = !cfg.nptv6.enable || nptv6BadAcceptRa == [ ];
        message = ''
          services.route-balancer.nptv6 has uplinks with prefixSource = "ra" whose
          net.ipv6.conf.<iface>.accept_ra sysctl is set to something other than 2:
            ${concatStringsSep ", " nptv6BadAcceptRa}

          accept_ra defaults to 0 once IPv6 forwarding is enabled, and 1 is ignored on a
          forwarding host; only 2 makes the kernel process Router Advertisements, which is
          what emits the RTM_NEWPREFIX events this source observes.

          Note that systemd-networkd's own RA client (IPv6AcceptRA=yes) processes RAs in
          userspace and turns the kernel's off. Set IPv6AcceptRA=no on these links, or use
          prefixSource = "route" instead.
        '';
      }
      {
        assertion = !cfg.nptv6.enable || nptv6BadProxyNdp == [ ];
        message = ''
          services.route-balancer.nptv6 has uplinks with proxy NDP enabled whose
          net.ipv6.conf.<iface>.proxy_ndp sysctl is forced to something other than 1:
            ${concatStringsSep ", " nptv6BadProxyNdp}

          A translated address exists only inside nftables, so the kernel answering
          neighbour solicitations on its behalf is the entire mechanism; with proxy_ndp
          off every entry the daemon installs is inert and an on-link external prefix
          stays unreachable.

          Either leave the sysctl alone (this module sets it to 1 for these uplinks), or
          set services.route-balancer.nptv6.uplinks.<name>.proxyNdp = false if the
          upstream router routes the prefix to you and nothing ever solicits for it.
        '';
      }
    ];

    warnings = optional (ipv6InPlay && unprobedIpv6Gateways != [ ]) ''
      services.route-balancer has IPv6 in play${optionalString cfg.nptv6.enable " (nptv6.enable)"}${optionalString cfg.ipv6Ecmp " (ipv6Ecmp)"},
      but these gateways configure a health probe that names an IPv4 destination and no
      probe6 beside it, so their IPv6 will not be measured at all:
        ${concatStringsSep ", " unprobedIpv6Gateways}

      Each family is probed and judged separately. An http, tcp or dns probe carries an
      address or URL belonging to exactly one family, so the daemon declines to reuse it
      for the other rather than failing a probe that could never pass — a failing probe
      does not merely fail here, it withdraws the route and stops NPTv6 translating.

      Set services.route-balancer.gateways.<name>.health.probe6, or set
      health.probe6.type = "none" to record that the family rides along untested on
      purpose. This is a degradation and not an error: an unmeasured family is treated as
      healthy, so these uplinks keep working.
    ''
    ++ optional (cfg.nptv6.enable && nptv6InboundUplinks != [ ] && !nptv6ForwardGuardWritten) ''
      services.route-balancer.nptv6 has allowInboundConnections set on these uplinks:
        ${concatStringsSep ", " nptv6InboundUplinks}

      Each of them carries a prerouting rule covering the whole external /64, so every
      host in the subnets it maps is reachable from the internet at its translated
      address. That is what the option asks for; this is only a reminder that the
      other half is yours, and no nftables table on this host mentions
      ${cfg.nptv6.internalPrefix}, which is where the other half would be.

      networking.firewall.filterForward does not close it. Its forward-allow chain
      accepts anything conntrack has flagged as DNAT, which is every translated flow,
      before extraForwardRules is reached. The defence has to be a base chain of its
      own at a lower priority:

        networking.nftables.tables.wan6-guard = {
          family = "ip6";
          content = '''
            chain forward {
              type filter hook forward priority filter - 10; policy accept;
              ct state established,related accept
              iifname { ${concatStringsSep ", " (map (n: "\"${n}\"") nptv6InboundUplinks)} } ip6 daddr ${cfg.nptv6.internalPrefix} drop
            }
          ''';
        };

      Loosen that per host, per port or per subnet from there. If you already have one
      the daemon cannot see — a named set, an interface variable — this warning is
      wrong and can be ignored; it fires on the absence of evidence, not on evidence of
      absence.
    '';

    boot.kernel.sysctl = { }
      // optionalAttrs cfg.ipv4Ecmp {
      "net.ipv4.fib_multipath_hash_policy" = mkDefault cfg.hashPolicy;
    } // optionalAttrs cfg.ipv6Ecmp {
      "net.ipv6.fib_multipath_hash_policy" = mkDefault cfg.hashPolicy;
    } // optionalAttrs cfg.nptv6.enable ({
      "net.ipv6.conf.all.forwarding" = mkDefault 1;
    } // listToAttrs (map
      (n: nameValuePair "net.ipv6.conf.${n}.accept_ra" (mkDefault 2))
      (nptv6UplinksWith "ra")
    ++ map
      (n: nameValuePair "net.ipv6.conf.${n}.proxy_ndp" (mkDefault 1))
      nptv6ProxyNdpUplinks));

    systemd.network.config.networkConfig = {
      ManageForeignRoutes = false;
      ManageForeignRoutingPolicyRules = false;
    };

    systemd.services.route-balancer = {
      description = "route-balancer ECMP load-balanced gateway daemon";
      documentation = [ "https://github.com/yourorg/route-balancer" ];

      after = [ "network-pre.target" ];
      wants = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];

      restartTriggers = [ configFile ];

      serviceConfig = {
        ExecStart = "${cfg.package}/bin/route-balancer --config ${configFile}";
        Restart = "on-failure";
        RestartSec = "5s";

        KillMode = "mixed";

        User = "root";
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" "CAP_NET_RAW" ];
        AmbientCapabilities = [ "CAP_NET_ADMIN" "CAP_NET_RAW" ];
        NoNewPrivileges = true;

        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelModules = true;
        RestrictAddressFamilies = [ "AF_NETLINK" "AF_INET" "AF_INET6" ];

        StandardOutput = "journal";
        StandardError = "journal";
        SyslogIdentifier = "route-balancer";
      };
    };
  };
}
