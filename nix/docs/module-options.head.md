# Module options

Every option the NixOS module declares, with its type, its default and the long
version of what it does. This page is generated from
`nix/module/route-balancer.nix` when the book is built — nothing on it is
written by hand, so nothing on it can be out of date.

`README.md` carries the same options as three short tables, which is the version
to read first: it fits on a screen and it says what most people need. That copy
*is* written by hand, so `nix flake check` runs `module-options-agree`, which
compares the two on names and defaults. An option added, renamed, removed or
given a different default fails the build until the README says so as well.

One option has no default and must be set: `services.route-balancer.package`,
the daemon to run.

