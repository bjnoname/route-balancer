{ pkgs }:

pkgs.testers.nixosTest {
  name = "route-balancer-health-probes";

  nodes = {
    machine = { lib, pkgs, ... }: {
      imports = [ (import ../module/route-balancer.nix) ];

      virtualisation.vlans = [ 1 2 3 4 5 ];

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
          eth3 = {
            ipv4.addresses = [{ address = "10.0.3.2"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "2001:db8:a3::2"; prefixLength = 64; }];
          };
          eth4 = {
            ipv4.addresses = [{ address = "10.0.4.2"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "2001:db8:a4::2"; prefixLength = 64; }];
          };
          eth5 = {
            ipv4.addresses = [{ address = "10.0.5.2"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "2001:db8:a5::2"; prefixLength = 64; }];
          };
        };
      };

      services.route-balancer = {
        enable = true;
        package = pkgs.callPackage ../pkgs/route-balancer.nix { };

        ipv6Ecmp = true;
        ipv6Metric = 5;

        gateways = {
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
              probe6 = {
                type = "http";
                url = "http://[2001:db8:a2::1]/";
                expectedStatus = [ 200 ];
              };
            };
          };

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
              probe6 = {
                type = "tcp";
                host = "2001:db8:a3::1";
                port = 80;
              };
            };
          };

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
              probe6 = {
                type = "dns";
                resolver = "2001:db8:a4::1";
                query = "test.local";
              };
            };
          };

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

      systemd.tmpfiles.rules = [ "f /run/health-exec-flag 0644 root root - -" ];
    };

    router1 = { lib, ... }: {
      virtualisation.vlans = [ 1 ];
      networking = {
        useDHCP = lib.mkForce false;
        interfaces.eth1 = {
          ipv4.addresses = [{ address = "10.0.1.1"; prefixLength = 24; }];
          ipv6.addresses = [{ address = "2001:db8:a1::1"; prefixLength = 64; }];
        };
        firewall.allowPing = true;
      };
    };

    router2 = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 2 3 ];
      networking = {
        useDHCP = lib.mkForce false;
        interfaces = {
          eth1 = {
            ipv4.addresses = [{ address = "10.0.2.1"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "2001:db8:a2::1"; prefixLength = 64; }];
          };
          eth2 = {
            ipv4.addresses = [{ address = "10.0.3.1"; prefixLength = 24; }];
            ipv6.addresses = [{ address = "2001:db8:a3::1"; prefixLength = 64; }];
          };
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

    router3 = { lib, pkgs, ... }: {
      virtualisation.vlans = [ 4 ];
      networking = {
        useDHCP = lib.mkForce false;
        interfaces.eth1 = {
          ipv4.addresses = [{ address = "10.0.4.1"; prefixLength = 24; }];
          ipv6.addresses = [{ address = "2001:db8:a4::1"; prefixLength = 64; }];
        };
        firewall.allowedTCPPorts = [ 53 ];
        firewall.allowedUDPPorts = [ 53 ];
      };
      services.dnsmasq = {
        enable = true;
        resolveLocalQueries = false;
        settings = {
          bind-interfaces = true;
          listen-address = "10.0.4.1,2001:db8:a4::1";
          address = "/test.local/10.0.4.1";
          no-resolv = true;
        };
      };
    };
  };

  testScript = ''
    start_all()

    # Wait for all nodes to finish booting.
    machine.wait_for_unit("network.target")
    machine.wait_for_unit("route-balancer.service")
    router1.wait_for_unit("network.target")
    router2.wait_for_unit("nginx.service")
    router3.wait_for_unit("dnsmasq.service")

    # One default route per gateway, so each is seeded and gets a monitor.
    machine.succeed("ip route add default via 10.0.1.1 dev eth1 metric 500")
    machine.succeed("ip route add default via 10.0.2.1 dev eth2 metric 600")
    machine.succeed("ip route add default via 10.0.3.1 dev eth3 metric 700")
    machine.succeed("ip route add default via 10.0.4.1 dev eth4 metric 800")
    machine.succeed("ip route add default via 10.0.5.1 dev eth5 metric 900")

    # And one per link in IPv6, so each link's second monitor has a nexthop.
    V6_NEXTHOP = {
        "eth1": "2001:db8:a1::1",
        "eth2": "2001:db8:a2::1",
        "eth3": "2001:db8:a3::1",
        "eth4": "2001:db8:a4::1",
        "eth5": "2001:db8:a5::1",
    }
    for i, (dev, gw) in enumerate(V6_NEXTHOP.items()):
        # One metric each: IPv6 refuses a second route at the same metric.
        machine.succeed(f"ip -6 route add default via {gw} dev {dev} metric {1024 + i}")

    # Assert whether a gateway IP is a nexthop in the managed ECMP route.
    def in_ecmp(gw_ip):
        return f"ip route show default metric 0 proto 111 | grep -q '{gw_ip}'"

    def in_ecmp6(dev):
        return f"ip -6 route show default metric 5 proto 111 | grep -q '{V6_NEXTHOP[dev]}'"

    # ── 0. Initial state: all five gateways healthy and in both routes ───────
    with subtest("initial: all gateways start healthy and appear in ECMP"):
        for gw in ["10.0.1.1", "10.0.2.1", "10.0.3.1", "10.0.4.1", "10.0.5.1"]:
            machine.wait_until_succeeds(in_ecmp(gw), timeout=15)
        for dev in V6_NEXTHOP:
            machine.wait_until_succeeds(in_ecmp6(dev), timeout=15)

    # ── 1. ICMP probe ────────────────────────────────────────────────────────
    # Drop ICMP echo requests on router1 to simulate link failure.

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

    # The same probe over ICMPv6, blocking only the v6 echo.
    with subtest("icmp probe: only the IPv6 route moves when only ICMPv6 is blocked"):
        router1.succeed(
            "ip6tables -I INPUT -p icmpv6 --icmpv6-type echo-request -j DROP"
        )
        machine.wait_until_fails(in_ecmp6("eth1"), timeout=15)
        machine.succeed(in_ecmp("10.0.1.1"))

        router1.succeed(
            "ip6tables -D INPUT -p icmpv6 --icmpv6-type echo-request -j DROP"
        )
        machine.wait_until_succeeds(in_ecmp6("eth1"), timeout=15)

    # ── 2. HTTP probe ────────────────────────────────────────────────────────
    # Stop nginx on router2, which serves both the http and the tcp probe.

    with subtest("http probe: gateway removed from ECMP when HTTP server is down"):
        router2.succeed("systemctl stop nginx")
        machine.wait_until_fails(in_ecmp("10.0.2.1"), timeout=15)

    with subtest("http probe: gateway re-added to ECMP when HTTP server recovers"):
        router2.succeed("systemctl start nginx")
        machine.wait_until_succeeds(in_ecmp("10.0.2.1"), timeout=15)

    # probe6 is the same nginx behind a bracketed IPv6 URL.
    with subtest("http probe: an IPv6-only outage moves the IPv6 route alone"):
        router2.succeed("ip6tables -I INPUT -i eth1 -p tcp --dport 80 -j DROP")
        machine.wait_until_fails(in_ecmp6("eth2"), timeout=15)
        machine.succeed(in_ecmp("10.0.2.1"))

        router2.succeed("ip6tables -D INPUT -i eth1 -p tcp --dport 80 -j DROP")
        machine.wait_until_succeeds(in_ecmp6("eth2"), timeout=15)

    # ── 3. TCP probe ─────────────────────────────────────────────────────────
    # Re-use router2's nginx — TCP connect to port 80 via the VLAN 3 interface.

    with subtest("tcp probe: gateway removed from ECMP when TCP port is closed"):
        router2.succeed("systemctl stop nginx")
        machine.wait_until_fails(in_ecmp("10.0.3.1"), timeout=15)

    with subtest("tcp probe: gateway re-added to ECMP when TCP port reopens"):
        router2.succeed("systemctl start nginx")
        machine.wait_until_succeeds(in_ecmp("10.0.3.1"), timeout=15)

    # The v6 leg's host is a bare IPv6 literal, joined with net.JoinHostPort.
    with subtest("tcp probe: an IPv6 host:port is spelled correctly and dialled"):
        router2.succeed("ip6tables -I INPUT -i eth2 -p tcp --dport 80 -j DROP")
        machine.wait_until_fails(in_ecmp6("eth3"), timeout=15)
        machine.succeed(in_ecmp("10.0.3.1"))

        router2.succeed("ip6tables -D INPUT -i eth2 -p tcp --dport 80 -j DROP")
        machine.wait_until_succeeds(in_ecmp6("eth3"), timeout=15)

    # ── 4. DNS probe ─────────────────────────────────────────────────────────
    # Stop dnsmasq on router3 to simulate DNS resolver failure.

    with subtest("dns probe: gateway removed from ECMP when DNS server is down"):
        router3.succeed("systemctl stop dnsmasq")
        machine.wait_until_fails(in_ecmp("10.0.4.1"), timeout=15)

    with subtest("dns probe: gateway re-added to ECMP when DNS server recovers"):
        router3.succeed("systemctl start dnsmasq")
        machine.wait_until_succeeds(in_ecmp("10.0.4.1"), timeout=15)

    # The v6 resolver is configured with no port, the "host, or host:port" form.
    with subtest("dns probe: a portless IPv6 resolver gets the default port"):
        router3.succeed("ip6tables -I INPUT -p udp --dport 53 -j DROP")
        machine.wait_until_fails(in_ecmp6("eth4"), timeout=15)
        machine.succeed(in_ecmp("10.0.4.1"))

        router3.succeed("ip6tables -D INPUT -p udp --dport 53 -j DROP")
        machine.wait_until_succeeds(in_ecmp6("eth4"), timeout=15)

    # ── 5. Exec probe ────────────────────────────────────────────────────────
    # The exec probe tests for a flag file; removing it fails both families.

    with subtest("exec probe: gateway removed from both routes when command fails"):
        machine.succeed("rm /run/health-exec-flag")
        machine.wait_until_fails(in_ecmp("10.0.5.1"), timeout=15)
        machine.wait_until_fails(in_ecmp6("eth5"), timeout=15)

    with subtest("exec probe: gateway re-added to both routes when command succeeds"):
        machine.succeed("touch /run/health-exec-flag")
        machine.wait_until_succeeds(in_ecmp("10.0.5.1"), timeout=15)
        machine.wait_until_succeeds(in_ecmp6("eth5"), timeout=15)

    # ── 6. All-gateways-unhealthy: retain last ECMP route ────────────────────
    # Every probe failing at once must keep the last known-good route.

    with subtest("all gateways unhealthy: ECMP route is retained"):
        router1.succeed(
            "iptables -I INPUT -p icmp --icmp-type echo-request -j DROP"
        )
        # Both families of eth1, so IPv6 is genuinely all-unhealthy too.
        router1.succeed(
            "ip6tables -I INPUT -p icmpv6 --icmpv6-type echo-request -j DROP"
        )
        router2.succeed("systemctl stop nginx")
        router3.succeed("systemctl stop dnsmasq")
        machine.succeed("rm -f /run/health-exec-flag")
        # Give health monitors time to detect all failures.
        machine.sleep(5)
        # The routes must still exist. Retention is per family.
        machine.succeed("ip route show default metric 0 proto 111 | grep -q .")
        machine.succeed("ip -6 route show default metric 5 proto 111 | grep -q .")
        # Restore everything.
        router1.succeed(
            "iptables -D INPUT -p icmp --icmp-type echo-request -j DROP"
        )
        router1.succeed(
            "ip6tables -D INPUT -p icmpv6 --icmpv6-type echo-request -j DROP"
        )
        router2.succeed("systemctl start nginx")
        router3.succeed("systemctl start dnsmasq")
        machine.succeed("touch /run/health-exec-flag")
        for gw in ["10.0.1.1", "10.0.2.1", "10.0.3.1", "10.0.4.1", "10.0.5.1"]:
            machine.wait_until_succeeds(in_ecmp(gw), timeout=30)
        for dev in V6_NEXTHOP:
            machine.wait_until_succeeds(in_ecmp6(dev), timeout=30)
  '';
}
