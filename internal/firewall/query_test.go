package firewall

import (
	"strings"
	"testing"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
	"github.com/bjnoname/route-balancer/internal/config"
)

const (
	nftIP    = "nft list table ip route-balancer"
	nftINet  = "nft list table inet route-balancer"
	iptShow  = "iptables -t mangle -S ROUTE-BALANCER"
	iptOut   = "iptables -t mangle -C OUTPUT -j ROUTE-BALANCER"
	iptPre   = "iptables -t mangle -C PREROUTING -j ROUTE-BALANCER"
	portRule = 443
)

func twoGatewayOptions(out map[string]string) (Options, *commandtest.Asker) {
	a := commandtest.New(out)
	return Options{
		IptablesChain: "ROUTE-BALANCER",
		NftablesTable: "route-balancer",
		Gateways:      []string{"eth0", "eth1"},
		Rules: []config.Rule{
			{MatchDstPort: []int{portRule}, MatchProtocol: "tcp", Gateway: "eth1"},
		},
	}, a
}

const nftBothChains = `table ip route-balancer {
	chain mangle_output {
		type filter hook output priority mangle; policy accept;
		tcp dport 443 meta mark set 0x00000002
	}

	chain mangle_prerouting {
		type filter hook forward priority mangle; policy accept;
		tcp dport 443 meta mark set 0x00000002
	}
}`

