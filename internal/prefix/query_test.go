package prefix

import (
	"testing"

	"github.com/bjnoname/route-balancer/internal/command/commandtest"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/netlink"
)

const (
	unreachable = "ip -6 route show type unreachable"
	blackhole   = "ip -6 route show type blackhole"
	prohibit    = "ip -6 route show type prohibit"
)

func noRejectRoutes() map[string]string {
	return map[string]string{unreachable: "", blackhole: "", prohibit: ""}
}

func TestKernelObservationsFindsDelegations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		out        map[string]string
		wantPrefix string
		wantLen    int
	}{
		{
			name:       "an unreachable discard route",
			out:        map[string]string{unreachable: "unreachable 2001:db8:100::/56 dev lo metric 1024 pref medium\n"},
			wantPrefix: "2001:db8:100::",
			wantLen:    56,
		},
		{
			name:       "a blackhole discard route",
			out:        map[string]string{blackhole: "blackhole 2001:db8:200::/60 dev lo\n"},
			wantPrefix: "2001:db8:200::",
			wantLen:    60,
		},
		{
			name:       "a prohibit discard route",
			out:        map[string]string{prohibit: "prohibit 2001:db8:300::/64 dev lo\n"},
			wantPrefix: "2001:db8:300::",
			wantLen:    64,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out := noRejectRoutes()
			for k, v := range c.out {
				out[k] = v
			}

			obs := Sources(nil).KernelObservations(commandtest.Recorded(out))
			if len(obs) != 1 {
				t.Fatalf("got %d observations %v, want 1", len(obs), obs)
			}
			if obs[0].Kind != netlink.ObsRoute {
				t.Errorf("kind = %q, want %q", obs[0].Kind, netlink.ObsRoute)
			}
			if obs[0].Prefix.String() != c.wantPrefix || obs[0].Length != c.wantLen {
				t.Errorf("got %s/%d, want %s/%d",
					obs[0].Prefix, obs[0].Length, c.wantPrefix, c.wantLen)
			}
		})
	}
}

func TestKernelObservationsRejectsUnusablePrefixes(t *testing.T) {
	t.Parallel()

	out := noRejectRoutes()
	out[unreachable] = "unreachable 2001:db8:100::/72 dev lo\n" +
		"unreachable default dev lo\n" +
		"unreachable 10.0.0.0/8 dev lo\n"

	if obs := Sources(nil).KernelObservations(commandtest.Recorded(out)); len(obs) != 0 {
		t.Errorf("got %v, want no observations", obs)
	}
}

func TestKernelObservationsSurvivesAFailedRead(t *testing.T) {
	t.Parallel()

	obs := Sources(nil).KernelObservations(commandtest.Recorded(map[string]string{
		prohibit: "prohibit 2001:db8:300::/56 dev lo\n",
	}))
	if len(obs) != 1 || obs[0].Length != 56 {
		t.Fatalf("got %v, want the one delegation the readable listing carried", obs)
	}
}

func TestKernelObservationsReadsRAPrefixesFromAddresses(t *testing.T) {
	t.Parallel()

	const addrShow = "ip -6 -o addr show dev lo scope global dynamic"

	t.Run("the live prefix wins over the one being retired", func(t *testing.T) {
		t.Parallel()
		out := noRejectRoutes()
		out[addrShow] = "1: lo    inet6 2001:db8:aaaa::5/64 scope global dynamic \\       " +
			"valid_lft 6000sec preferred_lft 0sec\n" +
			"1: lo    inet6 2001:db8:bbbb::5/64 scope global dynamic \\       " +
			"valid_lft 900sec preferred_lft 600sec\n"

		obs := Sources{newRASource("lo", nil)}.KernelObservations(commandtest.Recorded(out))
		if len(obs) != 1 {
			t.Fatalf("got %d observations %v, want 1", len(obs), obs)
		}
		if obs[0].Kind != netlink.ObsRA || obs[0].Length != 64 {
			t.Errorf("got %+v, want a /64 RA observation", obs[0])
		}
		if obs[0].Prefix.String() != "2001:db8:bbbb::" {
			t.Errorf("prefix = %s, want the one still being advertised", obs[0].Prefix)
		}
		if obs[0].IfName != "lo" {
			t.Errorf("ifname = %q, want \"lo\" — an RA is attributed by the link it arrived on",
				obs[0].IfName)
		}
	})

	t.Run("an all-deprecated interface yields nothing", func(t *testing.T) {
		t.Parallel()
		out := noRejectRoutes()
		out[addrShow] = "1: lo    inet6 2001:db8:aaaa::5/64 scope global dynamic \\       " +
			"valid_lft 6000sec preferred_lft 0sec\n"

		if obs := (Sources{newRASource("lo", nil)}).KernelObservations(commandtest.Recorded(out)); len(obs) != 0 {
			t.Errorf("got %v, want no observation from a withdrawn prefix", obs)
		}
	})

	t.Run("a proto ra route is not consulted", func(t *testing.T) {
		t.Parallel()
		out := noRejectRoutes()
		out[addrShow] = ""
		ss := Sources{newRASource("lo", nil)}

		if obs := ss.KernelObservations(commandtest.Recorded(out)); len(obs) != 0 {
			t.Fatalf("got %v, want nothing", obs)
		}
		for _, q := range ss.Queries() {
			if q.String() == "ip -6 route show dev lo proto ra" {
				t.Errorf("declared %q — the on-link route is not a second opinion", q)
			}
		}
	})
}

func TestObservedResolvesTheWholeDomain(t *testing.T) {
	t.Parallel()

	sources, err := BuildSources(&config.NPTv6{
		Uplinks: map[string]config.NPTv6Uplink{
			"wan0": {PrefixSource: config.SourceRoute, MatchPrefix: "2001:db8::/32"},
			"wan1": {PrefixSource: config.SourceStatic, StaticPrefix: "2001:db8:fed::/64"},
		},
	})
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}

	t.Run("a delegation is attributed to the uplink whose aggregate holds it", func(t *testing.T) {
		t.Parallel()
		out := noRejectRoutes()
		out[unreachable] = "unreachable 2001:db8:100::/56 dev lo\n"

		leases := sources.Observed(commandtest.Recorded(out), nil)
		if got := leases["wan0"]; got.String() != "2001:db8:100::/56" {
			t.Errorf("wan0 lease = %s, want 2001:db8:100::/56", got)
		}
		if got := leases["wan1"]; got.String() != "2001:db8:fed::/64" {
			t.Errorf("wan1 lease = %s, want its static prefix", got)
		}
	})

	t.Run("a delegation that has gone leaves its uplink absent", func(t *testing.T) {
		t.Parallel()
		leases := sources.Observed(commandtest.Recorded(noRejectRoutes()), nil)
		if _, ok := leases["wan0"]; ok {
			t.Errorf("wan0 still holds %s with no discard route in the kernel", leases["wan0"])
		}
		if _, ok := leases["wan1"]; !ok {
			t.Errorf("the static uplink lost its lease to another uplink's withdrawal")
		}
	})

	t.Run("a delegation outside the aggregate is attributed to nobody", func(t *testing.T) {
		t.Parallel()
		out := noRejectRoutes()
		out[unreachable] = "unreachable 2001:db9:100::/56 dev lo\n"

		if leases := sources.Observed(commandtest.Recorded(out), nil); leases["wan0"].Prefix != nil {
			t.Errorf("wan0 adopted %s, which is outside its match_prefix", leases["wan0"])
		}
	})
}
