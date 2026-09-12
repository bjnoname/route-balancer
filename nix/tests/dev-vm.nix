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

      virtualisation.graphics = false;

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
      networking.useNetworkd = true;

      systemd.network.networks = {
        "10-eth0" = {
          matchConfig.Name = "eth0";
          networkConfig = {
            Address = "10.0.0.2/24";
            DHCP = "no";
          };
          routes = [{ Gateway = "10.0.0.1"; Metric = 50; }];
        };
        "10-eth1" = {
          matchConfig.Name = "eth1";
          networkConfig.DHCP = "ipv4";
          dhcpV4Config.RouteMetric = 50;
        };
        "10-eth2" = {
          matchConfig.Name = "eth2";
          networkConfig = {
            Address = "10.0.2.2/24";
            DHCP = "no";
          };
        };
      };

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
