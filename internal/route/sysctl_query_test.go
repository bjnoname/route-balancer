package route

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
)

type logSink struct {
	lines []string
}

func (h *logSink) Enabled(context.Context, slog.Level) bool { return true }

func (h *logSink) Handle(_ context.Context, r slog.Record) error {
	line := r.Level.String() + " " + r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	h.lines = append(h.lines, line)
	return nil
}

func (h *logSink) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logSink) WithGroup(string) slog.Handler      { return h }

func (h *logSink) saw(substr string) bool {
	for _, l := range h.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func captureLogs(t *testing.T) *logSink {
	t.Helper()

	h := &logSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

func TestTheIPv6SourceCheckReadsOnlyWhatItDeclared(t *testing.T) {
	names := []string{"wan0", "wan1"}

	answers := func(forwarding string, ra map[string]string) command.Answers {
		out := map[string]string{"sysctl net.ipv6.conf.all.forwarding": forwarding}
		for iface, v := range ra {
			out["sysctl net.ipv6.conf."+iface+".accept_ra"] = v
		}
		return commandtest.Recorded(out)
	}

	if got := len(IPv6RouteSourceQueries(names)); got != len(names)+1 {
		t.Fatalf("declared %d queries for %d gateways, want %d", got, len(names), len(names)+1)
	}
	if IPv6RouteSourceQueries(nil) != nil {
		t.Error("a pass that will not run the check still declared reads for it")
	}

	t.Run("forwarding off says nothing", func(t *testing.T) {
		log := captureLogs(t)
		CheckIPv6RouteSources(answers("0", nil), names, map[string]bool{})
		if len(log.lines) != 0 {
			t.Errorf("logged %v with forwarding off, want nothing", log.lines)
		}
	})

	t.Run("only the gateway that needs the warning gets it", func(t *testing.T) {
		log := captureLogs(t)
		CheckIPv6RouteSources(
			answers("1", map[string]string{"wan0": "0", "wan1": "2"}),
			names, map[string]bool{},
		)
		if !log.saw("iface=wan0") {
			t.Errorf("wan0 has forwarding on, no v6 route and accept_ra=0 and was not warned about: %v", log.lines)
		}
		if log.saw("iface=wan1") {
			t.Errorf("wan1 has accept_ra=2 and was warned about anyway: %v", log.lines)
		}
		if log.saw("was not declared") {
			t.Errorf("the check read something the pass never declared: %v", log.lines)
		}
	})

	t.Run("a gateway that already has a v6 route is not warned about", func(t *testing.T) {
		log := captureLogs(t)
		CheckIPv6RouteSources(
			answers("1", map[string]string{"wan0": "0", "wan1": "0"}),
			names, map[string]bool{"wan0": true, "wan1": true},
		)
		if len(log.lines) != 0 {
			t.Errorf("logged %v for gateways that already hold a v6 route, want nothing", log.lines)
		}
	})
}

func TestRuleFamiliesDoesNotNarrowToWhatIsMarked(t *testing.T) {
	t.Parallel()

	for _, marked := range [][]int{nil, {FamilyV4}, {FamilyV4, FamilyV6}} {
		o := Options{Marked: marked}
		got := o.RuleFamilies()
		if len(got) != 2 || got[0] != FamilyV4 || got[1] != FamilyV6 {
			t.Errorf("RuleFamilies with Marked=%v = %v, want both: what is installed "+
				"outlives what is wanted, and nothing unread can be reaped", marked, got)
		}
	}
}
