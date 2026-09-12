package command

import (
	"cmp"
	"log/slog"
	"os/exec"
	"slices"
	"sort"
	"strings"
)

type Tool string

const (
	ToolIP       Tool = "ip"
	ToolIPTables Tool = "iptables"
	ToolNFT      Tool = "nft"
	ToolSysctl   Tool = "sysctl"
	ToolNetlink  Tool = "netlink"

	ToolProbe Tool = "probe"

	ToolSolicit       Tool = "solicit"
	ToolProbeClaim    Tool = "probe-claim"
	ToolClaimVerdict  Tool = "claim-verdict"
	ToolMonitor       Tool = "monitor"
	ToolHealthVerdict Tool = "health-verdict"
)

type FailPolicy int

const (
	FailWarn FailPolicy = iota
	FailQuiet
	FailIgnore
)

const (
	OrderMonitorStop  = -1
	OrderMonitorStart = 1

	OrderMangleChain = 5
	OrderTableRoutes = 10
	OrderTableRule   = 20
	OrderFwmarkRule  = 30
	OrderECMPRoute   = 40
	OrderNftTable    = 50
	OrderProxyKnob   = 55
	OrderProxyEntry  = 60
)

func Teardown(order int) int { return -order }

const (
	CleanupRoutes = 1000 * (iota + 1)
	CleanupFwmark
	CleanupMangle
	CleanupNptv6
	CleanupNdp
)

func InCleanupPhase(phase int, acts []Action) []Action {
	out := make([]Action, len(acts))
	for i, a := range acts {
		a.Order += phase
		out[i] = a
	}
	return out
}

type Action struct {
	Tool  Tool
	Args  []string
	Stdin string

	Write func() (string, error)

	OnFail FailPolicy

	Tolerate []string

	Order int
}

type Runner interface {
	Run(a Action) (string, error)
	RunAll(acts []Action)
}

func (s *System) Run(a Action) (string, error) { return execute(a) }

func (s *System) RunAll(acts []Action) {
	for _, a := range Ordered(acts) {
		_, _ = execute(a)
	}
}

func execute(a Action) (string, error) {
	out, err := perform(a)

	if err == nil {
		slog.Debug("executed", "tool", a.Tool, "args", a.Args)
		return out, nil
	}

	for _, t := range a.Tolerate {
		if strings.Contains(out, t) {
			slog.Debug("tolerated failure", "tool", a.Tool, "args", a.Args, "output", out)
			return out, err
		}
	}

	switch a.OnFail {
	case FailWarn:
		slog.Warn(string(a.Tool)+" command failed",
			"args", a.Args, "output", out, "error", err)
	case FailIgnore:
		slog.Debug(string(a.Tool)+" command failed (ignored)",
			"args", a.Args, "output", out)
	case FailQuiet:
	}
	return out, err
}

func perform(a Action) (string, error) {
	if a.Write != nil {
		out, err := a.Write()
		return strings.TrimSpace(out), err
	}

	cmd := exec.Command(string(a.Tool), a.Args...)
	if a.Stdin != "" {
		cmd.Stdin = strings.NewReader(a.Stdin)
	}
	raw, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(raw)), err
}

func Ordered(acts []Action) []Action {
	sort.SliceStable(acts, func(i, j int) bool { return acts[i].Order < acts[j].Order })
	return acts
}

func FieldAfter(fields []string, key string) string {
	for i, f := range fields {
		if f == key && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func FirstOf(fields []string, keys ...string) string {
	for _, key := range keys {
		if v := FieldAfter(fields, key); v != "" {
			return v
		}
	}
	return ""
}

func VerbOf(fields []string, verbs ...string) string {
	for _, f := range fields {
		for _, v := range verbs {
			if f == v {
				return v
			}
		}
	}
	return ""
}

func SortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	return SortedKeysFunc(m, cmp.Compare[K])
}

func SortedKeysFunc[K comparable, V any](m map[K]V, compare func(a, b K) int) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, compare)
	return out
}
