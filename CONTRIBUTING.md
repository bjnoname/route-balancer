# Contributing to route-balancer

## Project Structure

```
.
├── flake.nix                    # Nix flake — packages, devShell, formatter, NixOS module export
├── go.mod                       # Go module (no external deps, stdlib only)
├── main.go                      # Entry point — flags, config, startup, signal handling, event loop
├── config.go                    # Config struct, JSON loading, flag parsing
├── gateway.go                   # Gateway struct (incl. health fields), gateways map, gatewaysMu, activeGateways
├── netlink.go                   # Netlink constants and parseRouteEvent()
├── routing.go                   # applyECMP, setupGatewayRoutes, teardownGatewayRoutes, cleanupRoutes, runIP, gatewayPriority
├── iptables.go                  # iptables mangle backend for port-based fwmark rules
├── nftables.go                  # nftables backend for port-based fwmark rules + applyFwmarkRules + cleanupFwmarkRules
├── health.go                    # Probe interface, Target, HealthMonitor, monitor lifecycle, newProbe factory, bindToDeviceDialer
├── probe_icmp.go                # ICMPProbe — raw ICMP echo, SO_BINDTODEVICE
├── probe_http.go                # HTTPProbe — HTTP/HTTPS with interface-bound dialer
├── probe_tcp.go                 # TCPProbe — TCP connect with interface-bound dialer
├── probe_dns.go                 # DNSProbe — UDP DNS query with interface-bound socket
├── probe_exec.go                # ExecProbe — arbitrary command, exit 0 = healthy
├── nix/
│   ├── module/route-balancer.nix  # NixOS module (options, systemd unit, JSON config gen)
│   ├── pkgs/route-balancer.nix    # Nix derivation (buildGoModule + wrapProgram)
│   └── tests/
│       ├── route-balancer.nix     # NixOS VM integration tests (reactive ECMP, seeding, cleanup)
│       ├── health-probes.nix      # NixOS VM integration tests (ICMP/HTTP/TCP/DNS/exec probes)
│       └── dev-vm.nix             # Interactive three-NIC dev VM (nix run .#dev-vm)
```

## Commands

### Development

```bash
go build ./...             # build daemon binary
go test ./...              # run tests
go vet ./...               # static analysis
golangci-lint run          # full lint suite
nix fmt                    # format all files (Nix via nixpkgs-fmt, Go via gofmt)
```

### Nix

```bash
nix build                  # build the package
nix develop                # enter dev shell
nix fmt                    # format all Nix and Go files
nix flake check            # run all checks (build + VM tests + fmt check)
nix run .#dev-vm           # boot interactive three-NIC dev VM (Linux only)
```

### Manual routing tests (requires root)

```bash
sudo ./route-balancer --config config.json   # run daemon with config
sudo ./route-balancer                        # run without config (all defaults)
ip monitor route                             # watch route events in parallel
ip route add default via 10.0.0.1 dev eth0 metric 50  # trigger a route event
ip route show default                        # verify ECMP is applied (metric 0, proto 111)
ip route show proto 111                      # show only route-balancer managed routes
```

## Design constraints

- **No external Go dependencies** — stdlib only. This keeps the Nix derivation simple and avoids vendoring.
- **No IPv6 yet** — the netlink listener subscribes to `RTMGRP_IPV4_ROUTE` only.
- Probes must bind to the gateway interface via `SO_BINDTODEVICE` so they don't route through a sibling gateway.
- The ECMP route is always installed at metric 0. All other default routes on the system should use a metric > 0.

## Running the VM test suite

```bash
nix flake check            # builds + runs all NixOS VM tests
```

Tests live in `nix/tests/`. The integration tests spin up multi-NIC NixOS VMs using
`nixosTest` and verify reactive ECMP, health probe behaviour, seeding on startup, and
clean shutdown.
