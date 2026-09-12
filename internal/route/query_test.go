package route

import (
	"slices"
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
)

func opts(out map[string]string) (Options, *commandtest.Asker) {
	a := commandtest.New(out)
	return Options{Proto: 111, TableOffset: 100, IPv6Metric: 1}, a
}

const (
	v4Own      = "ip -4 route show default metric 0 proto 111"
	v4AtMetric = "ip -4 route show default metric 0"
	v4All      = "ip -4 route show default"
	ruleV4     = "ip -4 rule show"
)

func twoWayECMP() ECMPSpec {
	return ECMPSpec{
		Family: FamilyV4, Metric: "0", Proto: "111",
		Nexthops: []NexthopSpec{
			{Via: "10.0.0.1", Dev: "eth0", Weight: 3},
			{Via: "10.0.1.1", Dev: "eth1", Weight: 1},
		},
	}
}

func TestECMPNeedsRestore(t *testing.T) {
	t.Parallel()

	installed := "default proto 111 metric 0 \n" +
		"\tnexthop via 10.0.0.1 dev eth0 weight 3 \n" +
		"\tnexthop via 10.0.1.1 dev eth1 weight 1 "

	cases := []struct {
		name string
		out  map[string]string
		spec ECMPSpec
		want bool
	}{
		{
			name: "kernel holds exactly what was asked for",
			out:  map[string]string{v4Own: installed},
			spec: twoWayECMP(),
		},
		{
			name: "the read fails",
			out:  map[string]string{},
			spec: twoWayECMP(),
			want: true,
		},
		{
			name: "the route is gone",
			out:  map[string]string{v4Own: "\n"},
			spec: twoWayECMP(),
			want: true,
		},
		{
			name: "a nexthop has been dropped",
			out: map[string]string{v4Own: "default proto 111 metric 0 \n" +
				"\tnexthop via 10.0.0.1 dev eth0 weight 3 "},
			spec: twoWayECMP(),
			want: true,
		},
		{
			name: "a weight was changed under us",
			out: map[string]string{v4Own: "default proto 111 metric 0 \n" +
				"\tnexthop via 10.0.0.1 dev eth0 weight 1 \n" +
				"\tnexthop via 10.0.1.1 dev eth1 weight 1 "},
			spec: twoWayECMP(),
			want: true,
		},
		{
			name: "one nexthop, whose weight the kernel does not report",
			out:  map[string]string{v4Own: "default via 10.0.0.1 dev eth0 proto 111 metric 0 "},
			spec: ECMPSpec{Family: FamilyV4, Metric: "0", Proto: "111",
				Nexthops: []NexthopSpec{{Via: "10.0.0.1", Dev: "eth0", Weight: 5}}},
		},
		{
			name: "a point-to-point nexthop",
			out:  map[string]string{v4Own: "default dev ppp0 proto 111 metric 0 "},
			spec: ECMPSpec{Family: FamilyV4, Metric: "0", Proto: "111",
				Nexthops: []NexthopSpec{{Dev: "ppp0", Weight: 1}}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, a := opts(c.out)
			if got := ECMPNeedsRestore(c.spec, o.ReadInstalledECMP(c.spec.Family, a.Answers(o.ECMPQueries(c.spec.Family)...))); got != c.want {
				t.Errorf("ECMPNeedsRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestECMPNeedsRestoreReadsTheV6MetricNotZero(t *testing.T) {
	t.Parallel()

	o, a := opts(map[string]string{
		"ip -6 route show default metric 1 proto 111": "default via fe80::1 dev eth0 proto 111 metric 1 ",
	})
	spec := ECMPSpec{Family: FamilyV6, Metric: o.ECMPMetric(FamilyV6), Proto: o.ProtoString(),
		Nexthops: []NexthopSpec{{Via: "fe80::1", Dev: "eth0", Weight: 1}}}

	if ECMPNeedsRestore(spec, o.ReadInstalledECMP(FamilyV6, a.Answers(o.ECMPQueries(FamilyV6)...))) {
		t.Errorf("ECMPNeedsRestore reported drift on a route the kernel is holding")
	}
	if a.Count("ip -6 route show default metric 1 proto 111") != 1 {
		t.Errorf("read %v, want the v6 route read once at metric 1", a.Asked)
	}
}

func TestTableNeedsRestore(t *testing.T) {
	t.Parallel()

	spec := TableSpec{
		IfIndex: 2, IfName: "eth0", Table: "102",
		Src: "10.0.0.5", Subnet: "10.0.0.0/24", Via: "10.0.0.1", Prio: "1102",
	}
	rules := "0:\tfrom all lookup local\n" +
		"1102:\tfrom 10.0.0.5 lookup 102\n" +
		"32766:\tfrom all lookup main\n"
	table := "default via 10.0.0.1 dev eth0 src 10.0.0.5 \n" +
		"10.0.0.0/24 dev eth0 scope link src 10.0.0.5 \n"

	cases := []struct {
		name string
		out  map[string]string
		spec TableSpec
		want bool
	}{
		{
			name: "the rule and the table are both there",
			out:  map[string]string{ruleV4: rules, "ip -4 route show table 102": table},
			spec: spec,
		},
		{
			name: "the ip rule was flushed and the table survived",
			out: map[string]string{
				ruleV4:                       "0:\tfrom all lookup local\n32766:\tfrom all lookup main\n",
				"ip -4 route show table 102": table,
			},
			spec: spec,
			want: true,
		},
		{
			name: "the table was flushed and the rule survived",
			out:  map[string]string{ruleV4: rules, "ip -4 route show table 102": "\n"},
			spec: spec,
			want: true,
		},
		{
			name: "the policy database cannot be read",
			out:  map[string]string{"ip -4 route show table 102": table},
			spec: spec,
			want: true,
		},
		{
			name: "the table cannot be read",
			out:  map[string]string{ruleV4: rules},
			spec: spec,
			want: true,
		},
		{
			name: "a point-to-point gateway",
			out: map[string]string{
				ruleV4:                       "1103:\tfrom 192.0.2.1 lookup 103\n",
				"ip -4 route show table 103": "default dev ppp0 src 192.0.2.1 \n",
			},
			spec: TableSpec{IfIndex: 3, IfName: "ppp0", Table: "103",
				Src: "192.0.2.1", Prio: "1103"},
		},
		{
			name: "the source address changed",
			out: map[string]string{
				ruleV4:                       "1102:\tfrom 10.0.0.9 lookup 102\n",
				"ip -4 route show table 102": table,
			},
			spec: spec,
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, a := opts(c.out)
			if got := TableNeedsRestore(c.spec, ReadRules(FamilyV4, ans(a, c.spec)), ReadTableContents(c.spec.AddrFamily(), c.spec.Table, ans(a, c.spec))); got != c.want {
				t.Errorf("TableNeedsRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFwmarkRulesNeedRestore(t *testing.T) {
	t.Parallel()

	specs := []FwmarkSpec{
		{Mark: 1, Table: "102", Prio: "501"},
		{Mark: 2, Table: "103", Prio: "502"},
	}
	both := "501:\tfrom all fwmark 0x1 lookup 102\n" +
		"502:\tfrom all fwmark 0x2 lookup 103\n"

	cases := []struct {
		name  string
		out   map[string]string
		specs []FwmarkSpec
		want  bool
	}{
		{name: "both rules present", out: map[string]string{ruleV4: both}, specs: specs},
		{
			name:  "one rule missing",
			out:   map[string]string{ruleV4: "501:\tfrom all fwmark 0x1 lookup 102\n"},
			specs: specs,
			want:  true,
		},
		{
			name:  "the mark is right and the table is not",
			out:   map[string]string{ruleV4: "501:\tfrom all fwmark 0x1 lookup 199\n502:\tfrom all fwmark 0x2 lookup 103\n"},
			specs: specs,
			want:  true,
		},
		{name: "the read fails", out: map[string]string{}, specs: specs, want: true},
		{
			name:  "no fwmark rules are wanted",
			out:   map[string]string{},
			specs: nil,
		},
		{
			name:  "a mark whose hex spelling differs from its decimal one",
			out:   map[string]string{ruleV4: "510:\tfrom all fwmark 0xa lookup 111\n"},
			specs: []FwmarkSpec{{Mark: 10, Table: "111", Prio: "510"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, a := opts(c.out)
			if got := FwmarkRulesNeedRestore(c.specs, ReadRules(FamilyV4, a.Answers(RulesQuery(FamilyV4).Query))); got != c.want {
				t.Errorf("FwmarkRulesNeedRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestForeignDefaultRoutes(t *testing.T) {
	t.Parallel()

	t.Run("our own ECMP route is subtracted from the listing", func(t *testing.T) {
		t.Parallel()
		o, a := opts(map[string]string{
			v4All: "default proto 111 metric 0 \n" +
				"\tnexthop via 10.0.0.1 dev eth0 weight 1 \n" +
				"default via 10.0.0.1 dev eth0 proto dhcp metric 50 \n" +
				"default via 10.0.1.1 dev eth1 proto dhcp metric 60 \n",
			v4Own: "default proto 111 metric 0 \n" +
				"\tnexthop via 10.0.0.1 dev eth0 weight 1 \n",
		})

		got, err := o.ForeignDefaultRoutes(a.Answers(o.GatewayQueries(FamilyV4)...), FamilyV4)
		if err != nil {
			t.Fatalf("ForeignDefaultRoutes: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d routes %v, want the two uplink routes", len(got), got)
		}
		if got[0].IfName != "eth0" || got[0].Metric != 50 {
			t.Errorf("first route = %+v, want eth0 at metric 50", got[0])
		}
		if got[1].IfName != "eth1" || got[1].Metric != 60 {
			t.Errorf("second route = %+v, want eth1 at metric 60", got[1])
		}
	})

	t.Run("a failed own-route read subtracts nothing", func(t *testing.T) {
		t.Parallel()
		o, a := opts(map[string]string{
			v4All: "default via 10.0.0.1 dev eth0 proto dhcp metric 50 \n",
		})
		got, err := o.ForeignDefaultRoutes(a.Answers(o.GatewayQueries(FamilyV4)...), FamilyV4)
		if err != nil {
			t.Fatalf("ForeignDefaultRoutes: %v", err)
		}
		if len(got) != 1 || got[0].IfName != "eth0" {
			t.Errorf("got %v, want the one uplink route", got)
		}
	})

	t.Run("a failed listing is an error, not an empty world", func(t *testing.T) {
		t.Parallel()
		o, a := opts(map[string]string{v4Own: ""})
		if _, err := o.ForeignDefaultRoutes(a.Answers(o.GatewayQueries(FamilyV4)...), FamilyV4); err == nil {
			t.Errorf("ForeignDefaultRoutes returned no error for an unreadable table")
		}
	})
}

func TestECMPActionsChoosesTheVerbFromTheKernel(t *testing.T) {
	t.Parallel()

	spec := twoWayECMP()

	t.Run("nothing installed yet", func(t *testing.T) {
		t.Parallel()
		o, a := opts(map[string]string{v4Own: "", v4AtMetric: ""})
		acts := o.ECMPActions(spec, o.ReadInstalledECMP(FamilyV4, a.Answers(o.ECMPQueries(FamilyV4)...)))
		if len(acts) != 1 || acts[0].Args[2] != "add" {
			t.Fatalf("actions = %v, want a single add", acts)
		}
	})

	t.Run("our own route is already there", func(t *testing.T) {
		t.Parallel()
		o, a := opts(map[string]string{
			v4Own:      "default via 10.0.0.1 dev eth0 proto 111 metric 0 ",
			v4AtMetric: "default via 10.0.0.1 dev eth0 proto 111 metric 0 ",
		})
		acts := o.ECMPActions(spec, o.ReadInstalledECMP(FamilyV4, a.Answers(o.ECMPQueries(FamilyV4)...)))
		if len(acts) != 1 || acts[0].Args[2] != "replace" {
			t.Fatalf("actions = %v, want a single replace", acts)
		}
	})

	t.Run("a foreign route holds the metric", func(t *testing.T) {
		t.Parallel()
		o, a := opts(map[string]string{
			v4Own:      "",
			v4AtMetric: "default via 10.0.0.1 dev eth0 proto static metric 0 ",
		})
		acts := o.ECMPActions(spec, o.ReadInstalledECMP(FamilyV4, a.Answers(o.ECMPQueries(FamilyV4)...)))
		if len(acts) != 1 || acts[0].Args[2] != "add" {
			t.Fatalf("actions = %v, want a single add", acts)
		}
	})
}

func TestAnECMPPassAsksEachQuestionOnce(t *testing.T) {
	t.Parallel()

	o, a := opts(map[string]string{
		v4Own:      "default via 10.0.0.1 dev eth0 proto 111 metric 0 ",
		v4AtMetric: "default via 10.0.0.1 dev eth0 proto 111 metric 0 ",
	})
	spec := ECMPSpec{Family: FamilyV4, Metric: "0", Proto: "111",
		Nexthops: []NexthopSpec{{Via: "10.0.0.1", Dev: "eth0", Weight: 1}}}

	got := o.ReadInstalledECMP(FamilyV4, a.Answers(o.ECMPQueries(FamilyV4)...))
	ECMPNeedsRestore(spec, got)
	o.ECMPActions(spec, got)
	o.ReportECMPRetained(FamilyV4, "test", got)

	if got := a.Count(v4Own); got != 1 {
		t.Errorf("own route read %d times, want 1", got)
	}
	if got := len(a.Asked); got != 2 {
		t.Errorf("the pass read %v, want the two declared queries", a.Asked)
	}
}

func ans(a *commandtest.Asker, spec TableSpec) command.Answers {
	return a.Answers(TableQueries(spec)...)
}

const (
	ruleV6     = "ip -6 rule show"
	v6Table103 = "ip -6 route show table 103"
)

func v6Spec() TableSpec {
	return TableSpec{
		Family: FamilyV6, IfIndex: 3, IfName: "eth1",
		Table: "103", Via: "fe80::1", Prio: "1103",
	}
}

func TestAnIPv6TableIsReadFromItsContentsAlone(t *testing.T) {
	t.Parallel()

	got := TableQueries(v6Spec())
	if len(got) != 1 || got[0].String() != v6Table103 {
		t.Fatalf("TableQueries asks %v, want just %q", got, v6Table103)
	}
}

func TestIPv6TableNeedsRestore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		out  map[string]string
		want bool
	}{
		{
			name: "the default route is there",
			out:  map[string]string{v6Table103: "default via fe80::1 dev eth1 metric 1024 pref medium"},
		},
		{
			name: "the table is empty",
			out:  map[string]string{v6Table103: ""},
			want: true,
		},
		{
			name: "the route points somewhere else",
			out:  map[string]string{v6Table103: "default via fe80::2 dev eth1 metric 1024 pref medium"},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := commandtest.New(c.out)
			spec := v6Spec()

			got := TableNeedsRestore(spec, Rules{Err: errUnreadable},
				ReadTableContents(spec.AddrFamily(), spec.Table, ans(a, spec)))
			if got != c.want {
				t.Errorf("TableNeedsRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIPv6TableActionsPinNoSource(t *testing.T) {
	t.Parallel()

	acts := TableActions(v6Spec())
	if len(acts) != 1 {
		t.Fatalf("TableActions built %v, want just the default route", argsOf(acts))
	}

	got := strings.Join(acts[0].Args, " ")
	if want := "-6 route replace default via fe80::1 dev eth1 table 103"; got != want {
		t.Errorf("TableActions built %q, want %q", got, want)
	}

	if strings.Contains(got, "src") {
		t.Errorf("the IPv6 table pins a source address: %q", got)
	}
}

func TestIPv6TableTeardownFlushesAndLeavesTheRuleTableAlone(t *testing.T) {
	t.Parallel()

	o, _ := opts(nil)
	acts := o.TeardownTableActions(FamilyV6, "eth1", 3, "")
	if len(acts) != 1 {
		t.Fatalf("teardown ran %v, want just the flush", argsOf(acts))
	}
	if got, want := strings.Join(acts[0].Args, " "), "-6 route flush table 103"; got != want {
		t.Errorf("teardown ran %q, want %q", got, want)
	}
}

func TestFwmarkRulesCarryTheirFamily(t *testing.T) {
	t.Parallel()

	spec := FwmarkSpec{Mark: 2, Table: "103", Prio: "502"}
	for family, want := range map[int]string{
		FamilyV4: "-4 rule add fwmark 2 lookup 103 priority 502",
		FamilyV6: "-6 rule add fwmark 2 lookup 103 priority 502",
	} {
		acts := FwmarkActions(family, []FwmarkSpec{spec})
		if len(acts) != 1 || strings.Join(acts[0].Args, " ") != want {
			t.Errorf("FwmarkActions(%s) built %v, want %q", FamilyName(family), argsOf(acts), want)
		}
		if got, want := strings.Join(DeleteFwmarkAction(family, spec).Args, " "),
			strings.Replace(want, "rule add", "rule del", 1); got != want {
			t.Errorf("DeleteFwmarkAction(%s) built %q, want %q", FamilyName(family), got, want)
		}
	}
}

func TestInstalledFwmarks(t *testing.T) {
	t.Parallel()

	o, _ := opts(nil)
	got := o.InstalledFwmarks(Rules{Lines: []string{
		"0:	from all lookup local",
		"501:	from all fwmark 0x1 lookup 102",
		"502:	from all fwmark 0x2 lookup 103",
		"502:	from all fwmark 0x3 lookup 42",
		"32766:	from all lookup main",
	}})

	want := []InstalledFwmark{{IfIndex: 2, Prio: "501"}, {IfIndex: 3, Prio: "502"}}
	if !slices.Equal(got, want) {
		t.Errorf("InstalledFwmarks = %v, want %v", got, want)
	}

	if got := o.InstalledFwmarks(Rules{Err: errUnreadable}); got != nil {
		t.Errorf("an unreadable rule table yielded %v, want nothing torn down", got)
	}
}
