{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer-ipv6-ecmp";

  nodes = {
    machine = { lib, pkgs, ... }: {
      imports = [ (import ../module/route-balancer.nix) ];

      virtualisation.vlans = [ 1 2 3 ];

      networking = {
        useDHCP = lib.mkForce false;

        interfaces = {
          eth1 = {
            ipv4.addresses = [{ address = "10.0.1.2"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "2001:db8:a1::2"; prefixLength = 64; }];
          };
          eth2 = {
            ipv4.addresses = [{ address = "10.0.2.2"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "2001:db8:a2::2"; prefixLength = 64; }];
          };
          eth3 = {
            ipv4.addresses = [{ address = "10.0.3.2"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "2001:db8:a3::2"; prefixLength = 64; }];
          };
        };
      };

      boot.kernel.sysctl = {
        "net.ipv6.conf.all.forwarding" = 1;
        "net.ipv6.conf.eth1.accept_ra" = 0;
      };

      systemd.tmpfiles.rules = [ "f /run/wan1-healthy 0644 root root - -" ];

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };

        ipv6Ecmp = true;
        ipv6Metric = 5;

        gateways = {
          eth1 = {
            weight = 3;
            description = "Primary — health gates both families";
            health = {
              unhealthyThreshold = 2;
              healthyThreshold = 2;
              interval = "1s";
              timeout = "1s";
              probe = {
                type = "exec";
                command = [ "/bin/sh" "-c" "test -f /run/wan1-healthy" ];
              };
            };
          };
          eth2 = { weight = 1; description = "Backup"; };
        };

        reconcileInterval = "3s";
      };
    };

    optout = { lib, pkgs, ... }: {
      imports = [ (import ../module/route-balancer.nix) ];

      virtualisation.vlans = [ 1 ];

      networking = {
        useDHCP = lib.mkForce false;
        interfaces.eth1 = {
          ipv4.addresses = [{ address = "10.0.1.9"; prefixLength = 24; }];
          ipv6.addresses = [{ address = "2001:db8:a1::9"; prefixLength = 64; }];
        };
      };

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };
        gateways.eth1 = { weight = 1; };
        reconcileInterval = "3s";
      };
    };
  };

  testScript = ''
    start_all()

    machine.wait_for_unit("network.target")
    machine.wait_for_unit("route-balancer.service")

    # The managed route sits at the configured metric; uplink routes go near 1024.
    MANAGED = "ip -6 route show default metric 5 proto 111"
    UPLINKS = {"eth1": 1024, "eth2": 1025, "eth3": 1026}

    def managed():
        return machine.succeed(f"{MANAGED} 2>/dev/null || true")

    def add_uplink(gw, dev):
        machine.succeed(f"ip -6 route add default via {gw} dev {dev} metric {UPLINKS[dev]}")

    def del_uplink(gw, dev):
        machine.succeed(f"ip -6 route del default via {gw} dev {dev} metric {UPLINKS[dev]}")

    def uplink_route_present(gw, dev):
        machine.succeed(
            f"ip -6 route show default metric {UPLINKS[dev]} | grep -q 'via {gw}'"
        )

    def wait_nexthop(gw, timeout=15):
        machine.wait_until_succeeds(f"{MANAGED} | grep -q 'via {gw}'", timeout=timeout)

    def wait_no_nexthop(gw, timeout=15):
        machine.wait_until_fails(f"{MANAGED} 2>/dev/null | grep -q 'via {gw}'", timeout=timeout)

    ETH1 = "2001:db8:a1::1"
    ETH2 = "2001:db8:a2::1"
    ETH3 = "2001:db8:a3::1"

    # ── 0. What the module is responsible for ────────────────────────────────
    with subtest("module wires ipv6Ecmp through to the daemon and the kernel"):
        cfg = machine.succeed(
            "cat $(systemctl show -p ExecStart --value route-balancer.service | "
            "grep -o '/nix/store/[^ ]*\\.json' | head -1)"
        )
        assert '"ipv6_ecmp":true' in cfg, cfg
        assert '"ipv6_metric":5' in cfg, cfg
        # The IPv6 fib has its own copy of the hash-policy switch, defaulting to 0.
        assert machine.succeed(
            "cat /proc/sys/net/ipv6/fib_multipath_hash_policy"
        ).strip() == "1"

    # With forwarding on, accept_ra must be 2 or the uplink never acquires a route.
    with subtest("startup warns about a gateway that cannot learn an IPv6 route"):
        machine.wait_until_succeeds(
            "journalctl -u route-balancer.service --no-pager | "
            "grep 'not processing Router Advertisements' | grep -q 'iface=eth1'",
            timeout=20,
        )

    # ── 1. Reactive: the first IPv6 gateway ──────────────────────────────────
    with subtest("reactive: first IPv6 gateway is installed at the managed metric"):
        add_uplink(ETH1, "eth1")
        wait_nexthop(ETH1)

        # The uplink's own route must survive: the nexthop depends on it.
        uplink_route_present(ETH1, "eth1")

    # The kernel rewrites a requested metric 0 on an IPv6 route to 1024.
    with subtest("the managed route does not collide with IP6_RT_PRIO_USER"):
        assert machine.succeed(
            "ip -6 route show default metric 1024 proto 111 2>/dev/null || true"
        ).strip() == "", "managed route landed at 1024, where the uplink routes live"

    # ── 2. Reactive: a second gateway produces weighted ECMP ─────────────────
    with subtest("reactive: second IPv6 gateway produces weighted ECMP"):
        add_uplink(ETH2, "eth2")
        wait_nexthop(ETH1)
        wait_nexthop(ETH2)
        route = managed()
        assert "weight 3" in route, f"eth1's configured weight is missing:\n{route}"
        assert "weight 1" in route, f"eth2's configured weight is missing:\n{route}"

    # ── 3. The two families are managed independently ────────────────────────
    # One interface carrying a default route in each family: the gateway key
    # has to be family-tagged or the second evicts the first.
    with subtest("IPv4 and IPv6 routes are managed side by side"):
        machine.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")
        machine.succeed("ip route add default via 10.0.2.1 dev eth2 metric 600")
        machine.wait_until_succeeds(
            "ip -4 route show default metric 0 proto 111 | grep -q '10.0.1.1'", timeout=15,
        )
        machine.wait_until_succeeds(
            "ip -4 route show default metric 0 proto 111 | grep -q '10.0.2.1'", timeout=15,
        )
        # Neither route leaked into the other.
        v4 = machine.succeed("ip -4 route show default metric 0 proto 111")
        assert "2001:db8" not in v4, v4
        assert "10.0." not in managed(), managed()

    # ── 4. Health withdraws the gateway from both families at once ───────────
    # An exec probe names no destination, so both families run the same command.
    with subtest("a failing probe withdraws the gateway from both families"):
        machine.succeed("rm /run/wan1-healthy")
        wait_no_nexthop(ETH1)
        machine.wait_until_fails(
            "ip -4 route show default metric 0 proto 111 | grep -q '10.0.1.1'", timeout=15,
        )
        # The healthy sibling keeps both of its nexthops.
        assert ETH2 in managed(), managed()
        machine.succeed("ip -4 route show default metric 0 proto 111 | grep -q '10.0.2.1'")

    with subtest("a recovering probe restores the gateway in both families"):
        machine.succeed("touch /run/wan1-healthy")
        wait_nexthop(ETH1)
        machine.wait_until_succeeds(
            "ip -4 route show default metric 0 proto 111 | grep -q '10.0.1.1'", timeout=15,
        )

    # ── 5. Reconcile repairs external edits ──────────────────────────────────
    with subtest("reconcile: an externally deleted IPv6 route is restored"):
        machine.succeed("ip -6 route del default proto 111")
        machine.wait_until_fails(f"{MANAGED} | grep -q 'via {ETH1}'", timeout=5)
        wait_nexthop(ETH1)
        wait_nexthop(ETH2)

    with subtest("reconcile: a nexthop removed externally is restored"):
        # Rewrite our own route down to a single nexthop, which the kernel
        # stores with no weight token.
        machine.succeed(
            f"ip -6 route replace default via {ETH2} dev eth2 metric 5 proto 111"
        )
        wait_nexthop(ETH1)
        assert ETH2 in managed(), managed()

    # ── 6. Reject routes are not uplinks ─────────────────────────────────────
    # A PD client parks "unreachable default dev lo" when it has no route to offer.
    with subtest("a reject default route is never seeded as a gateway"):
        machine.succeed("ip -6 route add unreachable default metric 2048")
        machine.systemctl("restart route-balancer.service")
        machine.wait_for_unit("route-balancer.service")
        wait_nexthop(ETH1, timeout=30)
        wait_nexthop(ETH2, timeout=30)
        route = managed()
        assert "dev lo" not in route, f"loopback seeded from a reject route:\n{route}"
        assert "unreachable" not in route, route
        machine.succeed("ip -6 route del unreachable default metric 2048")

    # ── 7. Unconfigured interfaces stay unmanaged ────────────────────────────
    with subtest("an unconfigured interface is excluded from the IPv6 route"):
        add_uplink(ETH3, "eth3")
        machine.sleep(3)
        assert ETH3 not in managed(), managed()
        del_uplink(ETH3, "eth3")

    # ── 8. Seeding recovers both families from the kernel ────────────────────
    with subtest("restart re-seeds the IPv6 gateways without waiting for an event"):
        machine.systemctl("restart route-balancer.service")
        machine.wait_for_unit("route-balancer.service")
        wait_nexthop(ETH1, timeout=30)
        wait_nexthop(ETH2, timeout=30)
        machine.wait_until_succeeds(
            "ip -4 route show default metric 0 proto 111 | grep -q '10.0.1.1'", timeout=30,
        )
        # eth1 now holds a v6 route, so the startup check has nothing to say.
        since = machine.succeed(
            "systemctl show -p ExecMainStartTimestamp --value route-balancer.service"
        ).strip()
        machine.fail(
            f"journalctl -u route-balancer.service --no-pager --since '{since}' | "
            "grep 'not processing Router Advertisements' | grep -q 'iface=eth1'"
        )

    # ── 9. Shutdown removes only what the daemon installed ───────────────────
    with subtest("shutdown removes the managed routes and leaves the uplinks alone"):
        machine.systemctl("stop route-balancer.service")
        machine.wait_until_fails(f"{MANAGED} | grep -q .", timeout=15)
        machine.wait_until_fails(
            "ip -4 route show default metric 0 proto 111 | grep -q .", timeout=15,
        )
        # Deleting by proto rather than by metric keeps the uplinks' own routes.
        uplink_route_present(ETH1, "eth1")
        uplink_route_present(ETH2, "eth2")

    # ── 10. Opt-in means opt-in ──────────────────────────────────────────────
    # Without ipv6Ecmp the daemon writes no v6 route at all.
    with subtest("with ipv6Ecmp left at its default the daemon manages no IPv6"):
        optout.wait_for_unit("network.target")
        optout.wait_for_unit("route-balancer.service")
        optout.succeed("ip -6 route add default via 2001:db8:a1::1 dev eth1 metric 1024")
        optout.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")

        # The v4 half still works, which is what makes the v6 silence meaningful.
        optout.wait_until_succeeds(
            "ip -4 route show default metric 0 proto 111 | grep -q '10.0.1.1'", timeout=15,
        )
        optout.sleep(3)
        assert optout.succeed(
            "ip -6 route show default proto 111 2>/dev/null || true"
        ).strip() == "", "IPv6 route installed without ipv6Ecmp being enabled"
  '';
}
