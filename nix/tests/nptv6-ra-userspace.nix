{ pkgs }:

let
  first = "2001:db8:cafe::";
  second = "2001:db8:beef::";
  firstText = "2001:db8:cafe:";
  secondText = "2001:db8:beef:";

  raSniffer = pkgs.writeScript "ra-sniffer" ''
    #!${pkgs.python3}/bin/python3
    import socket, sys, time

    s = socket.socket(socket.AF_INET6, socket.SOCK_RAW, 58)
    s.setsockopt(socket.SOL_SOCKET, 25, b"eth1\0")  # SO_BINDTODEVICE
    s.settimeout(1)
    end = time.time() + int(sys.argv[1])
    while time.time() < end:
        try:
            data, addr = s.recvfrom(2048)
        except socket.timeout:
            continue
        if not data or data[0] != 134:  # ND_ROUTER_ADVERT
            continue
        prefixes, i = [], 16
        while i + 8 <= len(data):
            otype, olen = data[i], data[i + 1] * 8
            if olen == 0:
                break
            if otype == 3 and i + 32 <= len(data):  # Prefix Information
                plen = data[i + 2]
                flags = data[i + 3]
                pfx = socket.inet_ntop(socket.AF_INET6, data[i + 16:i + 32])
                prefixes.append(f"{pfx}/{plen} L={(flags >> 7) & 1} A={(flags >> 6) & 1}")
            i += olen
        print(f"RA from {addr[0]} {prefixes}", flush=True)
  '';

  client = { lib, pkgs, ... }: {
    virtualisation.vlans = [ 1 ];
    environment.systemPackages = [ pkgs.iproute2 pkgs.python3 ];
    networking = {
      useDHCP = lib.mkForce false;
      firewall.enable = false;
    };
  };

  networkd = { lib, ... }: {
    imports = [ client ];
    networking.useNetworkd = true;
    systemd.network.networks."40-eth1" = {
      matchConfig.Name = "eth1";
      networkConfig.IPv6AcceptRA = true;
      linkConfig.RequiredForOnline = false;
    };
  };
