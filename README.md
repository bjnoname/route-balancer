# route-balancer

Reactive ECMP load-balanced default gateway daemon for Linux, designed for NixOS.

route-balancer solves the multi-WAN problem: you have two or more upstream connections
(fibre + LTE backup, two ISPs, a VPN and a direct link) and want outbound traffic spread
across them automatically, with unhealthy links removed from rotation and re-added when
they recover — without running a full routing protocol daemon.

It listens on a Linux netlink socket for default route events (`RTM_NEWROUTE` / `RTM_DELROUTE`)
and automatically builds a weighted ECMP default route across all configured gateways.
When a gateway's default route disappears (interface goes down, DHCP lease expires) it is
removed from ECMP immediately. When it reappears it is re-added. Interfaces not listed in
the `gateways` config (or with `weight = 0`) are silently ignored, so you can safely run
it alongside other interfaces that route-balancer should not touch.

Point-to-point uplinks are supported. PPPoE links, WireGuard, and other tunnels install a
default route with no nexthop address (`default dev ppp0 scope link`) because there is
nothing to route *via*; those interfaces join ECMP keyed on the interface itself.

Optional per-gateway health probes (ICMP, HTTP, TCP, DNS, or a custom script) let the
daemon remove a gateway from ECMP before the kernel route disappears — for example when
an ISP is up at layer 2 but dropping packets upstream. Probes use `SO_BINDTODEVICE` so
they always travel through the gateway under test, not through a sibling gateway.

### Comparison with similar tools

| | route-balancer | iproute2 scripts | FRRouting / Bird | mwan3 | NetworkManager |
|---|---|---|---|---|---|
| **Reactive to route changes** | yes — netlink | no — static | yes — BGP/OSPF | yes | partial |
| **Health monitoring** | yes — built-in | manual | via BFD | yes | no |
| **Weighted ECMP** | yes | yes (manual) | yes | yes | no |
| **Policy routing (IP/port)** | yes | manual | no | yes | no |
| **NixOS module** | yes | DIY | packages exist | OpenWrt only | yes |
| **External dependencies** | none (stdlib Go) | iproute2 | many | OpenWrt | many |
| **Config complexity** | simple JSON | shell scripts | routing protocol config | UCI | GUI/D-Bus |

**vs. iproute2 scripts** — hand-written `ip route` scripts are static: they don't react to
DHCP lease renewals, interface flaps, or upstream failures. route-balancer handles all of
these automatically.

**vs. FRRouting / Bird** — full routing protocol daemons (BGP, OSPF) are the right tool
when you control both ends of the link and can run a routing protocol. For commodity ISP
connections where you have no BGP peering, they add significant complexity with no benefit.

**vs. mwan3** — mwan3 is the standard multi-WAN solution on OpenWrt/Linux but is tightly
coupled to OpenWrt's UCI config system and iptables-based policy routing. route-balancer
targets NixOS-managed systems and uses a declarative JSON config with a first-class NixOS
module.

**vs. NetworkManager / systemd-networkd** — these manage individual interfaces well but
have no concept of ECMP across multiple gateways or health-driven failover.

## Quick Start (NixOS)

Add to your system flake:

```nix
# /etc/nixos/flake.nix
{
  inputs = {
    nixpkgs.url        = "github:NixOS/nixpkgs/nixos-unstable";
    route-balancer.url = "github:bjnoname/route-balancer";
    route-balancer.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = { self, nixpkgs, route-balancer }: {
    nixosConfigurations.myhostname = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        route-balancer.nixosModules.default
        ./configuration.nix
      ];
    };
  };
}
```

Configure in `configuration.nix`:

```nix
services.route-balancer = {
  enable = true;

  gateways = {
    eth0 = {
      weight = 3;
      description = "Primary ISP";
      health = {
        unhealthyThreshold = 3;   # failures before removing from ECMP
        healthyThreshold   = 5;   # successes before re-adding
        interval           = "5s";
        timeout            = "2s";
        probe.type         = "icmp";
      };
    };
    eth1 = {
      weight = 1;
      description = "LTE backup";
      health = {
        unhealthyThreshold = 2;
        healthyThreshold   = 3;
        interval           = "10s";
        timeout            = "3s";
        probe = {
          type           = "http";
          url            = "http://detectportal.firefox.com/success.txt";
          expectedStatus = [ 200 ];
        };
      };
    };
  };

  rules = [
    { priority = 10; matchDstIp   = "10.0.0.0/8"; gateway = "eth0"; }
    { priority = 20; matchDstPort = [ 443 8443 ];  gateway = "eth1"; }
  ];

  # "iptables" (default) or "nftables"
  firewall = "iptables";
};
```