func TestNftNeedsRestore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		out  map[string]string
		want bool
	}{
		{
			name: "both chains stamp what the port rule asks for",
			out:  map[string]string{nftIP: nftBothChains},
		},
		{
			name: "one chain was flushed on its own",
			out: map[string]string{nftIP: `table ip route-balancer {
	chain mangle_output {
		type filter hook output priority mangle; policy accept;
		tcp dport 443 meta mark set 0x00000002
	}

	chain mangle_prerouting {
		type filter hook forward priority mangle; policy accept;
	}
}`},
			want: true,
		},
		{
			name: "the whole table is gone",
			out:  map[string]string{},
			want: true,
		},
		{
			name: "the legacy inet table is still there",
			out:  map[string]string{nftIP: nftBothChains, nftINet: "table inet route-balancer {\n}"},
			want: true,
		},
		{
			name: "the mark points at the wrong gateway",
			out: map[string]string{nftIP: `table ip route-balancer {
	chain mangle_output {
		tcp dport 443 meta mark set 0x00000001
	}
	chain mangle_prerouting {
		tcp dport 443 meta mark set 0x00000001
	}
}`},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, a := twoGatewayOptions(c.out)
			if got := o.NftNeedsRestore(o.DesiredMarkRules(), o.ReadInstalledNft(a.Answers(o.NftQueries()...))); got != c.want {
				t.Errorf("NftNeedsRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNftNeedsRestoreWithNothingToStamp(t *testing.T) {
	t.Parallel()

	t.Run("no table, nothing wanted", func(t *testing.T) {
		t.Parallel()
		a := commandtest.New(map[string]string{})
		o := Options{NftablesTable: "route-balancer"}
		if o.NftNeedsRestore(nil, o.ReadInstalledNft(a.Answers(o.NftQueries()...))) {
			t.Errorf("NftNeedsRestore reported drift with no rules wanted and no table present")
		}
	})

	t.Run("a table left over from a config that had rules", func(t *testing.T) {
		t.Parallel()
		a := commandtest.New(map[string]string{nftIP: nftBothChains})
		o := Options{NftablesTable: "route-balancer"}
		if !o.NftNeedsRestore(nil, o.ReadInstalledNft(a.Answers(o.NftQueries()...))) {
			t.Errorf("NftNeedsRestore accepted a table that should not exist")
		}
	})
}

const iptChain = `-N ROUTE-BALANCER
-A ROUTE-BALANCER -p tcp -m tcp --dport 443 -j MARK --set-xmark 0x2/0xffffffff`

func TestIptablesNeedsRestore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		out  map[string]string
		want bool
	}{
		{
			name: "the chain stamps the mark and is reached from both hooks",
			out:  map[string]string{iptShow: iptChain, iptOut: "", iptPre: ""},
		},
		{
			name: "the OUTPUT jump was deleted",
			out:  map[string]string{iptShow: iptChain, iptPre: ""},
			want: true,
		},
		{
			name: "the PREROUTING jump was deleted",
			out:  map[string]string{iptShow: iptChain, iptOut: ""},
			want: true,
		},
		{
			name: "the chain was flushed but still exists",
			out:  map[string]string{iptShow: "-N ROUTE-BALANCER", iptOut: "", iptPre: ""},
			want: true,
		},
		{
			name: "there is no such chain",
			out:  map[string]string{},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, a := twoGatewayOptions(c.out)
			if got := o.IptablesNeedsRestore(o.DesiredMarkRules(), o.ReadInstalledIptables(o.DesiredMarkRules(), a.Answers(o.IptablesQueries(o.DesiredMarkRules())...))); got != c.want {
				t.Errorf("IptablesNeedsRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestEveryFirewallReadIsDeclared(t *testing.T) {
	t.Parallel()

	t.Run("nftables", func(t *testing.T) {
		t.Parallel()
		o, a := twoGatewayOptions(map[string]string{nftIP: nftBothChains})
		if o.NftNeedsRestore(o.DesiredMarkRules(), o.ReadInstalledNft(a.Answers(o.NftQueries()...))) {
			t.Error("an in-sync nft table reported drift, which is what an undeclared read looks like")
		}
		assertAskedExactly(t, a, o.NftQueries())
	})

	t.Run("iptables", func(t *testing.T) {
		t.Parallel()
		o, a := twoGatewayOptions(map[string]string{iptShow: iptChain, iptOut: "", iptPre: ""})
		if o.IptablesNeedsRestore(o.DesiredMarkRules(), o.ReadInstalledIptables(o.DesiredMarkRules(), a.Answers(o.IptablesQueries(o.DesiredMarkRules())...))) {
			t.Error("an in-sync iptables chain reported drift, which is what an undeclared read looks like")
		}
		assertAskedExactly(t, a, o.IptablesQueries(o.DesiredMarkRules()))
	})
}

func assertAskedExactly(t *testing.T, a *commandtest.Asker, want []command.Query) {
	t.Helper()
	if len(a.Asked) != len(want) {
		t.Fatalf("asked %v, declared %v", a.Asked, want)
	}
	for i, q := range want {
		if a.Asked[i] != q.String() {
			t.Errorf("read %d was %q, declared %q", i, a.Asked[i], q)
		}
	}
}

func TestIptablesNeedsRestoreAcceptsWhatIptablesRewroteItTo(t *testing.T) {
	t.Parallel()

	a := commandtest.New(map[string]string{
		iptShow: "-N ROUTE-BALANCER\n" +
			"-A ROUTE-BALANCER -p tcp -m multiport --dports 443,8443 -j MARK --set-xmark 0x1/0xffffffff\n" +
			"-A ROUTE-BALANCER -p udp -m multiport --dports 443,8443 -j MARK --set-xmark 0x1/0xffffffff",
		iptOut: "", iptPre: "",
	})
	o := Options{
		IptablesChain: "ROUTE-BALANCER", Gateways: []string{"eth0"},
		Rules: []config.Rule{{MatchDstPort: []int{443, 8443}, Gateway: "eth0"}},
	}

	if o.IptablesNeedsRestore(o.DesiredMarkRules(), o.ReadInstalledIptables(o.DesiredMarkRules(), a.Answers(o.IptablesQueries(o.DesiredMarkRules())...))) {
		t.Errorf("IptablesNeedsRestore reported drift on rules iptables merely reprinted")
	}
}

func TestNftNeedsRestoreIsOrderSensitive(t *testing.T) {
	t.Parallel()

	a := commandtest.New(map[string]string{nftIP: `table ip route-balancer {
	chain mangle_output {
		udp dport 443 meta mark set 0x00000001
		tcp dport 443 meta mark set 0x00000001
	}
	chain mangle_prerouting {
		udp dport 443 meta mark set 0x00000001
		tcp dport 443 meta mark set 0x00000001
	}
}`})
	o := Options{
		NftablesTable: "route-balancer", Gateways: []string{"eth0"},
		Rules: []config.Rule{{MatchDstPort: []int{443}, Gateway: "eth0"}},
	}

	if !o.NftNeedsRestore(o.DesiredMarkRules(), o.ReadInstalledNft(a.Answers(o.NftQueries()...))) {
		t.Errorf("NftNeedsRestore accepted the wanted rules in the wrong order")
	}
}

const nft6 = "nft list table ip6 route-balancer"

const nft6Installed = `table ip6 route-balancer {
	chain mangle_prerouting {
		type filter hook prerouting priority mangle; policy accept;
		ip6 saddr fd00:beef:1::/64 tcp dport 443 meta mark set 0x00000002
		ip6 saddr fd00:beef:2::/64 tcp dport 443 meta mark set 0x00000002
	}
}`

func TestNft6NeedsRestore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		out    map[string]string
		mapped []MappedSubnet
		want   bool
	}{
		{
			name:   "the table steers both mapped subnets",
			out:    map[string]string{nft6: nft6Installed},
			mapped: mapped,
		},
		{
			name:   "the table is gone",
			mapped: mapped,
			want:   true,
		},
		{

			name: "nothing is translated any more, and the table is still there",
			out:  map[string]string{nft6: nft6Installed},
			want: true,
		},
		{
			name:   "nothing is translated and there is no table",
			mapped: nil,
		},
		{
			name: "a subnet stopped being mapped",
			out:  map[string]string{nft6: nft6Installed},
			mapped: []MappedSubnet{
				{Uplink: "eth1", Prefix: "fd00:beef:1::/64"},
			},
			want: true,
		},
		{
			name: "the mark points at the wrong uplink",
			out: map[string]string{nft6: `table ip6 route-balancer {
	chain mangle_prerouting {
		ip6 saddr fd00:beef:1::/64 tcp dport 443 meta mark set 0x00000001
		ip6 saddr fd00:beef:2::/64 tcp dport 443 meta mark set 0x00000001
	}
}`},
			mapped: mapped,
			want:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := opts6(v6Rule("eth1", portRule))
			a := commandtest.New(c.out)
			got := o.Nft6NeedsRestore(
				o.DesiredMarkRules6(c.mapped),
				o.ReadInstalledNft6(a.Answers(o.Nft6Queries()...)),
			)
			if got != c.want {
				t.Errorf("Nft6NeedsRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestEveryIPv6FirewallReadIsDeclared(t *testing.T) {
	t.Parallel()

	o := opts6(v6Rule("eth1", portRule))
	a := commandtest.New(map[string]string{nft6: nft6Installed})
	if o.Nft6NeedsRestore(o.DesiredMarkRules6(mapped), o.ReadInstalledNft6(a.Answers(o.Nft6Queries()...))) {
		t.Error("an in-sync ip6 table reported drift, which is what an undeclared read looks like")
	}
	assertAskedExactly(t, a, o.Nft6Queries())
}

func TestTheIPv4CleanupLeavesTheIPv6TableAlone(t *testing.T) {
	t.Parallel()

	for _, a := range opts6(v6Rule("eth1", portRule)).CleanupNftActions() {
		if strings.Join(a.Args, " ") == "delete table ip6 route-balancer" {
			t.Error("the IPv4 cleanup deletes the IPv6 mangle table")
		}
	}
}
