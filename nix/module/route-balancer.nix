# nix/module/route-balancer.nix
#
# NixOS module for route-balancer.
#
# Usage in configuration.nix:
#
#   {
#     imports = [ route-balancer.nixosModules.default ];
#
#     services.route-balancer = {
#       enable = true;
#       gateways = {
#         eth0 = { weight = 3; };   # primary ISP
#         eth1 = { weight = 1; };   # LTE backup
#       };
#       rules = [
#         { priority = 10; matchDstIp = "10.0.0.0/8"; gateway = "eth0"; }
#         { priority = 20; matchDstPort = [ 443 ]; gateway = "eth1"; }
#       ];
#     };
#   }

{ config, lib, pkgs, ... }:

with lib;

let
  cfg = config.services.route-balancer;

  # ── Sub-module types ──────────────────────────────────────────────────────

  # Probe sub-module: one probe definition per gateway health config.
  # Only the fields relevant to the selected type need to be set.
  probeModule = types.submodule {
    options = {
      type = mkOption {
        type = types.enum [ "icmp" "http" "https" "tcp" "dns" "exec" ];
        default = "icmp";
        description = ''
          Health probe type:
            icmp  — ICMP echo (layer 3 reachability)
            http  — HTTP/HTTPS request (layer 7, captive portal detection)
            tcp   — TCP connect (lightweight layer 4 check)
            dns   — DNS query via a specific resolver
            exec  — arbitrary command; exit 0 = healthy
        '';
      };

      # icmp
      payload = mkOption {
        type = types.ints.between 0 65535;
        default = 56;
        description = "ICMP echo payload size in bytes (icmp probe).";
      };

      # http / https
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

      # tcp
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

      # dns
      resolver = mkOption {
        type = types.str;
        default = "";
        example = "8.8.8.8:53";
        description = "DNS resolver address (host or host:port) to query (dns probe).";
      };

      query = mkOption {
        type = types.str;
        default = "example.com";
        description = "Domain name to resolve (dns probe).";
      };

      # exec
      command = mkOption {
        type = types.listOf types.str;
        default = [ ];
        example = [ "curl" "--interface" "eth0" "-sf" "https://example.com/" ];
        description = "Command to run; exit 0 = healthy (exec probe).";
      };
    };
  };

  # Health monitoring sub-module for a single gateway.
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
        type = types.str;
        default = "5s";
        example = "10s";
        description = "How often to run the probe (Go duration string).";
      };

      timeout = mkOption {
        type = types.str;
        default = "2s";
        example = "3s";
        description = "Per-probe deadline (Go duration string).";
      };

      probe = mkOption {
        type = probeModule;
        default = { };
        description = "Probe configuration. Defaults to an ICMP echo probe.";
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
      priority = mkOption {
        type = types.ints.between 1 32765;
        description = ''
          Rule priority. Lower numbers are evaluated first.
          Avoid 0 and 32766–32767 (reserved by the kernel).
        '';
      };

      matchDstIp = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "10.0.0.0/8";
        description = "Match packets destined for this CIDR prefix.";
      };

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

      gateway = mkOption {
        type = types.str;
        description = ''
          Interface name of the gateway to route matched traffic through.
          Must match a key in services.route-balancer.gateways.
        '';
      };
    };
  };

  # ── Config file generation ────────────────────────────────────────────────

  # Convert a probeModule attrset to the JSON representation consumed by the
  # Go daemon. Only fields relevant to the selected type are emitted.
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

  # Convert a healthModule attrset to the JSON representation.
  healthToJson = h: {
    unhealthy_threshold = h.unhealthyThreshold;
    healthy_threshold = h.healthyThreshold;
    interval = h.interval;
    timeout = h.timeout;
    probe = probeToJson h.probe;
  };

  configFile = pkgs.writeText "route-balancer.json" (builtins.toJSON (
    {
      log_level = cfg.logLevel;
      hash_policy = cfg.hashPolicy;
      firewall = cfg.firewall;
      route_proto = cfg.routeProto;
      route_table_offset = cfg.routeTableOffset;
      iptables_chain = cfg.iptablesChain;
      nftables_table = cfg.nftablesTable;
      gateways = mapAttrs
        (_: gw:
          { inherit (gw) weight description; }
          // optionalAttrs (gw.health != null) { health = healthToJson gw.health; }
        )
        cfg.gateways;
      rules = map
        (r:
          { inherit (r) priority gateway; }
          // optionalAttrs (r.matchDstIp != null) { match_dst_ip = r.matchDstIp; }
          // optionalAttrs (r.matchDstPort != null) { match_dst_port = r.matchDstPort; }
          // optionalAttrs (r.matchProtocol != null) { match_protocol = r.matchProtocol; }
        )
        cfg.rules;
    }
    // optionalAttrs (cfg.reconcileInterval != null) {
      reconcile_interval = cfg.reconcileInterval;
    }
  ));

  portRules = filter (r: r.matchDstPort != null) cfg.rules;

