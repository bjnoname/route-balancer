package nptv6

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/nft"
)

func ruleComment(a Assignment) string {
	return fmt.Sprintf("rb-nptv6 %s %s", a.Uplink, a.Subnet)
}

func guardComment(uplink string) string {
	return fmt.Sprintf("rb-nptv6-guard %s", uplink)
}

func closedComment(a Assignment) string {
	return fmt.Sprintf("rb-nptv6-closed %s %s", a.Uplink, a.Subnet)
}

func (o Options) translating(assignments []Assignment) map[string]bool {
	out := make(map[string]bool, len(assignments))
	for _, a := range assignments {
		out[a.Uplink] = true
	}
	return out
}

func (o Options) GuardUplinks(assignments []Assignment) []string {
	if !o.LeakProtection || o.Internal == nil {
		return nil
	}
	translating := o.translating(assignments)
	var out []string
	for _, u := range o.Uplinks {
		if !translating[u.Name] {
			out = append(out, u.Name)
		}
	}
	return out
}

func (o Options) allowsInbound(uplink string) bool {
	u, ok := o.uplink(uplink)
	return ok && u.AllowInbound
}

func (o Options) InboundUplinks(assignments []Assignment) (open, closed []string) {
	translating := o.translating(assignments)
	for _, u := range o.Uplinks {
		switch {
		case !translating[u.Name]:
		case u.AllowInbound:
			open = append(open, u.Name)
		default:
			closed = append(closed, u.Name)
		}
	}
	return open, closed
}

func (o Options) Ruleset(assignments []Assignment) string {
	guards := o.GuardUplinks(assignments)
	if len(assignments) == 0 && len(guards) == 0 {
		return ""
	}

	var pre, post, guard, closed []string
	for _, a := range assignments {
		post = append(post, fmt.Sprintf(
			`ip6 saddr %s oifname "%s" snat ip6 prefix to %s comment "%s"`,
			a.Internal, a.Uplink, a.External, ruleComment(a)))
		if o.allowsInbound(a.Uplink) {
			pre = append(pre, fmt.Sprintf(
				`ip6 daddr %s iifname "%s" dnat ip6 prefix to %s comment "%s"`,
				a.External, a.Uplink, a.Internal, ruleComment(a)))
			continue
		}
		closed = append(closed, fmt.Sprintf(
			`ip6 daddr %s iifname "%s" ct state new counter drop comment "%s"`,
			a.External, a.Uplink, closedComment(a)))
	}
	for _, uplink := range guards {
		guard = append(guard, fmt.Sprintf(
			`ip6 saddr %s oifname "%s" drop comment "%s"`,
			o.Internal, uplink, guardComment(uplink)))
	}

	inboundChain := ""
	if len(closed) > 0 {
		inboundChain = fmt.Sprintf(`
  chain inbound {
    type filter hook prerouting priority dstnat - 10; policy accept;
    %s
  }
`, strings.Join(closed, "\n    "))
	}

	guardChain := ""
	if len(guard) > 0 {
		guardChain = fmt.Sprintf(`
  chain guard {
    type filter hook postrouting priority srcnat + 10; policy accept;
    %s
  }
`, strings.Join(guard, "\n    "))
	}

	return nft.Ruleset(o.table(), fmt.Sprintf(`  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    %s
  }

  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    %s
  }
%s%s`, strings.Join(pre, "\n    "), strings.Join(post, "\n    "), inboundChain, guardChain))
}

type RuleKey struct {
	dir    string
	match  string
	iface  string
	target string
}

const (
	dirSNAT    = "snat"
	dirDNAT    = "dnat"
	dirDrop    = "drop"
	dirDropOut = "drop-out"
	dirDropIn  = "drop-in"
)

func (o Options) DesiredRules(assignments []Assignment) map[RuleKey]struct{} {
	want := make(map[RuleKey]struct{}, len(assignments)*2)
	for _, a := range assignments {
		want[RuleKey{dir: dirSNAT, match: a.Internal.String(), iface: a.Uplink, target: a.External.String()}] = struct{}{}
		if o.allowsInbound(a.Uplink) {
			want[RuleKey{dir: dirDNAT, match: a.External.String(), iface: a.Uplink, target: a.Internal.String()}] = struct{}{}
			continue
		}
		want[RuleKey{dir: dirDropIn, match: a.External.String(), iface: a.Uplink}] = struct{}{}
	}
	for _, uplink := range o.GuardUplinks(assignments) {
		want[RuleKey{dir: dirDropOut, match: o.Internal.String(), iface: uplink}] = struct{}{}
	}
	return want
}

