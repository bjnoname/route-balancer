package ndp

import (
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
)

const (
	learnTable = "nft list table ip6 nptv6-ndp"
	learnSet   = "nft list set ip6 nptv6-ndp learned"
	neighProxy = "ip -6 neigh show proxy"
)

func proxyOptions(t *testing.T, out map[string]string) (Options, *commandtest.Asker) {
	t.Helper()
	a := commandtest.New(out)
	return Options{

		Table:    "nptv6-ndp",
		Proto:    "111",
		MaxHosts: 512,
		Uplinks: []Uplink{{
			Name:    "wan0",
			Subnets: []Subnet{{Name: "main", Prefix: mustCIDR(t, "fd00:dead:beef:1::/64")}},
		}},
	}, a
}

func TestReadInstalledProxy(t *testing.T) {
	t.Parallel()

	t.Run("only our own entries are adopted", func(t *testing.T) {
		t.Parallel()
		o, a := proxyOptions(t, map[string]string{
			neighProxy: "2001:db8:1::10 dev wan0 proxy proto 111\n" +
				"2001:db8:1::20 dev wan0 proxy proto 111\n" +
				"2001:db8:1::99 dev wan0 proxy\n" +
				"192.0.2.1 dev wan0 proxy proto 111\n",
		})

		got := o.ReadInstalledProxy(a.Answers(o.EntriesQuery().Query))
		if got.Err != nil {
			t.Fatalf("ReadInstalledProxy: %v", got.Err)
		}
		want := map[ProxyEntry]struct{}{
			{Addr: "2001:db8:1::10", Uplink: "wan0"}: {},
			{Addr: "2001:db8:1::20", Uplink: "wan0"}: {},
		}
		if len(got.Entries) != len(want) {
			t.Fatalf("got %v, want %v", SortedEntries(got.Entries), SortedEntries(want))
		}
		for e := range want {
			if _, ok := got.Entries[e]; !ok {
				t.Errorf("entry %v missing from %v", e, SortedEntries(got.Entries))
			}
		}
	})

	t.Run("a failed read is an error, not an empty table", func(t *testing.T) {
		t.Parallel()
		o, a := proxyOptions(t, map[string]string{})
		if got := o.ReadInstalledProxy(a.Answers(o.EntriesQuery().Query)); got.Err == nil {
			t.Errorf("ReadInstalledProxy returned no error for an unreadable table")
		}
	})
}

func TestReadLearnedHosts(t *testing.T) {
	t.Parallel()

	t.Run("the set holds two hosts", func(t *testing.T) {
		t.Parallel()
		o, a := proxyOptions(t, map[string]string{
			learnSet: `table ip6 nptv6-ndp {
	set learned {
		type ipv6_addr . ifname
		size 512
		flags dynamic,timeout
		timeout 1h
		elements = { fd00:dead:beef:1::10 . "wan0" expires 59m59s989ms,
			     fd00:dead:beef:1::20 . "wan0" expires 12m3s }
	}
}`,
		})

		hosts, ok := o.ReadLearnedHosts(a.Answers(o.LearnedHostsQuery().Query))
		if !ok {
			t.Fatalf("ReadLearnedHosts reported the set unavailable")
		}
		if len(hosts) != 2 {
			t.Fatalf("got %d hosts %v, want 2", len(hosts), hosts)
		}
		if hosts[0].Uplink != "wan0" || hosts[0].Addr.String() != "fd00:dead:beef:1::10" {
			t.Errorf("first host = %+v", hosts[0])
		}
	})

	t.Run("an empty set is available and empty", func(t *testing.T) {
		t.Parallel()
		o, a := proxyOptions(t, map[string]string{
			learnSet: "table ip6 nptv6-ndp {\n\tset learned {\n\t\ttype ipv6_addr . ifname\n\t}\n}",
		})
		hosts, ok := o.ReadLearnedHosts(a.Answers(o.LearnedHostsQuery().Query))
		if !ok {
			t.Fatalf("ReadLearnedHosts reported an empty set unavailable")
		}
		if len(hosts) != 0 {
			t.Errorf("got %v, want no hosts", hosts)
		}
	})

	t.Run("a missing set is unavailable, not empty", func(t *testing.T) {
		t.Parallel()
		o, a := proxyOptions(t, map[string]string{})
		if _, ok := o.ReadLearnedHosts(a.Answers(o.LearnedHostsQuery().Query)); ok {
			t.Errorf("ReadLearnedHosts reported a missing set as available")
		}
	})
}

