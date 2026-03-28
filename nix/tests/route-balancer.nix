# nix/tests/route-balancer.nix
#
# NixOS VM integration test for route-balancer.
# Run with: nix build .#checks.x86_64-linux.route-balancer-vm

{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer";

  # ── Test node ──────────────────────────────────────────────────────────────
  nodes.machine = { lib, pkgs, ... }: {
    imports = [ (import ../module/route-balancer.nix) ];

    # Three extra vlans: eth1 and eth2 are configured gateways; eth3 is
    # deliberately absent from the gateways config to test exclusion.
    virtualisation.vlans = [ 1 2 3 ];

    networking = {
      useDHCP = lib.mkForce false;

      interfaces = {
        eth1.ipv4.addresses = [{ address = "10.0.1.2"; prefixLength = 24; }];
        eth2.ipv4.addresses = [{ address = "10.0.2.2"; prefixLength = 24; }];
        eth3.ipv4.addresses = [{ address = "10.0.3.2"; prefixLength = 24; }];
      };
    };

    services.route-balancer = {
      enable = true;
      package = pkgs.callPackage ../pkgs/route-balancer.nix { };
      gateways = {
        eth1 = { weight = 1; };
        eth2 = { weight = 1; };
      };
      # Exercise the new configurable fields explicitly so the generated
      # config file is tested end-to-end with non-default-looking values
      # that are still functionally equivalent to the defaults.
      routeProto = 111;
      routeTableOffset = 100;
      iptablesChain = "ROUTE-BALANCER";
      nftablesTable = "route-balancer";
      # Short interval so the reconcile test does not have to wait long.
      reconcileInterval = "3s";
    };
  };

  # ── Test script ────────────────────────────────────────────────────────────
  testScript = ''
    machine.wait_for_unit("network.target")
    machine.wait_for_unit("route-balancer.service")

    # Resolve per-gateway table IDs once: tableID = routeTableOffset + ifIndex
    eth1_idx = int(machine.succeed("cat /sys/class/net/eth1/ifindex").strip())
    eth2_idx = int(machine.succeed("cat /sys/class/net/eth2/ifindex").strip())
    eth3_idx = int(machine.succeed("cat /sys/class/net/eth3/ifindex").strip())
    table1 = 100 + eth1_idx
    table2 = 100 + eth2_idx
    table3 = 100 + eth3_idx

    # ── 1. Reactive: first gateway ───────────────────────────────────────────
    # Add a default route at a high metric so route-balancer can install its
    # own ECMP route at metric 0 without conflicting with the kernel route.
    with subtest("reactive: first gateway is installed"):
        machine.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")
        # route-balancer installs its ECMP route at metric 0 with proto 111.
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'proto 111'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q '10.0.1.1'",
            timeout=15,
        )
        # Per-provider routing table: subnet and default route with src hint
        machine.wait_until_succeeds(
            f"ip route show table {table1} | grep -q 'default via 10.0.1.1'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip route show table {table1} | grep -q 'src 10.0.1.2'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip route show table {table1} | grep -q '10.0.1.0/24'",
            timeout=15,
        )
        # ip rule: reply traffic from eth1's IP must use table1
        machine.wait_until_succeeds(
            f"ip rule show | grep -q 'from 10.0.1.2 lookup {table1}'",
            timeout=15,
        )

    # ── 2. Reactive: second gateway triggers ECMP ────────────────────────────
    # A second default route → daemon updates its ECMP route in-place
    # (replace, since proto 111 route is already at metric 0).
    with subtest("reactive: second gateway produces ECMP"):
        machine.succeed("ip route add default via 10.0.2.1 dev eth2 metric 600")
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'nexthop via 10.0.1.1'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'nexthop via 10.0.2.1'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'proto 111'",
            timeout=15,
        )
        # Per-provider table for eth2
        machine.wait_until_succeeds(
            f"ip route show table {table2} | grep -q 'default via 10.0.2.1'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip route show table {table2} | grep -q 'src 10.0.2.2'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip route show table {table2} | grep -q '10.0.2.0/24'",
            timeout=15,
        )
        # ip rule for eth2
        machine.wait_until_succeeds(
            f"ip rule show | grep -q 'from 10.0.2.2 lookup {table2}'",
            timeout=15,
        )

    # ── 3. Reactive: gateway removal collapses ECMP and cleans up table ──────
    with subtest("reactive: ECMP collapses and table is torn down on gateway removal"):
        machine.succeed("ip route del default via 10.0.2.1 dev eth2 metric 600")
        machine.wait_until_fails(
            "ip route show default metric 0 | grep -q 'nexthop via 10.0.2.1'",
            timeout=15,
        )
        # ip rule for eth2 must be gone
        machine.wait_until_fails(
            f"ip rule show | grep -q 'from 10.0.2.2 lookup {table2}'",
            timeout=15,
        )
        # Per-provider table for eth2 must be empty
        machine.wait_until_fails(
            f"ip route show table {table2} | grep -q .",
            timeout=15,
        )

    # ── 4. Startup seeding restores ECMP and tables ──────────────────────────
    with subtest("startup seeding: ECMP and tables restored after daemon restart"):
        machine.succeed("ip route add default via 10.0.2.1 dev eth2 metric 600")
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'nexthop via 10.0.2.1'",
            timeout=15,
        )
        machine.systemctl("restart route-balancer.service")
        machine.wait_for_unit("route-balancer.service")
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'nexthop via 10.0.1.1'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'nexthop via 10.0.2.1'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'proto 111'",
            timeout=15,
        )
        # Both per-provider tables must be populated after seeding
        machine.wait_until_succeeds(
            f"ip route show table {table1} | grep -q 'default via 10.0.1.1'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip route show table {table2} | grep -q 'default via 10.0.2.1'",
            timeout=15,
        )
        # Both ip rules must exist
        machine.wait_until_succeeds(
            f"ip rule show | grep -q 'from 10.0.1.2 lookup {table1}'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip rule show | grep -q 'from 10.0.2.2 lookup {table2}'",
            timeout=15,
        )

    # ── 5. Reconcile: externally modified state is restored ──────────────────
    # After test 4 both gateways are in ECMP and the daemon is running.
    # Verify that the reconciler (3 s interval) restores each kind of state
    # that can be externally modified: the ECMP route, a per-gateway table,
    # and a split-access ip rule.
    with subtest("reconcile: ECMP route deleted externally is restored"):
        machine.succeed("ip route del default proto 111")
        machine.wait_until_fails(
            "ip route show default metric 0 | grep -q 'proto 111'",
            timeout=5,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'proto 111'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'nexthop via 10.0.1.1'",
            timeout=5,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'nexthop via 10.0.2.1'",
            timeout=5,
        )

    with subtest("reconcile: per-gateway table flushed externally is restored"):
        machine.succeed(f"ip route flush table {table1}")
        machine.wait_until_fails(
            f"ip route show table {table1} | grep -q 'default via 10.0.1.1'",
            timeout=5,
        )
        machine.wait_until_succeeds(
            f"ip route show table {table1} | grep -q 'default via 10.0.1.1'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip route show table {table1} | grep -q 'src 10.0.1.2'",
            timeout=5,
        )

    with subtest("reconcile: split-access ip rule deleted externally is restored"):
        prio2 = 1000 + table2
        machine.succeed(f"ip rule del from 10.0.2.2 lookup {table2} priority {prio2}")
        machine.wait_until_fails(
            f"ip rule show | grep -q 'from 10.0.2.2 lookup {table2}'",
            timeout=5,
        )
        machine.wait_until_succeeds(
            f"ip rule show | grep -q 'from 10.0.2.2 lookup {table2}'",
            timeout=15,
        )

    # ── 7. Unconfigured interface is excluded from ECMP ──────────────────────
    # eth3 has an IP and a default route but is absent from the gateways config.
    # The daemon must ignore it: no nexthop in the ECMP route, no per-gateway
    # routing table, and no ip rule.
    with subtest("unconfigured interface is excluded from ECMP"):
        machine.systemctl("start route-balancer.service")
        machine.wait_for_unit("route-balancer.service")
        machine.succeed("ip route add default via 10.0.3.1 dev eth3 metric 700")
        # Give the daemon time to process the netlink event, then assert the
        # unconfigured gateway never appears in the ECMP route.
        machine.succeed("sleep 2")
        machine.fail(
            "ip route show default metric 0 | grep -q '10.0.3.1'",
        )
        # No per-gateway routing table must exist for eth3
        machine.fail(
            f"ip route show table {table3} | grep -q .",
        )
        # No ip rule must be added for eth3's address
        machine.fail(
            f"ip rule show | grep -q 'from 10.0.3.2 lookup {table3}'",
        )
        machine.succeed("ip route del default via 10.0.3.1 dev eth3 metric 700")
        machine.systemctl("stop route-balancer.service")

    # ── 8. Shutdown cleanup removes all route-balancer state ─────────────────
    with subtest("shutdown: ECMP route, tables, and ip rules are removed"):
        # Restart so the daemon re-seeds eth1/eth2 routes and installs state
        # that the shutdown path must then remove cleanly.
        machine.systemctl("restart route-balancer.service")
        machine.wait_for_unit("route-balancer.service")
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'proto 111'",
            timeout=15,
        )
        machine.systemctl("stop route-balancer.service")
        # ECMP route at metric 0 must be gone
        machine.wait_until_fails(
            "ip route show default metric 0 | grep -q 'proto 111'",
            timeout=15,
        )
        # Per-provider tables must be empty
        machine.wait_until_fails(
            f"ip route show table {table1} | grep -q .",
            timeout=15,
        )
        machine.wait_until_fails(
            f"ip route show table {table2} | grep -q .",
            timeout=15,
        )
        # ip rules must be gone
        machine.wait_until_fails(
            f"ip rule show | grep -q 'from 10.0.1.2 lookup {table1}'",
            timeout=15,
        )
        machine.wait_until_fails(
            f"ip rule show | grep -q 'from 10.0.2.2 lookup {table2}'",
            timeout=15,
        )
  '';
}