Then rebuild:

```bash
nixos-rebuild switch --flake .#myhostname
```

### Route metric requirements

route-balancer installs its ECMP route at **metric 0** — the lowest possible metric, so it
always wins over any other default route on the system. For this to work correctly, every
other default route (installed by DHCP, networkd, or static config) must use a **metric
greater than 0**. If a competing route sits at metric 0, the kernel treats them as equal-cost
paths to the same destination and may not honour the ECMP configuration.

A metric of 50–100 is a reasonable choice for non-route-balancer default routes; the exact
value does not matter as long as it is non-zero.

**With `networking.useNetworkd = true` (systemd-networkd):**

```nix
systemd.network.networks."10-eth0" = {
  matchConfig.Name = "eth0";
  networkConfig = {
    Address = "10.0.0.2/24";
    DHCP = "no";
  };
  # metric > 0 — route-balancer takes precedence at metric 0
  routes = [{ routeConfig = { Gateway = "10.0.0.1"; Metric = 50; }; }];
};

systemd.network.networks."10-eth1" = {
  matchConfig.Name = "eth1";
  networkConfig.DHCP = "ipv4";
  # RouteMetric applies to the default route installed by DHCP
  dhcpV4Config.RouteMetric = 50;
};
```

**With `networking.useNetworkd = false` (scripted networking):**

```nix
networking.interfaces.eth0.ipv4.addresses = [
  { address = "10.0.0.2"; prefixLength = 24; }
];
networking.interfaces.eth1.useDHCP = true;

# metric > 0 for the static default route on eth0
networking.defaultGateway = {
  address   = "10.0.0.1";
  interface = "eth0";
  metric    = 50;
};

# For DHCP interfaces, set the metric via dhclient or networkmanager config.
# On NixOS scripted networking the DHCP client defaults to metric 0 — override it:
networking.dhcpcd.extraConfig = ''
  interface eth1
    metric 50
'';
```

## Development

```bash
# Enter dev shell (includes Go, Nix tools, Claude Code)
nix develop

# Build
go build ./...

# Test
go test ./...

# Lint
golangci-lint run

# Format all files (Nix + Go)
nix fmt

# Boot an interactive three-NIC dev VM for manual testing
nix run .#dev-vm
```

## Architecture

```
Netlink (RTMGRP_IPV4_ROUTE)
   │
   ├── RTM_NEWROUTE ──▶ add gateway ──▶ setupGatewayRoutes() ──▶ startMonitor() ──▶ applyECMP()
   └── RTM_DELROUTE ──▶ rem gateway ──▶ teardownGatewayRoutes() ──▶ stopMonitor() ──▶ applyECMP()

Health monitor (one goroutine per gateway, only when health config is set):
   probe.Check() every Interval ──▶ streak counter
   unhealthyThreshold consecutive failures ──▶ EffectWeight = 0 ──▶ applyECMP()
   healthyThreshold  consecutive successes ──▶ EffectWeight = ConfigWeight ──▶ applyECMP()
   all gateways unhealthy ──▶ retain last ECMP route, log warning

applyECMP():
   filters gateways by EffectWeight > 0
   ip route add default metric 0 proto 111
   nexthop via gw1 dev eth0 weight 3
   nexthop via gw2 dev eth1 weight 1
   nexthop dev ppp0 weight 1          ← point-to-point links carry no "via"

reconcile() (every reconcileInterval):
   prunes gateways whose default route has vanished from the kernel,
   then restores the ECMP route, per-gateway tables, and ip rules if they drifted

Port-based rules (matchDstPort):
   iptables backend: iptables -t mangle ROUTE-BALANCER chain → MARK --set-mark N
   nftables backend: table inet route-balancer { chain mangle_output/mangle_forward }
   Both:             ip rule fwmark N lookup <per-gateway table>

Shutdown (SIGTERM/SIGINT):
   Stops all health monitor goroutines, removes all managed routes (proto 111),
   per-gateway tables, ip rules, and firewall chains — leaving the system in a clean state.
```

No external Go dependencies — pure stdlib netlink syscalls.

## Health Probes

Each gateway can have one probe. All probes bind their socket to the gateway's interface
via `SO_BINDTODEVICE`, ensuring probe traffic travels through the gateway under test
rather than the current default route.

