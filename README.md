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

For IPv6 there is an optional second half: [NPTv6 prefix translation](#nptv6-multihoming),
which tracks each uplink's DHCPv6-PD delegation and rewrites a stable internal ULA plan
into whichever external prefix that uplink currently holds — so a lease change renumbers
nothing inside the network.

### Comparison with similar tools

| | route-balancer | iproute2 scripts | FRRouting / Bird | mwan3 | NetworkManager |
|---|---|---|---|---|---|
| **Reactive to route changes** | yes — netlink | no — static | yes — BGP/OSPF | yes | partial |
| **Health monitoring** | yes — built-in | manual | via BFD | yes | no |
| **Weighted ECMP** | yes | yes (manual) | yes | yes | no |
| **Policy routing (dst port)** | yes | manual | no | yes (IP too) | no |
| **IPv6 NPTv6 + PD tracking** | yes | manual | no | no | no |
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

**vs. VyOS / OPNsense** — both offer NPTv6, but as a static mapping: you type in the
external prefix, and when the ISP hands you a different delegation you type in the new
one. route-balancer derives the mapping from the delegation the uplink actually holds and
re-derives it whenever that changes, including when the *length* changes. That is the
difference that matters on a residential connection, where the delegation is a lease
rather than an assignment — and neither is a NixOS-native, declaratively configured
option to begin with.

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
        { services.route-balancer.package = route-balancer.packages.x86_64-linux.default; }
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
    { matchDstPort = [ 443 8443 ]; gateway = "eth1"; }
    { matchDstPort = [ 25 ];       gateway = "eth0"; }
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

## IPv6 default routes

By default route-balancer manages the **IPv4** default route only (`ipv4Ecmp`, on
unless you turn it off — see [Turning IPv4 off](#turning-ipv4-off)). Set
`services.route-balancer.ipv6Ecmp = true` (JSON: `"ipv6_ecmp": true`) to have it manage
the IPv6 one the same way: observed on netlink, weighted, health-gated and reconciled.
The `gateways` block describes both families at once — an interface carrying a default
route in each gets one nexthop in each managed route, at its configured weight.

It is off by default because it is not a passive addition. Switching it on makes the
daemon install an IPv6 default route on a host where it has never written one, and
whatever ordering of RA or DHCPv6 route metrics currently decides which uplink IPv6 uses
stops deciding it.

### The IPv6 route does not live at metric 0

The IPv4 managed route sits at metric 0, below everything. IPv6 has no equivalent: ask
the kernel for metric 0 on an IPv6 route and it stores 1024 (`IP6_RT_PRIO_USER`) — which
is exactly where kernel-RA and DHCPv6 default routes go, so the managed route would
collide with the uplink routes it is derived from.

The managed IPv6 route therefore goes in at **metric 1** by default (`ipv6Metric`, any
value 1–1023). Anything below 1024 wins route selection while leaving the uplink routes
in place, so unlike IPv4 there is nothing to reconfigure on the uplinks: an RA or DHCPv6
default route at the usual 1024 already sits above the managed route.

### The uplinks need IPv6 default routes in the first place

Enabling forwarding makes the kernel stop acting on Router Advertisements unless
`net.ipv6.conf.<iface>.accept_ra` is `2` — the value `1` is silently ignored on a router.
And on any link managed by a userspace RA client the sysctl reads `0` whatever you
declare, because the client processes RAs itself and installs the route directly:
systemd-networkd always does this, and dhcpcd does it on any interface where it runs its
own router solicitation (on NixOS, every interface without a static IPv6 address, unless
`networking.dhcpcd.IPv6rs = false`).

Either arrangement is fine — the daemon only needs the resulting default route, not the
RA. What is not fine is neither: forwarding on, `accept_ra` at 0, and no userspace client.
The daemon checks for exactly that at startup, against `/proc` rather than against the
declared sysctl, and warns per interface:

```
WARN IPv6 ECMP is enabled but this gateway has no IPv6 default route, and the kernel is
     not processing Router Advertisements on it iface=wan0 accept_ra=0
```

### What is not managed

- **Split-access routing — reply traffic leaving by the interface it arrived on — stays
  IPv4-only.** That is what the `from <src> lookup <table>` rule does, and it has no IPv6
  counterpart: an interface holds several global addresses at once under privacy
  extensions and the set churns on its own schedule, so the selector cannot be one
  address cached at setup time. An IPv6 port rule does build a per-gateway table, but it
  is selected into by the mark rather than by a source, and holds a default route with no
  `src` on it — see [Port rules over IPv6](#port-rules-over-ipv6).
- **`rules` (policy routing by destination port) steers IPv6 only where `nptv6` is
  enabled** — see [Port rules over IPv6](#port-rules-over-ipv6). Without something
  rewriting the source per uplink, a forwarded flow pinned to the wrong uplink leaves
  with an address from the other one's delegation and is dropped by that ISP's
  filtering, which is why the daemon refuses the combination rather than installing it.
- **Health is measured per family**, so switching IPv6 management on adds an IPv6 probe
  to every health-monitored uplink. `icmp` and `exec` serve both; an `http`, `tcp` or
  `dns` probe names one family and needs a `probe6` beside it — see
  [Health Probes](#health-probes).

### Turning IPv4 off

The mirror image is `services.route-balancer.ipv4Ecmp = false` (JSON:
`"ipv4_ecmp": false`), which stops the daemon managing the **IPv4** default route. It
defaults to `true` — the asymmetry with `ipv6Ecmp` is deliberate, since IPv4 balancing is
what the daemon is for and omitting the field keeps every config written before the
option existed working exactly as it did.

What it turns off is the ECMP route, not IPv4 as a whole:

- **With `rules` configured, IPv4 stays observed.** A fwmark rule points at a
  per-gateway routing table, and that table is built from the IPv4 default routes the
  uplinks carry, so the daemon keeps watching them and keeps the tables, their `ip
  rule`s and the mangle chain in repair. It simply installs no default route of its own.
  This is the useful shape: IPv6 balanced across uplinks, IPv4 pinned per port.
- **With no `rules`, IPv4 is left entirely alone** — not observed, no tables, no rules,
  nothing at metric 0.

Turning it off on a host where it was on **withdraws what the previous run installed**.
There is no ledger of what the daemon wrote, so the trace it goes by is the route
itself: a default route carrying `routeProto` in a family it no longer manages is
removed on the next pass, along with the per-gateway tables. The same now applies to
IPv6 when `ipv6Ecmp` goes from `true` to `false`, which previously left its route
behind.

`ipv4Ecmp = false` and `ipv6Ecmp = false` together are only accepted when something else
is asked for — port `rules` or `nptv6`. With neither, the daemon refuses to start rather
than idling.

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

# Serve docs/ as a book on http://127.0.0.1:3000 (live reload)
nix run .#docs        # or `docs-serve` from inside the dev shell

# Render it to a static site instead
nix build .#docs
```

The `docs/` folder is an [mdBook](https://rust-lang.github.io/mdBook/); its
Mermaid diagrams are rendered in the browser and follow the book's theme. `nix
flake check` builds it, so a `SUMMARY.md` entry with no file behind it fails CI.

## Architecture

```
Netlink (route groups for the families the config observes)
   │
   ├── RTM_NEWROUTE   ─┐
   ├── RTM_DELROUTE   ─┤
   ├── health verdict ─┼─▶ one event on the queue ──▶ wakes the loop
   ├── lease change   ─┤
   └── reconcile tick ─┘

One pass, after the queue is drained (the drain is the debounce — a link flap or an
RA storm yields one recalculation, not one per message):
   fold each event into State ──▶ read the kernel: interface table, routes, rules,
   chains, translation rules ──▶ Desired(config, State) ──▶ compare ──▶ one batch
   of actions
   Nothing is applied while deciding, and nothing is read after. Events decide how
   soon a pass happens; reconcileInterval decides how long one can be delayed.

Health monitor (a child loop per link *and family*, only when health config is set
and the configuration says how to probe that family; started and stopped by actions
in the parent's batch, and joined by it on the way out):
   probe.Check() every Interval ──▶ streak counter
   unhealthyThreshold consecutive failures ──▶ (link, family) marked unhealthy ──▶ ECMP route
   healthyThreshold  consecutive successes ──▶ (link, family) marked healthy   ──▶ ECMP route
   the verdict is per (interface, family), so it withdraws the uplink from that
   family's route and no other; the v6 verdict also gates NPTv6 translation,
   because IPv6 is the stack translated traffic leaves over
   all gateways in a family unhealthy ──▶ retain that family's last ECMP route, log warning

The managed ECMP route:
   filters gateways by whether their link's probe is passing
   ip route add default metric 0 proto 111
   nexthop via gw1 dev eth0 weight 3
   nexthop via gw2 dev eth1 weight 1
   nexthop dev ppp0 weight 1          ← point-to-point links carry no "via"

Every pass reads before it decides, so there is no ledger of what was installed:
   whatever drifted — the ECMP route, a per-gateway table, an ip rule, a chain,
   a translation rule — is simply not what the calculator wants, and is repaired.
   A command that failed is retried by the next pass for the same reason.

Port-based rules (matchDstPort), IPv6 (matchFamily) needs nptv6:
   iptables backend: iptables -t mangle ROUTE-BALANCER chain → MARK --set-mark N
   nftables backend: table ip route-balancer { chain mangle_output/mangle_prerouting }
   Both:             ip rule fwmark N lookup <per-gateway table>
   IPv6:             table ip6 route-balancer { chain mangle_prerouting }, one mark per
                     (rule x subnet the gateway translates) ──▶ ip -6 rule fwmark N
   Hooks:            OUTPUT for traffic this host originates (the mark triggers a
                     second route lookup), PREROUTING for traffic it forwards
                     (routed on the way out of prerouting, so a mark stamped any
                     later cannot steer it)

NPTv6 (second netlink socket, RTMGRP_IPV6_ROUTE | RTMGRP_IPV6_PREFIX | RTMGRP_IPV6_IFADDR):
   PD discard route / RA prefix ──▶ prefixSource attributes it to an uplink
                                ──▶ slice the delegation into /64s
                                ──▶ one snat rule per (subnet, uplink), plus a dnat
                                    where allowInboundConnections opens the uplink
                                    and an ingress drop where it does not (default),
                                    applied as a single nft transaction
   unchanged lease ──▶ no rule churn (conntrack state is preserved)

Shutdown (SIGTERM/SIGINT):
   Stops and joins every health monitor child, removes all managed routes (proto 111),
   per-gateway tables, ip rules, and firewall chains — leaving the system in a clean state.
```

No external Go dependencies — pure stdlib netlink syscalls.

## Health Probes

All probes bind their socket to the gateway's interface via `SO_BINDTODEVICE`, ensuring
probe traffic travels through the gateway under test rather than the current default
route, and pin the address family they are measuring, so a verdict about one stack
cannot be earned over the other.

| Type   | What it tests                          | Serves       | Key config fields                              |
|--------|----------------------------------------|--------------|------------------------------------------------|
| `icmp` | Layer 3 reachability (ping)            | either       | `payload` (bytes, default 56)                  |
| `http` | Layer 7 + captive portal detection     | the family its URL names | `url`, `method`, `expectedStatus`, `expectedBody` |
| `tcp`  | Layer 4 TCP connect                    | the family its `host` names | `host`, `port`                    |
| `dns`  | DNS resolver reachability              | the family its `resolver` names | `resolver` (host, or host:port), `query` |
| `exec` | Custom script (exit 0 = healthy)       | either       | `command` (list)                               |
| `none` | nothing — this family is not probed    | —            | n/a                                            |

Each type's destination field is required, and a probe that omits it is refused rather
than started: an `http` probe needs `url`, `tcp` needs `host` and `port`, `dns` needs
`resolver`, `exec` needs `command`. There is no useful degraded mode for one of these —
a probe with nowhere to go fails every check, and a failing probe does not merely leave
its gateway unmeasured, it withdraws the gateway from ECMP and stops NPTv6 translating
through it. Use `type = "none"` to say a family is deliberately untested.

### One verdict per link *and family*

Each address family is measured separately and applied separately: the IPv4 verdict
decides the IPv4 default route, the IPv6 verdict decides the IPv6 default route and
whether NPTv6 keeps translating through the uplink. A link's two stacks reach the
internet by different paths and fail separately, so a dead IPv6 transit withdraws the
uplink from the IPv6 route while its IPv4 nexthop stays exactly where it was.

A link is therefore probed once per family it holds a default route in. `icmp` sends an
ICMPv6 echo to a v6 nexthop and an ICMP echo to a v4 one, and `exec` ignores the target
entirely, so both serve either family — the same block covers both, and a dual-stack link
running an `exec` probe simply runs the command twice per interval.

`health.probe` is IPv4's, and `health.probe6` is IPv6's. When `probe6` is unset, IPv6
falls back to `probe` **only if `probe` names no destination of its own**. An `http`,
`tcp` or `dns` probe carries an address or URL belonging to exactly one family, so reusing
it for the other is not a measurement — it is a probe that can never pass, and a failing
probe here does not merely fail: it withdraws the route and stops translating. The daemon
declines and warns at startup instead, naming the interface. Set `probe6`, or set
`probe6.type = "none"` to record that the family rides along untested on purpose.

`none` reads in reverse too: `probe.type = "none"` beside a real `probe6` states that an
uplink is IPv6-only, rather than leaving an IPv4 block that happens never to run.

**Absence means healthy.** An interface with no health config, a family with no probe
configured for it, a family set to `none`, and a family the link holds no default route
in are all treated as healthy — so leaving a family unmeasured costs it nothing. Gateways
also start optimistic on daemon startup, so ECMP is restored immediately after a restart
and the probe cycle corrects the state if a gateway is actually down.

`icmp` cannot probe a point-to-point link (PPP, tunnels), which has no nexthop address in
either family. Use `http`, `tcp`, `dns`, or `exec` there; the daemon warns if an ICMP
probe is configured on one.

## NPTv6 multihoming

Two ISPs and no provider-independent address space leaves IPv6 with an unpleasant choice.
Give every host an address from each delegation and you renumber the entire network every
time a lease changes; pick one ISP's prefix and you lose the second uplink the moment the
first one goes down.

NPTv6 (RFC 6296) takes the third option: hosts keep one stable internal address plan —
a ULA — and the border router rewrites the prefix on the way out, per uplink. The
translation is 1:1 and prefix-only: the host identifier (the low 64 bits) crosses
unchanged, so `fd00:dead:beef:1::10` is seen upstream as `2001:db8:a001::10`. Nothing
inside the network knows the external prefix exists, and a lease change is invisible to it.

```
  LAN address plan (stable)        route-balancer            uplinks (whatever they hold now)

 main   fd00:dead:beef:1::/64 ─┐                     ┌── wan0   ISP-A, /56 delegation
 guest  fd00:dead:beef:2::/64 ─┼── prefix rewrite ───┤          main  → 2001:db8:a001::/64
 iot    fd00:dead:beef:3::/64 ─┘   per uplink        │          guest → 2001:db8:a001:1::/64
                                                     │          iot   → 2001:db8:a001:2::/64
                                                     │
                                                     └── wan1   ISP-B, bare /64
                                                                main  → 2001:db8:b001::/64
                                                                (guest and iot do not fit)
```

### Where the external prefix comes from

| `prefixSource` | Observes | Attribution | Typical use |
|---|---|---|---|
| `route` (default) | the RFC 7084 discard route a DHCPv6-PD client installs for the delegation | `matchPrefix` | any ISP that delegates a prefix |
| `ra` | a Router Advertisement's Prefix Information Option — always a /64 — as an `RTM_NEWPREFIX` event, or as the SLAAC address it left behind ([below](#the-ra-source-and-who-processes-router-advertisements)) | interface index | LTE/5G links that hand out a bare /64 and delegate nothing |
| `static` | a fixed prefix from `staticPrefix` | n/a | static allocations, 6in4 tunnels |

The `route` source is the default because DHCPv6-PD itself is invisible to the kernel: it
is a userspace UDP exchange between the PD client and the ISP, so there is no netlink
event announcing "you have been delegated a /56". What *is* visible is the reject route
every conformant CE router installs to drop traffic for the unassigned part of the
delegation (RFC 7084), which systemd-networkd installs by default:

```
unreachable 2001:db8:a001::/56 dev lo proto dhcp
```

That route is the only object in the kernel carrying the delegation's true **length**. An
address derived from the delegation cannot substitute for it — it is always a /64,
whatever was delegated.

### The `ra` source and who processes Router Advertisements

`RTM_NEWPREFIX` is emitted only by the **kernel's** RA implementation, and on a modern
NixOS host the kernel is frequently not what processes RAs. That does not stop the `ra`
source working — it changes how quickly it notices a renumber — so it is worth knowing
which of the two you have.

The source learns a prefix one way and hears about it two ways, and it is worth keeping
those apart.

**What it reads** is always the address SLAAC built out of the Prefix Information Option:
a re-read takes the uplink's dynamic global addresses and picks the freshest, which
carries the advertised prefix by definition. A userspace RA client installs that address
too, non-permanent and with the PIO's own lifetimes, so the re-read finds it whoever did
the processing. "Freshest" means `preferred_lft` first and only then `valid_lft`, because
a graceful renumber re-advertises the outgoing prefix with `preferred_lft 0` while its
valid lifetime keeps counting down — rank on lifetime alone and you pick the prefix being
retired.

**What schedules that re-read** is any of three things: the `RTM_NEWPREFIX` event, which
only the kernel's RA implementation emits; an `RTM_NEWADDR`, which fires wherever the
address came from; or the periodic resync. They are not different answers — all three run
the same read — so the only thing at stake between them is latency.

| Who processes RAs | `RTM_NEWPREFIX` | `RTM_NEWADDR` | A renumber is noticed |
|---|---|---|---|
| the kernel — `accept_ra = 2`, nothing else soliciting | yes | yes | when the RA arrives |
| systemd-networkd (`IPv6AcceptRA = yes`) | no | yes | when the address appears |
| dhcpcd soliciting for itself | no | yes | when the address appears |

The address trigger is why the second and third rows are no longer a downgrade. It is a
trigger and not a prefix source: it carries no prefix of its own and decides nothing,
because one address is not a fact about a lease in either direction — privacy extensions
put several addresses in one prefix, and a graceful renumber puts one host in two. Sorting
that out is the re-read's job, and it does it by re-reading the whole set rather than by
maintaining a count.

**systemd-networkd never leaves RAs to the kernel.** From `systemd.network(5)` under
`IPv6AcceptRA=`: *"the kernel's implementation of the IPv6 RA protocol is always disabled,
regardless of this setting"*. A networkd-managed link reads `accept_ra = 0` whatever you
declare in `boot.kernel.sysctl` — the module sets that sysctl for `ra` uplinks and it is
simply overwritten at link-configure time. Setting `IPv6AcceptRA = no` does **not** hand
RA processing back to the kernel; it means nothing processes RAs at all, which takes the
address away too and leaves the source with nothing to read. Use it only if you are also
taking the link out of networkd's management.

**Scripted networking (dhcpcd) is conditional, and one line from the fast path.** dhcpcd
disables kernel RA only on links where it runs its own router solicitation, and nixpkgs
decides that per interface: it emits `noipv6rs` for interfaces carrying a **static** IPv6
address. A WAN uplink normally has none, so dhcpcd solicits and the kernel goes quiet.
`networking.dhcpcd.IPv6rs = false` emits `noipv6rs` globally through the same code path,
which restores kernel RA processing and with it the event latency.

All of the above is asserted by `nptv6-ra-userspace-vm`, which stands up one radvd and
five clients differing only in who owns RA processing: the networkd nodes saw zero prefix
events across a run in which the kernel-RA node saw eighteen, and every node's SLAAC
address was readable throughout. It exists as a tripwire — a systemd or nixpkgs release
that moves RA ownership again would otherwise change this silently.

**Keep the periodic resync on anyway.** It used to be the only thing standing between a
userspace RA client and a lease that was correct at boot and silently stale ever after;
the address trigger has taken that job. What it still covers is the trigger not arriving —
a missed message, a closed socket, an address the daemon was not running to see — and a
fallback that only runs when the fast path already worked is not a fallback. The NixOS
module sets `reconcileInterval = "30s"`; a hand-written JSON config that omits
`reconcile_interval` has no periodic re-read at all.

`nptv6-ra-networkd-vm` runs the daemon against all of this rather than the platform
alone: a networkd router with `accept_ra` at 0 and not one prefix event in the run,
tracking a graceful renumber with no restart — and beside it the same node with
`reconcileInterval = null`, which has no schedule but the address trigger and tracks the
same renumber anyway. Running the pair is what separates what the daemon concludes from
when it gets round to concluding it.

### Attribution: why `matchPrefix` exists

A reject route has no output device, so the kernel parks it on loopback. With two ISPs
delegating simultaneously, the two discard routes are indistinguishable: nothing in either
one says which uplink it belongs to.

Guessing is not a safe default. Attributing ISP-A's delegation to the uplink facing ISP-B
makes the router source packets out of one provider bearing another provider's prefix,
which transit providers drop silently as BCP38 spoofing — a failure that looks like a
routing problem and is diagnosed as one, for a long time.

`matchPrefix` resolves it with the one thing that is stable: the ISP's **aggregate**
allocation (e.g. `2001:db8:a000::/36`). The delegation changes on renewal; the RIR
allocation it is carved from does not. It is required only when more than one uplink uses
`prefixSource = "route"`, and `ra` never needs it because a Router Advertisement arrives
on the link it describes.

With a single `route` uplink it is optional but still worth setting. A delegation is
always inside `2000::/3`, and only routes in that range are considered — so a parked
`blackhole fd00::/8` or a link-local aggregate is never mistaken for one. Anything else
your router discards globally still can be: a bird or frr aggregate, a container
runtime's reject route. When more than one candidate matches, the daemon takes the last
one it saw and says so:

```
level=WARN msg="More than one observed prefix could be this uplink's delegation —
     taking the last one seen, which may differ from pass to pass" uplink=wan0
     source=route candidate=2001:db8:abcd::/56 also=2a02:1234:5678::/56
```

That warning means the external prefix can swap between passes, taking every translated
flow with it. `matchPrefix` is the fix.

There is no exact alternative available today: systemd's `[DHCPv6]` section has no
`RouteTable=` setting (checked against systemd 258), so the discard route always lands in
the main table and cannot be told apart by table ID either.

### Delegation size and `subnetPriority`

Both halves of a mapping are /64s. Rewriting a /64 into a shorter external prefix would
leave the boundary between prefix and host identifier ambiguous, so a delegation is sliced
into as many /64s as it holds and each named subnet takes one:

| Delegation | /64s it holds | Subnets mapped |
|---|---|---|
| /56 | 256 | all of them |
| /60 | 16 | all of them |
| /62 | 4 | the first four in `subnetPriority` |
| /64 | 1 | the first one in `subnetPriority` |

`subnetPriority` is what makes the shortfall an operator decision rather than an arbitrary
one. A /64 delegation — an LTE backup, or an ISP that quietly shortens what it hands out —
maps exactly one subnet, and the list says which. Subnets that did not fit are logged:

```
WARN NPTv6: delegation too small to map every subnet uplink=eth1 dropped=guest,iot
```

Traffic from an unmapped subnet leaves untranslated, still bearing its ULA source, and is
dropped by the first router upstream that has no route back to it. That is the intended
outcome: the alternative is two subnets sharing one external /64, which is not a 1:1
mapping and breaks the return path.

### Leak protection

The same "leaves untranslated" outcome is *not* intended when an uplink is translating
nothing at all — it holds no lease yet, or its IPv6 health probe has withdrawn it. There the
traffic has nowhere legitimate to go, and letting it leave with a ULA source means it dies
silently several hops away, indistinguishable from the uplink simply being broken.

`nptv6.leakProtection` (on by default) installs one rule per idle uplink:

```
chain guard {
  type filter hook postrouting priority srcnat + 10; policy accept;
  ip6 saddr fd00:dead:beef::/48 oifname "wan1" drop comment "rb-nptv6-guard wan1"
}
```

The chain hooks after `srcnat`, so translation has already had its chance at the packet —
anything still carrying an internal source is traffic no rule claimed. The drop happens
locally, where `nft list table ip6 route-balancer-nptv6 -a` shows the rule and the daemon
names the uplink:

```
WARN NPTv6: uplink is not translating — internal traffic leaving it is dropped locally
     uplinks=wan1 internal=fd00:dead:beef::/48
```

Only uplinks with **no** mappings whatsoever are guarded. An uplink mapping some subnets
but not others is the `subnetPriority` case above — a decision the operator made — and its
unmapped subnets keep leaving untranslated. Set `nptv6.leakProtection = false` to restore
the older behaviour, where an uplink translating nothing simply has no rules.

This matters most with `ipv6Ecmp` left off, which is the default. With IPv6 management on,
the same v6 verdict that stopped translation withdraws the uplink's v6 nexthop, so there
is usually no route left for the traffic to leave by; with it off, the v6 default route
via a sick uplink is untouched and the guard is the only thing standing between a probe
failure and a silent black hole.

### Port rules over IPv6

`rules` pins traffic to an uplink by destination port. Over IPv4 that is all it does —
something at the border NATs the source, so a flow forced out a second uplink still
leaves with an address that uplink owns. IPv6 has no such backstop: a LAN host's address
comes from one uplink's delegation, and sending its packets out another produces a source
the second ISP drops. So an IPv6 rule needs `nptv6` (the daemon refuses to start
otherwise), and `nptv6`'s `snat` rule keys on `oifname` — routing picks the uplink, and
translation follows whatever it picked.

That leaves one question, which the daemon answers by construction rather than by asking
you to keep two settings in agreement: **a rule steers only the subnets its gateway is
actually translating.**

```nix
rules = [
  { matchDstPort = [ 443 ]; gateway = "wan1"; matchFamily = "ipv6"; }
];

nptv6.uplinks.wan1.subnetPriority = [ "main" ];   # not "guest"
```

becomes one mark rather than two:

```
table ip6 route-balancer {
  chain mangle_prerouting {
    type filter hook prerouting priority mangle; policy accept;
    ip6 saddr fd00:dead:beef:1::/64 tcp dport { 443 } meta mark set 2
  }
}
```

`guest` is not marked, so its port-443 traffic keeps using the ECMP route. Marking it
would send it out `wan1`, where no `snat` rule claims it — it would meet the [leak
guard](#leak-protection)'s drop if `wan1` were idle, and leave with a `fd00::` source if
it were not. Neither failure is visible from here, which is why the rule does not
express it in the first place.

Three consequences worth knowing:

- **Health and leases need no wiring.** An uplink whose probe has withdrawn it, or whose
  lease has gone, is assigned no subnets — so it is marked for nothing, and traffic the
  rule used to pin falls back to ECMP on its own. (IPv4 marks have no such feedback:
  they keep pointing at a dead uplink until the config changes.)
- **A lease renewal changes nothing.** The marks match the *internal* prefix, which is
  fixed by your address plan. Only a change in which subnets are mapped rewrites them.
- **Locally originated traffic is not steered.** There is no `output` chain in the ip6
  table: this host's own IPv6 traffic leaves with a global address of its own, which
  `nptv6` does not translate, so steering it to an uplink that did not lend it that
  address would only produce a packet the far side drops.

The rest is the IPv4 mechanism with a family flag: the mark selects `ip -6 rule fwmark N
lookup <table>`, and that table holds one default route via the uplink. It carries no
`from <src>` rule and no `src` on the route — a fwmark rule selects on the mark, and the
kernel picks a source from the outgoing interface, which is what keeps this working while
privacy extensions churn the interface's addresses underneath it.

Requires `firewall = "nftables"`; there is no `ip6tables` backend, and `nptv6` already
requires nftables.

### Inbound reachability, and whose job it is

**Inbound-initiated connections are closed by default, per uplink.** Set
`nptv6.uplinks.<name>.allowInboundConnections = true` to open one, and the filtering is
then yours to write.

The translation is symmetric by construction. A mapping needs a postrouting SNAT and a
prerouting DNAT covering the whole external /64, because a 1:1 prefix map with no return
path is not a prefix map — the reply to anything the LAN sends is addressed to the
translated form and has to be rewritten back. But the DNAT is not what carries that reply.
The rules live in a `type nat` chain, so the chain is consulted once, on the first packet
of a flow, and every reply after that is reverse-translated from the conntrack entry
without it being consulted again. Delete the DNAT and a LAN-initiated flow keeps working.

What the DNAT alone decides is whether a packet **nobody asked for** can reach
`<internal>::<hostid>` by being addressed to `<external>::<hostid>`. That is the property
RFC 6296 has and NAT66 does not, it is a legitimate thing to want, and it is not something
that should arrive as a side effect of switching translation on. So it is asked for.

With `allowInboundConnections = false` — the default — the uplink gets no prerouting rule,
and gets this in its place:

```
chain inbound {
  type filter hook prerouting priority dstnat - 10; policy accept;
  ip6 daddr 2001:db8:abcd::/64 iifname "wan0" ct state new counter drop comment "rb-nptv6-closed wan0 main"
}
```

Three things about that rule are load-bearing:

- **It matches the external prefix, not the internal one.** With no DNAT there is nothing
  to put an internal address in the destination of an arriving packet, so a rule written
  the other way round would count zero forever and prove nothing.
- **It is `ct state new`.** Replies arrive with that same external destination and are
  un-translated by conntrack in the nat chain *below* this one, so on address alone they
  are indistinguishable from an unsolicited packet. `new` is what tells them apart.
  Inbound ICMPv6 errors are `related` and pass too, which is what keeps path MTU discovery
  working.
- **It is installed rather than merely omitted.** Left out, an inbound packet for the
  external prefix is not dropped for want of a route — it matches the default route on a
  `route` uplink, or the on-link prefix on an `ra` one, and is reflected straight back out
  the uplink it came from. This drops it where `nft` counts it and the daemon names the
  uplink.

On an `ra` uplink the external prefix *is* the upstream's on-link /64, which contains this
host's own WAN address, so the closed state covers connections to the host there as well.
Enabling `nptv6` already made that address unusable — the DNAT rewrote its prefix like any
other — so what changes is which mechanism refuses, not whether it works.

This is the opposite of the IPv4 habit, and it is why the default is what it is. Behind
NAPT the absence of a port forward *is* a firewall, by accident of there being nowhere for
an unsolicited packet to go. NPTv6 has somewhere to go for every address in the mapped
/64, so the filtering that was free under IPv4 has to be written down — and an operator
who has not yet thought about it should end up with no inbound v6 rather than an exposed
LAN. The first is a feature request; the second is an incident.

The daemon says which it did, on every apply:

```
INFO NPTv6: inbound-initiated connections are dropped at these uplinks uplinks=wan1
WARN NPTv6: uplink is reachable from the internet at its translated addresses uplinks=wan0
```

#### Once you have opened one

`allowInboundConnections = true` restores the DNAT, and with it the whole external /64.
Which hosts and which ports may be reached is not something the daemon can infer, and it
grows no option for it: whatever it generated would sit beside the host firewall as a
second, partly-informed opinion about the forward path, in a table the operator did not
write and would have to reverse-engineer before trusting.

On NixOS the obvious defence does not work. `networking.firewall.filterForward = true`
builds a `policy drop` forward chain whose `new` verdict jumps to `forward-allow`, and
that chain begins with:

```
ct status dnat accept comment "allow port forward"
```

A packet matched by the NPT prerouting rule has `IPS_DST_NAT` set by the time it reaches
the forward hook, so it is accepted there — ahead of `extraForwardRules`, with no hook to
get a rule in front of it. The defence has to be its own base chain at a lower priority:

```nix
networking.nftables.tables.wan6-guard = {
  family = "ip6";
  content = ''
    chain forward {
      type filter hook forward priority filter - 10; policy accept;
      ct state established,related accept
      iifname { "wan0", "wan1" } ip6 daddr fd00:dead:beef::/48 drop
    }
  '';
};
```

`accept` in a base chain ends only *that* chain, so established and related traffic still
falls through to everything else hooked at forward — the NixOS chain included. Only the
unsolicited inbound packets are dropped, and only the ones addressed into the internal
plan. Loosen it per subnet, per source, or per port from there; that is the point of it
being yours.

Note the difference between that guard and the daemon's own closed rule. The guard hooks
at forward, downstream of the DNAT, so it matches the *internal* prefix; the daemon's rule
hooks before the DNAT and matches the *external* one. Copying the match expression from
one into the other produces a rule that can never fire.

The module warns when an uplink sets `allowInboundConnections` and no nftables table on
the host mentions `internalPrefix`. That is evidence of absence rather than proof: a guard
that names an interface variable or a named set will not be seen, and the warning can be
ignored. It cannot key on `filterForward`, for the reason above.

Where the line sits: **whether** inbound-initiated reachability exists at all is a
capability, and only the daemon controls it, because the daemon is what installs the rule
that creates it. **Which** hosts and ports may be reached is policy, and the firewall is
where all of the context for that already is. The daemon's claim on your ruleset stops at
translation.

### Configuration

The realistic hard case — one ISP delegating a /56, a backup that offers only a bare /64
from an RA:

```nix
services.route-balancer = {
  enable = true;

  gateways = {
    wan0 = { weight = 3; description = "Fibre"; health.probe.type = "icmp"; };
    wan1 = { weight = 1; description = "LTE backup"; };
  };

  nptv6 = {
    enable = true;
    internalPrefix = "fd00:dead:beef::/48";

    subnets = {
      main  = { prefix = "fd00:dead:beef:1::/64"; description = "Trusted LAN"; };
      guest = { prefix = "fd00:dead:beef:2::/64"; };
      iot   = { prefix = "fd00:dead:beef:3::/64"; };
    };

    uplinks = {
      wan0 = {
        prefixSource   = "route";              # DHCPv6-PD delegation
        matchPrefix    = "2001:db8:a000::/36"; # this ISP's aggregate
        subnetPriority = [ "main" "guest" "iot" ];
      };
      wan1 = {
        prefixSource   = "ra";                 # bare /64 from an RA
        subnetPriority = [ "main" ];           # only one subnet fits a /64
      };
    };
  };
};

networking.nftables.enable = true;
```

An uplink that is also declared in `gateways` only carries translation rules while its
**IPv6** health probe passes — the same verdict that decides whether it is in the IPv6
ECMP route, since IPv6 is the stack this traffic leaves over. A failing IPv4 probe on the
same uplink says nothing about it and does not stop translation. An uplink that is not a
gateway has no health signal and translates whenever it holds a prefix, and so does one
whose IPv6 nothing probes.

### Prerequisites

- **`networking.nftables.enable = true`** — the translation rules are nftables NETMAP
  rules. Unlike the port-based fwmark rules there is no iptables fallback; the module
  asserts this rather than failing at runtime.
- **IPv6 forwarding** — the module sets `net.ipv6.conf.all.forwarding = 1` (as a default
  you can override). Translation happens on the forwarding path, so without it every rule
  is a no-op; the daemon warns at startup if it is off.
- **A PD client that installs the discard route** — systemd-networkd does by default.
  Setting `UnassignedSubnetPolicy=none` removes the only signal the `route` source has.
- **`accept_ra = 2` on `ra` uplinks, if you want event latency** — `accept_ra` defaults to
  0 once forwarding is enabled, and 1 is ignored on a forwarding host, so only 2 makes the
  kernel process Router Advertisements at all. The module sets it and asserts if something
  else declares a different value — but a userspace RA client overwrites it at
  link-configure time, and no build-time assertion can see that. The source still works
  there, and since the address that client installs triggers the same re-read, it works at
  much the same latency; see
  [who processes Router Advertisements](#the-ra-source-and-who-processes-router-advertisements)
  for which case you are in and what each costs.
- **A periodic resync, as the backstop** — `reconcileInterval` (the module defaults it to
  30s) re-reads the SLAAC address on a schedule. An `RTM_NEWADDR` asks for the same
  re-read as soon as the address appears, so a renumber behind networkd or a soliciting
  dhcpcd is noticed without it; what the interval covers is the event not arriving.
- **Internal addressing** — hosts need addresses inside `internalPrefix` (advertise the
  ULA subnets on the LAN yourself, e.g. via `IPv6SendRA`). route-balancer translates; it
  does not hand out addresses.

### Proxy NDP for an on-link external prefix

A translated address exists only inside nftables. No interface holds it, so nothing on the
uplink answers a neighbour solicitation for it. That never comes up when the upstream
router *routes* your prefix to you — which is what a DHCPv6-PD delegation and a 3GPP PDN
connection both do — but a phone sharing its own /64 advertises it as on-link and keeps it
connected, and then every reply to a translated address is solicited on the link and goes
unanswered. The translation is correct and the prefix is unreachable.

route-balancer answers those solicitations itself, on by default for `prefixSource = "ra"`
uplinks and off elsewhere:

```nix
nptv6.uplinks.wan1 = {
  prefixSource   = "ra";
  subnetPriority = [ "main" ];
  proxyNdp       = true;   # the default for "ra"; set false to opt out
};
```

The kernel has no prefix-wide NDP proxy — `ip -6 neigh add proxy` takes one address at a
time — so the daemon has to know which addresses to claim. It learns them from the data
plane rather than from the LAN neighbour table, because a host caches the router's MAC
keyed by the router's address and can start sourcing from a freshly rotated privacy
address without ever soliciting again. An nftables dynamic set records the source address
of forwarded traffic; NETMAP preserves the low 64 bits, so the translated form is just the
current external prefix with that host identifier spliced in, and a renumber recomputes
every entry without relearning anything.

Three consequences worth knowing:

- **Inbound-first connections to a silent host don't work.** An identifier that has not
  been seen for `proxyNdpTimeout` (default 1h, refreshed by every forwarded packet) ages
  out and stops being answered for until that host speaks again. The reply path for
  anything the LAN initiated is unaffected. On a tethered uplink the carrier firewalls
  inbound anyway.
- **The first flow from a never-seen identifier can stall briefly.** nftables emits no
  notification when the packet path adds a set element, so the set is only read back on a
  reconcile pass — which means `reconcileInterval` (default 30s) bounds how long a host
  identifier nobody has seen before waits to become reachable from the other side. The
  sender's own retransmit covers it, and it happens once per host rather than once per
  connection, but shorten `reconcileInterval` if that wait matters to you.
- **An address someone else answers for is claimed briefly, then retracted.** The external
  prefix is shared with the operator, and because the host identifier is preserved a LAN
  host can map straight onto the phone's own address. Detecting that takes one round trip,
  while *proving* an address is free takes the kernel's whole solicitation burst — so an
  address with no history is claimed at once and checked immediately after, and retracted
  a hundred milliseconds or two later if anything answers — the probe polls the neighbour
  table at that granularity, and the retraction itself runs on the daemon's event loop. A host identifier that has ever been contested is
  proved free before every later claim — across renumbering too — so the window is one
  sub-second exposure per identifier for the life of the daemon. The one thing to know about that window: a proxy entry answers
  duplicate-address-detection probes, so an owner that brings the address up *during* it
  will see DAD fail. Both halves are logged (`claim=speculative` / `claim=probed`).

The learned set is capped (`proxyNdpMaxHosts`, default 256) by the kernel rather than by
the daemon, because each entry counts against `net.ipv6.neigh.default.gc_thresh1/2/3`
(128/512/1024) and an uncapped set would let a host spraying source addresses evict genuine
neighbours. A clean shutdown removes the set along with everything else, so a restart
starts from nothing and relearns on the first forwarded packet from each host.

Entries are tagged `proto 111` (the configured `routeProto`), so `ip -6 neigh show proxy
proto 111` shows exactly what the daemon installed, and shutdown removes exactly that.
`ndppd` remains an alternative if you would rather answer for the whole /64 unconditionally
and keep inbound-first connections.

### Known limitations

- **The translation is stateful, not stateless.** RFC 6296 specifies a stateless,
  checksum-neutral algorithm. nftables `snat … prefix` is NETMAP, which runs through
  `nf_nat` and therefore conntrack; Linux has no true stateless NPTv6 implementation, so
  this is inherent rather than a shortcut. Consequences:
  - conntrack table usage scales with flow count;
  - a flow that leaves via one uplink and returns via another breaks — which is the
    multihoming case this feature targets, so keep return traffic symmetric;
  - checksum neutrality is moot, since `nf_nat` fixes checksums directly;
  - on the upside, ICMPv6 errors are tracked as RELATED and `nf_nat` rewrites the
    addresses embedded in their payloads, so RFC 6296's embedded-header requirement is met
    without explicit rules — verified in both directions by `nptv6-forwarding-vm`,
    including a LAN host caching a path MTU it learned through a translated error.
- **Proxy NDP only covers hosts that have been heard from.** An on-link external prefix
  needs something to answer neighbour solicitations for translated addresses, which
  route-balancer does (see above) — but only for host identifiers learned from forwarded
  traffic, so an inbound-first connection to a host silent for `proxyNdpTimeout` fails
  until it speaks again. `ndppd` answers for the whole /64 instead if that matters more
  than the attack surface and the address-collision handling it gives up.
- **IPsec AH does not survive translation.** AH authenticates the IP header including the
  addresses. This is inherent to NPTv6, not to this implementation; ESP is unaffected.
- **No iptables backend** for NPTv6.
- **Inbound is all or nothing, per uplink.** `allowInboundConnections = false` (the
  default) means none, for anyone; `true` means the whole external /64 and a host firewall
  is needed again. There is no port forwarding layer here because there is nothing to
  forward past: the DNAT half covers the whole /64, and restricting it is the host
  firewall's job. See
  [Inbound reachability](#inbound-reachability-and-whose-job-it-is).

## NixOS Module Options

### Core

| Option                          | Type           | Default           | Description                               |
| ------------------------------- | -------------- | ----------------- | ----------------------------------------- |
| `enable`                        | bool           | `false`           | Enable the daemon                         |
| `package`                       | package        | —                 | The daemon to run. Required: the module declares no default |
| `gateways`                      | attrset        | {}                | Per-interface weight + health config      |
| `gateways.<name>.weight`        | int 0–100      | 1                 | ECMP weight (0 = exclude from ECMP)       |
| `gateways.<name>.description`   | str            | ""                | Human-readable label                      |
| `rules`                         | list           | []                | Destination-port policy routing rules     |
| `rules[].matchDstPort`          | [int]          | null              | Destination ports to match (required)     |
| `rules[].matchProtocol`         | tcp\|udp\|both | `"both"`          | Protocol for port-based rules             |
| `rules[].matchFamily`           | ipv4\|ipv6\|both | null (= ipv4)   | Address family the rule steers; ipv6 needs `nptv6` |
| `rules[].gateway`               | str            | —                 | Interface name to route to                |
| `firewall`                      | enum           | `"iptables"`      | Firewall backend for port marks           |
| `routeProto`                    | int 1–255      | 111               | Protocol number stamped on managed routes |
| `routeTableOffset`              | int 1–251      | 100               | Base for per-gateway table IDs (offset + ifIndex must stay below 253) |
| `iptablesChain`                 | str            | `"ROUTE-BALANCER"`| iptables mangle chain name                |
| `nftablesTable`                 | str            | `"route-balancer"`| nftables table name (must differ from `nptv6.nftablesTable`) |
| `hashPolicy`                    | 0\|1\|2        | 1                 | Kernel ECMP hash policy, set at boot for each managed family |
| `ipv4Ecmp`                      | bool           | true              | Manage the IPv4 default route              |
| `ipv6Ecmp`                      | bool           | false             | Also manage the IPv6 default route         |
| `ipv6Metric`                    | int 1–1023     | 1                 | Metric for the managed IPv6 route          |
| `logLevel`                      | enum           | `"info"`          | Log verbosity                             |
| `reconcileInterval`             | str\|null      | `"30s"`           | How often to check and restore managed state; null = the 30s default |

### Health monitoring (`gateways.<name>.health`)

| Option               | Type   | Default | Description                                          |
|----------------------|--------|---------|------------------------------------------------------|
| `unhealthyThreshold` | int    | 3       | Consecutive failures before removing from ECMP       |
| `healthyThreshold`   | int    | 5       | Consecutive successes before re-adding to ECMP       |
| `interval`           | string | `"5s"`  | Probe interval (Go duration)                         |
| `timeout`            | string | `"2s"`  | Per-probe deadline (Go duration)                     |
| `probe.type`         | enum   | `"icmp"`| Probe type: `icmp` `http` `https` `tcp` `dns` `exec` `none` |
| `probe6`             | attrs\|null | null | IPv6 probe, same fields as `probe`; null = fall back to `probe` when it can serve IPv6 |
| `probe.payload`      | int    | 56      | ICMP payload size in bytes (`icmp`)                  |
| `probe.url`          | string | —       | URL to GET (`http`/`https`)                          |
| `probe.method`       | string | `"GET"` | HTTP method (`http`/`https`)                         |
| `probe.expectedStatus` | [int] | `[200]`| Accepted status codes (`http`/`https`)               |
| `probe.expectedBody` | string | —       | Required response body substring (`http`/`https`)    |
| `probe.host`         | string | —       | Host to connect to (`tcp`)                           |
| `probe.port`         | int    | —       | Port to connect to (`tcp`)                           |
| `probe.resolver`     | string | —       | DNS resolver host, or `host:port` (`dns`)            |
| `probe.query`        | string | `"example.com"` | Domain to resolve (`dns`)                  |
| `probe.command`      | [str]  | —       | Command argv; exit 0 = healthy (`exec`)              |

### NPTv6 (`nptv6`)

| Option                              | Type                    | Default                    | Description                                              |
|-------------------------------------|-------------------------|----------------------------|----------------------------------------------------------|
| `nptv6.enable`                      | bool                    | false                      | Enable IPv6 prefix translation                           |
| `nptv6.internalPrefix`              | str                     | —                          | Internal address plan, normally a ULA /48                |
| `nptv6.allowNonUla`                 | bool                    | false                      | Permit an internal prefix outside `fd00::/8`             |
| `nptv6.nftablesTable`               | str                     | `"route-balancer-nptv6"`   | ip6 table holding the translation rules (must differ from `nftablesTable`, and from it with `-ndp` appended) |
| `nptv6.leakProtection`              | bool                    | true                       | Drop internal-sourced traffic leaving an uplink that is not translating |
| `nptv6.subnets.<name>.prefix`       | str                     | —                          | Internal /64 for this subnet                             |
| `nptv6.subnets.<name>.description`  | str                     | `""`                       | Human-readable label                                     |
| `nptv6.uplinks.<name>.prefixSource` | `route`\|`ra`\|`static` | `"route"`                  | How the external prefix is discovered                    |
| `nptv6.uplinks.<name>.matchPrefix`  | str\|null               | null                       | ISP aggregate used to attribute a delegation             |
| `nptv6.uplinks.<name>.staticPrefix` | str\|null               | null                       | External prefix for `prefixSource = "static"`            |
| `nptv6.uplinks.<name>.subnetPriority` | [str]                 | `[]`                       | Subnets to map through this uplink, most important first |
| `nptv6.uplinks.<name>.proxyNdp`     | bool\|null              | null (`ra` → on)           | Answer neighbour solicitations for translated addresses  |
| `nptv6.uplinks.<name>.allowInboundConnections` | bool         | false                      | Let the internet start connections to the subnets this uplink maps |
| `nptv6.proxyNdpMaxHosts`            | int\|null               | null (256)                 | Cap on learned host identifiers per uplink               |
| `nptv6.proxyNdpTimeout`             | str\|null               | null (`"1h"`)              | Idle timeout for a learned host identifier               |

## Requirements

- Linux kernel ≥ 4.4 (multipath routing support)
- `CAP_NET_ADMIN` + `CAP_NET_RAW` (granted automatically by the NixOS module)
- `networking.nftables.enable = true` when `firewall = "nftables"`, and always when `nptv6.enable = true`
- User/DHCP default routes should use a metric > 0 so route-balancer can install its ECMP route at metric 0
- For NPTv6: IPv6 forwarding (set by the module), a DHCPv6-PD client that installs the RFC 7084 discard route, and `accept_ra = 2` on any `ra` uplink

## License

MIT