func parseRules(out string) map[RuleKey]struct{} {
	got := make(map[RuleKey]struct{})
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, " comment "); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)

		k := RuleKey{
			match:  command.FirstOf(fields, "saddr", "daddr"),
			iface:  strings.Trim(command.FirstOf(fields, "iifname", "oifname"), `"`),
			target: command.FieldAfter(fields, "to"),
			dir:    command.VerbOf(fields, dirSNAT, dirDNAT, dirDrop),
		}
		if k.dir == dirDrop {
			k.dir = dirDropOut
			if command.FieldAfter(fields, "daddr") != "" {
				k.dir = dirDropIn
			}
		}
		switch {
		case k.dir == dirDropOut || k.dir == dirDropIn:
			if k.match != "" && k.iface != "" {
				got[k] = struct{}{}
			}
		case k.dir != "" && k.match != "" && k.iface != "" && k.target != "":
			got[k] = struct{}{}
		}
	}
	return got
}

func (o Options) table() nft.Table {
	return nft.Table{
		Family:      "ip6",
		Name:        o.Table,
		ApplyOrder:  command.OrderNftTable,
		RemoveOrder: command.OrderNftTable,
	}
}

func (o Options) DeleteAction() command.Action { return nft.Delete(o.table()) }

func (o Options) Actions(ruleset string) []command.Action {
	return nft.Apply(o.table(), ruleset)
}

func (o Options) ReportApplied(assignments []Assignment, dropped map[string][]string, guarded []string) {
	for _, uplink := range command.SortedKeys(dropped) {
		slog.Warn("NPTv6: delegation too small to map every subnet",
			"uplink", uplink,
			"dropped", strings.Join(dropped[uplink], ","),
			"hint", "the delegation holds fewer /64s than subnet_priority names; "+
				"the subnets listed first were mapped")
	}

	if len(assignments) == 0 && len(guarded) == 0 {
		slog.Info("NPTv6: no uplink holds a usable lease — translation rules removed")
		return
	}

	if len(guarded) > 0 {
		slog.Warn("NPTv6: uplink is not translating — internal traffic leaving it is dropped locally",
			"uplinks", strings.Join(guarded, ","),
			"internal", o.Internal.String(),
			"reason", "no usable lease, or health has withdrawn the uplink")
	}

	slog.Info("NPTv6 translation rules applied",
		"mappings", len(assignments), "table", o.Table)
	for _, a := range assignments {
		slog.Info("NPTv6 mapping",
			"uplink", a.Uplink, "subnet", a.Subnet,
			"internal", a.Internal.String(), "external", a.External.String())
	}

	open, closed := o.InboundUplinks(assignments)
	if len(closed) > 0 {
		slog.Info("NPTv6: inbound-initiated connections are dropped at these uplinks",
			"uplinks", strings.Join(closed, ","),
			"hint", "replies to traffic the LAN started are unaffected; set "+
				"allow_inbound_connections to make the mapped subnets reachable")
	}
	if len(open) > 0 {
		slog.Warn("NPTv6: uplink is reachable from the internet at its translated addresses",
			"uplinks", strings.Join(open, ","),
			"internal", o.Internal.String(),
			"reason", "allow_inbound_connections is set, so the whole of each mapped /64 "+
				"is reachable; which hosts and ports may be reached is the host firewall's")
	}
}

func (o Options) TableQuery() command.Typed[map[RuleKey]struct{}] {
	return nft.Query(o.table(), parseRules)
}

func (o Options) Queries() []command.Query {
	if !o.Enabled {
		return nil
	}
	return []command.Query{o.TableQuery().Query}
}

type InstalledTable = nft.Installed[map[RuleKey]struct{}]

func (o Options) ReadInstalledTable(a command.Answers) InstalledTable {
	return nft.Read(o.table(), parseRules, a)
}

func (o Options) NeedsRestore(want map[RuleKey]struct{}, inst InstalledTable) bool {
	if !o.Enabled {
		return false
	}
	if !inst.Present {
		return len(want) > 0
	}
	return !nft.SameSet(want, inst.Rules)
}
