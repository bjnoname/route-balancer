# nix/tests/health-probes.nix
#
# NixOS VM integration test for route-balancer health probes.
#
# Exercises ICMP, HTTP, TCP, DNS, and exec probe types end-to-end.
# Each probe is assigned its own gateway so failures can be isolated.
#
# Run with: nix build .#checks.x86_64-linux.health-probes-vm
#
# Topology
# ────────
#   machine  ── VLAN 1 ──  router1  (10.0.1.1)  ICMP probe target
#            ── VLAN 2 ──  router2  (10.0.2.1)  HTTP probe target  (nginx)
#            ── VLAN 3 ──  router2  (10.0.3.1)  TCP probe target   (same nginx)
#            ── VLAN 4 ──  router3  (10.0.4.1)  DNS probe target   (dnsmasq)
#            ── VLAN 5 ──  (none)   (10.0.5.1)  exec probe — flag-file command,
#                                               no peer VM needed
#
# All probes use fast parameters (1 s interval, 2-of-2 thresholds) so that
# failure detection and recovery each complete within ~3 seconds.

{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer-health-probes";

  # ── Nodes ──────────────────────────────────────────────────────────────────

  nodes = {

    # ── DUT: runs route-balancer with one health-monitored gateway per probe type
    machine = { lib, pkgs, ... }: {
      imports = [ (import ../module/route-balancer.nix) ];

      virtualisation.vlans = [ 1 2 3 4 5 ];

      networking = {
        useDHCP = lib.mkForce false;
        interfaces = {
          eth1.ipv4.addresses = [{ address = "10.0.1.2"; prefixLength = 24; }];
          eth2.ipv4.addresses = [{ address = "10.0.2.2"; prefixLength = 24; }];
          eth3.ipv4.addresses = [{ address = "10.0.3.2"; prefixLength = 24; }];
          eth4.ipv4.addresses = [{ address = "10.0.4.2"; prefixLength = 24; }];
          eth5.ipv4.addresses = [{ address = "10.0.5.2"; prefixLength = 24; }];
        };
      };

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };

        gateways = {
          # eth1 — ICMP probe: send echo request to gateway IP, expect reply.
          eth1 = {
            weight = 1;
            health = {
              unhealthyThreshold = 2;
              healthyThreshold = 2;
              interval = "1s";
              timeout = "800ms";
              probe.type = "icmp";
            };
          };

          # eth2 — HTTP probe: GET http://10.0.2.1/, expect 200.
          eth2 = {
            weight = 1;
            health = {
              unhealthyThreshold = 2;
              healthyThreshold = 2;
              interval = "1s";
              timeout = "800ms";
              probe = {
                type = "http";
                url = "http://10.0.2.1/";
                expectedStatus = [ 200 ];
              };
            };
          };

          # eth3 — TCP probe: connect to 10.0.3.1:80 (same nginx on router2, VLAN 3).
          eth3 = {
            weight = 1;
            health = {
              unhealthyThreshold = 2;
              healthyThreshold = 2;
              interval = "1s";
              timeout = "800ms";
              probe = {
                type = "tcp";
                host = "10.0.3.1";
                port = 80;
              };
            };
          };

          # eth4 — DNS probe: query test.local from dnsmasq on router3.
          eth4 = {
            weight = 1;
            health = {
              unhealthyThreshold = 2;
              healthyThreshold = 2;
              interval = "1s";
              timeout = "800ms";
              probe = {
                type = "dns";
                resolver = "10.0.4.1:53";
                query = "test.local";
              };
            };
          };

          # eth5 — exec probe: check for a flag file; no peer VM needed.
          eth5 = {
            weight = 1;
            health = {
              unhealthyThreshold = 2;
              healthyThreshold = 2;
              interval = "1s";
              timeout = "800ms";
              probe = {
                type = "exec";
                command = [ "/bin/sh" "-c" "test -f /run/health-exec-flag" ];
              };
            };
          };
        };
      };

      # Create the exec probe flag file at boot so eth5 starts healthy.
      systemd.tmpfiles.rules = [ "f /run/health-exec-flag 0644 root root - -" ];
    };

    # ── router1: ICMP probe target.
    # The default NixOS firewall (enabled) brings in the iptables binary and
    # allows ping.  The test inserts/deletes a DROP rule to simulate failure.
    router1 = { lib, ... }: {
      virtualisation.vlans = [ 1 ];
      networking = {
        useDHCP = lib.mkForce false;
        interfaces.eth1.ipv4.addresses = [{ address = "10.0.1.1"; prefixLength = 24; }];
        # Keep the default firewall enabled — it provides the iptables binary
        # and allows ICMP by default (allowPing = true).
        firewall.allowPing = true;
      };
    };

    # ── router2: HTTP and TCP probe target — runs nginx on both VLAN 2 and VLAN 3.
    # eth1 = 10.0.2.1 (VLAN 2, HTTP probe), eth2 = 10.0.3.1 (VLAN 3, TCP probe).
    router2 = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 2 3 ];
      networking = {
        useDHCP = lib.mkForce false;
        interfaces = {
          eth1.ipv4.addresses = [{ address = "10.0.2.1"; prefixLength = 24; }];
          eth2.ipv4.addresses = [{ address = "10.0.3.1"; prefixLength = 24; }];
        };
        firewall.allowedTCPPorts = [ 80 ];
      };
      services.nginx = {
        enable = true;
        virtualHosts."probe-target" = {
          default = true;
          locations."/" = {
            extraConfig = ''
              return 200 'ok';
              add_header Content-Type text/plain;
            '';
          };
        };
      };
    };

    # ── router3: DNS probe target — runs dnsmasq serving test.local.
    router3 = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 4 ];
      networking = {
        useDHCP = lib.mkForce false;
        interfaces.eth1.ipv4.addresses = [{ address = "10.0.4.1"; prefixLength = 24; }];
        firewall.allowedTCPPorts = [ 53 ];
        firewall.allowedUDPPorts = [ 53 ];
      };
      services.dnsmasq = {
        enable = true;
        # Prevent dnsmasq from becoming the system resolver for router3 itself.
        resolveLocalQueries = false;
        settings = {
          # Listen only on the VLAN 4 interface so it doesn't interfere with
          # other networking on the VM.
          bind-interfaces = true;
          listen-address = "10.0.4.1";
          # Serve a static record so the probe always gets a NOERROR response
          # even without an upstream DNS server in the test environment.
          address = "/test.local/10.0.4.1";
          # Don't forward to /etc/resolv.conf — no real upstream in VM.
          no-resolv = true;
        };
      };
    };
  };

  # ── Test script ────────────────────────────────────────────────────────────
  testScript = ''
    start_all()

    # Wait for all nodes to finish booting.
    machine.wait_for_unit("network.target")
    machine.wait_for_unit("route-balancer.service")
    router1.wait_for_unit("network.target")
    router2.wait_for_unit("nginx.service")
    router3.wait_for_unit("dnsmasq.service")

    # Add one default route per gateway so route-balancer seeds them into its
    # gateway map and starts a health monitor for each.  Gateways start
    # optimistic (HEALTHY) so they enter ECMP immediately without waiting for
    # the first probe cycle.
    machine.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")
    machine.succeed("ip route add default via 10.0.2.1 dev eth2 metric 600")
    machine.succeed("ip route add default via 10.0.3.1 dev eth3 metric 700")
    machine.succeed("ip route add default via 10.0.4.1 dev eth4 metric 800")
    machine.succeed("ip route add default via 10.0.5.1 dev eth5 metric 900")

    # Helper: assert that a gateway IP appears (or not) as a nexthop in the
    # ECMP route installed by route-balancer (proto 111, metric 0).
    def in_ecmp(gw_ip):
        return f"ip route show default metric 0 proto 111 | grep -q '{gw_ip}'"

    def not_in_ecmp(gw_ip):
        return f"ip route show default metric 0 proto 111 | grep -qv '{gw_ip}'"

    # ── 0. Initial state: all five gateways healthy and in ECMP ──────────────
    with subtest("initial: all gateways start healthy and appear in ECMP"):
        for gw in ["10.0.1.1", "10.0.2.1", "10.0.3.1", "10.0.4.1", "10.0.5.1"]:
            machine.wait_until_succeeds(in_ecmp(gw), timeout=15)

    # ── 1. ICMP probe ────────────────────────────────────────────────────────
    # Simulate link failure by dropping ICMP echo requests on router1.
    # With 2-of-2 threshold at 1 s interval, the gateway is removed in ≤ 3 s.

    with subtest("icmp probe: gateway removed from ECMP when ICMP is blocked"):
        router1.succeed(
            "iptables -I INPUT -p icmp --icmp-type echo-request -j DROP"
        )
        machine.wait_until_fails(in_ecmp("10.0.1.1"), timeout=15)

    with subtest("icmp probe: gateway re-added to ECMP when ICMP is unblocked"):
        router1.succeed(
            "iptables -D INPUT -p icmp --icmp-type echo-request -j DROP"
        )
        machine.wait_until_succeeds(in_ecmp("10.0.1.1"), timeout=15)

    # ── 2. HTTP probe ────────────────────────────────────────────────────────
    # Simulate server failure by stopping nginx on router2.
    # eth2 (10.0.2.1) uses an HTTP probe; eth3 (10.0.3.1) a TCP probe — both
    # share the same nginx.  Test HTTP failure/recovery first, then TCP below.

    with subtest("http probe: gateway removed from ECMP when HTTP server is down"):
        router2.succeed("systemctl stop nginx")
        machine.wait_until_fails(in_ecmp("10.0.2.1"), timeout=15)

    with subtest("http probe: gateway re-added to ECMP when HTTP server recovers"):
        router2.succeed("systemctl start nginx")
        machine.wait_until_succeeds(in_ecmp("10.0.2.1"), timeout=15)

    # ── 3. TCP probe ─────────────────────────────────────────────────────────
    # Re-use router2's nginx — TCP connect to port 80 via the VLAN 3 interface.

    with subtest("tcp probe: gateway removed from ECMP when TCP port is closed"):
        router2.succeed("systemctl stop nginx")
        machine.wait_until_fails(in_ecmp("10.0.3.1"), timeout=15)

    with subtest("tcp probe: gateway re-added to ECMP when TCP port reopens"):
        router2.succeed("systemctl start nginx")
        machine.wait_until_succeeds(in_ecmp("10.0.3.1"), timeout=15)

    # ── 4. DNS probe ─────────────────────────────────────────────────────────
    # Stop dnsmasq on router3 to simulate DNS resolver failure.

    with subtest("dns probe: gateway removed from ECMP when DNS server is down"):
        router3.succeed("systemctl stop dnsmasq")
        machine.wait_until_fails(in_ecmp("10.0.4.1"), timeout=15)

    with subtest("dns probe: gateway re-added to ECMP when DNS server recovers"):
        router3.succeed("systemctl start dnsmasq")
        machine.wait_until_succeeds(in_ecmp("10.0.4.1"), timeout=15)

    # ── 5. Exec probe ────────────────────────────────────────────────────────
    # The exec probe runs `test -f /run/health-exec-flag` on the machine
    # itself; no peer VM is needed.  Removing the flag file causes probe
    # failure; recreating it triggers recovery.

    with subtest("exec probe: gateway removed from ECMP when command fails"):
        machine.succeed("rm /run/health-exec-flag")
        machine.wait_until_fails(in_ecmp("10.0.5.1"), timeout=15)

    with subtest("exec probe: gateway re-added to ECMP when command succeeds"):
        machine.succeed("touch /run/health-exec-flag")
        machine.wait_until_succeeds(in_ecmp("10.0.5.1"), timeout=15)

    # ── 6. All-gateways-unhealthy: retain last ECMP route ────────────────────
    # When every probe fails simultaneously, route-balancer must NOT delete the
    # ECMP route (it logs a warning and keeps the last known-good state).

    with subtest("all gateways unhealthy: ECMP route is retained"):
        router1.succeed(
            "iptables -I INPUT -p icmp --icmp-type echo-request -j DROP"
        )
        router2.succeed("systemctl stop nginx")
        router3.succeed("systemctl stop dnsmasq")
        machine.succeed("rm -f /run/health-exec-flag")
        # Give health monitors time to detect all failures.
        machine.sleep(5)
        # The route must still exist (daemon retains it when all gateways fail).
        machine.succeed("ip route show default metric 0 proto 111")
        # Restore everything.
        router1.succeed(
            "iptables -D INPUT -p icmp --icmp-type echo-request -j DROP"
        )
        router2.succeed("systemctl start nginx")
        router3.succeed("systemctl start dnsmasq")
        machine.succeed("touch /run/health-exec-flag")
        for gw in ["10.0.1.1", "10.0.2.1", "10.0.3.1", "10.0.4.1", "10.0.5.1"]:
            machine.wait_until_succeeds(in_ecmp(gw), timeout=30)
  '';
}
