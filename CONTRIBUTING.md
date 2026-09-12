# Contributing to route-balancer

## Project Structure

One `main.go` — the composition root — and fifteen packages under `internal/`.
What follows is the shape you need before touching anything.

```
main.go        flags, config, logger, root check — then it resolves the config
               once, creates the two mailboxes and the two loops, starts them
               and waits. It cannot read or write the machine itself
internal/
  config/      Config struct, JSON wire format, defaults, Load. Imports nothing
  command/     the I/O seam: a mutation is a command.Action, a read is a command.Query.
               Tool and Args are identity; the Write and Read closures supply the
               I/O that is not a subprocess
  settings/    the config resolved once into the options each loop runs on
  actor/       the loop runtime: Actor, Child, Mailbox, Behaviour, Observer
  sysctl/      the /proc/sys knobs, read and written in one place
  probe/       the five health checks and the SO_BINDTODEVICE dialer
  route/       Gateway, the specs, the `ip` invocations, the drift reads
  netlink/     the route socket and its Watcher, plus wire decoding: route events,
               PD discard routes, RA prefixes, addresses
  firewall/    the fwmark stamping backends, iptables and nftables
  nft/         one nftables table, managed: list it, read it, replace it
               atomically, remove it — for the three mechanisms that keep one
  nptv6/       the address plan, subnet assignment, the translation ruleset
  prefix/      where an uplink's external prefix comes from: route, RA or static
  ndp/         proxy NDP: the learning table, the entries, the conflict probe
  reconcile/   the reconciler: one queue, one state, one calculator, one differ
  claim/       the conflict prober: no state, one conversation
nix/
  checks/      the invariant checker: one program, five rules, run per check —
               and module-options-agree.sh, which holds README.md and the
               module to the same set of options
  module/      NixOS module (options, systemd unit, JSON config gen)
  pkgs/        Nix derivations: the daemon (buildGoModule), the docs (mdBook)
               and options-doc.nix, which renders the module's own options
  docs/        mermaid-init.js — the book's diagram runtime, see nix/pkgs/docs.nix
               — and module-options.head.md, the generated page's opening prose
  tests/       NixOS VM integration tests, plus the interactive dev-vm
book.toml      mdBook config; docs/ is its source, `nix run .#docs` serves it
docs/          the book: SUMMARY.md is the table of contents. module-options.md
               is generated into it at build time and is not in git
```

Four layers, each owning exactly one capability. **The root** (`main`) decides
which loops exist and how they are wired, and can do nothing else. **The runtime**
(`internal/actor`) is the only thing that runs a plan, answers a query, or starts
the goroutine a loop lives in. **The loops** (`internal/reconcile`,
`internal/claim`) decide: they plan, and they never run, read undeclared, or
spawn. **Producers and I/O** (`command`, `netlink`, `sysctl`, `probe`, `prefix`)
touch the world and report through typed callbacks.

**The one rule: nothing imports a loop except `main`.** Dependencies point one
way — a loop imports the domain packages, they import `config` and `command`, and
there is no way back. That is what makes "one reader, many producers" a compiler
error rather than a review comment: `State` and the event queue live in a package
nothing else can name, so a producer *cannot* write kernel state. `nix flake
check` fails the `import-direction` check if this stops holding, and it fails the
same way if the two loops start importing each other.

Three things follow, and all will come up the first time you add a producer, a
domain package or a loop:

- **Nothing below or beside a loop can see its event type.** A producer reports
  through a typed callback instead — `prefix.Watcher` takes `OnChange`/`OnRecheck`,
  `netlink.Watcher` takes `OnRoute`/`OnOverrun` — and the loop translates that
  into an event. Do not add a shared `event` package to get around this; the
  queue's vocabulary belongs to the loop that reads it.

  A *peer* loop is the same rule rather than an exception to it. `internal/claim`
  is one: it takes a `Report` callback, `internal/reconcile` takes an `offer`
  callback, both return a `command.Action` — a batch to probe on the way in, a
  ruling on the way out — and neither package imports the other. `main` creates
  both mailboxes first and both loops second, so every edge is a constructor
  argument and no field is assigned afterwards. A `Mailbox` has no exported reader
  and no exported writer, so the root can wire one without being able to use it.

  A loop that comes and goes is a **child** of the loop that starts it — the
  health monitors are the population this is for. Starting one is a
  `command.Action` like every other effect a loop has (`monitor start eth1/v4`),
  so it happens inside the parent's `RunAll` and nowhere else; on the way out the
  parent stops each child and joins it before its own `Wait` releases. Never add
  a bare `go` to a behaviour — `no-spawning-behaviours` fails on it — and never
  add a second `Wait` to `main`.
- **A package below the loops does not read the config.** It receives a resolved
  options struct (`route.Options`, `ndp.Options`, …) built in `settings.Resolve`
  from `*config.Config`. A domain package that needs a new setting gets a new
  field on its options struct, not an import of `config`'s accessors. Both loops
  are built from the same `settings.Resolved`, so no loop is the source of
  another loop's options.
- **A behaviour never holds the seam.** `command.Runner`, `command.Asker` and
  `command.CountingAsker` are named in `internal/command` and `internal/actor`
  and nowhere else. A behaviour implements `Step` and is handed an
  `actor.Observer` — one method, which answers a declared plan and nothing
  else, and only for the length of that step.

The five invariant checks — `import-direction`, `seam-in-the-runtime`,
`no-spawning-behaviours`, `no-undeclared-io`, `no-package-state` — share one
implementation under `nix/checks/`, with each rule stated next to the reason it
exists in `rules.go`. Run one without Nix while iterating:

```bash
go run ./nix/checks -check=no-undeclared-io -root=. -runtime=internal/actor \
  -loops=internal/reconcile,internal/claim -skip=nix/checks \
  -io-allow=internal/command,internal/netlink,internal/sysctl,internal/probe,internal/config,internal/prefix
