# route-balancer

A reactive ECMP load-balanced default gateway daemon for Linux and NixOS. It
watches the kernel for default routes appearing and disappearing, probes each
uplink's health per address family, and keeps one multipath default route, one
set of policy-routing rules and one firewall ruleset in agreement with what it
has observed. Optionally it also translates an IPv6 address plan onto whatever
prefix each uplink currently holds (NPTv6, RFC 6296) and answers neighbour
solicitations for the translated addresses.

It has no external Go dependencies: the standard library is the whole of it.

## Where to start

- **[Architecture](./architecture.md)** — start here. What route-balancer
  maintains, what the daemon does on each pass, the three loops it is made of
  and what they say to each other, and why every read and every change is
  described before anything runs.
- **[Module options](./module-options.md)** — every NixOS module option in
  full, generated from the module itself.

The `README.md` in the repository root is the shorter way in: the configuration
reference, the module options as three tables, and the operational notes. This
book carries the design, and the long form of the options.

## Building this book

```bash
nix build .#docs     # static site under ./result
nix run  .#docs      # mdbook serve with live reload on :3000
```

Adding a page is a file under `docs/` and a line in `docs/SUMMARY.md`.
