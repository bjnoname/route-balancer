# The NixOS module's options, rendered from the module itself.
#
# Two outputs from one evaluation, so the book and the check that guards the
# README cannot disagree about what the options are:
#
#   page         the book's Module options page — the head written by hand in
#                nix/docs/, then the generated body
#   optionsJSON  the same options as data, for nix/checks/module-options-agree.sh
{ lib
, pkgs
, nixosOptionsDoc
, runCommand
}:

let
  eval = lib.evalModules {
    modules = [
      ../module/route-balancer.nix

      # The module also defines systemd, networking and boot options, which
      # only a full NixOS evaluation declares. Nothing here evaluates its
      # config — only the option tree it declares — so those definitions have
      # nothing to be checked against.
      { _module.check = false; }
    ];
    specialArgs = { inherit pkgs; };
  };

  doc = nixosOptionsDoc {
    options = eval.options.services;

    # The declaration site is a /nix/store path here: true, and no use to a
    # reader of the book.
    transformOptions = o: o // { declarations = [ ]; };
  };
in

{
  page = runCommand "route-balancer-module-options.md" { } ''
    cat ${../docs/module-options.head.md} ${doc.optionsCommonMark} > $out
  '';

  inherit (doc) optionsJSON;
}
