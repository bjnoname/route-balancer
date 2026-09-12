{ pkgs }:

# ipv4Ecmp = false is not "IPv4 support removed": it withdraws the managed
# default route and nothing else, and whether IPv4 is watched at all then
# depends on whether anything downstream still needs it. Two nodes, one for
# each side of that: `balanced` has no port rules and must leave IPv4 entirely
# alone, `pinned` has one and must keep the whole per-gateway apparatus that a
# fwmark rule points into.
pkgs.testers.nixosTest {
  name = "route-balancer-ipv4-off";

  nodes = {
    balanced = { lib, pkgs, ... }: {
      imports = [ (import ../module/route-balancer.nix) ];

      virtualisation.vlans = [ 1 2 ];

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
        };
      };

      boot.kernel.sysctl."net.ipv6.conf.all.forwarding" = 1;

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };

        ipv4Ecmp = false;
        ipv6Ecmp = true;

        gateways = {
          eth1 = { weight = 3; };
          eth2 = { weight = 1; };
        };

        reconcileInterval = "2s";
      };
    };

    pinned = { lib, pkgs, ... }: {
      imports = [ (import ../module/route-balancer.nix) ];

      virtualisation.vlans = [ 1 2 ];

      environment.systemPackages = [ pkgs.iptables ];

      networking = {
        useDHCP = lib.mkForce false;

        interfaces = {
          eth1.ipv4.addresses = [{ address = "10.0.1.3"; prefixLength = 24; }];
          eth2.ipv4.addresses = [{ address = "10.0.2.3"; prefixLength = 24; }];
        };
      };

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };

        ipv4Ecmp = false;

        gateways = {
          eth1 = { weight = 1; };
          eth2 = { weight = 1; };
        };

        rules = [
          { matchDstPort = [ 443 ]; matchProtocol = "tcp"; gateway = "eth2"; }
        ];

        reconcileInterval = "2s";
      };
    };
  };

  testScript = ''
    start_all()

    for node in (balanced, pinned):
        node.wait_for_unit("network.target")
        node.wait_for_unit("route-balancer.service")

    def config_of(node):
        return node.succeed(
            "cat $(systemctl show -p ExecStart --value route-balancer.service | "
            "grep -o '/nix/store/[^ ]*\\.json' | head -1)"
        )

    def managed_v4(node):
        return node.succeed("ip -4 route show default proto 111 2>/dev/null || true").strip()

    with subtest("module wires ipv4Ecmp through to the daemon"):
        assert '"ipv4_ecmp":false' in config_of(balanced), config_of(balanced)
        assert '"ipv6_ecmp":true' in config_of(balanced), config_of(balanced)

    # ── 1. No port rules: IPv4 is not watched and not written ────────────────
    with subtest("IPv4 uplinks come and go without the daemon writing a route"):
        balanced.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")
        balanced.succeed("ip route add default via 10.0.2.1 dev eth2 metric 600")

        # Two reconcile intervals is long past when a managed route would appear.
        balanced.sleep(6)
        assert managed_v4(balanced) == "", (
            "an IPv4 default route was installed with ipv4Ecmp = false:\n"
            + managed_v4(balanced)
        )

    with subtest("and no per-gateway tables are built for those uplinks"):
        rules = balanced.succeed("ip -4 rule show")
        assert "lookup 10" not in rules, f"a per-gateway table rule was installed:\n{rules}"

    # ── 2. The other family still works, so this is IPv4 off and not idle ────
    with subtest("IPv6 is still balanced across both uplinks"):
        balanced.succeed("ip -6 route add default via 2001:db8:a1::1 dev eth1 metric 1024")
        balanced.succeed("ip -6 route add default via 2001:db8:a2::1 dev eth2 metric 1025")

        MANAGED6 = "ip -6 route show default metric 1 proto 111"
        balanced.wait_until_succeeds(f"{MANAGED6} | grep -q '2001:db8:a1::1'", timeout=20)
        balanced.wait_until_succeeds(f"{MANAGED6} | grep -q '2001:db8:a2::1'", timeout=20)

        route = balanced.succeed(MANAGED6)
        assert "weight 3" in route, f"eth1's weight is missing:\n{route}"
        assert "weight 1" in route, f"eth2's weight is missing:\n{route}"

    # ── 3. A route left behind by a run that did manage IPv4 ─────────────────
    # There is no ledger, so the only trace of the previous configuration is the
    # route itself: our proto, in a family we no longer write.
    with subtest("a route from a run with ipv4Ecmp on is withdrawn at the next start"):
        balanced.succeed("systemctl stop route-balancer.service")
        balanced.succeed(
            "ip -4 route add default proto 111 metric 0 "
            "nexthop via 10.0.1.1 dev eth1 weight 3 "
            "nexthop via 10.0.2.1 dev eth2 weight 1"
        )
        assert managed_v4(balanced) != "", "setup: the stale route was not installed"

        balanced.succeed("systemctl start route-balancer.service")
        balanced.wait_until_succeeds(
            "test -z \"$(ip -4 route show default proto 111 2>/dev/null)\"", timeout=20,
        )

        # The uplink routes it was built from are none of our business.
        balanced.succeed("ip -4 route show default metric 500 | grep -q 'via 10.0.1.1'")
        balanced.succeed("ip -4 route show default metric 600 | grep -q 'via 10.0.2.1'")

    # ── 4. Port rules keep the IPv4 plumbing they depend on ──────────────────
    with subtest("with port rules the per-gateway table and fwmark rule remain"):
        pinned.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")
        pinned.succeed("ip route add default via 10.0.2.1 dev eth2 metric 600")

        eth2_idx = int(pinned.succeed("cat /sys/class/net/eth2/ifindex").strip())
        table2 = 100 + eth2_idx

        pinned.wait_until_succeeds(
            f"ip -4 route show table {table2} | grep -q 'via 10.0.2.1'", timeout=20,
        )
        pinned.wait_until_succeeds(
            f"ip rule show | grep -q 'from 10.0.2.3 lookup {table2}'", timeout=20,
        )
        # Mark 2: the 1-based index of eth2 in the sorted gateway names.
        pinned.wait_until_succeeds(
            f"ip rule show | grep -q 'fwmark 0x2 lookup {table2}'", timeout=20,
        )
        pinned.wait_until_succeeds(
            "iptables -t mangle -S ROUTE-BALANCER | grep -q 'tcp --dport 443'", timeout=20,
        )

    with subtest("but still no managed IPv4 default route"):
        pinned.sleep(4)
        assert managed_v4(pinned) == "", (
            "an IPv4 default route was installed alongside port rules:\n" + managed_v4(pinned)
        )
  '';
}
