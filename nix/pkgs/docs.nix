{ lib
, stdenvNoCC
, mdbook
, mdbook-mermaid
, moduleOptionsPage
}:

let
  src = lib.fileset.toSource {
    root = ./../..;
    fileset = lib.fileset.unions [
      ./../../book.toml
      ./../../docs
    ];
  };

  mermaidAssets = ''
    mdbook-mermaid install .
    cp --no-preserve=mode ${./../docs/mermaid-init.js} mermaid-init.js
  '';

  # docs/module-options.md is generated from the NixOS module rather than
  # written, so it is not in git and every way of building this book has to
  # put it there first. SUMMARY.md names it and create-missing = false, so a
  # build that skipped this fails rather than quietly dropping the page.
  generatedPages = ''
    cp --no-preserve=mode ${moduleOptionsPage} docs/module-options.md
  '';
in

stdenvNoCC.mkDerivation {
  pname = "route-balancer-docs";
  version = "0.1.0";

  inherit src;

  nativeBuildInputs = [ mdbook mdbook-mermaid ];

  dontConfigure = true;

  buildPhase = ''
    runHook preBuild
    ${mermaidAssets}
    ${generatedPages}
    mdbook build
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    mv book $out
    runHook postInstall
  '';

  passthru = { inherit mermaidAssets generatedPages; };

  meta = with lib; {
    description = "route-balancer documentation (mdBook)";
    homepage = "https://github.com/bjnoname/route-balancer";
    license = licenses.mit;
    platforms = platforms.all;
    maintainers = [ ];
  };
}
