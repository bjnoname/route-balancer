package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type pkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Imports    []string
	Deps       []string
}

type conf struct {
	module  string
	root    string
	loops   []string
	runtime string
	ioAllow []string

	pkgs []pkg
	byID map[string]pkg
}

func load(dir, root string, loops []string, runtimePkg string, ioAllow []string, skip []string) (conf, error) {
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = dir
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return conf{}, fmt.Errorf("go list: %w", err)
	}

	c := conf{root: root, loops: loops, runtime: runtimePkg, ioAllow: ioAllow, byID: map[string]pkg{}}

	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			return conf{}, fmt.Errorf("decoding go list output: %w", err)
		}
		c.pkgs = append(c.pkgs, p)
	}
	if len(c.pkgs) == 0 {
		return conf{}, fmt.Errorf("go list found no packages under %s", dir)
	}

	c.module = c.pkgs[0].ImportPath
	for _, p := range c.pkgs {
		if len(p.ImportPath) < len(c.module) {
			c.module = p.ImportPath
		}
	}
	kept := c.pkgs[:0]
	for _, p := range c.pkgs {
		if c.is(c.short(p.ImportPath), skip) {
			continue
		}
		kept = append(kept, p)
		c.byID[c.short(p.ImportPath)] = p
	}
	c.pkgs = kept

	if err := c.subjectsExist(); err != nil {
		return conf{}, err
	}
	return c, nil
}

func (c conf) subjectsExist() error {
	for _, s := range []struct {
		flag  string
		names []string
	}{
		{"-root", []string{c.root}},
		{"-runtime", []string{c.runtime}},
		{"-loops", c.loops},
		{"-io-allow", c.ioAllow},
	} {
		for _, name := range s.names {
			if name == "" {
				continue
			}
			if _, ok := c.byID[name]; !ok {
				return fmt.Errorf("%s names %q, which is not a package in this module: "+
					"a check whose subject does not exist passes without checking anything, "+
					"so rename it in flake.nix rather than leaving it dangling", s.flag, name)
			}
		}
	}
	return nil
}

func (c conf) short(path string) string {
	if path == c.module {
		return "."
	}
	return strings.TrimPrefix(path, c.module+"/")
}

func (c conf) is(short string, set []string) bool {
	for _, s := range set {
		if s == short {
			return true
		}
	}
	return false
}

func sources(p pkg) []string {
	out := make([]string, 0, len(p.GoFiles))
	for _, f := range p.GoFiles {
		out = append(out, filepath.Join(p.Dir, f))
	}
	sort.Strings(out)
	return out
}

func grep(paths []string, re *regexp.Regexp) []string {
	var hits []string
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", rel(path), i+1, strings.TrimSpace(line)))
			}
		}
	}
	return hits
}

func rel(path string) string {
	if wd, err := os.Getwd(); err == nil {
		if r, err := filepath.Rel(wd, path); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
	}
	return path
}
