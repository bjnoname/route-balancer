package ndp

import (
	"log/slog"
	"net"
	"sort"
	"strings"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/nptv6"
	"github.com/bjnoname/route-balancer/internal/sysctl"
)

type ProxyEntry struct {
	Addr   string
	Uplink string
}

func externalAddr(external *net.IPNet, host net.IP) net.IP {
	if external == nil {
		return nil
	}
	ext, h := external.IP.To16(), host.To16()
	if ext == nil || h == nil {
		return nil
	}
	out := make(net.IP, net.IPv6len)
	copy(out, ext.Mask(net.CIDRMask(64, 128)))
	copy(out[8:], h[8:])
	return out
}

func (o Options) DesiredEntries(assignments []nptv6.Assignment, hosts []LearnedHost) map[ProxyEntry]struct{} {
	want := map[ProxyEntry]struct{}{}
	for _, a := range assignments {
		if !o.proxies(a.Uplink) {
			continue
		}
		for _, h := range hosts {
			if h.Uplink != a.Uplink || !a.Internal.Contains(h.Addr) {
				continue
			}
			if ext := externalAddr(a.External, h.Addr); ext != nil {
				want[ProxyEntry{Addr: ext.String(), Uplink: a.Uplink}] = struct{}{}
			}
		}
	}
	return want
}

type InstalledProxy struct {
	Entries map[ProxyEntry]struct{}
	Err     error
}

func (o Options) ReadInstalledProxy(a command.Answers) InstalledProxy {
	entries, err := o.EntriesQuery().Value(a)
	if err != nil {
		return InstalledProxy{Err: err}
	}
	return InstalledProxy{Entries: entries}
}

func (o Options) EntriesQuery() command.Typed[map[ProxyEntry]struct{}] {
	return command.NewTyped(
		command.Query{Tool: command.ToolIP, Args: []string{"-6", "neigh", "show", "proxy"}},
		func(out string) map[ProxyEntry]struct{} { return parseProxyNeigh(out, o.Proto) },
	)
}

func parseProxyNeigh(out, proto string) map[ProxyEntry]struct{} {
	entries := map[ProxyEntry]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil || ip.To4() != nil {
			continue
		}

		dev := command.FieldAfter(fields, "dev")
		if dev == "" || command.FieldAfter(fields, "proto") != proto {
			continue
		}
		entries[ProxyEntry{Addr: ip.String(), Uplink: dev}] = struct{}{}
	}
	return entries
}

func SortedEntries(m map[ProxyEntry]struct{}) []ProxyEntry {
	out := make([]ProxyEntry, 0, len(m))
	for e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Uplink != out[j].Uplink {
			return out[i].Uplink < out[j].Uplink
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

func (o Options) InstallEntryAction(e ProxyEntry, how string) command.Action {
	slog.Info("NDP proxy: answering for translated address",
		"address", e.Addr, "uplink", e.Uplink, "claim", how)
	return ipAction(command.OrderProxyEntry,
		"-6", "neigh", "add", "proxy", e.Addr, "dev", e.Uplink, "protocol", o.Proto)
}

func (o Options) WithdrawEntryAction(e ProxyEntry) command.Action {
	return ipAction(command.Teardown(command.OrderProxyEntry),
		"-6", "neigh", "del", "proxy", e.Addr, "dev", e.Uplink)
}

func ProxyNDPQuery(ifName string) command.Typed[bool] {
	return command.NewTyped(
		command.Query{
			Tool: command.ToolSysctl,
			Args: []string{"net.ipv6.conf." + ifName + ".proxy_ndp"},
			Read: func() (any, error) { return sysctl.ReadIface(ifName, "proxy_ndp"), nil },
		},
		func(out string) bool { return out == "1" },
	)
}

func ReadProxyNDP(a command.Answers, ifName string) bool {
	return ProxyNDPQuery(ifName).ValueOr(a, false)
}

func EnableProxyNDPAction(ifName string) command.Action {
	return command.Action{
		Tool:   command.ToolSysctl,
		Args:   []string{"net.ipv6.conf." + ifName + ".proxy_ndp=1"},
		Order:  command.OrderProxyKnob,
		OnFail: command.FailWarn,
		Write: func() (string, error) {
			changed, err := sysctl.WriteIface(ifName, "proxy_ndp", "1")
			if err != nil {
				return "cannot enable proxy_ndp — proxy entries will be inert", err
			}
			if changed {
				slog.Info("NDP proxy: enabled proxy_ndp", "iface", ifName)
			}
			return "", nil
		},
	}
}
