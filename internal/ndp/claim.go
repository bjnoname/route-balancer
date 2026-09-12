package ndp

import (
	"net"
	"sort"
	"strings"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/probe"
)

const ProbeTimeout = 2500 * time.Millisecond

type ClaimKind int

const (
	ClaimSpeculative ClaimKind = iota
	ClaimScreened
)

type Claim struct {
	Entry ProxyEntry
	Kind  ClaimKind
}

type Verdict struct {
	Claim
	Contested bool
}

func SortedClaims(claims []Claim) []Claim {
	out := append([]Claim(nil), claims...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Entry.Uplink != out[j].Entry.Uplink {
			return out[i].Entry.Uplink < out[j].Entry.Uplink
		}
		return out[i].Entry.Addr < out[j].Entry.Addr
	})
	return out
}

func (o Options) PurgeClaimActions(uplink string, batch []Claim, neighbors map[string]bool) []command.Action {
	pending := make(map[string]struct{}, len(batch))
	for _, c := range batch {
		pending[c.Entry.Addr] = struct{}{}
	}

	var acts []command.Action
	for _, addr := range command.SortedKeys(neighbors) {
		if _, ours := pending[addr]; !ours {
			continue
		}
		acts = append(acts, command.Action{
			Tool:   command.ToolIP,
			Args:   []string{"-6", "neigh", "del", addr, "dev", uplink},
			OnFail: command.FailIgnore,
			Order:  command.Teardown(command.OrderProxyEntry),
		})
	}
	return acts
}

func SolicitAction(addr, uplink string, unsent func()) command.Action {
	return command.Action{
		Tool:   command.ToolSolicit,
		Args:   []string{addr, "dev", uplink},
		OnFail: command.FailIgnore,
		Order:  command.OrderProxyEntry,
		Write: func() (string, error) {
			err := sendSolicit(addr, uplink)
			if err != nil {
				unsent()
			}
			return "", err
		},
	}
}

func sendSolicit(addr, uplink string) error {
	dialer := probe.BindToDeviceDialer(uplink)
	dialer.Timeout = ProbeTimeout

	conn, err := dialer.Dial("udp6", net.JoinHostPort(addr, "9"))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte{0})
	return err
}

func NeighborsQuery(uplink string) command.Typed[map[string]bool] {
	return command.NewTyped(
		command.Query{Tool: command.ToolIP, Args: []string{"-6", "neigh", "show", "dev", uplink}},
		parseNeighbors,
	)
}

func ReadNeighbors(uplink string, a command.Answers) map[string]bool {
	return NeighborsQuery(uplink).ValueOr(a, nil)
}

func parseNeighbors(out string) map[string]bool {
	resolved := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.Contains(line, " proxy") {
			continue
		}
		if !strings.Contains(line, "lladdr") ||
			strings.Contains(line, "FAILED") || strings.Contains(line, "INCOMPLETE") {
			continue
		}
		if ip := net.ParseIP(fields[0]); ip != nil {
			resolved[ip.String()] = true
		}
	}
	return resolved
}
