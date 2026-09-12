package route

import (
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/sysctl"
)

type Options struct {
	Proto       int
	TableOffset int
	IPv6Metric  int

	Managed  []int
	Observed []int

	Marked []int
}

func (o Options) RuleFamilies() []int {
	return []int{FamilyV4, FamilyV6}
}

func (o Options) ProtoString() string { return strconv.Itoa(o.Proto) }

func (o Options) ManagedFamilies() []int { return o.Managed }

func (o Options) ObservedFamilies() []int { return o.Observed }

func (o Options) Manages(family int) bool { return slices.Contains(o.Managed, family) }

func (o Options) Observes(family int) bool { return slices.Contains(o.Observed, family) }

func (o Options) UnmanagedFamilies() []int {
	var out []int
	for _, family := range []int{FamilyV4, FamilyV6} {
		if !o.Manages(family) {
			out = append(out, family)
		}
	}
	return out
}

func (o Options) AbandonedECMPQuery(family int) command.Typed[[]ObservedRoute] {
	return DefaultRoutesQuery(family, "proto", o.ProtoString())
}

func (o Options) ReadAbandonedECMP(family int, a command.Answers) bool {
	routes, err := o.AbandonedECMPQuery(family).Value(a)
	return err == nil && len(routes) > 0
}

func (o Options) ECMPMetric(family int) string {
	if family == FamilyV6 {
		return strconv.Itoa(o.IPv6Metric)
	}
	return "0"
}

const ReservedTable = 253

func (o Options) TableForIface(ifIndex int) (int, bool) {
	table := o.TableOffset + ifIndex
	return table, table > 0 && table < ReservedTable
}

func (o Options) GatewayPriority(ifIndex int) (string, bool) {
	table, ok := o.TableForIface(ifIndex)
	return strconv.Itoa(1000 + table), ok
}

func delIP(order int, args ...string) command.Action {
	return command.Action{
		Tool:   command.ToolIP,
		Args:   args,
		OnFail: command.FailWarn,
		Order:  command.Teardown(order),
	}
}

func (o Options) ECMPQueries(family int) []command.Query {
	metric := o.ECMPMetric(family)
	return []command.Query{
		ownRouteQuery(family, metric, o.ProtoString()).Query,
		DefaultRoutesQuery(family, "metric", metric).Query,
	}
}

func (o Options) ECMPActions(spec ECMPSpec, got InstalledECMP) []command.Action {
	warnForeignAtMetric(got.atMetric, spec.Family, spec.Metric, spec.Proto)

	verb := "add"
	if got.Present {
		verb = "replace"
	}
	args := spec.Args(verb)

	slog.Info("Applying ECMP route", "command", "ip "+strings.Join(args, " "))
	return []command.Action{{
		Tool:   command.ToolIP,
		Args:   args,
		OnFail: command.FailWarn,
		Order:  command.OrderECMPRoute,
	}}
}

type InstalledECMP struct {
	Present  bool
	nexthops map[nexthopKey]int

	atMetric string
}

func (o Options) ReadInstalledECMP(family int, a command.Answers) InstalledECMP {
	var got InstalledECMP

	if ans := a.Get(DefaultRoutesQuery(family, "metric", o.ECMPMetric(family)).Query); ans.Err == nil {
		got.atMetric = ans.Out
	}

	own := ownRouteQuery(family, o.ECMPMetric(family), o.ProtoString())
	ans := a.Get(own.Query)
	if ans.Err != nil || strings.TrimSpace(ans.Out) == "" {
		return got
	}

	nexthops, err := own.ValueOf(ans)
	if err != nil {
		return got
	}
	got.Present = true
	got.nexthops = nexthops
	return got
}

func ECMPNeedsRestore(spec ECMPSpec, got InstalledECMP) bool {
	if !got.Present {
		return true
	}
	return !nexthopsMatch(spec.nexthopMap(), got.nexthops)
}

func (o Options) ReportECMPRetained(family int, reason string, got InstalledECMP) {
	if got.Present {
		slog.Warn("All gateways unhealthy — retaining last ECMP route rather than removing default route. Check probe configuration.",
			"family", FamilyName(family), "reason", reason)
		return
	}
	slog.Info("No active gateways — leaving routing table unchanged",
		"family", FamilyName(family), "reason", reason)
}

func warnForeignAtMetric(atMetric string, family int, metric, proto string) {
	for _, line := range strings.Split(atMetric, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "nexthop") || strings.Contains(line, "proto "+proto) {
			continue
		}
		slog.Warn("Default route at the managed metric already exists",
			"family", FamilyName(family), "metric", metric, "route", line)
	}
}

func (o Options) DeleteECMPAction(family int) command.Action {
	return delIP(command.OrderECMPRoute,
		FamilyFlag(family), "route", "del", "default", "proto", o.ProtoString())
}

func (o Options) GatewayQueries(family int) []command.Query {
	return []command.Query{
		ownRouteQuery(family, o.ECMPMetric(family), o.ProtoString()).Query,
		DefaultRoutesQuery(family).Query,
	}
}

