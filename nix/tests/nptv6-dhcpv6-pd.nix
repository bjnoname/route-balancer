{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer-nptv6-dhcpv6-pd";

  nodes = {
    isp = { lib, ... }: {
      virtualisation.vlans = [ 1 ];

      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        interfaces.eth1.ipv6.addresses = [
          { address = "2001:db8:a00f::1"; prefixLength = 64; }
        ];
      };

      services.kea.dhcp6 = {
        enable = true;
        settings = {
          interfaces-config.interfaces = [ "eth1" ];
          lease-database = { type = "memfile"; persist = false; };
          preferred-lifetime = 600;
          valid-lifetime = 900;
          subnet6 = [{
            id = 1;
            subnet = "2001:db8:a00f::/64";
            interface = "eth1";
            pools = [{ pool = "2001:db8:a00f::100 - 2001:db8:a00f::200"; }];
            pd-pools = [{
              prefix = "2001:db8:a001::";
              prefix-len = 48;
              delegated-len = 56;
            }];
          }];
        };
      };
    };

    router = { lib, pkgs, ... }: {
      imports = [ (import ../module/route-balancer.nix) ];

      virtualisation.vlans = [ 1 2 ];

      networking = {
        useDHCP = lib.mkForce false;
        useNetworkd = true;
        firewall.enable = false;
        nftables.enable = true;
      };

      systemd.network.networks = {
        "10-wan" = {
          matchConfig.Name = "eth1";
          networkConfig = {
            DHCP = "ipv6";
            IPv6AcceptRA = false;
            LinkLocalAddressing = "ipv6";
          };
          dhcpV6Config = {
            WithoutRA = "solicit";
            PrefixDelegationHint = "::/56";
          };
          linkConfig.RequiredForOnline = false;
        };

        "10-lan" = {
          matchConfig.Name = "eth2";
          address = [ "fd00:dead:beef:1::1/64" ];
          linkConfig.RequiredForOnline = false;
        };
      };

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };
        firewall = "nftables";
        reconcileInterval = "3s";
        logLevel = "debug";

        gateways.eth2 = { weight = 1; };

        nptv6 = {
          enable = true;
          internalPrefix = "fd00:dead:beef::/48";
          subnets = {
            main = { prefix = "fd00:dead:beef:1::/64"; };
            guest = { prefix = "fd00:dead:beef:2::/64"; };
          };
          uplinks.eth1 = {
            prefixSource = "route";
            matchPrefix = "2001:db8:a000::/36";
            subnetPriority = [ "main" "guest" ];
          };
        };
      };
    };
  };

  testScript = ''
    # Not start_all(): kea must hold its socket before the client's first SOLICIT.
    isp.start()
    isp.wait_for_unit("kea-dhcp6-server.service")
    isp.wait_until_succeeds("ip -6 addr show dev eth1 scope link | grep -q inet6")
    isp.wait_until_fails("ip -6 addr show dev eth1 | grep -q tentative")
    isp.systemctl("restart kea-dhcp6-server.service")
    isp.wait_for_unit("kea-dhcp6-server.service")
    isp.fail(
        "journalctl --no-pager _SYSTEMD_INVOCATION_ID=$("
        "systemctl show -p InvocationID --value kea-dhcp6-server.service) "
        "| grep -q DHCPSRV_OPEN_SOCKET_FAIL"
    )

    router.start()
    router.wait_for_unit("systemd-networkd.service")
    router.wait_for_unit("route-balancer.service")

    with subtest("the PD client installs a discard route for the delegation"):
        router.wait_until_succeeds(
            "ip -6 route show type unreachable | grep -q '/56'", timeout=90,
        )
        route = router.succeed("ip -6 route show type unreachable").strip()
        print(f"discard route installed by systemd-networkd: {route}")
        assert "2001:db8:a001" in route, route

    with subtest("route-balancer maps the delegation it was actually given"):
        # A /56 holds 256 /64s, so both subnets are mapped, in priority order.
        router.wait_until_succeeds(
            "nft list table ip6 route-balancer-nptv6 | "
            "grep -q 'saddr fd00:dead:beef:1::/64 oifname \"eth1\" snat prefix'",
            timeout=60,
        )
        rules = router.succeed("nft list table ip6 route-balancer-nptv6")
        assert "rb-nptv6 eth1 main" in rules, rules
        assert "rb-nptv6 eth1 guest" in rules, rules

        # The first two /64s of whatever kea handed out.
        delegation = router.succeed(
            "ip -6 route show type unreachable | awk '{print $2}'"
        ).strip()
        base = delegation.split("::")[0]  # e.g. 2001:db8:a001
        assert f"snat prefix to {base}::/64" in rules, (delegation, rules)
        assert f"snat prefix to {base}:1::/64" in rules, (delegation, rules)

    with subtest("the discard route is in the main table, not a per-uplink one"):
        # match_prefix is the only discriminator available for attribution.
        assert router.succeed(
            "ip -6 route show table main type unreachable | grep -c '/56'"
        ).strip() == "1"
  '';
}
