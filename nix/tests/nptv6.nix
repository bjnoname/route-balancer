{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer-nptv6";

  nodes.router = { lib, pkgs, ... }: {
    imports = [ (import ../module/route-balancer.nix) ];

    virtualisation.vlans = [ 1 2 ];

    networking = {
      useDHCP = lib.mkForce false;
      nftables.enable = true;
      interfaces = {
        eth1.ipv4.addresses = [{ address = "10.0.1.2"; prefixLength = 24; }];
        eth2.ipv4.addresses = [{ address = "10.0.2.2"; prefixLength = 24; }];
      };
    };

    systemd.tmpfiles.rules = [ "f /run/wan1-healthy 0644 root root - -" ];

    services.route-balancer = {
      enable = true;
      package = pkgs.callPackage ../pkgs/route-balancer.nix { };
      firewall = "nftables";
      reconcileInterval = "3s";
      logLevel = "debug";

      gateways.eth1 = {
        weight = 1;
        description = "Fibre — health gates its NPTv6 rules";
        health = {
          unhealthyThreshold = 2;
          healthyThreshold = 2;
          interval = "1s";
          timeout = "2s";
          probe = {
            type = "exec";
            command = [ "/bin/sh" "-c" "test -f /run/wan1-healthy" ];
          };
        };
      };

      nptv6 = {
        enable = true;
        internalPrefix = "fd00:dead:beef::/48";

        subnets = {
          main = { prefix = "fd00:dead:beef:1::/64"; description = "Trusted LAN"; };
          guest = { prefix = "fd00:dead:beef:2::/64"; };
          iot = { prefix = "fd00:dead:beef:3::/64"; };
        };

        uplinks = {
          eth1 = {
            prefixSource = "route";
            matchPrefix = "2001:db8:a000::/36";
            subnetPriority = [ "main" "guest" "iot" ];
            # Opened deliberately, so both halves of the option are exercised
            # by the same run; eth2 below keeps the default.
            allowInboundConnections = true;
          };
          eth2 = {
            prefixSource = "route";
            matchPrefix = "2001:db8:b000::/36";
            subnetPriority = [ "main" "guest" ];
          };
        };
      };
    };
  };

  testScript = ''
    router.wait_for_unit("network.target")
    router.wait_for_unit("route-balancer.service")

    TABLE = "nft list table ip6 route-balancer-nptv6"

    def ruleset():
        """Current translation rules; empty when the table does not exist."""
        return router.succeed(f"{TABLE} 2>/dev/null || true")

    # Only eth1 asks for inbound. The outbound half of a mapping is the same
    # either way; the ingress half is a dnat there and a counted drop on eth2.
    INBOUND = {"eth1"}

    def ingress_rule(uplink, internal, external):
        if uplink in INBOUND:
            return f'ip6 daddr {external} iifname "{uplink}" dnat prefix to {internal}'
        return f'ip6 daddr {external} iifname "{uplink}" ct state new counter'

    def mapped(uplink, internal, external):
        """Both halves of one mapping present?"""
        rules = ruleset()
        snat = f'ip6 saddr {internal} oifname "{uplink}" snat prefix to {external}'
        return snat in rules and ingress_rule(uplink, internal, external) in rules

    def wait_mapped(uplink, internal, external, timeout=20):
        router.wait_until_succeeds(
            f"{TABLE} | grep -q 'saddr {internal} oifname \"{uplink}\" snat prefix to {external}'",
            timeout=timeout,
        )
        router.wait_until_succeeds(
            f"{TABLE} | grep -qF '{ingress_rule(uplink, internal, external)}'",
            timeout=timeout,
        )

    def wait_unmapped(uplink, timeout=20):
        router.wait_until_fails(
            f"{TABLE} 2>/dev/null | grep -qE 'rb-nptv6(-closed)? {uplink} '", timeout=timeout,
        )

    def mapping_count():
        # One snat per mapping whatever the uplink's inbound setting; neither
        # the guard nor the closed-inbound rule carries one.
        return int(router.succeed(
            f"{TABLE} 2>/dev/null | grep -c 'snat prefix to' || true"
        ).strip())

    def guarded(uplink):
        """Is this uplink's internal traffic being dropped rather than leaked?"""
        return f'oifname "{uplink}" drop' in ruleset()

    def wait_guarded(uplink, timeout=20):
        router.wait_until_succeeds(
            f"{TABLE} | grep -q 'oifname \"{uplink}\" drop'", timeout=timeout,
        )

    def applied_count():
        """How many times the daemon has pushed a ruleset to nft."""
        return int(router.succeed(
            "journalctl -u route-balancer.service --no-pager | "
            "grep -c 'NPTv6 translation rules applied' || true"
        ).strip())

    def handles():
        """Rule handles; they survive an in-place no-op but not a table rebuild."""
        return router.succeed(
            "nft -a list table ip6 route-balancer-nptv6 2>/dev/null || true"
        )

    MAIN = "fd00:dead:beef:1::/64"
    GUEST = "fd00:dead:beef:2::/64"
    IOT = "fd00:dead:beef:3::/64"

    # ── 0. Prerequisites the module is responsible for ───────────────────────
    with subtest("module enables IPv6 forwarding and generates an nptv6 config"):
        assert router.succeed("cat /proc/sys/net/ipv6/conf/all/forwarding").strip() == "1"
        cfg = router.succeed(
            "cat $(systemctl show -p ExecStart --value route-balancer.service | "
            "grep -o '/nix/store/[^ ]*\\.json' | head -1)"
        )
        assert '"internal_prefix":"fd00:dead:beef::/48"' in cfg, cfg
        assert '"match_prefix":"2001:db8:a000::/36"' in cfg, cfg

    with subtest("no lease yet: nothing is translated, and nothing leaks either"):
        assert mapping_count() == 0, f"rules installed before any prefix was observed:\n{ruleset()}"
        # An uplink with no lease gets a guard, so the table exists from the start.
        wait_guarded("eth1")
        wait_guarded("eth2")

    # ── 1. Health gating: a lease alone is not enough ────────────────────────
    # eth1 is a configured gateway: with no default route it is not live, and
    # its delegation must stay unused.
    with subtest("lease on a gateway uplink with no route installs nothing"):
        router.succeed("ip -6 route add unreachable 2001:db8:a000::/56 dev lo")
        router.sleep(5)
        assert mapping_count() == 0, f"rules installed for an inactive gateway:\n{ruleset()}"
        assert guarded("eth1"), f"an inactive gateway is not guarded:\n{ruleset()}"

    with subtest("uplink becoming an active gateway installs its mappings"):
        router.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")
        # And an IPv6 one: translation is gated on the IPv6 verdict.
        router.succeed("ip -6 route add default via 2001:db8:1::99 dev eth1 metric 1024")
        # A /56 holds 256 /64s, so every named subnet fits, in priority order.
        wait_mapped("eth1", MAIN, "2001:db8:a000::/64")
        wait_mapped("eth1", GUEST, "2001:db8:a000:1::/64")
        wait_mapped("eth1", IOT, "2001:db8:a000:2::/64")
        # A translating uplink needs no guard.
        assert not guarded("eth1"), f"a translating uplink is still guarded:\n{ruleset()}"

    # ── 2. Attribution between two simultaneous delegations ──────────────────
    with subtest("a second delegation is attributed by ISP aggregate"):
        # eth2 is not a gateway, so it needs no health signal to translate.
        router.succeed("ip -6 route add unreachable 2001:db8:b000::/60 dev lo")
        wait_mapped("eth2", MAIN, "2001:db8:b000::/64")
        wait_mapped("eth2", GUEST, "2001:db8:b000:1::/64")
        # eth2 names only main and guest, so iot is not mapped through it.
        assert not mapped("eth2", IOT, "2001:db8:b000:2::/64")
        # And the first uplink kept its own prefix.
        assert mapped("eth1", MAIN, "2001:db8:a000::/64")
        assert mapping_count() == 5, ruleset()

    # ── 2a. Inbound is a per-uplink decision ─────────────────────────────────
    # eth1 asked for it and carries the dnat; eth2 did not and carries the drop
    # that stands in its place — matching the external prefix, since with no
    # dnat nothing puts an internal address in an arriving destination.
    with subtest("only the uplink that asked for inbound carries a dnat"):
        rules = ruleset()
        assert 'iifname "eth2" dnat' not in rules, f"a closed uplink got a dnat:\n{rules}"
        # nft prints the counter's own packet and byte totals mid-rule, so the
        # match stops at the keyword.
        assert (
            'ip6 daddr 2001:db8:b000::/64 iifname "eth2" ct state new counter'
            in rules
        ), rules
        assert "rb-nptv6-closed eth2 main" in rules, rules
        assert "rb-nptv6-closed eth1" not in rules, f"an opened uplink got a drop:\n{rules}"
        # The closed rule sits in front of the nat chain, not in it.
        assert "type filter hook prerouting priority dstnat - 10" in rules, rules

    with subtest("the daemon says which uplinks are reachable and which are not"):
        router.wait_until_succeeds(
            "journalctl -u route-balancer.service --no-pager | "
            "grep 'inbound-initiated connections are dropped' | grep -q 'uplinks=eth2'",
            timeout=20,
        )
        router.wait_until_succeeds(
            "journalctl -u route-balancer.service --no-pager | "
            "grep 'reachable from the internet' | grep -q 'uplinks=eth1'",
            timeout=20,
        )

    with subtest("a delegation from an unknown aggregate is ignored"):
        before = mapping_count()
        router.succeed("ip -6 route add unreachable 2001:db8:c000::/56 dev lo")
        router.sleep(4)
        assert mapping_count() == before, "an unattributable delegation changed the rules"
        assert "c000" not in ruleset(), ruleset()
        router.succeed("ip -6 route del unreachable 2001:db8:c000::/56 dev lo")

    # ── 3. Renewal that returns the same prefix must not churn ───────────────
    # These rules are NETMAP and therefore stateful: a rebuild drops conntrack.
    with subtest("lease renewal with an unchanged prefix causes no rule churn"):
        before_applied, before_handles = applied_count(), handles()
        router.succeed("ip -6 route replace unreachable 2001:db8:a000::/56 dev lo")
        # The kernel does re-emit RTM_NEWROUTE for an identical replace.
        router.wait_until_succeeds(
            "journalctl -u route-balancer.service --no-pager | "
            "grep -q 'lease unchanged, no rule churn'", timeout=20,
        )
        router.sleep(6)  # two reconcile intervals
        assert applied_count() == before_applied, "unchanged renewal re-applied the ruleset"
        assert handles() == before_handles, "unchanged renewal replaced the nft rules"

    # ── 4. Renewal that returns a different prefix of the same length ────────
    with subtest("changed prefix, same length: rules move, assignment does not"):
        router.succeed("ip -6 route del unreachable 2001:db8:a000::/56 dev lo")
        router.succeed("ip -6 route add unreachable 2001:db8:a001::/56 dev lo")
        wait_mapped("eth1", MAIN, "2001:db8:a001::/64")
        wait_mapped("eth1", GUEST, "2001:db8:a001:1::/64")
        wait_mapped("eth1", IOT, "2001:db8:a001:2::/64")
        assert "2001:db8:a000:" not in ruleset(), ruleset()

    # ── 5. Renewal that shortens the delegation ──────────────────────────────
    # A /64 holds exactly one subnet, so priority order decides which one works.
    with subtest("shortened delegation drops the lowest-priority subnets"):
        router.succeed("ip -6 route del unreachable 2001:db8:a001::/56 dev lo")
        router.succeed("ip -6 route add unreachable 2001:db8:a002::/64 dev lo")
        wait_mapped("eth1", MAIN, "2001:db8:a002::/64")
        router.wait_until_fails(
            f"{TABLE} | grep -q 'rb-nptv6 eth1 guest'", timeout=20,
        )
        assert not mapped("eth1", IOT, "2001:db8:a002::/64")
        router.wait_until_succeeds(
            "journalctl -u route-balancer.service --no-pager | "
            "grep 'delegation too small' | grep -q 'guest,iot'", timeout=20,
        )
        # guest and iot now leave untranslated: the operator's own priority order.
        assert not guarded("eth1"), (
            f"a partially-mapped uplink was guarded, which would drop the "
            f"subnets subnet_priority chose to leave unmapped:\n{ruleset()}"
        )

    with subtest("delegation growing again re-adds the dropped subnets"):
        router.succeed("ip -6 route del unreachable 2001:db8:a002::/64 dev lo")
        router.succeed("ip -6 route add unreachable 2001:db8:a003::/56 dev lo")
        wait_mapped("eth1", MAIN, "2001:db8:a003::/64")
        wait_mapped("eth1", GUEST, "2001:db8:a003:1::/64")
        wait_mapped("eth1", IOT, "2001:db8:a003:2::/64")

    # ── 6. Health-driven withdrawal ──────────────────────────────────────────
    # An exec probe serves both families, and translation reads the v6 verdict.
    with subtest("unhealthy uplink stops translating, healthy siblings do not"):
        router.succeed("rm /run/wan1-healthy")
        wait_unmapped("eth1")
        assert mapped("eth2", MAIN, "2001:db8:b000::/64"), "healthy uplink lost its rules"

    # The v6 default route is untouched, so the guard turns the leak into a
    # local drop.
    with subtest("an uplink withdrawn by health is guarded, not left to leak"):
        wait_guarded("eth1")
        assert not guarded("eth2"), f"a healthy uplink was guarded:\n{ruleset()}"
        router.wait_until_succeeds(
            "journalctl -u route-balancer.service --no-pager | "
            "grep -q 'internal traffic leaving it is dropped locally'", timeout=20,
        )

    with subtest("recovered uplink translates again and the guard is lifted"):
        router.succeed("touch /run/wan1-healthy")
        wait_mapped("eth1", MAIN, "2001:db8:a003::/64")
        assert not guarded("eth1"), f"guard survived the uplink recovering:\n{ruleset()}"

    # ── 7. Restart mid-lease ─────────────────────────────────────────────────
    # Nothing is cached across a restart: the discard routes are re-read.
    with subtest("restart mid-lease restores the full rule set from the kernel"):
        router.systemctl("restart route-balancer.service")
        router.wait_for_unit("route-balancer.service")
        wait_mapped("eth1", MAIN, "2001:db8:a003::/64", timeout=30)
        wait_mapped("eth1", IOT, "2001:db8:a003:2::/64", timeout=30)
        wait_mapped("eth2", GUEST, "2001:db8:b000:1::/64", timeout=30)

    # ── 8. Drift repair ──────────────────────────────────────────────────────
    with subtest("externally deleted table is restored by the reconcile loop"):
        router.succeed("nft delete table ip6 route-balancer-nptv6")
        wait_mapped("eth1", MAIN, "2001:db8:a003::/64", timeout=30)

    with subtest("externally deleted single rule is restored by the reconcile loop"):
        handle = router.succeed(
            "nft -a list table ip6 route-balancer-nptv6 | "
            "grep 'rb-nptv6 eth2 main' | grep snat | "
            "sed 's/.*# handle //'"
        ).strip()
        router.succeed(f"nft delete rule ip6 route-balancer-nptv6 postrouting handle {handle}")
        wait_mapped("eth2", MAIN, "2001:db8:b000::/64", timeout=30)

    # ── 9. Withdrawal ────────────────────────────────────────────────────────
    with subtest("withdrawn delegation removes only that uplink's rules"):
        router.succeed("ip -6 route del unreachable 2001:db8:a003::/56 dev lo")
        wait_unmapped("eth1")
        assert mapped("eth2", MAIN, "2001:db8:b000::/64")

    # ── 10. Clean shutdown ───────────────────────────────────────────────────
    with subtest("shutdown removes the translation table"):
        router.systemctl("stop route-balancer.service")
        router.wait_until_fails("nft list table ip6 route-balancer-nptv6", timeout=20)
  '';
}