func (o Options) ForeignDefaultRoutes(a command.Answers, family int) ([]ObservedRoute, error) {

	own := routesView(family, ownRouteQuery(family, o.ECMPMetric(family), o.ProtoString()).Query)

	all, err := DefaultRoutesQuery(family).Value(a)
	if err != nil {
		return nil, err
	}
	return subtractRoutes(all, own.ValueOr(a, nil)), nil
}

func (o Options) InstalledTableIfIndexes(rules Rules) []int {
	if rules.Err != nil {
		return nil
	}

	var out []int
	for _, line := range rules.Lines {
		prio, table, ok := parseRulePriorityTable(line)
		if !ok || prio != 1000+table || table < o.TableOffset || table >= ReservedTable {
			continue
		}
		out = append(out, table-o.TableOffset)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

type InstalledFwmark struct {
	IfIndex int
	Prio    string
}

func (o Options) InstalledFwmarks(rules Rules) []InstalledFwmark {
	if rules.Err != nil {
		return nil
	}

	seen := map[int]bool{}
	var out []InstalledFwmark
	for _, line := range rules.Lines {
		if !strings.Contains(line, "fwmark") {
			continue
		}
		prio, table, ok := parseRulePriorityTable(line)
		if !ok || table < o.TableOffset || table >= ReservedTable || seen[table] {
			continue
		}
		seen[table] = true
		out = append(out, InstalledFwmark{IfIndex: table - o.TableOffset, Prio: strconv.Itoa(prio)})
	}
	slices.SortFunc(out, func(a, b InstalledFwmark) int { return a.IfIndex - b.IfIndex })
	return out
}

func DeleteRuleByPriorityAction(family int, prio string) command.Action {
	return delIP(command.OrderFwmarkRule, FamilyFlag(family), "rule", "del", "priority", prio)
}

func parseRulePriorityTable(line string) (prio, table int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, 0, false
	}
	prio, err := strconv.Atoi(strings.TrimSuffix(fields[0], ":"))
	if err != nil {
		return 0, 0, false
	}
	for i, f := range fields {
		if f != "lookup" || i+1 >= len(fields) {
			continue
		}
		table, err := strconv.Atoi(fields[i+1])
		if err != nil {
			return 0, 0, false
		}
		return prio, table, true
	}
	return 0, 0, false
}

func TableActions(spec TableSpec) []command.Action {
	if spec.AddrFamily() == FamilyV6 {
		args := []string{"-6", "route", "replace", "default"}
		if spec.Via != "" {
			args = append(args, "via", spec.Via)
		}
		args = append(args, "dev", spec.IfName, "table", spec.Table)
		return []command.Action{{
			Tool: command.ToolIP, Order: command.OrderTableRoutes,
			OnFail: command.FailWarn, Args: args,
		}}
	}

	var acts []command.Action

	if spec.Subnet != "" {
		acts = append(acts, command.Action{
			Tool: command.ToolIP, Order: command.OrderTableRoutes, OnFail: command.FailWarn,
			Args: []string{"-4", "route", "replace", spec.Subnet, "dev", spec.IfName,
				"src", spec.Src, "table", spec.Table},
		})
	}

	args := []string{"-4", "route", "replace", "default"}
	if spec.Via != "" {
		args = append(args, "via", spec.Via)
	}
	args = append(args, "dev", spec.IfName, "src", spec.Src, "table", spec.Table)
	acts = append(acts, command.Action{Tool: command.ToolIP, Order: command.OrderTableRoutes, OnFail: command.FailWarn, Args: args})

	acts = append(acts, command.Action{
		Tool: command.ToolIP, Order: command.OrderTableRule, OnFail: command.FailWarn,
		Args:     []string{"-4", "rule", "add", "from", spec.Src, "lookup", spec.Table, "priority", spec.Prio},
		Tolerate: []string{"File exists"},
	})

	return acts
}

func RulesQuery(family int) command.Typed[[]string] {
	return command.NewTyped(
		command.Query{Tool: command.ToolIP, Args: []string{FamilyFlag(family), "rule", "show"}},
		func(out string) []string { return strings.Split(out, "\n") },
	)
}

type Rules struct {
	Lines []string
	Err   error
}

func ReadRules(family int, a command.Answers) Rules {
	lines, err := RulesQuery(family).Value(a)
	if err != nil {
		return Rules{Err: err}
	}
	return Rules{Lines: lines}
}

func (r Rules) selects(sel, table string) bool {
	for _, line := range r.Lines {
		if strings.Contains(line, sel) && strings.Contains(line, "lookup "+table) {
			return true
		}
	}
	return false
}

func TableNeedsRestore(spec TableSpec, rules Rules, got TableContents) bool {
	if spec.AddrFamily() != FamilyV6 {
		if rules.Err != nil {
			return true
		}
		if !rules.selects("from "+spec.Src, spec.Table) {
			return true
		}
	}

	want := "default dev " + spec.IfName
	if spec.Via != "" {
		want = "default via " + spec.Via
	}
	return got.Err != nil || !strings.Contains(got.Out, want)
}

type TableContents struct {
	Out string
	Err error
}

func ReadTableContents(family int, table string, a command.Answers) TableContents {
	ans := a.Get(tableContentsQuery(family, table))
	return TableContents{Out: ans.Out, Err: ans.Err}
}

func tableContentsQuery(family int, table string) command.Query {
	return command.Query{
		Tool: command.ToolIP,
		Args: []string{FamilyFlag(family), "route", "show", "table", table},
	}
}

func TableQueries(spec TableSpec) []command.Query {
	contents := tableContentsQuery(spec.AddrFamily(), spec.Table)
	if spec.AddrFamily() == FamilyV6 {
		return []command.Query{contents}
	}
	return []command.Query{RulesQuery(FamilyV4).Query, contents}
}

func TableContentsQuery(family, table int) command.Query {
	return tableContentsQuery(family, strconv.Itoa(table))
}

func (o Options) TeardownTableActions(family int, ifName string, ifIndex int, src string) []command.Action {
	id, ok := o.TableForIface(ifIndex)
	if !ok {
		return nil
	}
	table := strconv.Itoa(id)
	prio, _ := o.GatewayPriority(ifIndex)

	slog.Info("Tearing down per-gateway routing table",
		"family", FamilyName(family), "iface", ifName, "table", table)

	if family == FamilyV6 {
		return []command.Action{delIP(command.OrderTableRoutes, "-6", "route", "flush", "table", table)}
	}

	rule := delIP(command.OrderTableRule, "-4", "rule", "del", "from", src, "lookup", table, "priority", prio)
	if src == "" {
		slog.Debug("no source address known, deleting ip rule by priority",
			"iface", ifName, "priority", prio)
		rule = delIP(command.OrderTableRule, "-4", "rule", "del", "priority", prio)
	}

	return []command.Action{
		rule,
		delIP(command.OrderTableRoutes, "-4", "route", "flush", "table", table),
	}
}

func FwmarkActions(family int, specs []FwmarkSpec) []command.Action {
	var acts []command.Action
	for _, spec := range specs {
		slog.Info("Installing fwmark ip rule",
			"family", FamilyName(family), "mark", spec.Mark, "table", spec.Table)
		acts = append(acts, command.Action{
			Tool: command.ToolIP, Order: command.OrderFwmarkRule, OnFail: command.FailWarn,
			Args: []string{FamilyFlag(family), "rule", "add", "fwmark", strconv.Itoa(spec.Mark),
				"lookup", spec.Table, "priority", spec.Prio},
			Tolerate: []string{"File exists"},
		})
	}
	return acts
}

func FwmarkRulesNeedRestore(specs []FwmarkSpec, rules Rules) bool {
	if len(specs) == 0 {
		return false
	}

	if rules.Err != nil {
		return true
	}

	for _, spec := range specs {
		markHex := "0x" + strconv.FormatInt(int64(spec.Mark), 16)
		if !rules.selects("fwmark "+markHex, spec.Table) {
			return true
		}
	}
	return false
}

func DeleteFwmarkAction(family int, spec FwmarkSpec) command.Action {
	return delIP(command.OrderFwmarkRule, FamilyFlag(family), "rule", "del",
		"fwmark", strconv.Itoa(spec.Mark), "lookup", spec.Table, "priority", spec.Prio)
}

func IPv6RouteSourceQueries(names []string) []command.Query {
	if len(names) == 0 {
		return nil
	}
	qs := []command.Query{ipv6ForwardingQuery().Query}
	for _, name := range names {
		qs = append(qs, acceptRAQuery(name))
	}
	return qs
}

const ipv6ForwardingPath = "/proc/sys/net/ipv6/conf/all/forwarding"

func ipv6ForwardingQuery() command.Typed[bool] {
	return command.NewTyped(
		command.Query{
			Tool: command.ToolSysctl,
			Args: []string{"net.ipv6.conf.all.forwarding"},
			Read: func() (any, error) { return sysctl.Read(ipv6ForwardingPath), nil },
		},
		func(out string) bool { return out == "1" },
	)
}

func acceptRAQuery(ifName string) command.Query {
	return command.Query{
		Tool: command.ToolSysctl,
		Args: []string{"net.ipv6.conf." + ifName + ".accept_ra"},
		Read: func() (any, error) { return sysctl.ReadIface(ifName, "accept_ra"), nil },
	}
}

func CheckIPv6RouteSources(a command.Answers, names []string, hasV6Route map[string]bool) {
	if on, err := ipv6ForwardingQuery().Value(a); err != nil || !on {
		return
	}

	for _, name := range names {
		if hasV6Route[name] {
			continue
		}
		if ra := a.Get(acceptRAQuery(name)).Out; ra != "2" {
			slog.Warn("IPv6 ECMP is enabled but this gateway has no IPv6 default route, "+
				"and the kernel is not processing Router Advertisements on it",
				"iface", name, "accept_ra", ra,
				"hint", "forwarding is on, which makes the kernel ignore RAs unless accept_ra is 2; "+
					"a userspace RA client (systemd-networkd, dhcpcd) holds accept_ra at 0 and "+
					"installs the route itself, in which case this warning is spurious")
		}
	}
}
