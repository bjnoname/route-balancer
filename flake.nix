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

      flake = {
        nixosModules.route-balancer = import ./nix/module/route-balancer.nix;

        nixosModules.default = self.nixosModules.route-balancer;
      };

      perSystem = { pkgs, lib, system, ... }:
        let
          claude = inputs.claude-code.packages.${system}.default;
          route-balancer = pkgs.callPackage ./nix/pkgs/route-balancer.nix { };
          moduleOptions = pkgs.callPackage ./nix/pkgs/options-doc.nix { inherit pkgs; };
          docs = pkgs.callPackage ./nix/pkgs/docs.nix {
            moduleOptionsPage = moduleOptions.page;
          };
          dev-vm = pkgs.callPackage ./nix/tests/dev-vm.nix { inherit nixpkgs route-balancer; };

          # The invariants. Each is a property of the package graph or of what
          # a package is allowed to name, so each is checked against the graph
          # or the source — never against the formatting of one line in one
          # file. nix/checks/rules.go holds the rules and the reasons.
          invariant = name: pkgs.runCommand "route-balancer-${name}"
            {
              nativeBuildInputs = [ pkgs.go ];
              src = ./.;
            } ''
            set -euo pipefail
            cp -r $src src && chmod -R u+w src && cd src
            export HOME=$TMPDIR GOCACHE=$TMPDIR/go-cache GOPROXY=off

            go run ./nix/checks -check=${name} \
              -root=. \
              -runtime=internal/actor \
              -loops=internal/reconcile,internal/claim \
              -io-allow=internal/command,internal/netlink,internal/sysctl,internal/probe,internal/config,internal/prefix \
              -skip=nix/checks

            touch $out
          '';

          docs-serve = pkgs.writeShellApplication {
            name = "docs-serve";
            runtimeInputs = [ pkgs.mdbook pkgs.mdbook-mermaid ];
            text = ''
              if [ ! -f book.toml ]; then
                echo "run this from the repository root (no book.toml here)" >&2
                exit 1
              fi
              ${docs.mermaidAssets}
              ${docs.generatedPages}
              exec mdbook serve --hostname 127.0.0.1 --port "''${PORT:-3000}" "$@"
            '';
          };
        in
        {
          treefmt = {
            projectRootFile = "flake.nix";
            programs.nixpkgs-fmt.enable = true;
            programs.gofmt.enable = true;
          };

          packages = {
            inherit route-balancer docs;
            default = route-balancer;
          } // lib.optionalAttrs pkgs.stdenv.isLinux {
            inherit dev-vm;
          };

          apps = {
            default = {
              type = "app";
              program = "${route-balancer}/bin/route-balancer";
            };

            docs = {
              type = "app";
              program = "${docs-serve}/bin/docs-serve";
            };
          } // lib.optionalAttrs pkgs.stdenv.isLinux {
            dev-vm = {
              type = "app";
              program = "${dev-vm}/bin/run-nixos-vm";
            };
          };

          devShells.default = pkgs.mkShell {
            name = "route-balancer-dev";

            packages = with pkgs; [
              go
              gopls
              gotools
              golangci-lint

              iproute2
              nftables
              tcpdump
              conntrack-tools

              mdbook
              mdbook-mermaid

              jq
              ripgrep
              curl

              claude
            ] ++ [
              docs-serve
            ];

            shellHook = ''
              echo ""
              echo "┌─────────────────────────────────────────────┐"
              echo "│          route-balancer dev shell           │"
              echo "├─────────────────────────────────────────────┤"
              echo "│  go build ./...        build the daemon     │"
              echo "│  go test ./...         run tests            │"
              echo "│  golangci-lint run     lint                 │"
              echo "│  nix fmt               format all files     │"
              echo "│  docs-serve            serve docs/ on :3000 │"
              echo "│  claude                start Claude Code    │"
              echo "└─────────────────────────────────────────────┘"
              echo ""
            '';
          };

          checks = {
            inherit docs;

            # The README's option tables and the module say the same thing.
            # The book's page is generated from the module; the README's is
            # not, and this is what keeps the two from drifting apart.
            module-options-agree = pkgs.runCommand "route-balancer-module-options-agree"
              {
                nativeBuildInputs = [ pkgs.jq pkgs.gawk ];
              } ''
              set -euo pipefail

              bash ${./nix/checks/module-options-agree.sh} \
                ${moduleOptions.optionsJSON}/share/doc/nixos/options.json \
                ${./README.md}

              touch $out
            '';
          } // lib.optionalAttrs pkgs.stdenv.isLinux {
            build = route-balancer;

            # A loop is a leaf: the composition root wires it, nothing names it.
            import-direction = invariant "import-direction";

            # Only the runtime holds the seam an Actor drives a behaviour with.
            seam-in-the-runtime = invariant "seam-in-the-runtime";

            # Concurrency belongs to the runtime, a producer, or the root.
            no-spawning-behaviours = invariant "no-spawning-behaviours";

            # Everything else declares its reads and is handed the answers.
            no-undeclared-io = invariant "no-undeclared-io";

            # A loop's working set lives on one value, never on the package.
            no-package-state = invariant "no-package-state";

            go-tests = pkgs.runCommand "route-balancer-go-tests"
              {
                nativeBuildInputs = [ pkgs.go pkgs.gcc ];
                src = ./.;
              } ''
              set -euo pipefail
              cp -r $src src && chmod -R u+w src && cd src
              export HOME=$TMPDIR GOCACHE=$TMPDIR/go-cache GOPROXY=off

              go vet ./...
              go test -race ./...

              touch $out
            '';

            route-balancer-vm = import ./nix/tests/route-balancer.nix { inherit pkgs; };

            health-probes-vm = import ./nix/tests/health-probes.nix { inherit pkgs; };

            point-to-point-vm = import ./nix/tests/point-to-point.nix { inherit pkgs; };

            ipv6-ecmp-vm = import ./nix/tests/ipv6-ecmp.nix { inherit pkgs; };

            ipv4-off-vm = import ./nix/tests/ipv4-off.nix { inherit pkgs; };

            dual-stack-probes-vm = import ./nix/tests/dual-stack-probes.nix { inherit pkgs; };

            nptv6-vm = import ./nix/tests/nptv6.nix { inherit pkgs; };

            nptv6-forwarding-vm = import ./nix/tests/nptv6-forwarding.nix { inherit pkgs; };

            nptv6-port-rules-vm = import ./nix/tests/nptv6-port-rules.nix { inherit pkgs; };

            nptv6-dhcpv6-pd-vm = import ./nix/tests/nptv6-dhcpv6-pd.nix { inherit pkgs; };

            nptv6-ra-vm = import ./nix/tests/nptv6-ra.nix { inherit pkgs; };

            nptv6-ra-userspace-vm = import ./nix/tests/nptv6-ra-userspace.nix { inherit pkgs; };

            nptv6-ra-networkd-vm = import ./nix/tests/nptv6-ra-networkd.nix { inherit pkgs; };
          };
        };
    };
}