```

## Commands

### Development

```bash
go build ./...             # build daemon binary
go test ./...              # run tests
go vet ./...               # static analysis
golangci-lint run          # full lint suite
nix fmt                    # format all files (Nix via nixpkgs-fmt, Go via gofmt)
```

### Nix

```bash
nix build                  # build the package
nix develop                # enter dev shell
nix fmt                    # format all Nix and Go files
nix flake check            # all checks (build, the five invariants, the options
                           # check, docs, VM tests, fmt)
nix run .#dev-vm           # boot interactive three-NIC dev VM (Linux only)
nix run .#docs             # serve docs/ on :3000 with live reload
nix build .#docs           # render docs/ to a static site
```

### Documentation

`docs/` is an mdBook. A new page is a file under `docs/` and a line in
`docs/SUMMARY.md`; `create-missing = false` means a link with no file behind it
fails `nix flake check` rather than producing a blank page.

Diagrams are ` ```mermaid ` fences, drawn in the browser. Both JS files
`book.toml` names are produced at build time — `mermaid.min.js` is extracted
from the `mdbook-mermaid` binary (the only offline source for it) and
`mermaid-init.js` is copied over the one that tool writes, because upstream's
copy drives theme switching off mdBook 0.5.0 element ids that 0.5.2 renamed.
Ours is `nix/docs/mermaid-init.js`; it watches the `class` attribute on `<html>`
instead and redraws in place. Both are gitignored, so plain `mdbook serve` in a
fresh checkout will fail until `docs-serve` (or `nix run .#docs`) has written
them once.

`docs/module-options.md` is generated the same way, and for the same reason a
page nobody edits cannot go stale: `nix/pkgs/options-doc.nix` evaluates
`nix/module/route-balancer.nix` and renders every option it declares. To change
what that page says, change the option's `description` in the module. Its
opening prose is `nix/docs/module-options.head.md`.

The README's terser tables are still written by hand, because a reader meeting
the project wants three tables and not fifty-three headings. The
`module-options-agree` check is what keeps that copy honest: it compares the two
on the parts that have one right answer — the set of option names, and every
default the README states as a plain literal — and fails the build when they
disagree. Prose is not compared. Run it alone with:

```bash
nix build .#checks.x86_64-linux.module-options-agree
```

### Manual routing tests (requires root)

```bash
sudo ./route-balancer --config config.json   # run daemon with config
sudo ./route-balancer                        # run without config (all defaults)
ip monitor route                             # watch route events in parallel
ip route add default via 10.0.0.1 dev eth0 metric 50  # trigger a route event
ip route show default                        # verify ECMP is applied (metric 0, proto 111)
ip route show proto 111                      # show only route-balancer managed routes
```

## Design constraints

