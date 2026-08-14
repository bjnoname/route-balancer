{
  description = "route-balancer — Reactive ECMP load-balanced gateway daemon for NixOS";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    flake-parts.url = "github:hercules-ci/flake-parts";
    treefmt-nix.url = "github:numtide/treefmt-nix";
    claude-code.url = "github:sadjow/claude-code-nix";
  };

  outputs = inputs @ { flake-parts, nixpkgs, self, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {

      imports = [ inputs.treefmt-nix.flakeModule ];

      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];

      # ── Architecture-independent outputs ─────────────────────────────────────
      flake = {

        nixosModules.route-balancer = import ./nix/module/route-balancer.nix;

        nixosModules.default = self.nixosModules.route-balancer;
      };

      # ── Per-system outputs ───────────────────────────────────────────────────
      perSystem = { pkgs, lib, system, ... }:
        let
          claude = inputs.claude-code.packages.${system}.default;
          route-balancer = pkgs.callPackage ./nix/pkgs/route-balancer.nix { };
          dev-vm = pkgs.callPackage ./nix/tests/dev-vm.nix { inherit nixpkgs route-balancer; };
        in
        {
          # ── Formatter (nix fmt) ───────────────────────────────────────────────
          # treefmt-nix wires up both formatters and exposes them via nix fmt.
          treefmt = {
            projectRootFile = "flake.nix";
            programs.nixpkgs-fmt.enable = true;
            programs.gofmt.enable = true;
          };

          # ── Packages ──────────────────────────────────────────────────────────
          packages = {
            inherit route-balancer;
            default = route-balancer;
          } // lib.optionalAttrs pkgs.stdenv.isLinux {
            inherit dev-vm;
          };

          # ── Runnable apps ─────────────────────────────────────────────────────
          apps = {
            default = {
              type = "app";
              program = "${route-balancer}/bin/route-balancer";
            };
          } // lib.optionalAttrs pkgs.stdenv.isLinux {
            # nix run .#dev-vm — boots the interactive three-NIC VM
            dev-vm = {
              type = "app";
              program = "${dev-vm}/bin/run-nixos-vm";
            };
          };

          # ── Dev shell ─────────────────────────────────────────────────────────
          # Enter with: nix develop
          devShells.default = pkgs.mkShell {
            name = "route-balancer-dev";

            packages = with pkgs; [
              # ── Go toolchain ─────────────────────────────────────────────────
              go
              gopls # language server
              gotools # goimports, godoc, etc.
              golangci-lint # linter suite

              # ── Network debugging (useful when working on routing code) ──────
              iproute2 # ip, ss, tc
              nftables # nft
              tcpdump
              conntrack-tools

              # ── Formatting ───────────────────────────────────────────────────
              # gofmt ships with go; nixpkgs-fmt is pulled in by treefmt.
              # Run both via: nix fmt

              # ── General dev tools ────────────────────────────────────────────
              jq
              ripgrep
              curl

              # ── Claude Code ──────────────────────────────────────────────────
              claude
            ];

            # Make npm global installs land in the project, not ~/.npm-global,
            # so the claude binary is on PATH inside the dev shell automatically
            # after running claude-install.
            shellHook = ''
              echo ""
              echo "┌─────────────────────────────────────────────┐"
              echo "│          route-balancer dev shell           │"
              echo "├─────────────────────────────────────────────┤"
              echo "│  go build ./...        build the daemon     │"
              echo "│  go test ./...         run tests            │"
              echo "│  golangci-lint run     lint                 │"
              echo "│  nix fmt               format all files     │"
              echo "│  claude                start Claude Code    │"
              echo "└─────────────────────────────────────────────┘"
              echo ""
            '';
          };

          # ── Checks (run with: nix flake check) ────────────────────────────────
          # treefmt-nix automatically adds a checks.treefmt entry that fails
          # if any file is not formatted. No manual nixpkgs-fmt check needed.
          checks = lib.optionalAttrs pkgs.stdenv.isLinux {
            # Build check — confirms the package compiles on Linux
            build = route-balancer;

            # NixOS VM integration test — reactive ECMP, seeding, cleanup
            route-balancer-vm = import ./nix/tests/route-balancer.nix { inherit pkgs; };

            # NixOS VM integration test — health probe types (ICMP/HTTP/TCP/DNS/exec)
            health-probes-vm = import ./nix/tests/health-probes.nix { inherit pkgs; };

            # NixOS VM integration test — nexthop-less (point-to-point) uplinks,
            # reconcile stability, and teardown after an address is withdrawn
            point-to-point-vm = import ./nix/tests/point-to-point.nix { inherit pkgs; };
          };
        };
    };
}
