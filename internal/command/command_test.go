package command

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

type captureHandler struct{ records []slog.Record }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) highest() slog.Level {
	level := slog.LevelDebug
	for _, r := range h.records {
		if r.Level > level {
			level = r.Level
		}
	}
	return level
}

func captureLogs(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

func TestExecuteTrimsOutput(t *testing.T) {
	out, err := execute(Action{Tool: "echo", Args: []string{"  spaced  "}})
	if err != nil {
		t.Fatalf("echo failed: %v", err)
	}
	if out != "spaced" {
		t.Errorf("got %q, want %q", out, "spaced")
	}
}

func TestExecutePipesStdin(t *testing.T) {
	out, err := execute(Action{Tool: "cat", Stdin: "a ruleset\n"})
	if err != nil {
		t.Fatalf("cat failed: %v", err)
	}
	if out != "a ruleset" {
		t.Errorf("got %q, want %q", out, "a ruleset")
	}
}

func TestExecuteToleratedFailureIsNotWarned(t *testing.T) {
	h := captureLogs(t)

	out, err := execute(Action{
		Tool:     "cat",
		Args:     []string{"/nonexistent/route-balancer-test"},
		OnFail:   FailWarn,
		Tolerate: []string{"No such file"},
	})
	if err == nil {
		t.Fatal("expected an error from cat on a missing path")
	}
	if !strings.Contains(out, "No such file") {
		t.Fatalf("output did not carry the failure: %q", out)
	}
	if lvl := h.highest(); lvl > slog.LevelDebug {
		t.Errorf("tolerated failure logged at %v, want debug only", lvl)
	}
}

func TestExecuteUntoleratedFailureIsWarned(t *testing.T) {
	h := captureLogs(t)

	if _, err := execute(Action{
		Tool:   "cat",
		Args:   []string{"/nonexistent/route-balancer-test"},
		OnFail: FailWarn,
	}); err == nil {
		t.Fatal("expected an error from cat on a missing path")
	}
	if lvl := h.highest(); lvl != slog.LevelWarn {
		t.Errorf("logged at %v, want warn", lvl)
	}
}

func TestExecuteSilentFailPolicies(t *testing.T) {
	for name, policy := range map[string]FailPolicy{
		"quiet":  FailQuiet,
		"ignore": FailIgnore,
	} {
		t.Run(name, func(t *testing.T) {
			h := captureLogs(t)
			if _, err := execute(Action{
				Tool:   "cat",
				Args:   []string{"/nonexistent/route-balancer-test"},
				OnFail: policy,
			}); err == nil {
				t.Fatal("expected an error from cat on a missing path")
			}
			if lvl := h.highest(); lvl > slog.LevelDebug {
				t.Errorf("logged at %v, want debug only", lvl)
			}
		})
	}
}

func TestOrderActionsSortsByDependency(t *testing.T) {
	acts := Ordered([]Action{
		{Args: []string{"ecmp"}, Order: OrderECMPRoute},
		{Args: []string{"fwmark"}, Order: OrderFwmarkRule},
		{Args: []string{"rule"}, Order: OrderTableRule},
		{Args: []string{"nft"}, Order: OrderNftTable},
		{Args: []string{"route"}, Order: OrderTableRoutes},
	})

	want := []string{"route", "rule", "fwmark", "ecmp", "nft"}
	for i, w := range want {
		if acts[i].Args[0] != w {
			t.Fatalf("position %d = %q, want %q (full order: %v)", i, acts[i].Args[0], w, argsOf(acts))
		}
	}
}

func TestOrderActionsIsStableWithinARank(t *testing.T) {
	acts := Ordered([]Action{
		{Args: []string{"subnet"}, Order: OrderTableRoutes},
		{Args: []string{"default"}, Order: OrderTableRoutes},
		{Args: []string{"rule"}, Order: OrderTableRule},
	})
	if acts[0].Args[0] != "subnet" || acts[1].Args[0] != "default" {
		t.Errorf("order = %v, want subnet before default", argsOf(acts))
	}
}

func TestTeardownRanksReverseTheInstallOrder(t *testing.T) {
	install := []int{OrderMangleChain, OrderTableRoutes, OrderTableRule,
		OrderFwmarkRule, OrderECMPRoute, OrderNftTable, OrderProxyEntry}

	acts := make([]Action, 0, len(install))
	for _, order := range install {
		acts = append(acts, Action{Args: []string{fmt.Sprint(order)}, Order: Teardown(order)})
	}
	Ordered(acts)

	for i := range install {
		order := install[len(install)-1-i]
		if got := acts[i].Args[0]; got != fmt.Sprint(order) {
			t.Fatalf("position %d = %s, want %d (full order: %v)", i, got, order, argsOf(acts))
		}
	}
}

func TestEveryTeardownSortsBelowEveryInstall(t *testing.T) {
	acts := Ordered([]Action{
		{Args: []string{"install"}, Order: OrderMangleChain},
		{Args: []string{"remove"}, Order: Teardown(OrderProxyEntry)},
	})
	if acts[0].Args[0] != "remove" {
		t.Errorf("order = %v, want the removal first", argsOf(acts))
	}
}

func argsOf(acts []Action) []string {
	out := make([]string, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Args[0])
	}
	return out
}