in
{
  # ── Options ────────────────────────────────────────────────────────────────

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
        Sets net.ipv4.fib_multipath_hash_policy at boot.
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
      type = types.ints.between 1 250;
      default = 100;
      description = ''
        Base value added to an interface's kernel index to derive its
        per-gateway routing table ID (tableID = offset + ifIndex).
        Ensure offset + max(ifIndex) stays below 253 (reserved tables).
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
      type = types.nullOr types.str;
      default = "30s";
      description = ''
        How often route-balancer checks whether the ECMP route, per-gateway
        routing tables, and ip rules still match the desired state, restoring
        any that were modified externally (e.g. by networkd or an operator).
        Accepts Go duration strings ("30s", "1m"). When null (the default)
        periodic reconciliation is disabled and the daemon only reacts to
        netlink route events.
      '';
    };

    rules = mkOption {
      type = types.listOf ruleModule;
      default = [ ];
      example = literalExpression ''
        [
          { priority = 10; matchDstIp   = "192.168.0.0/16"; gateway = "eth0"; }
          { priority = 20; matchDstPort = [ 443 8443 ];      gateway = "eth1"; }
        ]
      '';
      description = ''
        Policy routing rules evaluated top-down by priority.
        IP-based rules use ip rule + per-gateway routing tables.
        Port-based rules additionally require nftables fwmark.
        First matching rule wins; unmatched traffic uses ECMP.
      '';
    };

  };

  # ── Implementation ─────────────────────────────────────────────────────────

  config = mkIf cfg.enable {

    # ── Assertions ───────────────────────────────────────────────────────────

    assertions = [
      {
        assertion = cfg.gateways != { };
        message = "services.route-balancer.gateways must not be empty.";
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
    ];

    # ── Kernel sysctl ────────────────────────────────────────────────────────

    boot.kernel.sysctl = {
      "net.ipv4.fib_multipath_hash_policy" = cfg.hashPolicy;
    };

    # ── Prevent networkd from fighting over routes and policy rules ───────────
    # systemd-networkd will otherwise remove or overwrite routes/rules that
    # route-balancer installs, causing flapping or broken ECMP state.
    systemd.network.config.networkConfig = {
      ManageForeignRoutes = false;
      ManageForeignRoutingPolicyRules = false;
    };

    # ── Main daemon service ──────────────────────────────────────────────────

    systemd.services.route-balancer = {
      description = "route-balancer ECMP load-balanced gateway daemon";
      documentation = [ "https://github.com/yourorg/route-balancer" ];

      after = [ "network-pre.target" ];
      wants = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];

      # Restart on config change (triggered by nixos-rebuild switch)
      restartTriggers = [ configFile ];

      serviceConfig = {
        ExecStart = "${cfg.package}/bin/route-balancer --config ${configFile}";
        Restart = "on-failure";
        RestartSec = "5s";

        # Capabilities — only what's needed
        User = "root";
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" "CAP_NET_RAW" ];
        AmbientCapabilities = [ "CAP_NET_ADMIN" "CAP_NET_RAW" ];
        NoNewPrivileges = true;

        # Hardening
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelModules = true;
        RestrictAddressFamilies = [ "AF_NETLINK" "AF_INET" "AF_INET6" ];

        # Logging
        StandardOutput = "journal";
        StandardError = "journal";
        SyslogIdentifier = "route-balancer";
      };
    };
  };
}
