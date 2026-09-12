package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	name := flag.String("check", "", "which invariant to check")
	dir := flag.String("dir", ".", "module root")
	root := flag.String("root", "", "import path of the composition root")
	loops := flag.String("loops", "", "comma-separated behaviour packages")
	runtimePkg := flag.String("runtime", "", "the loop runtime package")
	ioAllow := flag.String("io-allow", "", "comma-separated packages that may touch the world")
	skip := flag.String("skip", "", "comma-separated packages that are not part of the daemon")
	flag.Parse()

	c, ok := checks[*name]
	if !ok {
		fail("unknown check %q; want one of %s", *name, strings.Join(sortedNames(), ", "))
	}

	cfg, err := load(*dir, *root, split(*loops), *runtimePkg, split(*ioAllow), split(*skip))
	if err != nil {
		fail("%v", err)
	}

	if bad := c.run(cfg); len(bad) > 0 {
		sort.Strings(bad)
		fmt.Fprintf(os.Stderr, "%s:\n", *name)
		for _, b := range bad {
			fmt.Fprintf(os.Stderr, "  %s\n", b)
		}
		fmt.Fprintf(os.Stderr, "\n%s\n", c.why)
		os.Exit(1)
	}
	fmt.Println(c.ok)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
