{ pkgs }:

let
  ispA = { link = "2001:db8:a00f"; delegation = "2001:db8:a001::/56"; };
  ispB = { link = "2001:db8:b00f"; delegation = "2001:db8:b001::/64"; };
in
pkgs.testers.nixosTest {
  name = "route-balancer-nptv6-forwarding";

  nodes = {
    isp = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 1 3 ];

      environment.systemPackages = [ pkgs.tcpdump pkgs.iputils pkgs.socat ];

      boot.kernel.sysctl."net.ipv6.conf.all.forwarding" = 1;

      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        interfaces = {
          eth1 = {
            ipv4.addresses = [{ address = "10.0.1.1"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "${ispA.link}::1"; prefixLength = 64; }];
          };
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
          eth1 = {
            ipv4.addresses = [{ address = "10.0.1.2"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "${ispA.link}::2"; prefixLength = 64; }];
          };
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

        gateways.eth1 = { weight = 1; description = "ISP-A"; };

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
              # §8 starts a flow at the ISP to make the LAN answer it. eth3
              # keeps the default, which is what §4a is about.
              allowInboundConnections = true;
            };
            eth3 = {
              prefixSource = "route";
              matchPrefix = "2001:db8:b000::/36";
              subnetPriority = [ "main" "guest" ];
            };
          };
        };
      };
    };

    client = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 2 ];

      environment.systemPackages = [ pkgs.iputils pkgs.tcpdump pkgs.socat ];

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

    # No route to fd00::/8 on the ISP: untranslated traffic is unanswerable.
    isp.succeed("ip -6 route replace ${ispA.delegation} via ${ispA.link}::2 dev eth1")
    isp.succeed("ip -6 route replace ${ispB.delegation} via ${ispB.link}::2 dev eth2")
    client.succeed("ip -6 route replace default via fd00:dead:beef:1::1 dev eth1")

    def start_capture(iface):
        isp.succeed(
            f"systemd-run --unit=cap --collect tcpdump -n -U -i {iface} -w /tmp/cap.pcap icmp6"
        )
        isp.sleep(1)

    def stop_capture():
        isp.succeed("systemctl stop cap || true")
        text = isp.succeed("tcpdump -n -r /tmp/cap.pcap 2>/dev/null || true")
        isp.succeed("rm -f /tmp/cap.pcap")
        return text

    def capture(iface, src, dst):
        """Ping dst from src while capturing on the ISP side."""
        start_capture(iface)
        rc, _ = client.execute(f"ping -6 -c 3 -W 2 -I {src} {dst}")
        isp.sleep(1)
        return rc, stop_capture()

    # ── 1. Both delegations arrive ───────────────────────────────────────────
    # The discard routes a DHCPv6-PD client installs per RFC 7084.
    with subtest("delegations are attributed and mapped"):
        router.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")
        router.succeed("ip -6 route add unreachable ${ispA.delegation} dev lo")
        router.succeed("ip -6 route add unreachable ${ispB.delegation} dev lo")

        # ISP-A's /56 maps both subnets…
        router.wait_until_succeeds(
            "nft list table ip6 route-balancer-nptv6 | "
            "grep -q 'saddr fd00:dead:beef:1::/64 oifname \"eth1\" snat prefix to 2001:db8:a001::/64'",
            timeout=30,
        )
        router.wait_until_succeeds(
            "nft list table ip6 route-balancer-nptv6 | "
            "grep -q 'saddr fd00:dead:beef:2::/64 oifname \"eth1\" snat prefix to 2001:db8:a001:1::/64'",
            timeout=30,
        )
        # …ISP-B's /64 holds one subnet, so only main is mapped there.
        router.wait_until_succeeds(
            "nft list table ip6 route-balancer-nptv6 | "
            "grep -q 'saddr fd00:dead:beef:1::/64 oifname \"eth3\" snat prefix to 2001:db8:b001::/64'",
            timeout=30,
        )
        rules = router.succeed("nft list table ip6 route-balancer-nptv6")
        assert "rb-nptv6 eth3 guest" not in rules, rules

    # ── 2. Traffic out of ISP-A is translated in both directions ─────────────
    with subtest("main subnet reaches the ISP through ISP-A's delegation"):
        router.succeed("ip -6 route replace default via ${ispA.link}::1 dev eth1")
        client.succeed(f"ping -6 -c 1 -W 5 -I {MAIN} fd00:dead:beef:1::1")  # LAN sanity

        rc, cap = capture("eth1", MAIN, "${ispA.link}::1")
        assert rc == 0, f"ping through ISP-A failed:\n{cap}"
        # The prefix is rewritten and the host identifier survives it.
        assert "2001:db8:a001::10 >" in cap, f"source not translated as expected:\n{cap}"
        assert "fd00:" not in cap, f"untranslated ULA traffic reached the ISP:\n{cap}"

    with subtest("guest subnet gets ISP-A's second /64"):
        rc, cap = capture("eth1", GUEST, "${ispA.link}::1")
        assert rc == 0, f"ping from guest through ISP-A failed:\n{cap}"
        assert "2001:db8:a001:1::10 >" in cap, f"guest source not translated as expected:\n{cap}"

    # ── 3. Switching uplinks renumbers nothing inside ────────────────────────
    # The client keeps its ULA address; only the prefix it is seen as changes.
    with subtest("main subnet reaches the ISP through ISP-B after a failover"):
        router.succeed("ip -6 route replace default via ${ispB.link}::1 dev eth3")

        rc, cap = capture("eth2", MAIN, "${ispB.link}::1")
        assert rc == 0, f"ping through ISP-B failed:\n{cap}"
        assert "2001:db8:b001::10 >" in cap, f"source not translated as expected:\n{cap}"

    # ── 4. The /64-delegation edge case, from the LAN's point of view ────────
    with subtest("guest subnet is unreachable through a /64 delegation"):
        rc, cap = capture("eth2", GUEST, "${ispB.link}::1")
        assert rc != 0, "guest traffic was answered through an uplink that cannot map it"
        # It leaves untranslated, and the ISP has no route back to a ULA.
        assert "fd00:dead:beef:2::10 >" in cap, f"expected untranslated guest traffic:\n{cap}"

    # ── 4a. Inbound is closed unless the uplink asked for it ─────────────────
    # eth3 keeps the default. What the LAN starts must be unaffected in both
    # directions — that traffic is reverse-translated from the conntrack entry
    # and never sees this rule — while a flow the ISP starts must not arrive.
    with subtest("an unsolicited packet to a closed uplink is dropped where it is counted"):
        def closed_counter():
            line = router.succeed(
                "nft list table ip6 route-balancer-nptv6 | grep 'rb-nptv6-closed eth3 main'"
            )
            return int(line.split("packets")[1].split()[0])

        assert 'iifname "eth3" dnat' not in router.succeed(
            "nft list table ip6 route-balancer-nptv6"
        ), "an uplink that did not ask for inbound carries a dnat"

        before = closed_counter()
        rc, _ = isp.execute("ping -6 -c 2 -W 2 -I ${ispB.link}::1 2001:db8:b001::10")
        assert rc != 0, "a closed uplink answered a packet the ISP started"
        after = closed_counter()
        assert after > before, f"the drop was not counted where it happened: {before} -> {after}"

    with subtest("closing inbound leaves the reply path and ICMPv6 errors alone"):
        rc, cap = capture("eth2", MAIN, "${ispB.link}::1")
        assert rc == 0, f"a flow the LAN started stopped working:\n{cap}"

        # An inbound error for that flow arrives as ct state related, so the
        # drop does not see it. This is the half a stateless drop would break.
        client.succeed(
            "systemd-run --unit=cap --collect tcpdump -n -U -i eth1 -w /tmp/cap.pcap icmp6"
        )
        client.sleep(1)
        client.succeed(
            f"echo probe | socat -T 2 - 'UDP6-SENDTO:[${ispB.link}::1]:9999,bind=[{MAIN}]'"
        )
        client.sleep(2)
        client.succeed("systemctl stop cap || true")
        cap = client.succeed("tcpdump -n -r /tmp/cap.pcap 2>/dev/null || true")
        client.succeed("rm -f /tmp/cap.pcap")

        assert "unreachable" in cap.lower(), f"no ICMPv6 error came back through eth3:\n{cap}"
        assert f"> {MAIN}" in cap, f"the error was not delivered to the LAN host:\n{cap}"

    # ── 5. Health probes are not caught by the translation rules (§5) ────────
    # A probe sources from the uplink's own address, which no SNAT rule matches.
    with subtest("uplink-sourced traffic bypasses translation"):
        isp.succeed(
            "systemd-run --unit=cap --collect tcpdump -n -l -i eth1 -w /tmp/cap.pcap icmp6"
        )
        isp.sleep(1)
        router.succeed("ping -6 -c 3 -W 2 -I ${ispA.link}::2 ${ispA.link}::1")
        isp.sleep(1)
        isp.succeed("systemctl stop cap || true")
        cap = isp.succeed("tcpdump -n -r /tmp/cap.pcap 2>/dev/null || true")
        isp.succeed("rm -f /tmp/cap.pcap")
        assert "${ispA.link}::2 >" in cap, f"probe-shaped traffic was rewritten:\n{cap}"
        assert "2001:db8:a001:" not in cap, f"probe-shaped traffic was translated:\n{cap}"

    # ── 6. Losing an uplink withdraws only its rules ─────────────────────────
    with subtest("uplink going down tears down its translation rules"):
        router.succeed("ip link set eth3 down")
        # The kernel drops the discard route with the link, withdrawing the lease.
        router.succeed("ip -6 route del unreachable ${ispB.delegation} dev lo")
        router.wait_until_fails(
            "nft list table ip6 route-balancer-nptv6 | grep -q 'rb-nptv6 eth3 '", timeout=30,
        )
        rules = router.succeed("nft list table ip6 route-balancer-nptv6")
        assert "rb-nptv6 eth1 main" in rules, rules

        router.succeed("ip -6 route replace default via ${ispA.link}::1 dev eth1")
        rc, cap = capture("eth1", MAIN, "${ispA.link}::1")
        assert rc == 0, f"ISP-A stopped working after ISP-B went away:\n{cap}"

    # ── 7. ICMPv6 errors, inbound ────────────────────────────────────────────
    # RFC 6296 §3.7: the addresses inside an ICMPv6 error must be rewritten too.
    with subtest("a Packet Too Big from upstream is translated in its payload"):
        # Somewhere beyond the ISP with an MTU too small for what we send.
        isp.succeed("ip link add pmtu type dummy")
        isp.succeed("ip link set pmtu mtu 1280 up")
        isp.succeed("ip -6 addr add 2001:db8:a00e::1/64 dev pmtu nodad")

        client.succeed(
            "systemd-run --unit=cap --collect tcpdump -n -vv -U -i eth1 -w /tmp/cap.pcap icmp6"
        )
        client.sleep(1)
        client.execute(f"ping -6 -c 3 -W 2 -s 1400 -I {MAIN} 2001:db8:a00e::2")
        client.sleep(1)
        client.succeed("systemctl stop cap || true")
        cap = client.succeed("tcpdump -n -vv -r /tmp/cap.pcap 2>/dev/null || true")
        client.succeed("rm -f /tmp/cap.pcap")

        assert "too big" in cap.lower(), f"no Packet Too Big reached the LAN host:\n{cap}"
        # The ISP quoted the translated source it saw.
        assert "2001:db8:a001::10" not in cap, f"embedded header left untranslated:\n{cap}"

        # The host caches the reduced MTU, which it can only key off its own address.
        client.wait_until_succeeds(
            f"ip -6 route get 2001:db8:a00e::2 from {MAIN} | grep -q 'mtu 1280'", timeout=20,
        )

    # ── 8. ICMPv6 errors, outbound ───────────────────────────────────────────
    # The same requirement outbound, where the embedded destination is rewritten.
    with subtest("an error generated inside is translated on the way out"):
        isp.succeed(
            "systemd-run --unit=cap --collect tcpdump -n -vv -U -i eth1 -w /tmp/cap.pcap icmp6"
        )
        isp.sleep(1)
        # Nothing is listening, so the LAN host answers port-unreachable.
        isp.succeed("echo probe | socat -T 2 - UDP6-SENDTO:[2001:db8:a001::10]:9999")
        isp.sleep(2)
        isp.succeed("systemctl stop cap || true")
        cap = isp.succeed("tcpdump -n -vv -r /tmp/cap.pcap 2>/dev/null || true")
        isp.succeed("rm -f /tmp/cap.pcap")

        assert "unreachable" in cap.lower(), f"no error came back from the LAN host:\n{cap}"
        # Neither the error's source nor its quote may name the internal plan.
        assert "fd00:" not in cap, f"an internal address leaked in an ICMPv6 payload:\n{cap}"
        assert "2001:db8:a001::10" in cap, f"error does not come from the translated address:\n{cap}"
  '';
}
