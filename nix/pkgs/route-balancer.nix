# nix/pkgs/route-balancer.nix
#
# Builds the route-balancer binary from the Go source in the repo root.
# Called via pkgs.callPackage ./nix/pkgs/route-balancer.nix { }

{ lib
, buildGoModule
, makeWrapper
, iproute2   # runtime dep: we exec `ip route replace`
, iptables   # runtime dep: we exec `iptables` for port-based fwmark rules (default backend)
, nftables   # runtime dep: we exec `nft` for port-based fwmark rules (optional backend)
}:

buildGoModule {
  pname = "route-balancer";
  version = "0.1.0";

  # Source is the repo root (two levels up from nix/pkgs/)
  src = ./../..;

  # go.sum hash — update with:
  #   cd <repo> && go mod tidy && nix run nixpkgs#gomod2nix -- generate
  # For a module with no external deps this is the empty-modules hash:
  vendorHash = null;

  nativeBuildInputs = [ makeWrapper ];

  postInstall = ''
    # Wrap the binary so `ip` resolves to the Nix iproute2, not a
    # host-provided one. This makes the daemon work correctly even on
    # minimal NixOS configs without iproute2 in environment.systemPackages.
    wrapProgram $out/bin/route-balancer \
      --prefix PATH : ${lib.makeBinPath [ iproute2 iptables nftables ]}
  '';

  # Required capabilities (documented, not enforced at build time):
  #   CAP_NET_ADMIN  — modify routing table
  #   CAP_NET_RAW    — raw ICMP socket for health probing (future)
  #
  # These are granted via the systemd service unit, not here.

  meta = with lib; {
    description = "Reactive ECMP load-balanced default gateway daemon";
    longDescription = ''
      route-balancer listens on a Linux netlink socket for RTM_NEWROUTE and
      RTM_DELROUTE events. When a new default route (0.0.0.0/0) appears, it
      automatically configures an ECMP multipath default route across all
      known gateways with equal (or configured) weights.

      Designed for multi-WAN setups where DHCP or static routes are added
      by multiple interfaces and you want automatic load balancing without
      manual intervention.
    '';
    homepage = "https://github.com/bjnoname/route-balancer";
    license = licenses.mit;
    platforms = platforms.linux;
    maintainers = [ ];
  };
}
