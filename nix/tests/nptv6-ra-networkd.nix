{ pkgs }:

let
  ra1 = "2001:db8:cafe";
  ra2 = "2001:db8:beef";
  ra3 = "2001:db8:f00d";

  translator = { lan, reconcileInterval }: { lib, pkgs, ... }: {
    imports = [ (import ../module/route-balancer.nix) ];

    virtualisation.vlans = [ 1 lan ];

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
          DHCP = "no";
          IPv6AcceptRA = true;
          LinkLocalAddressing = "ipv6";
        };
        linkConfig.RequiredForOnline = false;
      };

      "10-lan" = {
        matchConfig.Name = "eth2";
        address = [ "fd00:dead:beef:1::1/64" "fd00:dead:beef:2::1/64" ];
        linkConfig.RequiredForOnline = false;
      };
    };

    services.route-balancer = {
      inherit reconcileInterval;
      enable = true;
      package = pkgs.callPackage ../pkgs/route-balancer.nix { };
      firewall = "nftables";
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
          prefixSource = "ra";
          subnetPriority = [ "main" "guest" ];
          proxyNdp = false;
          # This test is about where the external prefix comes from, not about
          # who may cross; the dnat is what its assertions look for.
          allowInboundConnections = true;
        };
      };
    };
  };
in
pkgs.testers.nixosTest {
  name = "route-balancer-nptv6-ra-networkd";

  nodes = {
    operator = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 1 ];

      environment.systemPackages = [ pkgs.radvd pkgs.tcpdump pkgs.iputils ];

      boot.kernel.sysctl."net.ipv6.conf.all.forwarding" = 1;

      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        interfaces.eth1.ipv6.addresses = [
          { address = "2001:db8:ff01::1"; prefixLength = 64; }
        ];
      };
    };

    router = translator { lan = 2; reconcileInterval = "3s"; };
    evented = translator { lan = 3; reconcileInterval = null; };

    client = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 2 ];

      environment.systemPackages = [ pkgs.iputils ];

      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        interfaces.eth1.ipv6.addresses = [
          { address = "fd00:dead:beef:1::10"; prefixLength = 64; }
        ];
      };
    };
  };

  testScript = ''
    import time

    IP = "/run/current-system/sw/bin/ip"
    STDBUF = "/run/current-system/sw/bin/stdbuf"

    RA1 = "${ra1}"
    RA2 = "${ra2}"
    RA3 = "${ra3}"
    TRANSIT = "2001:db8:ff01::1"

    MAIN = "fd00:dead:beef:1::10"
    MAIN_NET = "fd00:dead:beef:1::/64"

    TABLE = "nft list table ip6 route-balancer-nptv6"

    # Verbatim the command seedRAAddresses() runs.
    SEED = "ip -6 -o addr show dev eth1 scope global dynamic"

    def ruleset(node=None):
        return (node or router).succeed(f"{TABLE} 2>/dev/null || true")

    def seed(node=None):
        return (node or router).succeed(f"{SEED} || true")

    def journal(pattern, node=None):
        return (node or router).succeed(
            "journalctl -u route-balancer.service --no-pager | "
            f"grep '{pattern}' || true"
        )

    def main_pid(node=None):
        return (node or router).succeed(
            "systemctl show -p MainPID --value route-balancer.service"
        ).strip()

    def wait_mapped(external, internal=MAIN_NET, node=None, timeout=60):
        (node or router).wait_until_succeeds(
            f"{TABLE} | grep -q 'saddr {internal} oifname \"eth1\" "
            f"snat prefix to {external}::/64'",
            timeout=timeout,
        )
        (node or router).wait_until_succeeds(
            f"{TABLE} | grep -q 'daddr {external}::/64 iifname \"eth1\" "
            f"dnat prefix to {internal}'",
            timeout=timeout,
        )

    def lifetimes(node=None):
        """The lifetimes freshestRAPrefix() ranks on, per advertised /64.

        Keyed by prefix stem and keeping the longest-lived address out of each,
        since privacy extensions give a prefix more than one."""
        out = {}
        for line in seed(node).splitlines():
            fields = line.split()
            addr, valid, preferred = None, 0, 0
            for i, f in enumerate(fields):
                if f == "inet6" and i + 1 < len(fields):
                    addr = fields[i + 1]
                elif f == "valid_lft" and i + 1 < len(fields):
                    valid = int(fields[i + 1].removesuffix("sec"))
                elif f == "preferred_lft" and i + 1 < len(fields):
                    preferred = int(fields[i + 1].removesuffix("sec"))
            if not addr:
                continue
            stem = ":".join(addr.split("/")[0].split(":")[:3])
            if valid > out.get(stem, (-1, -1))[0]:
                out[stem] = (valid, preferred)
        return out

    def advertise(prefixes):
        """(Re)start radvd advertising exactly this set of prefixes.

        Each entry is (stem, valid, preferred, onlink). A graceful renumber is
        expressed the way a real one is: the retired prefix stays in the
        advertisement with preferred 0 and a valid lifetime still counting
        down, beside the replacement."""
        body = []
        for stem, valid, preferred, onlink in prefixes:
            body += [
                f"  prefix {stem}::/64 " + "{",
                f"    AdvOnLink {onlink};",
                "    AdvAutonomous on;",
                f"    AdvValidLifetime {valid};",
                f"    AdvPreferredLifetime {preferred};",
                "  };",
            ]
        conf = "\n".join(
            ["interface eth1 {", "  AdvSendAdvert on;",
             "  MinRtrAdvInterval 3;", "  MaxRtrAdvInterval 4;"]
            + body + ["};"]
        )
        operator.succeed(f"cat > /run/radvd.conf <<'RADVD'\n{conf}\nRADVD")
        operator.succeed("systemctl stop radvd 2>/dev/null || true")
        operator.succeed(
            "systemd-run --unit=radvd --collect radvd -C /run/radvd.conf -m stderr -d 1 -n"
        )

    def route_to_router(stem):
        """Route the advertised /64 to the customer edge, 3GPP-style."""
        ll = router.succeed(
            "ip -6 -o addr show dev eth1 scope link | awk '{print $4}' | cut -d/ -f1 | head -1"
        ).strip()
        operator.succeed(f"ip -6 route replace {stem}::/64 via {ll} dev eth1")

    def capture(dst, src=MAIN):
        """Ping dst from the client while capturing on the operator."""
        operator.succeed(
            "systemd-run --unit=cap --collect tcpdump -n -U -i eth1 -w /tmp/cap.pcap icmp6"
        )
        operator.sleep(1)
        rc, _ = client.execute(f"ping -6 -c 3 -W 2 -I {src} {dst}")
        operator.sleep(1)
        operator.succeed("systemctl stop cap || true")
        text = operator.succeed("tcpdump -n -r /tmp/cap.pcap 2>/dev/null || true")
        operator.succeed("rm -f /tmp/cap.pcap")
        return rc, text

    start_all()

    for m in (operator, router, evented, client):
        m.wait_for_unit("network.target")
    for m in (router, evented):
        m.wait_for_unit("systemd-networkd.service")
        m.wait_for_unit("route-balancer.service")

    # Watch for prefix events across the whole run; none should ever appear.
    for m in (router, evented):
        m.succeed(
            "systemd-run --collect --unit=mon-prefix "
            "--property=StandardOutput=file:/tmp/prefix.log "
            f"timeout 1200 {STDBUF} -oL {IP} monitor prefix"
        )

    def prefix_events(node=None):
        return (node or router).succeed("cat /tmp/prefix.log || true").strip()

    started = {"router": main_pid(router), "evented": main_pid(evented)}

    client.succeed("ip -6 route replace default via fd00:dead:beef:1::1 dev eth1")

    # ── 0. The platform this test exists for ─────────────────────────────────
    with subtest("networkd holds the kernel's RA implementation off, sysctl and all"):
        assert router.succeed("cat /proc/sys/net/ipv6/conf/all/forwarding").strip() == "1"

        # networkd writes over the module's declared accept_ra at link-configure time.
        declared = router.succeed("grep -h 'eth1.accept_ra' /etc/sysctl.d/*.conf || true")
        assert "2" in declared, f"the module no longer declares accept_ra:\n{declared}"
        effective = router.succeed("cat /proc/sys/net/ipv6/conf/eth1/accept_ra").strip()
        assert effective == "0", (
            f"expected networkd to win over the declared sysctl, got {effective}"
        )

        # Nothing is translated yet, but the uplink is already guarded.
        assert "rb-nptv6 " not in ruleset(), (
            f"translation rules installed before any prefix was observed:\n{ruleset()}"
        )
        assert 'oifname "eth1" drop' in ruleset(), (
            f"uplink with no lease is not guarded:\n{ruleset()}"
        )

    # ── 1. A lease with no event to carry it ─────────────────────────────────
    with subtest("the daemon acquires the prefix from the address alone"):
        advertise([(RA1, 600, 300, "on")])

        for m in (router, evented):
            m.wait_until_succeeds(f"{SEED} | grep -q {RA1}:", timeout=90)

        # What networkd leaves behind: a dynamic global address and a proto ra
        # route. The daemon reads the address and deliberately not the route.
        routes = router.succeed("ip -6 route show dev eth1")
        print(f"routes networkd installed from the RA:\n{routes}")
        assert f"{RA1}::/64" in routes and "proto ra" in routes, routes

        wait_mapped(RA1)
        assert prefix_events() == "", (
            f"the kernel emitted prefix events after all:\n{prefix_events()}"
        )
        assert "source=ra" in journal("external prefix acquired"), (
            journal("NPTv6")
        )

        # A /64 holds one internal subnet, so the loser is named in the log.
        router.wait_until_succeeds(
            "journalctl -u route-balancer.service --no-pager | "
            "grep 'delegation too small' | grep -q 'guest'", timeout=30,
        )

    with subtest("the node with no ticker gets the same lease, from the address event"):
        # With reconcileInterval = null, every read after boot is one an
        # RTM_NEWADDR asked for.
        wait_mapped(RA1, node=evented)

        # The mechanism, not just the outcome.
        assert journal("IPv6 address change", node=evented) != "", (
            "the lease arrived on a node with no ticker, but not via an address "
            f"event:\n{journal('resync', node=evented)}"
        )
        assert main_pid(evented) == started["evented"], (
            "the daemon restarted; a startup resync would explain the lease "
            "without the trigger doing anything"
        )

    # ── 2. And it forwards ───────────────────────────────────────────────────
    with subtest("LAN traffic leaves translated into the advertised prefix"):
        route_to_router(RA1)
        rc, cap = capture(TRANSIT)
        assert rc == 0, f"ping through the networkd uplink failed:\n{cap}"
        assert f"{RA1}::10 >" in cap, f"source not translated as expected:\n{cap}"
        assert "fd00:" not in cap, f"untranslated ULA traffic reached the operator:\n{cap}"

    # ── 3. The renumber, which is the whole of §11.8 ─────────────────────────
    with subtest("a graceful renumber is tracked without a restart or a prefix event"):
        advertise([(RA1, 600, 0, "on"), (RA2, 120, 60, "on")])
        router.wait_until_succeeds(f"{SEED} | grep -q {RA2}:", timeout=90)
        t0 = time.monotonic()

        # Both /64s are live at once and the retired one has the longer valid
        # lifetime, so only preferred_lft tells them apart.
        lft = lifetimes()
        print(f"lifetimes across the renumber: {lft}")
        assert RA1 in lft and RA2 in lft, f"expected both prefixes live at once: {lft}"
        assert lft[RA1][1] == 0, f"the retired prefix is not deprecated: {lft}"
        assert lft[RA1][0] > lft[RA2][0], (
            f"the retired prefix does not outlive the replacement, so this run "
            f"does not exercise the tiebreak: {lft}"
        )

        wait_mapped(RA2)
        elapsed = time.monotonic() - t0
        print(f"renumber tracked {elapsed:.1f}s after the address appeared")
        # This node has both a ticker and the address trigger.
        assert elapsed < 30, (
            f"took {elapsed:.1f}s to notice a new address with a 3s reconcile interval"
        )

        rules = ruleset()
        assert f"{RA1}::/64" not in rules, (
            f"still translating into the retired prefix:\n{rules}"
        )
        assert main_pid() == started["router"], (
            "the daemon restarted; the point of this test is that it did not"
        )
        assert prefix_events() == "", prefix_events()

        route_to_router(RA2)
        rc, cap = capture(TRANSIT)
        assert rc == 0, f"ping after the renumber failed:\n{cap}"
        assert f"{RA2}::10 >" in cap, f"source not translated as expected:\n{cap}"

    with subtest("the address event alone tracks the renumber, with no ticker (§11.7)"):
        # The same renumber on the node with no periodic re-read at all.
        evented.wait_until_succeeds(f"{SEED} | grep -q {RA2}:", timeout=90)
        t0 = time.monotonic()

        wait_mapped(RA2, node=evented)
        elapsed = time.monotonic() - t0
        print(f"event-only node tracked the renumber {elapsed:.1f}s after the address appeared")
        # Generous, because t0 is a polling loop's idea of when the address appeared.
        assert elapsed < 20, (
            f"the address trigger took {elapsed:.1f}s on a node with no reconcile interval"
        )

        rules = ruleset(evented)
        assert f"{RA1}::/64" not in rules, (
            f"still translating into the retired prefix:\n{rules}"
        )
        assert main_pid(evented) == started["evented"], (
            "the daemon restarted; the point of this subtest is that it did not"
        )
        assert prefix_events(evented) == "", prefix_events(evented)

    # ── 4. The mobile shape: an advertisement that leaves only an address ────
    with subtest("an on-link-clear PIO installs no route, and the address suffices"):
        # The PIO's L bit is clear, so networkd installs no route for the prefix.
        advertise([(RA1, 600, 0, "on"), (RA2, 600, 0, "on"), (RA3, 900, 450, "off")])
        router.wait_until_succeeds(f"{SEED} | grep -q {RA3}:", timeout=90)

        routes = router.succeed("ip -6 route show dev eth1")
        assert f"{RA3}::/64" not in routes, (
            f"expected no route for an on-link-clear prefix:\n{routes}"
        )

        wait_mapped(RA3)
        route_to_router(RA3)
        rc, cap = capture(TRANSIT)
        assert rc == 0, f"ping through the address-only prefix failed:\n{cap}"
        assert f"{RA3}::10 >" in cap, f"source not translated as expected:\n{cap}"

    # ── 5. Withdrawal, which here is an absence and nothing else ─────────────
    with subtest("deprecating every prefix withdraws the lease"):
        # An interface holding nothing but deprecated addresses is a withdrawal.
        advertise([(RA1, 600, 0, "on"), (RA2, 600, 0, "on"), (RA3, 900, 0, "off")])
        router.wait_until_fails(
            f"{TABLE} 2>/dev/null | grep -q 'rb-nptv6 eth1 '", timeout=90,
        )
        assert journal("external prefix withdrawn") != "", journal("NPTv6")
        # And the guard is back, so the LAN fails locally rather than upstream.
        assert 'oifname "eth1" drop' in ruleset(), ruleset()

        lft = lifetimes()
        assert all(p == 0 for _, p in lft.values()), (
            f"expected every address deprecated: {lft}"
        )

    with subtest("re-advertising brings it back, still with no event"):
        # Recovery is the resync seeing a live address again, and nothing more.
        advertise([(RA3, 900, 450, "off")])
        wait_mapped(RA3)

    # ── 6. One daemon lifetime, no prefix events ─────────────────────────────
    with subtest("the whole run happened without a restart or a prefix event"):
        for name, node in (("router", router), ("evented", evented)):
            assert main_pid(node) == started[name], f"{name}: the daemon restarted"
            assert prefix_events(node) == "", (
                f"{name}: prefix events appeared:\n{prefix_events(node)}"
            )
  '';
}
