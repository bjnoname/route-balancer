# nix/tests/point-to-point.nix
#
# NixOS VM integration test for uplinks that have no nexthop address.
#
# PPP links (and other point-to-point uplinks — WireGuard, tunnels) install
#
#   default dev ppp-ee proto boot scope link metric 51
#
# with no RTA_GATEWAY, because there is no address to route via. A dummy
# interface carrying a /32 address and a scope-link default route reproduces
# exactly that shape on the netlink wire without needing a PPPoE server.
#
# Also covers two failures that surfaced alongside it:
#   - a lone gateway configured above weight 1 being reconciled forever,
#     because the kernel drops the weight token for single-path routes;
#   - the split-access ip rule leaking when an interface loses its address,
#     which the kernel does without emitting a route delete event.
#
# Run with: nix build .#checks.x86_64-linux.point-to-point-vm

{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer-point-to-point";

  nodes.machine = { lib, pkgs, ... }: {
    imports = [ (import ../module/route-balancer.nix) ];

    virtualisation.vlans = [ 1 ];

    # ptp0 stands in for a PPP interface; it is created by the test script.
    boot.kernelModules = [ "dummy" ];

    networking = {
      useDHCP = lib.mkForce false;
      interfaces.eth1.ipv4.addresses = [{ address = "10.0.1.2"; prefixLength = 24; }];
    };

    services.route-balancer = {
      enable = true;
      package = pkgs.callPackage ../pkgs/route-balancer.nix { };
      gateways = {
        # Weights above 1 on purpose: weight 1 accidentally matches the value
        # the kernel implies for a single-path route, which is what hid the
        # reconcile loop until now.
        eth1 = { weight = 5; };
        ptp0 = { weight = 3; };
      };
      # Short interval so the reconcile assertions do not have to wait long.
      reconcileInterval = "2s";
    };
  };

  testScript = ''
    machine.wait_for_unit("network.target")
    machine.wait_for_unit("route-balancer.service")

    def churn_count():
        """Number of 'modified externally' warnings logged so far."""
        return int(machine.succeed(
            "journalctl -u route-balancer.service --no-pager | "
            "grep -c 'modified externally' || true"
        ).strip())

    def assert_no_churn(seconds, what):
        before = churn_count()
        machine.sleep(seconds)
        after = churn_count()
        assert after == before, (
            f"{what}: {after - before} spurious reconcile restore(s) in {seconds}s "
            f"(reconcile interval is 2s)"
        )

    eth1_idx = int(machine.succeed("cat /sys/class/net/eth1/ifindex").strip())
    table1 = 100 + eth1_idx
    prio1 = 1000 + table1

    # ── 1. A nexthop-less default route enters ECMP ──────────────────────────
    with subtest("point-to-point gateway is picked up and enters ECMP"):
        machine.succeed("ip link add ptp0 type dummy")
        machine.succeed("ip link set ptp0 up")
        machine.succeed("ip addr add 10.9.9.2/32 dev ptp0")
        ptp_idx = int(machine.succeed("cat /sys/class/net/ptp0/ifindex").strip())
        tablep = 100 + ptp_idx
        priop = 1000 + tablep

        # The route pppd installs: no "via", scope link.
        machine.succeed("ip route add default dev ptp0 metric 500")

        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'proto 111'", timeout=15,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'dev ptp0'", timeout=15,
        )
        # Per-gateway table: a default route with no "via", plus the src hint.
        machine.wait_until_succeeds(
            f"ip route show table {tablep} | grep -q 'default dev ptp0'", timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip route show table {tablep} | grep -q 'src 10.9.9.2'", timeout=15,
        )
        # A /32 address has no subnet beyond the host itself, so no subnet
        # route may be derived from it — the table holds the default route only.
        routes = machine.succeed(f"ip route show table {tablep} | grep -c .").strip()
        assert routes == "1", f"table {tablep} has {routes} routes, want only the default route"
        # Split-access rule
        machine.wait_until_succeeds(
            f"ip rule show | grep -q 'from 10.9.9.2 lookup {tablep}'", timeout=15,
        )

    # ── 2. A lone point-to-point gateway above weight 1 does not churn ───────
    with subtest("no reconcile churn: lone point-to-point gateway at weight 3"):
        assert_no_churn(8, "point-to-point gateway at weight 3")

    # ── 3. Point-to-point and nexthop gateways share one ECMP route ──────────
    with subtest("point-to-point and nexthop gateways coexist in ECMP"):
        machine.succeed("ip route add default via 10.0.1.1 dev eth1 metric 600")
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'nexthop dev ptp0 weight 3'", timeout=15,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | "
            "grep -q 'nexthop via 10.0.1.1 dev eth1 weight 5'", timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip route show table {table1} | grep -q 'default via 10.0.1.1'", timeout=15,
        )

    with subtest("no reconcile churn: two gateways at weights 3 and 5"):
        assert_no_churn(8, "two gateways")

    # ── 4. Restart re-seeds the point-to-point gateway ───────────────────────
    with subtest("startup seeding recovers the point-to-point gateway"):
        machine.systemctl("restart route-balancer.service")
        machine.wait_for_unit("route-balancer.service")
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'nexthop dev ptp0 weight 3'", timeout=15,
        )
        machine.wait_until_succeeds(
            "ip route show default metric 0 | "
            "grep -q 'nexthop via 10.0.1.1 dev eth1 weight 5'", timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip route show table {tablep} | grep -q 'default dev ptp0'", timeout=15,
        )

    # ── 5. Removing the point-to-point route tears its state down ────────────
    with subtest("point-to-point gateway removal tears down table and rule"):
        machine.succeed("ip route del default dev ptp0 metric 500")
        machine.wait_until_fails(
            "ip route show default metric 0 | grep -q 'dev ptp0'", timeout=15,
        )
        machine.wait_until_fails(
            f"ip rule show | grep -q 'from 10.9.9.2 lookup {tablep}'", timeout=15,
        )
        machine.wait_until_fails(
            f"ip route show table {tablep} | grep -q .", timeout=15,
        )
        # eth1 is now the only gateway, and the kernel stores it as a plain
        # single-path route with no weight token.
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'via 10.0.1.1 dev eth1'", timeout=15,
        )

    # ── 6. A lone nexthop gateway above weight 1 does not churn ──────────────
    # Regression: the expected set carries weight 5 while `ip route show` prints
    # no weight at all for a single-path route. Comparing the two declared
    # external modification on every tick and re-applied the route forever.
    with subtest("no reconcile churn: lone nexthop gateway at weight 5"):
        assert_no_churn(8, "nexthop gateway at weight 5")

    # ── 7. Losing an address must not leak the rule or the table ─────────────
    # The kernel drops routes that depend on a withdrawn address without
    # emitting RTM_DELROUTE, so the daemon must notice the gateway is gone on
    # its own. Live evidence of the leak: an ip rule for an interface whose
    # DHCP lease had expired hours earlier.
    with subtest("gateway losing its address is torn down, leaving no ip rule"):
        machine.succeed(f"ip rule show | grep -q 'from 10.0.1.2 lookup {table1}'")
        machine.succeed("ip addr flush dev eth1")
        machine.wait_until_fails(
            f"ip rule show | grep -q 'from 10.0.1.2 lookup {table1}'", timeout=20,
        )
        machine.wait_until_fails(
            f"ip rule show | grep -q '^{prio1}:'", timeout=20,
        )
        machine.wait_until_fails(
            f"ip route show table {table1} | grep -q .", timeout=20,
        )
  '';
}
