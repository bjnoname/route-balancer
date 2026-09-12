{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer-dual-stack-probes";

  nodes = {
    machine = { lib, pkgs, ... }: {
      imports = [ (import ../module/route-balancer.nix) ];

      virtualisation.vlans = [ 1 2 ];

      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        nftables.enable = true;

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

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };
        firewall = "nftables";
        reconcileInterval = "3s";

        ipv6Ecmp = true;
        ipv6Metric = 5;

        gateways =
          let
            health = {
              unhealthyThreshold = 2;
              healthyThreshold = 2;
              interval = "1s";
              timeout = "800ms";
              probe.type = "icmp";
            };
          in
          {
            eth1 = { weight = 1; description = "Family under test"; inherit health; };
            eth2 = { weight = 1; description = "Healthy control"; inherit health; };
          };

        nptv6 = {
          enable = true;
          internalPrefix = "fd00:dead:beef::/48";
          subnets.main = { prefix = "fd00:dead:beef:1::/64"; };
          uplinks = {
            eth1 = {
              prefixSource = "static";
              staticPrefix = "2001:db8:a001::/64";
              subnetPriority = [ "main" ];
            };
            eth2 = {
              prefixSource = "static";
              staticPrefix = "2001:db8:a002::/64";
              subnetPriority = [ "main" ];
            };
          };
        };
      };
    };

    peer1 = { lib, ... }: {
      virtualisation.vlans = [ 1 ];
      networking = {
        useDHCP = lib.mkForce false;
        firewall.allowPing = true;
        interfaces.eth1 = {
          ipv4.addresses = [{ address = "10.0.1.1"; prefixLength = 24; }];
          ipv6.addresses = [{ address = "2001:db8:a1::1"; prefixLength = 64; }];
        };
      };
    };

    peer2 = { lib, ... }: {
      virtualisation.vlans = [ 2 ];
      networking = {
        useDHCP = lib.mkForce false;
        firewall.allowPing = true;
        interfaces.eth1 = {
          ipv4.addresses = [{ address = "10.0.2.1"; prefixLength = 24; }];
          ipv6.addresses = [{ address = "2001:db8:a2::1"; prefixLength = 64; }];
        };
      };
    };
  };

  testScript = ''
    start_all()

    machine.wait_for_unit("network.target")
    machine.wait_for_unit("route-balancer.service")
    peer1.wait_for_unit("network.target")
    peer2.wait_for_unit("network.target")

    V4 = "ip -4 route show default metric 0 proto 111"
    V6 = "ip -6 route show default metric 5 proto 111"
    TABLE = "nft list table ip6 route-balancer-nptv6"

    NEXTHOP = {
        ("eth1", 4): "10.0.1.1",
        ("eth1", 6): "2001:db8:a1::1",
        ("eth2", 4): "10.0.2.1",
        ("eth2", 6): "2001:db8:a2::1",
    }
    EXTERNAL = {"eth1": "2001:db8:a001::/64", "eth2": "2001:db8:a002::/64"}

    def route(family):
        show = V4 if family == 4 else V6
        return machine.succeed(f"{show} 2>/dev/null || true")

    def wait_in_route(iface, family, timeout=20):
        show = V4 if family == 4 else V6
        machine.wait_until_succeeds(
            f"{show} | grep -q '{NEXTHOP[(iface, family)]}'", timeout=timeout
        )

    def wait_out_of_route(iface, family, timeout=20):
        show = V4 if family == 4 else V6
        machine.wait_until_fails(
            f"{show} 2>/dev/null | grep -q '{NEXTHOP[(iface, family)]}'", timeout=timeout
        )

    def in_route(iface, family):
        return NEXTHOP[(iface, family)] in route(family)

    def ruleset():
        return machine.succeed(f"{TABLE} 2>/dev/null || true")

    # A translating uplink has a snat rule; a declined one has a guard drop.
    def translating(iface):
        return f'oifname "{iface}" snat prefix to {EXTERNAL[iface]}' in ruleset()

    def guarded(iface):
        return f'oifname "{iface}" drop' in ruleset()

    def wait_translating(iface, timeout=20):
        machine.wait_until_succeeds(
            f"""{TABLE} | grep -q 'oifname "{iface}" snat prefix to {EXTERNAL[iface]}'""",
            timeout=timeout,
        )

    def wait_guarded(iface, timeout=20):
        machine.wait_until_succeeds(
            f"""{TABLE} | grep -q 'oifname "{iface}" drop'""", timeout=timeout
        )

    def block(peer, family, iface="eth1"):
        cmd = "iptables" if family == 4 else "ip6tables"
        proto = "-p icmp --icmp-type echo-request" if family == 4 else "-p icmpv6 --icmpv6-type echo-request"
        peer.succeed(f"{cmd} -I INPUT {proto} -j DROP")

    def unblock(peer, family):
        cmd = "iptables" if family == 4 else "ip6tables"
        proto = "-p icmp --icmp-type echo-request" if family == 4 else "-p icmpv6 --icmpv6-type echo-request"
        peer.succeed(f"{cmd} -D INPUT {proto} -j DROP")

    # ── 0. The starting state ────────────────────────────────────────────────
    with subtest("both uplinks are in both routes and both translate"):
        # Link nets avoid 2001:db8:N::/64, which the driver already assigns per VLAN.
        machine.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")
        machine.succeed("ip route add default via 10.0.2.1 dev eth2 metric 600")
        machine.succeed("ip -6 route add default via 2001:db8:a1::1 dev eth1 metric 1024")
        machine.succeed("ip -6 route add default via 2001:db8:a2::1 dev eth2 metric 1025")

        for iface in ["eth1", "eth2"]:
            wait_in_route(iface, 4)
            wait_in_route(iface, 6)
            wait_translating(iface)

    # ── 1. One monitor per link and family ───────────────────────────────────
    # Four monitors on two links, each named by the pair it measures.
    with subtest("each link is probed once per family"):
        for iface in ["eth1", "eth2"]:
            for family in ["v4", "v6"]:
                machine.wait_until_succeeds(
                    "journalctl -u route-balancer.service --no-pager | "
                    f"grep 'Health monitor started' | grep -q 'gateway={iface}/{family}'",
                    timeout=20,
                )

    # ── 2. IPv6 broken, IPv4 fine ────────────────────────────────────────────
    # A dead v6 path on a live uplink must move the v6 route and nothing else.
    with subtest("a failing IPv6 probe withdraws the uplink from IPv6 alone"):
        block(peer1, 6)
        wait_out_of_route("eth1", 6)

        assert in_route("eth1", 4), (
            f"a v6 probe failure withdrew the v4 nexthop it measured nothing about:\n{route(4)}"
        )
        # The healthy sibling is untouched in both families.
        assert in_route("eth2", 6), route(6)
        assert in_route("eth2", 4), route(4)

    with subtest("translation follows the IPv6 verdict, and the guard catches the rest"):
        wait_guarded("eth1")
        assert not translating("eth1"), (
            f"an uplink whose IPv6 is down still attracts translated traffic:\n{ruleset()}"
        )
        assert translating("eth2"), ruleset()
        assert not guarded("eth2"), ruleset()

    # Recovery needs two consecutive successful ICMPv6 echoes.
    with subtest("ICMPv6 echo round-trips, so the uplink recovers in IPv6"):
        unblock(peer1, 6)
        wait_in_route("eth1", 6)
        wait_translating("eth1")
        assert not guarded("eth1"), f"guard survived the uplink recovering:\n{ruleset()}"

    # ── 3. IPv4 broken, IPv6 fine ────────────────────────────────────────────
    # The mirror image: a v4 outage must not withdraw the v6 route.
    with subtest("a failing IPv4 probe withdraws the uplink from IPv4 alone"):
        block(peer1, 4)
        wait_out_of_route("eth1", 4)

        assert in_route("eth1", 6), (
            f"a v4 probe failure withdrew the v6 nexthop it measured nothing about:\n{route(6)}"
        )
        assert in_route("eth2", 4), route(4)

    with subtest("translation is untouched by an IPv4 verdict"):
        # Give the loop a full reconcile interval to get it wrong.
        machine.sleep(4)
        assert translating("eth1"), (
            f"a v4 probe failure stopped IPv6 translation through a live IPv6 path:\n{ruleset()}"
        )
        assert not guarded("eth1"), ruleset()

    with subtest("the uplink recovers in IPv4"):
        unblock(peer1, 4)
        wait_in_route("eth1", 4)

    # ── 4. Both families down is still two verdicts ──────────────────────────
    # Two independent verdicts that happen to agree.
    with subtest("both families of one uplink can fail at once"):
        block(peer1, 4)
        block(peer1, 6)
        wait_out_of_route("eth1", 4)
        wait_out_of_route("eth1", 6)
        wait_guarded("eth1")

        # eth2 carries both routes alone, so this is not the retention case.
        assert in_route("eth2", 4), route(4)
        assert in_route("eth2", 6), route(6)

    with subtest("and both recover independently"):
        unblock(peer1, 6)
        wait_in_route("eth1", 6)
        wait_translating("eth1")
        # v4 is still blocked, so its verdict must not have followed v6 back.
        assert not in_route("eth1", 4), (
            f"the v4 nexthop returned on the strength of the v6 probe recovering:\n{route(4)}"
        )

        unblock(peer1, 4)
        wait_in_route("eth1", 4)
  '';
}
