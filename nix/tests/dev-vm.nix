# nix/tests/dev-vm.nix
#
# Interactive dev VM with three NAT-connected NICs for manual route-balancer testing.
# Run with: nix run .#dev-vm
#
# Interfaces (besides lo):
#   eth0 — 10.0.0.2/24  gateway 10.0.0.1  static   (QEMU user-mode NAT → internet)  [configured, HTTP probe]
#   eth1 — DHCP from 10.0.1.0/24           dynamic  (QEMU user-mode NAT → internet)  [configured]
#   eth2 — 10.0.2.2/24  static             NOT in gateways config                    [unconfigured]
#
# eth0 and eth1 participate in ECMP load balancing.
# eth0 is health-monitored via an HTTP probe against nginx running on this VM
# (bound to 10.0.0.2).  Stop/start nginx to observe health-driven ECMP changes:
#
#   systemctl stop nginx    → eth0 probe fails → removed from ECMP after 3 consecutive failures
#   systemctl start nginx   → eth0 probe passes → re-added after 2 consecutive successes
#
# eth2 is intentionally absent from the gateways config — use it to verify that
# default routes added on eth2 are ignored by the daemon.
#
# The VM boots to an auto-logged-in root shell on the serial console.

{ pkgs, nixpkgs, route-balancer }:

let
  lib = pkgs.lib;
in
(nixpkgs.lib.nixosSystem {
  system = pkgs.stdenv.hostPlatform.system;
  modules = [
    ({ lib, modulesPath, pkgs, ... }: {
      imports = [
        "${modulesPath}/virtualisation/qemu-vm.nix"
        (import ../module/route-balancer.nix)
      ];

      # Serial console — attach directly to the terminal, no VNC/SDL window.
      virtualisation.graphics = false;

      # Three user-mode NAT NICs — override the module default so no extra
      # management interface sneaks in alongside ours.
      virtualisation.qemu.networkingOptions = lib.mkForce [
        "-device"
        "virtio-net-pci,netdev=net0"
        "-netdev"
        "user,id=net0,net=10.0.0.0/24,host=10.0.0.1"
        "-device"
        "virtio-net-pci,netdev=net1"
        "-netdev"
        "user,id=net1,net=10.0.1.0/24,host=10.0.1.1"
        "-device"
        "virtio-net-pci,netdev=net2"
        "-netdev"
        "user,id=net2,net=10.0.2.0/24,host=10.0.2.1"
      ];

      networking.usePredictableInterfaceNames = false;
      # Use networkd to verify route-balancer works correctly with it.
      # With DHCP on eth1, networkd installs a default route that route-balancer
      # needs to discover the gateway IP.  Observe ECMP includes both eth0 and eth1.
      networking.useNetworkd = true;

      systemd.network.networks = {
        "10-eth0" = {
          matchConfig.Name = "eth0";
          networkConfig = {
            Address = "10.0.0.2/24";
            DHCP = "no";
          };
          # High metric so route-balancer can install its ECMP route at metric 0.
          routes = [{ routeConfig = { Gateway = "10.0.0.1"; Metric = 50; }; }];
        };
        "10-eth1" = {
          matchConfig.Name = "eth1";
          networkConfig.DHCP = "ipv4";
          dhcpV4Config.RouteMetric = 50;
        };
        # eth2 has an address but is deliberately absent from the gateways config.
        "10-eth2" = {
          matchConfig.Name = "eth2";
          networkConfig = {
            Address = "10.0.2.2/24";
            DHCP = "no";
          };
        };
      };

      # ── Local nginx — HTTP probe target for eth0 ───────────────────────────
      # The route-balancer HTTP probe for eth0 sends GET http://10.0.0.2/ bound
      # to eth0 via SO_BINDTODEVICE.  Stopping nginx simulates a link failure.
      services.nginx = {
        enable = true;
        virtualHosts."health-probe" = {
          default = true;
          listen = [{ addr = "10.0.0.2"; port = 80; }];
          locations."/" = {
            extraConfig = ''
              return 200 'route-balancer probe ok\n';
              add_header Content-Type text/plain;
            '';
          };
        };
      };

      services.route-balancer = {
        enable = true;
        package = route-balancer;
        gateways = {
          eth0 = {
            weight = 1;
            health = {
              # Remove from ECMP after 3 consecutive failures, re-add after 2
              # consecutive successes.  Fast interval for interactive demos.
              unhealthyThreshold = 3;
              healthyThreshold = 2;
              interval = "3s";
              timeout = "2s";
              probe = {
                type = "http";
                url = "http://10.0.0.2/";
                expectedStatus = [ 200 ];
              };
            };
          };
          eth1 = { weight = 1; };
        };
      };

      # Drop straight into a root shell on boot.
      services.getty.autologinUser = lib.mkDefault "root";
      users.users.root.password = lib.mkForce "";

      environment.systemPackages = with pkgs; [
        route-balancer
        iproute2
        tcpdump
        nftables
        iptables
        conntrack-tools
        jq
      ];

      system.stateVersion = "24.11";
    })
  ];
}).config.system.build.vm