- **No external Go dependencies** — stdlib only. This keeps the Nix derivation simple and avoids vendoring.
- **Nothing imports a loop except `main`** — see above. This is the invariant the layout exists for.
- **The `.go` files carry no comments**, deliberately, and a patch should not reintroduce
  them. A comment is the one claim about this code nothing checks: it cannot fail a build,
  a test or one of the five invariants, so it drifts silently and is then believed. The
  reasons live where something holds them to the code instead — an invariant's reason is a
  `why` string in `nix/checks/rules.go`, printed when that check fails; a design constraint
  is this file; user-visible behaviour is README.md; and a reason that belongs to one
  decision goes in the commit message that made it, which `git blame` still reaches. Where
  a comment would have named a hazard, prefer a test whose failure message says the same
  thing — `internal/reconcile` and `internal/claim` do this throughout. The convention
  covers every `.go` file in the tree, tests and `nix/checks/` included; the `.nix` and
  `.sh` files are not subject to it and do carry their reasons inline.
- Probes must bind to the gateway interface via `SO_BINDTODEVICE` so they don't route through a sibling gateway.
- The IPv4 ECMP route, when `ipv4_ecmp` leaves it managed (the default), is installed at metric 0, so every other default route on the system should use a metric > 0. IPv6 cannot do the same: the kernel rewrites a requested metric 0 on a v6 route to 1024, which is exactly where kernel-RA and DHCPv6 default routes live, so the managed v6 route uses `ipv6_metric` (default 1) instead.
- Every map walk that feeds generated text or an argument list is sorted. This is not tidiness: "a lease renewal is not a change" holds only while identical inputs produce byte-identical output, and an unsorted walk re-applies the ruleset — resetting conntrack for every established flow — on an arbitrary subset of renewals.

## Running the VM test suite

```bash
nix flake check            # builds + runs all NixOS VM tests
```

Tests live in `nix/tests/`. The integration tests spin up multi-NIC NixOS VMs using
`nixosTest` and verify reactive ECMP, health probe behaviour, seeding on startup,
clean shutdown, the IPv6 default route, and NPTv6 against real DHCPv6-PD and RA
clients. They are the acceptance criterion for anything that moves code around:
a restructure that needs a VM test edited has changed behaviour.

Two things about running it that are learned the expensive way:

- **`git add -A` before any `nix build`.** The flake builds from the git tree, so
  an untracked new `.go` file fails with `undefined: ...` rather than with
  anything pointing at the real cause.
- **`all checks passed!` at the tail is the pass condition**, not `EXIT=0` — a
  compound command's exit code is its last statement's.

**Reading the output.** `nix flake check`'s own log is build progress; the
daemon's logs are not in it. Per test:

```bash
nix log .#checks.x86_64-linux.route-balancer-vm   # or nptv6-vm, nptv6-ra-vm, …
```

Worth doing on a *green* run, which is how several real faults were found: the
daemon reaching the right state by the wrong route logs nothing a passing
assertion would notice. Two signals to compare against the previous run:

- `NPTv6 translation rules applied`, per test — `nptv6-vm` 12, `nptv6-ra-vm` 7,
  `nptv6-port-rules-vm` 4, `nptv6-forwarding-vm` 4, `nptv6-dhcpv6-pd-vm` 2,
  measured on a green run. The tests assert the *delta* across a renewal
  directly, because that is what proves a lease renewal causes no rule churn;
  these totals are the second signal, and the direction that matters is up.

  Count the daemon's own lines, not every line that contains the string:

  ```bash
  nix log .#checks.x86_64-linux.nptv6-vm |
    grep -cE 'route-balancer\[[0-9]+\]:.*NPTv6 translation rules applied'
  ```

  `nptv6-vm` greps for this message itself, and the driver echoes each command
  into the build log twice — once issuing, once finishing — so a bare
  `grep -c` over `nix log` reports 16 there against a real 12. No other test
  greps for it, which is why only that one number is inflated.

  Expect ±1 between runs on the same code. A pass is triggered per drain, so
  two prefix events arriving together are one application and arriving apart
  are two, and which happens is VM timing. `nptv6-vm` has been seen at 12 and
  13 on the very same derivation, `nptv6-forwarding-vm` at 3 and 4. A single
  count moving is not a finding; confirm it by re-running the identical
  derivation with `nix build --rebuild` before going looking for a cause.
- `msg=Installing`, which the differ logs whenever `InSync` comes back false.
  A converged daemon plans nothing, so in a quiet window this count must not
  move at all. `point-to-point-vm` and `nptv6-port-rules-vm` assert exactly that,
  the first across three windows and the second across three reconcile ticks.
  A count that climbs on an idle machine means some resource's spec and its own
  drift read disagree, so it is being re-applied every pass — which for an
  `nft -f` resource resets conntrack for every established flow. Compare it in
  the other tests too; the two named above are the ones that currently fail on it.