func TestLearnNeedsRestore(t *testing.T) {
	t.Parallel()

	present := `table ip6 nptv6-ndp {
	set learned {
		type ipv6_addr . ifname
		size 512
		flags dynamic,timeout
		timeout 1h
	}

	chain learn {
		type filter hook forward priority filter; policy accept;
		ip6 saddr fd00:dead:beef:1::/64 oifname "wan0" update @learned { ip6 saddr . oifname timeout 1h } comment "rb-ndp wan0 main"
	}
}`

	cases := []struct {
		name string
		out  map[string]string
		want bool
	}{
		{
			name: "the rules are there as configured",
			out:  map[string]string{learnTable: present},
		},
		{
			name: "the table is gone",
			out:  map[string]string{},
			want: true,
		},
		{
			name: "the chain was flushed",
			out: map[string]string{learnTable: `table ip6 nptv6-ndp {
	set learned {
		type ipv6_addr . ifname
	}

	chain learn {
		type filter hook forward priority filter; policy accept;
	}
}`},
			want: true,
		},
		{
			name: "a rule names an uplink the configuration does not",
			out: map[string]string{learnTable: `table ip6 nptv6-ndp {
	chain learn {
		ip6 saddr fd00:dead:beef:1::/64 oifname "wan9" update @learned { ip6 saddr . oifname timeout 1h }
	}
}`},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, a := proxyOptions(t, c.out)
			if got := o.LearnNeedsRestore(o.DesiredLearnRules(), o.ReadInstalledLearn(a.Answers(o.LearnQueries()...))); got != c.want {
				t.Errorf("LearnNeedsRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLearnNeedsRestoreWithNoUplinksConsultsNothing(t *testing.T) {
	t.Parallel()

	a := commandtest.New(map[string]string{learnTable: "table ip6 nptv6-ndp {\n}"})
	o := Options{Table: "nptv6-ndp"}
	inst := o.ReadInstalledLearn(a.Answers(o.LearnQueries()...))

	if o.LearnNeedsRestore(o.DesiredLearnRules(), inst) {
		t.Errorf("LearnNeedsRestore reported drift with proxy NDP switched off")
	}
	if !o.TableExists(inst) {
		t.Errorf("TableExists missed a learning table a previous configuration left behind")
	}
	if got := a.Count(learnTable); got != 1 {
		t.Errorf("the learning table was read %d times, want the one declared read", got)
	}
}

func TestReadNeighbors(t *testing.T) {
	t.Parallel()

	t.Run("only resolved neighbours that are not our own proxies", func(t *testing.T) {
		t.Parallel()
		_, a := proxyOptions(t, map[string]string{
			"ip -6 neigh show dev wan0": "2001:db8:1::1 lladdr aa:bb:cc:dd:ee:ff router REACHABLE\n" +
				"2001:db8:1::10 dev wan0 proxy proto 111\n" +
				"2001:db8:1::20 lladdr 00:11:22:33:44:55 FAILED\n" +
				"2001:db8:1::30  INCOMPLETE\n",
		})

		got := ReadNeighbors("wan0", a.Answers(NeighborsQuery("wan0").Query))
		if len(got) != 1 || !got["2001:db8:1::1"] {
			t.Errorf("got %v, want only the one reachable neighbour", got)
		}
	})

	t.Run("a failed read reads as nobody being out there", func(t *testing.T) {
		t.Parallel()
		_, a := proxyOptions(t, map[string]string{})
		if got := ReadNeighbors("wan0", a.Answers(NeighborsQuery("wan0").Query)); len(got) != 0 {
			t.Errorf("got %v, want an empty set", got)
		}
	})
}

func TestPurgeClaimActions(t *testing.T) {
	t.Parallel()

	batch := []Claim{
		{Entry: ProxyEntry{Addr: "2001:db8:1::20", Uplink: "wan0"}},
		{Entry: ProxyEntry{Addr: "2001:db8:1::10", Uplink: "wan0"}},
	}

	t.Run("only the batch's own stale entries are cleared", func(t *testing.T) {
		t.Parallel()
		o, a := proxyOptions(t, map[string]string{
			"ip -6 neigh show dev wan0": "2001:db8:1::10 lladdr aa:bb:cc:dd:ee:ff REACHABLE\n" +
				"2001:db8:1::20 lladdr aa:bb:cc:dd:ee:00 STALE\n" +
				"2001:db8:1::99 lladdr aa:bb:cc:dd:ee:11 REACHABLE\n",
		})

		acts := o.PurgeClaimActions("wan0", batch, ReadNeighbors("wan0", a.Answers(NeighborsQuery("wan0").Query)))
		want := []string{"2001:db8:1::10", "2001:db8:1::20"}
		if len(acts) != len(want) {
			t.Fatalf("got %d deletes, want %d", len(acts), len(want))
		}
		for i, addr := range want {
			if acts[i].Args[3] != addr {
				t.Errorf("delete %d is for %s, want %s", i, acts[i].Args[3], addr)
			}
			if acts[i].OnFail != command.FailIgnore {
				t.Errorf("delete %d does not tolerate its own failure", i)
			}
		}
	})

	t.Run("a quiet link needs no deletes", func(t *testing.T) {
		t.Parallel()
		o, a := proxyOptions(t, map[string]string{"ip -6 neigh show dev wan0": ""})
		if acts := o.PurgeClaimActions("wan0", batch, ReadNeighbors("wan0", a.Answers(NeighborsQuery("wan0").Query))); len(acts) != 0 {
			t.Errorf("got %d deletes on a link with nothing resolved, want none", len(acts))
		}
	})
}

func TestSolicitAction(t *testing.T) {
	t.Parallel()

	a := SolicitAction("2001:db8:1::10", "wan0", func() {})

	if a.Tool != command.ToolSolicit {
		t.Errorf("tool is %q, want %q", a.Tool, command.ToolSolicit)
	}
	want := []string{"2001:db8:1::10", "dev", "wan0"}
	if len(a.Args) != len(want) {
		t.Fatalf("got %d args, want %d", len(a.Args), len(want))
	}
	for i := range want {
		if a.Args[i] != want[i] {
			t.Errorf("arg %d is %q, want %q", i, a.Args[i], want[i])
		}
	}
	if a.Write == nil {
		t.Error("without Write this action forks a tool named \"solicit\", which does not exist")
	}
	if a.OnFail != command.FailIgnore {
		t.Error("a solicitation nobody answers is the ordinary case, not a failure to warn about")
	}

	r := commandtest.NewRunner()
	r.RunAll([]command.Action{a})

	got := r.Commands()
	if len(got) != 1 || got[0] != "solicit 2001:db8:1::10 dev wan0" {
		t.Errorf("a stubbed runner recorded %v, want one solicit", got)
	}
}

func TestSolicitActionReportsASendThatNeverLeft(t *testing.T) {
	t.Parallel()

	unsent := false
	a := SolicitAction("2001:db8:1::10", "rb-no-such-device", func() { unsent = true })

	if _, err := a.Write(); err == nil {
		t.Fatal("soliciting through a device that does not exist reported success")
	}
	if !unsent {
		t.Error("the send failed and the caller was not told, so the claim will time out " +
			"as unanswered and be reported free")
	}
}
