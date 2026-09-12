{ pkgs }:

let
  ispA = { link = "2001:db8:a00f"; delegation = "2001:db8:a001::/56"; };
  ispB = { link = "2001:db8:b00f"; delegation = "2001:db8:b001::/56"; };

  # Beyond both ISPs, so the only way to it is whichever default route the
  # packet's mark selected.
  beyond = "2001:db8:c000::1";
in
pkgs.testers.nixosTest {
  name = "route-balancer-nptv6-port-rules";

  nodes = {
    isp = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 1 3 ];

      environment.systemPackages = [ pkgs.tcpdump pkgs.socat ];

      boot.kernel.sysctl."net.ipv6.conf.all.forwarding" = 1;

      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        interfaces = {
          eth1.ipv6.addresses = [{ address = "${ispA.link}::1"; prefixLength = 64; }];
          eth2.ipv6.addresses = [{ address = "${ispB.link}::1"; prefixLength = 64; }];
        };
      };
    };

    router = { lib, pkgs, ... }: {
      imports = [ (import ../module/route-balancer.nix) ];

      virtualisation.vlans = [ 1 2 3 ];

      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        nftables.enable = true;

        interfaces = {
          eth1.ipv6.addresses = [{ address = "${ispA.link}::2"; prefixLength = 64; }];
          eth2.ipv6.addresses = [
            { address = "fd00:dead:beef:1::1"; prefixLength = 64; }
            { address = "fd00:dead:beef:2::1"; prefixLength = 64; }
          ];
          eth3.ipv6.addresses = [{ address = "${ispB.link}::2"; prefixLength = 64; }];
        };
      };

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };
        firewall = "nftables";
        reconcileInterval = "3s";

        # Nothing here is IPv4; leaving it on only produces warnings about
        # gateways with no IPv4 route.
        ipv4Ecmp = false;

        gateways = {
          eth1 = { weight = 1; description = "ISP-A"; };
          eth3 = { weight = 1; description = "ISP-B"; };
        };

        # Marks are the 1-based index into the sorted gateway names, so eth3
        # is 2 and its ip rule sits at priority 502.
        rules = [
          { matchDstPort = [ 443 ]; matchProtocol = "tcp"; gateway = "eth3"; matchFamily = "ipv6"; }
        ];

        nptv6 = {
          enable = true;
          internalPrefix = "fd00:dead:beef::/48";

          subnets = {
            main = { prefix = "fd00:dead:beef:1::/64"; };
            guest = { prefix = "fd00:dead:beef:2::/64"; };
          };

          uplinks = {
            eth1 = {
              prefixSource = "route";
              matchPrefix = "2001:db8:a000::/36";
              subnetPriority = [ "main" "guest" ];
            };
            # Deliberately not "guest": the rule steers port 443 here, and
            # what it may steer is exactly what this list says.
            eth3 = {
              prefixSource = "route";
              matchPrefix = "2001:db8:b000::/36";
              subnetPriority = [ "main" ];
            };
          };
        };
      };
    };

    client = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 2 ];

      environment.systemPackages = [ pkgs.socat ];

      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        interfaces.eth1.ipv6.addresses = [
          { address = "fd00:dead:beef:1::10"; prefixLength = 64; }
          { address = "fd00:dead:beef:2::10"; prefixLength = 64; }
        ];
      };
    };
  };

  testScript = ''
    start_all()

    for m in (isp, router, client):
        m.wait_for_unit("network.target")
    router.wait_for_unit("route-balancer.service")

    MAIN = "fd00:dead:beef:1::10"
    GUEST = "fd00:dead:beef:2::10"

    eth3_idx = int(router.succeed("cat /sys/class/net/eth3/ifindex").strip())
    table3 = 100 + eth3_idx

    # Somewhere past both ISPs. Neither delegation covers it, so a packet only
    # arrives there through whichever default route was chosen for it.
    isp.succeed("ip link add beyond type dummy")
    isp.succeed("ip link set beyond up")
    isp.succeed("ip -6 addr add ${beyond}/64 dev beyond nodad")
    isp.succeed("ip -6 route replace ${ispA.delegation} via ${ispA.link}::2 dev eth1")
    isp.succeed("ip -6 route replace ${ispB.delegation} via ${ispB.link}::2 dev eth2")

    client.succeed("ip -6 route replace default via fd00:dead:beef:1::1 dev eth1")

    # Both uplinks carry a default route — an uplink with none is one nptv6
    # will not translate for, and one the rule has no table to build. ISP-A's
    # is the better metric, so the main table prefers it and only the port
    # rule can pull anything away.
    router.succeed("ip -6 route replace default via ${ispA.link}::1 dev eth1 metric 100")
    router.succeed("ip -6 route replace default via ${ispB.link}::1 dev eth3 metric 200")
    router.succeed("ip -6 route add unreachable ${ispA.delegation} dev lo")
    router.succeed("ip -6 route add unreachable ${ispB.delegation} dev lo")

    def leaves_by(iface, src, port):
        """Send one SYN from src to the far side and report what the ISP saw."""
        isp.succeed(
            f"systemd-run --unit=cap --collect tcpdump -n -U -i {iface} -w /tmp/cap.pcap tcp"
        )
        isp.sleep(1)
        client.execute(f"timeout 3 socat -T 2 - TCP6:[${beyond}]:{port},bind=[{src}] </dev/null")
        isp.sleep(1)
        isp.succeed("systemctl stop cap || true")
        text = isp.succeed("tcpdump -n -r /tmp/cap.pcap 2>/dev/null || true")
        isp.succeed("rm -f /tmp/cap.pcap")
        return text

    # ── 1. The three pieces of the mechanism ─────────────────────────────────
    with subtest("the mark, the rule and the table it selects into"):
        # One mark, not two: eth3 translates "main" and not "guest", so that is
        # all the rule is allowed to steer there.
        router.wait_until_succeeds(
            "nft list table ip6 route-balancer | "
            "grep -q 'ip6 saddr fd00:dead:beef:1::/64 tcp dport 443 meta mark set 0x00000002'",
            timeout=30,
        )
        marks = router.succeed("nft list table ip6 route-balancer")
        assert "fd00:dead:beef:2::/64" not in marks, (
            f"guest was steered to an uplink that does not translate it:\n{marks}"
        )
        # And no output chain: this host's own IPv6 traffic is not translated.
        assert "hook output" not in marks, marks

        router.wait_until_succeeds(
            f"ip -6 rule show | grep -q '^502:.*fwmark 0x2 lookup {table3}'", timeout=30,
        )
        router.wait_until_succeeds(
            f"ip -6 route show table {table3} | grep -q 'default via ${ispB.link}::1 dev eth3'",
            timeout=30,
        )
        # The IPv4 rule table is not where an IPv6 mark is read.
        router.fail("ip -4 rule show | grep -q 'fwmark 0x2'")

    # ── 2. The rule steers, and translation follows it ───────────────────────
    with subtest("port 443 leaves by ISP-B, translated into ISP-B's prefix"):
        cap = leaves_by("eth2", MAIN, 443)
        assert "2001:db8:b001::10." in cap, f"port 443 did not leave by ISP-B:\n{cap}"
        assert "fd00:" not in cap, f"untranslated traffic reached the ISP:\n{cap}"

    with subtest("everything else still leaves by ISP-A"):
        cap = leaves_by("eth1", MAIN, 80)
        assert "2001:db8:a001::10." in cap, f"port 80 did not follow the main table:\n{cap}"

    # ── 3. The subnet the uplink cannot translate is not steered ─────────────
    # This is the whole design question: a rule that pinned guest to eth3 would
    # put a fd00:: source on the wire, and nothing here would ever hear about
    # the drop at the far end.
    with subtest("a subnet ISP-B does not map keeps using the main table"):
        cap = leaves_by("eth1", GUEST, 443)
        assert "2001:db8:a001:1::10." in cap, (
            f"guest port-443 traffic did not fall back to ISP-A:\n{cap}"
        )

        cap = leaves_by("eth2", GUEST, 443)
        assert "fd00:" not in cap, f"an internal source reached ISP-B:\n{cap}"
        assert "2001:db8:" not in cap, f"guest traffic was steered to ISP-B anyway:\n{cap}"

    # ── 4. Losing the uplink's lease unmarks it ──────────────────────────────
    # Nothing about the rule changed; it stopped applying because there is no
    # longer a translation for it to compose with.
    with subtest("the mark goes when the uplink stops translating"):
        router.succeed("ip -6 route del unreachable ${ispB.delegation} dev lo")
        router.wait_until_fails(
            "nft list table ip6 route-balancer | grep -q 'meta mark set 0x00000002'", timeout=30,
        )

        cap = leaves_by("eth1", MAIN, 443)
        assert "2001:db8:a001::10." in cap, (
            f"port 443 did not fall back to ISP-A after ISP-B lost its lease:\n{cap}"
        )

    # ── 5. And comes back with it ────────────────────────────────────────────
    with subtest("the mark returns when the lease does"):
        router.succeed("ip -6 route add unreachable ${ispB.delegation} dev lo")
        router.wait_until_succeeds(
            "nft list table ip6 route-balancer | "
            "grep -q 'ip6 saddr fd00:dead:beef:1::/64 tcp dport 443 meta mark set 0x00000002'",
            timeout=30,
        )
        cap = leaves_by("eth2", MAIN, 443)
        assert "2001:db8:b001::10." in cap, f"steering did not resume:\n{cap}"

    # ── 6. A converged daemon sits still ─────────────────────────────────────
    with subtest("nothing is reinstalled while the machine already agrees"):
        cursor = router.succeed(
            "journalctl -u route-balancer.service -n 1 -o export "
            "| sed -n 's/^__CURSOR=//p'"
        ).strip()
        router.sleep(10)  # three reconcile ticks
        installs = router.succeed(
            f"journalctl -u route-balancer.service --after-cursor='{cursor}' "
            "| grep -c 'Installing\\|Removing' || true"
        ).strip()
        assert installs == "0", (
            f"a converged daemon touched {installs} resource(s) over three idle ticks"
        )
  '';
}
