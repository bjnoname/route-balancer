package reconcile

import (
	"net"
	"time"

	"github.com/bjnoname/route-balancer/internal/firewall"
	"github.com/bjnoname/route-balancer/internal/ndp"
	"github.com/bjnoname/route-balancer/internal/nptv6"
	"github.com/bjnoname/route-balancer/internal/prefix"
	"github.com/bjnoname/route-balancer/internal/route"
)

type State struct {
	Routes    routeState
	Links     linkState
	Prefixes  prefixState
	Health    linkHealth
	Learn     learnState
	Claims    claimState
	Installed installedState
}

type installedState struct {
	Rules map[int]route.Rules

	ECMP map[int]route.InstalledECMP

	Tables map[tableKey]route.TableContents

	Abandoned map[int]bool

	Nft      firewall.InstalledNft
	Nft6     firewall.InstalledNft
	Iptables firewall.InstalledIptables

	Npt   nptv6.InstalledTable
	Learn ndp.InstalledLearn

	Proxy ndp.InstalledProxy
}

type routeState struct {
	Gateways map[string]route.Gateway

	V6SourcesChecked bool
}

type linkState struct {
	Ifaces map[string]int

	Addrs map[int]*net.IPNet

	ProxyNDP map[string]bool

	LocalV6 map[string]bool

	Stale bool
}

type prefixState struct {
	Leases map[string]prefix.Lease

	Withdrawn map[string]bool
}

type linkHealth struct {
	Unhealthy map[healthKey]bool

	Monitors map[healthKey]*monitor
}

type learnState struct {
	Hosts []ndp.LearnedHost

	SetFull bool
}

type inflight struct {
	Kind     ndp.ClaimKind
	Deadline time.Time
}

type claimState struct {
	Conflicts map[ndp.ProxyEntry]time.Time

	Burned map[ndp.ProxyEntry]bool

	Probing map[ndp.ProxyEntry]inflight

	Proved map[ndp.ProxyEntry]bool
}

func newState() State {
	return State{
		Routes: routeState{
			Gateways: map[string]route.Gateway{},
		},
		Links: linkState{
			Ifaces: map[string]int{},
			Addrs:  map[int]*net.IPNet{},
		},
		Prefixes: prefixState{
			Leases:    map[string]prefix.Lease{},
			Withdrawn: map[string]bool{},
		},
		Health: linkHealth{
			Unhealthy: map[healthKey]bool{},
			Monitors:  map[healthKey]*monitor{},
		},
		Claims: claimState{
			Conflicts: map[ndp.ProxyEntry]time.Time{},
			Burned:    map[ndp.ProxyEntry]bool{},
			Probing:   map[ndp.ProxyEntry]inflight{},
			Proved:    map[ndp.ProxyEntry]bool{},
		},
	}
}

func (h linkHealth) healthy(ifName string, family int) bool {
	return !h.Unhealthy[healthKey{IfName: ifName, Family: family}]
}
