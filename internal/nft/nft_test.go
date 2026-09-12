package nft

import (
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
)

func table() Table {
	return Table{
		Family:      "ip6",
		Name:        "t",
		ApplyOrder:  command.OrderNftTable,
		RemoveOrder: command.Teardown(command.OrderNftTable),
	}
}

func TestRulesetReplacesRatherThanAdds(t *testing.T) {
	t.Parallel()

	got := Ruleset(table(), "  chain c {\n    drop\n  }\n")
	want := "table ip6 t { }\n" +
		"delete table ip6 t\n" +
		"table ip6 t {\n" +
		"  chain c {\n    drop\n  }\n" +
		"}\n"

	if got != want {
		t.Errorf("Ruleset() =\n%q\nwant\n%q", got, want)
	}
}

func TestRulesetDeclaresBeforeItDeletes(t *testing.T) {
	t.Parallel()

	lines := strings.Split(Ruleset(table(), ""), "\n")
	if len(lines) < 2 || lines[0] != "table ip6 t { }" || lines[1] != "delete table ip6 t" {
		t.Errorf("first two lines are %q, want the empty declaration then the delete", lines[:min(2, len(lines))])
	}
}

func TestApplyWithNoRulesetRemovesTheTable(t *testing.T) {
	t.Parallel()

	acts := Apply(table(), "")
	if len(acts) != 1 {
		t.Fatalf("Apply(\"\") planned %d actions, want 1", len(acts))
	}
	if got := strings.Join(acts[0].Args, " "); got != "delete table ip6 t" {
		t.Errorf("Apply(\"\") = %q, want the delete", got)
	}
	if acts[0].Order != command.Teardown(command.OrderNftTable) {
		t.Errorf("a removal ran at order %d, want the table's RemoveOrder", acts[0].Order)
	}
}

func TestApplyWritesTheRulesetOnStdin(t *testing.T) {
	t.Parallel()

	acts := Apply(table(), "table ip6 t { }\n")
	if len(acts) != 1 {
		t.Fatalf("Apply planned %d actions, want 1", len(acts))
	}
	if got := strings.Join(acts[0].Args, " "); got != "-f -" {
		t.Errorf("args = %q, want %q", got, "-f -")
	}
	if acts[0].Stdin != "table ip6 t { }\n" {
		t.Errorf("stdin = %q, want the ruleset", acts[0].Stdin)
	}
	if acts[0].Order != command.OrderNftTable {
		t.Errorf("a write ran at order %d, want the table's ApplyOrder", acts[0].Order)
	}
}

func parse(out string) map[string]struct{} {
	got := map[string]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			got[line] = struct{}{}
		}
	}
	return got
}

func TestReadTellsAbsentApartFromEmpty(t *testing.T) {
	t.Parallel()

	q := Query(table(), parse).Query

	t.Run("a table that does not list is absent", func(t *testing.T) {
		t.Parallel()
		a := commandtest.New(nil).Answers(q)
		if inst := Read(table(), parse, a); inst.Present {
			t.Error("a table nft could not list read as present")
		}
	})

	t.Run("a table that lists nothing is present and empty", func(t *testing.T) {
		t.Parallel()
		a := commandtest.New(map[string]string{q.String(): ""}).Answers(q)

		inst := Read(table(), parse, a)
		if !inst.Present {
			t.Fatal("a table that listed read as absent")
		}
		if len(inst.Rules) != 0 {
			t.Errorf("rules = %v, want none", inst.Rules)
		}
	})
}

func TestSameSet(t *testing.T) {
	t.Parallel()

	set := func(keys ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, k := range keys {
			m[k] = struct{}{}
		}
		return m
	}

	cases := []struct {
		name      string
		want, got map[string]struct{}
		same      bool
	}{
		{"both empty", set(), set(), true},
		{"same members", set("a", "b"), set("b", "a"), true},
		{"one missing", set("a", "b"), set("a"), false},
		{"one extra", set("a"), set("a", "b"), false},
		{"same size, different members", set("a"), set("b"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if SameSet(c.want, c.got) != c.same {
				t.Errorf("SameSet(%v, %v) = %v, want %v", c.want, c.got, !c.same, c.same)
			}
		})
	}
}