| Type   | What it tests                          | Key config fields                              |
|--------|----------------------------------------|------------------------------------------------|
| `icmp` | Layer 3 reachability (ping)            | `payload` (bytes, default 56)                  |
| `http` | Layer 7 + captive portal detection     | `url`, `method`, `expectedStatus`, `expectedBody` |
| `tcp`  | Layer 4 TCP connect                    | `host`, `port`                                 |
| `dns`  | DNS resolver reachability              | `resolver` (host:port), `query`                |
| `exec` | Custom script (exit 0 = healthy)       | `command` (list)                               |

`icmp` needs a nexthop address to ping, so it cannot probe a point-to-point link
(PPP, tunnels) — those have none. Use `http`, `tcp`, `dns`, or `exec` there; the
daemon warns if an ICMP probe is configured on one.

Gateways without a health config are always considered healthy.
Gateways start optimistic (healthy) on daemon startup so ECMP is restored immediately
after a restart; the probe cycle corrects the state if a gateway is actually down.

## NixOS Module Options

### Core

| Option                          | Type           | Default           | Description                               |
| ------------------------------- | -------------- | ----------------- | ----------------------------------------- |
| `enable`                        | bool           | false             | Enable the daemon                         |
| `gateways`                      | attrset        | {}                | Per-interface weight + health config      |
| `gateways.<name>.weight`        | int 0–100      | 1                 | ECMP weight (0 = exclude from ECMP)       |
| `gateways.<name>.description`   | str            | ""                | Human-readable label                      |
| `rules`                         | list           | []                | Policy routing rules                      |
| `rules[].priority`              | int            | —                 | Rule priority (lower = first)             |
| `rules[].matchDstIp`            | str            | null              | Destination CIDR to match                 |
| `rules[].matchDstPort`          | [int]          | null              | Destination ports to match                |
| `rules[].matchProtocol`         | tcp\|udp\|both | both              | Protocol for port-based rules             |
| `rules[].gateway`               | str            | —                 | Interface name to route to                |
| `firewall`                      | enum           | `"iptables"`      | Firewall backend for port marks           |
| `routeProto`                    | int 1–255      | 111               | Protocol number stamped on managed routes |
| `routeTableOffset`              | int 1–250      | 100               | Base for per-gateway table IDs            |
| `iptablesChain`                 | str            | `"ROUTE-BALANCER"`| iptables mangle chain name                |
| `nftablesTable`                 | str            | `"route-balancer"`| nftables table name                       |
| `hashPolicy`                    | 0\|1\|2        | 1                 | Kernel ECMP hash policy                   |
| `logLevel`                      | enum           | `"info"`          | Log verbosity                             |
| `reconcileInterval`             | str\|null      | `"30s"`           | How often to check and restore managed state; null disables |

### Health monitoring (`gateways.<name>.health`)

| Option               | Type   | Default | Description                                          |
|----------------------|--------|---------|------------------------------------------------------|
| `unhealthyThreshold` | int    | 3       | Consecutive failures before removing from ECMP       |
| `healthyThreshold`   | int    | 5       | Consecutive successes before re-adding to ECMP       |
| `interval`           | string | `"5s"`  | Probe interval (Go duration)                         |
| `timeout`            | string | `"2s"`  | Per-probe deadline (Go duration)                     |
| `probe.type`         | enum   | `"icmp"`| Probe type: `icmp` `http` `https` `tcp` `dns` `exec` |
| `probe.payload`      | int    | 56      | ICMP payload size in bytes (`icmp`)                  |
| `probe.url`          | string | —       | URL to GET (`http`/`https`)                          |
| `probe.method`       | string | `"GET"` | HTTP method (`http`/`https`)                         |
| `probe.expectedStatus` | [int] | `[200]`| Accepted status codes (`http`/`https`)               |
| `probe.expectedBody` | string | —       | Required response body substring (`http`/`https`)    |
| `probe.host`         | string | —       | Host to connect to (`tcp`)                           |
| `probe.port`         | int    | —       | Port to connect to (`tcp`)                           |
| `probe.resolver`     | string | —       | DNS resolver `host:port` (`dns`)                     |
| `probe.query`        | string | `"example.com"` | Domain to resolve (`dns`)                  |
| `probe.command`      | [str]  | —       | Command argv; exit 0 = healthy (`exec`)              |

## Requirements

- Linux kernel ≥ 4.4 (multipath routing support)
- `CAP_NET_ADMIN` + `CAP_NET_RAW` (granted automatically by the NixOS module)
- `networking.nftables.enable = true` only when `firewall = "nftables"`
- User/DHCP default routes should use a metric > 0 so route-balancer can install its ECMP route at metric 0

## License

MIT
