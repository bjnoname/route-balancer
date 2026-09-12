{ lib
, buildGoModule
, makeWrapper
, iproute2
, iptables
, nftables
}:

buildGoModule {
  pname = "route-balancer";
  version = "0.1.0";

  src = ./../..;

  vendorHash = null;

  # Only the daemon: nix/checks is a build-time invariant checker, not a
  # program this package ships.
  subPackages = [ "." ];

  nativeBuildInputs = [ makeWrapper ];

  postInstall = ''
    wrapProgram $out/bin/route-balancer \
      --prefix PATH : ${lib.makeBinPath [ iproute2 iptables nftables ]}
  '';

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
