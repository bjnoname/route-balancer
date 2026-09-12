{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer-nptv6-ra";

  nodes = {
    operator = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 1 ];

      environment.systemPackages = [ pkgs.radvd pkgs.tcpdump pkgs.iputils ];

      boot.kernel.sysctl."net.ipv6.conf.all.forwarding" = 1;

      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        interfaces.eth1 = {
          ipv4.addresses = [{ address = "10.0.1.1"; prefixLength = 24; }];
          ipv6.addresses = [{ address = "2001:db8:ff01::1"; prefixLength = 64; }];
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
          eth1.ipv4.addresses = [{ address = "10.0.1.2"; prefixLength = 24; }];

          eth2.ipv6.addresses = [
            { address = "fd00:dead:beef:1::1"; prefixLength = 64; }
            { address = "fd00:dead:beef:2::1"; prefixLength = 64; }
          ];

          eth3.ipv4.addresses = [{ address = "10.0.3.2"; prefixLength = 24; }];
        };
      };

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };
        firewall = "nftables";
        reconcileInterval = "3s";
        logLevel = "debug";

        gateways.eth3 = { weight = 1; description = "Fibre"; };

        nptv6 = {
          enable = true;
          internalPrefix = "fd00:dead:beef::/48";

          subnets = {
            main = { prefix = "fd00:dead:beef:1::/64"; description = "Trusted LAN"; };
            guest = { prefix = "fd00:dead:beef:2::/64"; };
          };

          uplinks = {
            eth1 = {
              prefixSource = "ra";
              subnetPriority = [ "main" "guest" ];
              # §6 starts a flow at the operator, which is the capability this
              # opts into. eth3 below keeps the default and is checked for the
              # rule that stands in the dnat's place.
              allowInboundConnections = true;
            };
            eth3 = {
              prefixSource = "route";
              matchPrefix = "2001:db8:a000::/36";
              subnetPriority = [ "main" "guest" ];
            };
          };
        };
      };
    };

    client = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 2 ];

      environment.systemPackages = [ pkgs.iputils ];

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

    for m in (operator, router, client):
        m.wait_for_unit("network.target")
    router.wait_for_unit("route-balancer.service")

    # The advertised prefix, before and after the operator renumbers.
    RA1 = "2001:db8:ff00::/64"
    RA2 = "2001:db8:ff02::/64"
    TRANSIT = "2001:db8:ff01::1"

    MAIN = "fd00:dead:beef:1::10"
    GUEST = "fd00:dead:beef:2::10"
    MAIN_NET = "fd00:dead:beef:1::/64"
    GUEST_NET = "fd00:dead:beef:2::/64"

    TABLE = "nft list table ip6 route-balancer-nptv6"
    LEARNED = "nft list set ip6 route-balancer-nptv6-ndp learned"
    PROXY = "ip -6 neigh show proxy"

    def ruleset():
        return router.succeed(f"{TABLE} 2>/dev/null || true")

    def learned():
        return router.succeed(f"{LEARNED} 2>/dev/null || true")

    def proxies():
        return router.succeed(f"{PROXY} 2>/dev/null || true")

    def wait_proxy(addr, uplink="eth1", timeout=90):
        """Wait for the daemon to answer for a translated address."""
        router.wait_until_succeeds(
            f"{PROXY} | grep -q '^{addr} dev {uplink} proxy proto 111'", timeout=timeout,
        )

    def wait_no_proxy(addr, timeout=90):
        router.wait_until_fails(f"{PROXY} | grep -q '^{addr} '", timeout=timeout)

    # eth1 asks for inbound so that §6 can start a flow at the operator; eth3
    # keeps the default, so its ingress half is the counted drop instead.
    INBOUND = {"eth1"}

    def ingress_rule(uplink, internal, external):
        if uplink in INBOUND:
            return f'ip6 daddr {external} iifname "{uplink}" dnat prefix to {internal}'
        return f'ip6 daddr {external} iifname "{uplink}" ct state new counter'

    def wait_mapped(uplink, internal, external, timeout=60):
        router.wait_until_succeeds(
            f"{TABLE} | grep -q 'saddr {internal} oifname \"{uplink}\" snat prefix to {external}'",
            timeout=timeout,
        )
        router.wait_until_succeeds(
            f"{TABLE} | grep -qF '{ingress_rule(uplink, internal, external)}'",
            timeout=timeout,
        )

    def advertise(prefix, valid=300, preferred=150):
        """(Re)start radvd advertising one prefix with the given lifetimes."""
        conf = "\n".join([
            "interface eth1 {",
            "  AdvSendAdvert on;",
            "  MinRtrAdvInterval 3;",
            "  MaxRtrAdvInterval 4;",
            f"  prefix {prefix} " + "{",
            "    AdvOnLink on;",
            "    AdvAutonomous on;",
            f"    AdvValidLifetime {valid};",
            f"    AdvPreferredLifetime {preferred};",
            "  };",
            "};",
        ])
        operator.succeed(f"cat > /run/radvd.conf <<'RADVD'\n{conf}\nRADVD")
        operator.succeed("systemctl stop radvd 2>/dev/null || true")
        operator.succeed(
            "systemd-run --unit=radvd --collect radvd -C /run/radvd.conf -m stderr -d 1 -n"
        )

    def route_delegation_to_router(prefix):
        """Route the advertised /64 to the customer edge, 3GPP-style."""
        ll = router.succeed(
            "ip -6 -o addr show dev eth1 scope link | awk '{print $4}' | cut -d/ -f1 | head -1"
        ).strip()
        operator.succeed(f"ip -6 route replace {prefix} via {ll} dev eth1")

    def capture(src, dst, node=None):
        """Ping dst from src on the client while capturing on the operator."""
        operator.succeed(
            "systemd-run --unit=cap --collect tcpdump -n -U -i eth1 -w /tmp/cap.pcap icmp6"
        )
        operator.sleep(1)
        rc, _ = (node or client).execute(f"ping -6 -c 3 -W 2 -I {src} {dst}")
        operator.sleep(1)
        operator.succeed("systemctl stop cap || true")
        text = operator.succeed("tcpdump -n -r /tmp/cap.pcap 2>/dev/null || true")
        operator.succeed("rm -f /tmp/cap.pcap")
        return rc, text

    client.succeed("ip -6 route replace default via fd00:dead:beef:1::1 dev eth1")

    # ── 0. Prerequisites the module is responsible for ───────────────────────
    with subtest("module sets accept_ra=2 on the ra uplink"):
        # With forwarding on, only accept_ra = 2 makes the kernel parse an RA.
        assert router.succeed("cat /proc/sys/net/ipv6/conf/all/forwarding").strip() == "1"
        assert router.succeed("cat /proc/sys/net/ipv6/conf/eth1/accept_ra").strip() == "2", (
            router.succeed("sysctl -a 2>/dev/null | grep 'eth1.accept_ra'")
        )
        # The unit is Type=simple, so "active" means the process was forked,
        # not that it has reconciled anything. Both tables below are the work
        # of the first pass, and reading them without waiting asserts how fast
        # this machine boots rather than what the daemon did: on a warm page
        # cache the router reaches this line half a second after the pass is
        # planned, and on a cold one a good fifteen seconds after it finished.
        router.wait_until_succeeds(
            f"{TABLE} | grep -q 'oifname \"eth1\" drop'", timeout=60
        )
        router.wait_until_succeeds("nft list table ip6 route-balancer-nptv6-ndp", timeout=60)

        # The uplink holds no lease, so it is guarded — and the wait above is
        # what makes this next one mean something: against a table that does
        # not exist yet, "no translation rules" passes for the wrong reason.
        assert "rb-nptv6 " not in ruleset(), (
            f"translation rules installed before any prefix was observed:\n{ruleset()}"
        )

        # Proxy NDP defaults on for an "ra" uplink and off for a delegation.
        assert router.succeed("cat /proc/sys/net/ipv6/conf/eth1/proxy_ndp").strip() == "1"
        assert proxies() == "", f"proxy entries exist before any host was seen:\n{proxies()}"

        # The learning table is built from the configuration alone.
        learn = router.succeed("nft list table ip6 route-balancer-nptv6-ndp")
        print(f"learning table:\n{learn}")
        assert 'saddr fd00:dead:beef:1::/64 oifname "eth1"' in learn, learn
        assert 'saddr fd00:dead:beef:2::/64 oifname "eth1"' in learn, learn
        assert "eth3" not in learn, f"the routed uplink should not be learned on:\n{learn}"
        assert "elements" not in learned(), f"host learned before any traffic:\n{learned()}"

    # ── 1. The kernel really does process the RA on a forwarding host ────────
    with subtest("a forwarding router accepts the RA and configures itself from it"):
        advertise(RA1)
        # A SLAAC address out of the advertised prefix: the trace an RA leaves.
        router.wait_until_succeeds(
            "ip -6 -o addr show dev eth1 scope global dynamic | grep -q '2001:db8:ff00:'",
            timeout=60,
        )
        # And a default route, so the uplink is usable.
        router.wait_until_succeeds(
            "ip -6 route show default | grep -q 'dev eth1'", timeout=60,
        )

        # The kernel tags the PIO's on-link route proto kernel, not proto ra.
        routes = router.succeed("ip -6 route show dev eth1")
        print(f"routes the kernel installed from the RA:\n{routes}")
        assert "2001:db8:ff00::/64" in routes, routes
        assert router.succeed(
            "ip -6 route show proto ra dev eth1 | grep -c '2001:db8:ff00::/64' || true"
        ).strip() == "0", routes

    # ── 2. One /64 maps exactly one subnet ───────────────────────────────────
    with subtest("the RA prefix is attributed to its interface and maps one subnet"):
        wait_mapped("eth1", MAIN_NET, "2001:db8:ff00::/64")
        rules = ruleset()
        assert "rb-nptv6 eth1 guest" not in rules, rules
        # The subnet that lost is named, not silently dropped.
        router.wait_until_succeeds(
            "journalctl -u route-balancer.service --no-pager | "
            "grep 'delegation too small' | grep -q 'guest'", timeout=30,
        )
        router.succeed(
            "journalctl -u route-balancer.service --no-pager | grep -q 'source=ra'"
        )

    # ── 3. Traffic out of the tethered uplink is translated ──────────────────
    with subtest("the mapped subnet reaches the operator through the RA prefix"):
        route_delegation_to_router(RA1)
        rc, cap = capture(MAIN, TRANSIT)
        assert rc == 0, f"ping through the tethered uplink failed:\n{cap}"
        # Prefix rewritten, host identifier carried across.
        assert "2001:db8:ff00::10 >" in cap, f"source not translated as expected:\n{cap}"
        assert "fd00:" not in cap, f"untranslated ULA traffic reached the operator:\n{cap}"

    # ── 4. The subnet that did not fit ───────────────────────────────────────
    # guest leaves bearing its ULA source, which nothing upstream can answer.
    with subtest("the unmapped subnet leaves untranslated and is unanswerable"):
        rc, cap = capture(GUEST, TRANSIT)
        assert rc != 0, "guest traffic was answered through an uplink that cannot map it"
        assert "fd00:dead:beef:2::10 >" in cap, f"expected untranslated guest traffic:\n{cap}"

    # ── 5. The data plane teaches the daemon which hosts are live ────────────
    # The first forwarded packet is the announcement, recorded in a dynamic set.
    with subtest("forwarded traffic teaches the daemon a host identifier"):
        elements = learned()
        print(f"learned set:\n{elements}")
        assert 'fd00:dead:beef:1::10 . "eth1"' in elements, elements
        # Guest traffic is learned too, even though nothing maps it today.
        assert 'fd00:dead:beef:2::10 . "eth1"' in elements, elements

        # Only the mapped subnet becomes an answerable address, proto-tagged.
        wait_proxy("2001:db8:ff00::10")
        entries = proxies()
        assert "2001:db8:ff00::10 dev eth1 proxy proto 111" in entries, entries
        # The unmapped subnet has no external address to answer for.
        assert len([l for l in entries.splitlines() if l.strip()]) == 1, entries

    # ── 6. Inbound-initiated flows exercise the other half of the mapping ────
    # This flow starts at the operator, so only the dnat rule can deliver it.
    with subtest("an unsolicited inbound packet is translated to the LAN host"):
        operator.succeed(f"ping -6 -c 3 -W 5 -I {TRANSIT} 2001:db8:ff00::10")

    # ── 7. A delegation alongside an RA ──────────────────────────────────────
    # eth1 attributes by interface index, eth3 by ISP aggregate.
    with subtest("a PD delegation on a second uplink does not disturb the RA uplink"):
        router.succeed("ip route add default via 10.0.3.1 dev eth3 metric 500")
        router.succeed("ip -6 route add unreachable 2001:db8:a000::/56 dev lo")

        wait_mapped("eth3", MAIN_NET, "2001:db8:a000::/64")
        wait_mapped("eth3", GUEST_NET, "2001:db8:a000:1::/64")
        # The RA uplink kept its own prefix and still maps only main.
        rules = ruleset()
        assert "rb-nptv6 eth1 guest" not in rules, rules
        eth1_rules = [l for l in rules.splitlines() if 'eth1' in l]
        assert len(eth1_rules) == 2, eth1_rules
        assert all("2001:db8:ff00:" in l for l in eth1_rules), eth1_rules
        # The routed uplink is not learned on and gets no proxy entries.
        assert "eth3" not in proxies(), proxies()
        # It also did not ask for inbound, so it carries no dnat at all.
        assert 'iifname "eth3" dnat' not in rules, rules
        assert "rb-nptv6-closed eth3 main" in rules, rules

    # ── 8. Restart recovers both sources from the kernel ─────────────────────
    # Nothing is cached: the RA half comes back out of the SLAAC address.
    with subtest("restart mid-lease restores the RA mapping without waiting for an RA"):
        router.systemctl("restart route-balancer.service")
        router.wait_for_unit("route-balancer.service")
        wait_mapped("eth1", MAIN_NET, "2001:db8:ff00::/64")
        wait_mapped("eth3", MAIN_NET, "2001:db8:a000::/64")

        # A clean shutdown takes the learning table and its identifiers with it.
        assert "elements" not in learned(), learned()
        assert proxies() == "", f"entries survived a restart with an empty set:\n{proxies()}"

        # One forwarded packet is enough to put it all back.
        client.execute(f"ping -6 -c 2 -W 2 -I {MAIN} {TRANSIT}")
        wait_proxy("2001:db8:ff00::10")

    # ── 9. The operator renumbers ────────────────────────────────────────────
    # The LAN keeps its ULA plan across the renumber.
    with subtest("a renumbered RA moves the mapping and leaves the LAN alone"):
        advertise(RA2)
        wait_mapped("eth1", MAIN_NET, "2001:db8:ff02::/64")
        route_delegation_to_router(RA2)

        rc, cap = capture(MAIN, TRANSIT)
        assert rc == 0, f"ping after renumbering failed:\n{cap}"
        assert "2001:db8:ff02::10 >" in cap, f"source not translated as expected:\n{cap}"

    with subtest("the proxy entries follow the renumbering"):
        # The learned state is the internal address, which a renumber leaves alone.
        wait_proxy("2001:db8:ff02::10")
        wait_no_proxy("2001:db8:ff00::10")
        assert 'fd00:dead:beef:1::10 . "eth1"' in learned(), learned()

    # ── 10. Withdrawal ───────────────────────────────────────────────────────
    # There is no RTM_DELPREFIX: a zero valid lifetime is the only signal.
    with subtest("a zero valid lifetime withdraws the mapping"):
        advertise(RA2, valid=0, preferred=0)
        router.wait_until_fails(
            f"{TABLE} 2>/dev/null | grep -q 'rb-nptv6 eth1 '", timeout=60,
        )
        # The fibre uplink is untouched by its neighbour going away.
        assert "rb-nptv6 eth3 main" in ruleset(), ruleset()
        # An address nothing translates any more must not keep being answered for.
        wait_no_proxy("2001:db8:ff02::10")

    with subtest("re-advertising the prefix brings the mapping back"):
        advertise(RA2)
        wait_mapped("eth1", MAIN_NET, "2001:db8:ff02::/64")
        wait_proxy("2001:db8:ff02::10")

    # ── 11. The on-link tether ───────────────────────────────────────────────
    # An on-link prefix means a translated address is neighbour-solicited on
    # the link, for an address that exists only inside nftables.
    with subtest("an on-link operator reaches translated addresses"):
        operator.succeed(f"ip -6 route del {RA2} 2>/dev/null || true")
        operator.succeed("ip -6 addr replace 2001:db8:ff02::1/64 dev eth1")
        operator.sleep(2)
        operator.succeed("ip -6 neigh flush dev eth1")

        wait_proxy("2001:db8:ff02::10")
        client.succeed(f"ping -6 -c 3 -W 5 -I {MAIN} {TRANSIT}")

    with subtest("the entries are inert without proxy_ndp, which is why the module sets it"):
        # The entry stays in the table; the kernel simply stops consulting it.
        #
        # The daemon is frozen for the demonstration, because the knob is a
        # resource it owns: a reconcile landing inside the ping's wait window
        # turns it back on and the solicitation is answered after all. That
        # restore is proxy-ndp/<uplink> working, so the test gets out of its
        # way rather than racing it.
        router.succeed("systemctl kill -s SIGSTOP route-balancer.service")
        router.succeed("sysctl -w net.ipv6.conf.eth1.proxy_ndp=0")
        operator.succeed("ip -6 neigh flush dev eth1")
        rc, _ = client.execute(f"ping -6 -c 2 -W 3 -I {MAIN} {TRANSIT}")
        assert rc != 0, "a translated address was reachable with proxy_ndp off"

        router.succeed("systemctl kill -s SIGCONT route-balancer.service")
        router.succeed("sysctl -w net.ipv6.conf.eth1.proxy_ndp=1")
        operator.succeed("ip -6 neigh flush dev eth1")
        client.wait_until_succeeds(f"ping -6 -c 2 -W 3 -I {MAIN} {TRANSIT}", timeout=30)

    # ── 12. The address that is not ours to answer for ───────────────────────
    # A LAN host can map onto an address something upstream already owns. An
    # address with no history is claimed at once and checked immediately after.
    def journal(pattern):
        return router.succeed(
            "journalctl -u route-balancer.service --no-pager | "
            f"grep '{pattern}' || true"
        )

    with subtest("a claim another host answers for is retracted"):
        operator.succeed("ip -6 addr replace 2001:db8:ff02::77/64 dev eth1")
        operator.sleep(2)
        client.succeed("ip -6 addr add fd00:dead:beef:1::77/64 dev eth1 nodad")
        # One forwarded packet is all the learning needs.
        client.execute(f"ping -6 -c 2 -W 2 -I fd00:dead:beef:1::77 {TRANSIT}")

        router.wait_until_succeeds(
            "journalctl -u route-balancer.service --no-pager | "
            "grep -q 'withdrawing a speculative claim'", timeout=60,
        )
        router.succeed(
            "journalctl -u route-balancer.service --no-pager | "
            "grep -q 'refusing to answer for an address that is already claimed'"
        )
        wait_no_proxy("2001:db8:ff02::77")
        # The host that did not collide is unaffected.
        assert "2001:db8:ff02::10 dev eth1 proxy" in proxies(), proxies()

    with subtest("a contested address is never speculated on twice"):
        # A conflicted address keeps its history and is proved free before every
        # later claim.
        router.sleep(40)
        claims = journal("address=2001:db8:ff02::77")
        print(f"claims on the contested address:\n{claims}")
        speculative = len([l for l in claims.splitlines() if "claim=speculative" in l])
        assert speculative == 1, (
            f"contested address was claimed on spec {speculative} times:\n{claims}"
        )
        assert "2001:db8:ff02::77" not in proxies(), proxies()

    with subtest("the contested address is reconsidered once it is released"):
        # Released, it becomes claimable again, but by the cautious path.
        operator.succeed("ip -6 addr del 2001:db8:ff02::77/64 dev eth1")
        wait_proxy("2001:db8:ff02::77")
        claims = journal("address=2001:db8:ff02::77")
        assert "claim=probed" in claims, claims
        assert len([l for l in claims.splitlines() if "claim=speculative" in l]) == 1, claims

    # ── 13. Clean shutdown ───────────────────────────────────────────────────
    with subtest("shutdown removes the translation table and every proxy entry"):
        router.systemctl("stop route-balancer.service")
        router.wait_until_fails("nft list table ip6 route-balancer-nptv6", timeout=20)
        # Proxy entries are kernel state outside nftables.
        router.wait_until_fails("nft list table ip6 route-balancer-nptv6-ndp", timeout=20)
        assert proxies() == "", f"proxy entries survived shutdown:\n{proxies()}"
  '';
}
