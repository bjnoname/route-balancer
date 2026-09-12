{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer";

  nodes.machine = { lib, pkgs, ... }: {
    imports = [ (import ../module/route-balancer.nix) ];

    virtualisation.vlans = [ 1 2 3 ];

    environment.systemPackages = [ pkgs.iptables ];

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
      routeProto = 111;
      routeTableOffset = 100;
      iptablesChain = "ROUTE-BALANCER";
      nftablesTable = "route-balancer";
      reconcileInterval = "3s";

      rules = [
        { matchDstPort = [ 443 8443 ]; matchProtocol = "tcp"; gateway = "eth1"; }
        { matchDstPort = [ 53 ]; matchProtocol = "udp"; gateway = "eth2"; }
      ];
    };
  };

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
    # A default route at a high metric leaves metric 0 free for the daemon.
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
    # A second default route updates the ECMP route in place.
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
    # The reconciler restores the ECMP route, a per-gateway table and an ip rule.
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

    # ── 6. Port rules: mangle chain and fwmark ip rules ──────────────────────
    # Marks are the 1-based index into the sorted gateway names.
    with subtest("port rules: mangle chain and fwmark rules are installed"):
        machine.wait_until_succeeds(
            "iptables -t mangle -S ROUTE-BALANCER | grep -q 'multiport --dports 443,8443'",
            timeout=15,
        )
        machine.succeed(
            "iptables -t mangle -S ROUTE-BALANCER | grep -q 'udp --dport 53'"
        )
        # Both hooks jump into the chain: OUTPUT for traffic this host
        # originates, PREROUTING for traffic it forwards. Not FORWARD — a
        # forwarded packet is routed before it gets there, so a mark stamped
        # in FORWARD arrives after the decision it exists to influence.
        machine.succeed("iptables -t mangle -S OUTPUT | grep -q ROUTE-BALANCER")
        machine.succeed("iptables -t mangle -S PREROUTING | grep -q ROUTE-BALANCER")
        machine.fail("iptables -t mangle -S FORWARD | grep -q ROUTE-BALANCER")
        # ip rule prints the mark in hex, at priority 500 + mark.
        machine.wait_until_succeeds(
            f"ip rule show | grep -q '^501:.*fwmark 0x1 lookup {table1}'",
            timeout=15,
        )
        machine.wait_until_succeeds(
            f"ip rule show | grep -q '^502:.*fwmark 0x2 lookup {table2}'",
            timeout=15,
        )

    # ── 6b. The other half of the mechanism drifts too ───────────────────────
    # The mangle chain stamps a mark and an ip rule reads it; both are reconciled.
    with subtest("reconcile: flushed mangle chain is restored"):
        machine.succeed("iptables -t mangle -F ROUTE-BALANCER")
        machine.wait_until_fails(
            "iptables -t mangle -S ROUTE-BALANCER | grep -q 'multiport --dports 443,8443'",
            timeout=5,
        )
        machine.wait_until_succeeds(
            "iptables -t mangle -S ROUTE-BALANCER | grep -q 'multiport --dports 443,8443'",
            timeout=15,
        )
        machine.succeed(
            "iptables -t mangle -S ROUTE-BALANCER | grep -q 'udp --dport 53'"
        )
        # And the fwmark rule that reads those marks is still there.
        machine.succeed(f"ip rule show | grep -q '^501:.*fwmark 0x1 lookup {table1}'")

    # A correct chain can also be left unreachable: a flushed OUTPUT takes the
    # jump with it, which no contents check would see.
    with subtest("reconcile: deleted mangle jump is restored"):
        machine.succeed("iptables -t mangle -D OUTPUT -j ROUTE-BALANCER")
        machine.wait_until_fails(
            "iptables -t mangle -S OUTPUT | grep -q ROUTE-BALANCER",
            timeout=5,
        )
        machine.wait_until_succeeds(
            "iptables -t mangle -S OUTPUT | grep -q ROUTE-BALANCER",
            timeout=15,
        )
        machine.succeed("iptables -t mangle -S PREROUTING | grep -q ROUTE-BALANCER")
        # The rebuild is the whole chain, so its contents came back with it.
        machine.succeed(
            "iptables -t mangle -S ROUTE-BALANCER | grep -q 'multiport --dports 443,8443'"
        )

    # SIGKILL leaves the chain standing, and the daemon coming back up must
    # adopt it rather than rebuild it.
    with subtest("startup adopts a mangle chain that survived a crash"):
        before = machine.succeed("iptables -t mangle -S ROUTE-BALANCER")
        machine.succeed("systemctl kill -s SIGKILL route-balancer.service || true")
        machine.wait_until_fails("systemctl is-active route-balancer.service", timeout=15)
        # Nothing cleaned up, so the chain is exactly as the daemon left it.
        assert before == machine.succeed("iptables -t mangle -S ROUTE-BALANCER"), (
            "SIGKILL removed the mangle chain, so this subtest is testing nothing"
        )

        # A journal cursor rather than a timestamp: --since has one-second
        # granularity, which is coarser than the gap to the previous subtest.
        cursor = machine.succeed(
            "journalctl -u route-balancer.service -n 1 -o export "
            "| sed -n 's/^__CURSOR=//p'"
        ).strip()
        machine.systemctl("start route-balancer.service")
        machine.wait_for_unit("route-balancer.service")
        machine.wait_until_succeeds(
            "ip route show default metric 0 | grep -q 'proto 111'",
            timeout=15,
        )

        assert before == machine.succeed("iptables -t mangle -S ROUTE-BALANCER"), (
            "the mangle chain changed across a startup that found it correct"
        )
        applied = machine.succeed(
            f"journalctl -u route-balancer.service --after-cursor='{cursor}' "
            "| grep -c 'Installing.*mangle-marks' || true"
        ).strip()
        assert applied == "0", (
            f"the chain was reinstalled {applied} time(s) on a startup that "
            "found it already correct"
        )

    # ── 7. Unconfigured interface is excluded from ECMP ──────────────────────
    # eth3 has an IP and a default route but is absent from the gateways config.
    with subtest("unconfigured interface is excluded from ECMP"):
        machine.systemctl("start route-balancer.service")
        machine.wait_for_unit("route-balancer.service")
        machine.succeed("ip route add default via 10.0.3.1 dev eth3 metric 700")
        # The unconfigured gateway must never appear in the ECMP route.
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
        # Restart so there is state for the shutdown path to remove.
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

    # ── 9. Stopping the unit mid-startup leaves nothing behind ───────────────
    # A stop arriving at any point in startup must leave no managed state. The
    # delays sweep the window rather than aiming at one point in it.
    delays = ["0", "0.02", "0.05", "0.1", "0.2", "0.4", "0.8"]

    # Count earlier starts so the check below reads only the sweep's own.
    starts_before = int(machine.succeed(
        "journalctl -u route-balancer.service --no-pager -o cat"
        " | grep -c 'route-balancer starting' || true"
    ).strip())

    for delay in delays:
        with subtest(f"stop {delay}s into startup leaves nothing behind"):
            # One round trip for the whole cycle, so delay 0 is as early as the
            # harness can signal. reset-failed clears systemd's start rate limit.
            machine.succeed(
                "systemctl reset-failed route-balancer.service; "
                "systemctl start route-balancer.service && "
                f"sleep {delay} && "
                "systemctl stop route-balancer.service"
            )
            # No wait_until_fails: `systemctl stop` is synchronous.
            machine.fail("ip route show default metric 0 | grep -q 'proto 111'")
            machine.fail(f"ip route show table {table1} | grep -q .")
            machine.fail(f"ip route show table {table2} | grep -q .")
            machine.fail(f"ip rule show | grep -q 'lookup {table1}'")
            machine.fail(f"ip rule show | grep -q 'lookup {table2}'")
            machine.fail("iptables -t mangle -S | grep -q ROUTE-BALANCER")

    # The sweep only means something if at least one iteration got far enough
    # to have state to leave behind.
    with subtest("the sweep ran a daemon that got somewhere"):
        journal = machine.succeed("journalctl -u route-balancer.service --no-pager -o cat")
        sweep_runs = journal.split("route-balancer starting")[1:][starts_before:]
        assert [r for r in sweep_runs if "Applying ECMP route" in r], (
            "no sweep invocation reached the first pass, so none of them had any "
            "managed state to leave behind; lengthen the delays"
        )
  '';
}