in
pkgs.testers.nixosTest {
  name = "route-balancer-nptv6-ra-userspace";

  nodes = {
    operator = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 1 ];
      environment.systemPackages = [ pkgs.radvd ];
      boot.kernel.sysctl."net.ipv6.conf.all.forwarding" = 1;
      networking = {
        useDHCP = lib.mkForce false;
        firewall.enable = false;
        interfaces.eth1.ipv6.addresses = [{ address = "2001:db8:ff::1"; prefixLength = 64; }];
      };
      services.radvd = {
        enable = true;
        config = ''
          interface eth1 {
            AdvSendAdvert on;
            MinRtrAdvInterval 3;
            MaxRtrAdvInterval 4;
            prefix ${first}/64 { AdvOnLink on; AdvAutonomous on; };
          };
        '';
      };
    };

    kernelra = { ... }: { imports = [ client ]; };

    netd = { ... }: { imports = [ networkd ]; };

    netdsysctl = { ... }: {
      imports = [ networkd ];
      boot.kernel.sysctl."net.ipv6.conf.eth1.accept_ra" = 2;
    };

    dhcpcdrs = { lib, ... }: {
      imports = [ client ];
      networking.interfaces.eth1.useDHCP = true;
      networking.interfaces.eth1.ipv6.addresses = lib.mkForce [ ];
    };

    dhcpcdnoirs = { ... }: {
      imports = [ client ];
      networking.interfaces.eth1.useDHCP = true;
    };
  };

  testScript = ''
    IP = "/run/current-system/sw/bin/ip"
    STDBUF = "/run/current-system/sw/bin/stdbuf"

    # Verbatim the command seedRAAddresses() runs.
    SEED = "ip -6 -o addr show dev eth1 scope global dynamic"

    FIRST = "${firstText}"
    SECOND = "${secondText}"

    def seed(m):
        return m.succeed(f"{SEED} || true")

    def freshest_prefix(out):
        """What freshestRAPrefix() would pick: the /64 with the most valid
        lifetime left. After a renumber both prefixes are live at once and the
        superseded one stops being wound back up, which is the whole tiebreak."""
        best, best_lft = None, -1
        for line in out.splitlines():
            fields = line.split()
            addr, lft = None, 0
            for i, f in enumerate(fields):
                if f == "inet6" and i + 1 < len(fields):
                    addr = fields[i + 1]
                if f == "valid_lft" and i + 1 < len(fields):
                    lft = int(fields[i + 1].removesuffix("sec"))
            if addr and lft > best_lft:
                best, best_lft = addr.split("/")[0], lft
        return best

    def accept_ra(m):
        return m.succeed("cat /proc/sys/net/ipv6/conf/eth1/accept_ra").strip()

    def captured(m, obj):
        return m.succeed(f"cat /tmp/{obj}.log || true")

    start_all()
    operator.wait_for_unit("radvd.service")

    kernel_side = [("kernelra", kernelra), ("dhcpcdnoirs", dhcpcdnoirs)]
    userspace_side = [("netd", netd), ("netdsysctl", netdsysctl), ("dhcpcdrs", dhcpcdrs)]
    clients = kernel_side + userspace_side

    for _, m in clients:
        m.wait_for_unit("multi-user.target")

    # Line-buffered: ip monitor block-buffers into a file otherwise.
    for _, m in clients:
        for obj in ("address", "route", "prefix"):
            m.succeed(
                f"systemd-run --collect --unit=mon-{obj} "
                f"--property=StandardOutput=file:/tmp/{obj}.log "
                f"timeout 180 {STDBUF} -oL {IP} monitor {obj}"
            )
    netd.succeed(
        "systemd-run --collect --unit=rasniff "
        "--property=StandardOutput=file:/tmp/ra.log "
        "${raSniffer} 180"
    )

    for _, m in clients:
        m.wait_until_succeeds(f"{SEED} | grep -q ${firstText}", timeout=60)

    # ── 1. Who ended up owning RA processing ─────────────────────────────────
    with subtest("kernel RA survives only where nothing in userspace claims it"):
        for name, m in kernel_side:
            assert accept_ra(m) != "0", f"{name}: kernel RA unexpectedly off"
        for name, m in userspace_side:
            assert accept_ra(m) == "0", (
                f"{name}: expected the userspace RA client to force accept_ra=0, "
                f"got {accept_ra(m)}"
            )

    with subtest("networkd overwrites a declared accept_ra=2 (§11.4)"):
        # The module's assertion reads the declared value, not the live one.
        declared = netdsysctl.succeed(
            "sysctl -n net.ipv6.conf.eth1.accept_ra"
        ).strip()
        assert declared == "0", (
            f"expected networkd to win over the declared sysctl, got {declared}"
        )

    # ── 2. What that costs, and what it does not ─────────────────────────────
    with subtest("RTM_NEWPREFIX fires only on the kernel side"):
        for name, m in kernel_side:
            assert FIRST in captured(m, "prefix"), (
                f"{name}: no prefix events:\n{captured(m, 'prefix')}"
            )
        for name, m in userspace_side:
            assert captured(m, "prefix").strip() == "", (
                f"{name}: unexpected prefix events:\n{captured(m, 'prefix')}"
            )

    with subtest("the seed still finds the prefix everywhere (§11.5)"):
        # `scope global dynamic` returns the userspace client's address too.
        for name, m in clients:
            out = seed(m)
            assert FIRST in out, f"{name}: seed finds nothing:\n{out}"
            assert "valid_lft" in out, f"{name}: no lifetime to sort on:\n{out}"
            assert freshest_prefix(out).startswith(FIRST), (
                f"{name}: freshestRAPrefix would pick the wrong address:\n{out}"
            )

    # ── 3. A renumber, which is where the missing event actually bites ───────
    with subtest("a renumber is invisible to RTM_NEWPREFIX but not to everything"):
        operator.succeed("systemctl stop radvd.service")
        operator.succeed(
            "printf 'interface eth1 {\\n"
            "  AdvSendAdvert on;\\n  MinRtrAdvInterval 3;\\n  MaxRtrAdvInterval 4;\\n"
            "  prefix ${second}/64 { AdvOnLink on; AdvAutonomous on; };\\n};\\n'"
            " > /run/radvd2.conf"
        )
        operator.succeed(
            "systemd-run --collect --unit=radvd2 "
            "$(command -v radvd) -C /run/radvd2.conf -n -m stderr"
        )

        for name, m in clients:
            m.wait_until_succeeds(f"{SEED} | grep -q ${secondText}", timeout=60)

        for name, m in userspace_side:
            addrlog, routelog = captured(m, "address"), captured(m, "route")
            # RTM_NEWADDR fires wherever the address came from.
            assert SECOND in addrlog, (
                f"{name}: no address event for the new prefix:\n{addrlog}"
            )
            # A PIO with the on-link bit clear installs no route at all.
            assert SECOND in routelog, (
                f"{name}: no route event for the new prefix:\n{routelog}"
            )
            assert "proto ra" in routelog, (
                f"{name}: expected the userspace client's routes to carry "
                f"RTPROT_RA:\n{routelog}"
            )
            assert captured(m, "prefix").strip() == "", (
                f"{name}: prefix events appeared after the renumber:\n"
                f"{captured(m, 'prefix')}"
            )

    with subtest("both /64s are live at once and the freshest one wins"):
        # Four global dynamic addresses across two /64s while the old one lingers.
        out = seed(netd)
        assert FIRST in out and SECOND in out, (
            f"expected both prefixes live at once:\n{out}"
        )
        assert freshest_prefix(out).startswith(SECOND), (
            f"freshestRAPrefix would still pick the superseded prefix:\n{out}"
        )

    with subtest("a raw ICMPv6 socket sees the RA regardless of accept_ra"):
        ra = netd.succeed("cat /tmp/ra.log || true")
        assert FIRST in ra and SECOND in ra, (
            f"raw socket missed the advertisement on an accept_ra=0 link:\n{ra}"
        )
        # The PIO flags come with it, which no derived signal carries.
        assert "L=1 A=1" in ra, ra
  '';
}
