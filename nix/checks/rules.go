package main

import (
	"fmt"
	"regexp"
	"sort"
)

type check struct {
	why string
	run func(conf) []string
	ok  string
}

var checks = map[string]check{
	"import-direction": {
		why: "a loop is a leaf: it is wired by the composition root and named by nothing else.\n" +
			"a package that can name a loop can reach its queue and its State, and\n" +
			"\"one reader, many producers\" stops being a compile error.",
		run: oneCompositionRoot,
		ok:  "the loops are imported by the composition root and by nothing else",
	},
	"seam-in-the-runtime": {
		why: "an Actor holds the seam and drives a behaviour; a behaviour plans and is\n" +
			"driven. a second holder would be a loop that runs its own actions, or one\n" +
			"that reads mid-decision instead of declaring what it needs.",
		run: seamInTheRuntime,
		ok:  "only the runtime names a Runner or an Asker",
	},
	"no-spawning-behaviours": {
		why: "concurrency belongs to the runtime, to a producer package, or to the\n" +
			"composition root. a behaviour that starts its own goroutine or holds its\n" +
			"own clock is a second scheduler nobody joins on the way out.",
		run: noSpawningBehaviours,
		ok:  "no behaviour starts a goroutine or holds a clock",
	},
	"no-undeclared-io": {
		why: "a read is a command.Query and a mutation is a command.Action. both carry a\n" +
			"closure for I/O that is not a subprocess, so an in-process read is declared,\n" +
			"counted, deduplicated and stubbable like every other one.",
		run: noUndeclaredIO,
		ok:  "only the I/O implementations touch the world directly",
	},
	"no-package-state": {
		why: "a loop's whole working set lives on one value, so a second loop is a second\n" +
			"value and two tests cannot leak into each other.",
		run: noPackageState,
		ok:  "the runtime and the loops declare no package-level variables",
	},
}

func sortedNames() []string {
	out := make([]string, 0, len(checks))
	for k := range checks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func oneCompositionRoot(c conf) []string {
	var bad []string

	for _, p := range c.pkgs {
		from := c.short(p.ImportPath)
		for _, imp := range p.Imports {
			to := c.short(imp)
			if !c.is(to, c.loops) || from == to {
				continue
			}
			switch {
			case from == c.root:
			case c.is(from, c.loops):
				bad = append(bad, fmt.Sprintf("%s imports the sibling loop %s", from, to))
			default:
				bad = append(bad, fmt.Sprintf("%s imports the loop %s; only %s may", from, to, c.root))
			}
		}
	}
	return bad
}

func seamInTheRuntime(c conf) []string {
	re := regexp.MustCompile(`command\.(Runner|Asker|CountingAsker)\b`)

	var bad []string
	for _, p := range c.pkgs {
		short := c.short(p.ImportPath)
		if short == "internal/command" || short == c.runtime {
			continue
		}
		bad = append(bad, grep(sources(p), re)...)
	}
	return bad
}

func noSpawningBehaviours(c conf) []string {
	re := regexp.MustCompile(`^\s*go\s+\w|time\.NewTicker|time\.AfterFunc|signal\.Notify`)

	var bad []string
	for _, short := range c.loops {
		p, ok := c.byID[short]
		if !ok {
			continue
		}
		bad = append(bad, grep(sources(p), re)...)
	}
	return bad
}

func noUndeclaredIO(c conf) []string {
	banned := map[string]bool{"os": true, "os/exec": true, "net/http": true, "os/signal": true}

	rootMay := map[string]bool{"os": true, "os/signal": true}

	calls := regexp.MustCompile(`\bsyscall\.[A-Z]\w*\(|\bnet\.(Dial|Listen|Interface|Lookup|Resolve)\w*\(`)

	var bad []string
	for _, p := range c.pkgs {
		short := c.short(p.ImportPath)
		if c.is(short, c.ioAllow) {
			continue
		}
		for _, imp := range p.Imports {
			if !banned[imp] {
				continue
			}
			if short == c.root && rootMay[imp] {
				continue
			}
			bad = append(bad, fmt.Sprintf("%s imports %q", short, imp))
		}
		bad = append(bad, grep(sources(p), calls)...)
	}
	return bad
}

func noPackageState(c conf) []string {
	re := regexp.MustCompile(`^var[\s(]`)

	var bad []string
	for _, short := range append([]string{c.runtime}, c.loops...) {
		p, ok := c.byID[short]
		if !ok {
			continue
		}
		bad = append(bad, grep(sources(p), re)...)
	}
	return bad
}
