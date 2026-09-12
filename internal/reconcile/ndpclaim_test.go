package reconcile

import (
	"slices"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/ndp"
)

func probing(kind ndp.ClaimKind) inflight {
	return inflight{Kind: kind, Deadline: time.Now().Add(claimGiveUp)}
}

func (d *testReconciler) planFor(entries ...ndp.ProxyEntry) []ndp.Claim {
	claims, _, _ := planClaims(d.state.Claims, entries, nil, time.Now())
	return claims
}

func kindOf(claims []ndp.Claim, e ndp.ProxyEntry) (ndp.ClaimKind, bool) {
	for _, c := range claims {
		if c.Entry == e {
			return c.Kind, true
		}
	}
	return 0, false
}

func TestPlanClaimsSpeculatesOnlyOnCleanAddresses(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	fresh := ndp.ProxyEntry{Addr: "2001:db8:ff02::10", Uplink: "eth1"}
	burned := ndp.ProxyEntry{Addr: "2001:db8:ff02::1", Uplink: "eth1"}

	d.recordConflict(burned, "test")

	claims := d.planFor(fresh, burned)

	if kind, planned := kindOf(claims, fresh); !planned || kind != ndp.ClaimSpeculative {
		t.Errorf("clean address: kind = %v, planned = %v, want speculative", kind, planned)
	}
	if _, planned := kindOf(claims, burned); planned {
		t.Error("an address refused a moment ago was probed again immediately")
	}

	d.clearConflict(burned)
	if kind, planned := kindOf(d.planFor(burned), burned); !planned || kind != ndp.ClaimScreened {
		t.Errorf("burned address: kind = %v, planned = %v, want screened", kind, planned)
	}
}

func TestPlanClaimsRefusesAnAddressThisHostHolds(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	mine := ndp.ProxyEntry{Addr: "2001:db8:ff02::5", Uplink: "eth1"}
	theirs := ndp.ProxyEntry{Addr: "2001:db8:ff02::6", Uplink: "eth1"}

	claims, _, refused := planClaims(d.state.Claims, []ndp.ProxyEntry{mine, theirs},
		map[string]bool{mine.Addr: true}, time.Now())

	if _, planned := kindOf(claims, mine); planned {
		t.Error("an address configured on this host was queued for a claim")
	}
	if _, planned := kindOf(claims, theirs); !planned {
		t.Error("a clean address was not queued alongside the refused one")
	}
	if len(refused) != 1 || refused[0].Entry != mine || refused[0].Why == "" {
		t.Fatalf("refused = %v, want exactly %v with a reason", refused, mine)
	}
	if d.state.Claims.isBurned(mine) {
		t.Error("planClaims burned an address itself; recording is the caller's")
	}
}

func TestConflictSuppressionLapsesWithTheWindow(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	e := ndp.ProxyEntry{Addr: "2001:db8:ff02::1", Uplink: "eth1"}
	d.recordConflict(e, "test")
	at := d.state.Claims.Conflicts[e]

	if !d.state.Claims.conflictRemembered(e, at.Add(proxyConflictRecheck-time.Second)) {
		t.Error("the refusal was forgotten inside its own window, so the link is re-probed on every poll")
	}
	if d.state.Claims.conflictRemembered(e, at.Add(proxyConflictRecheck+time.Second)) {
		t.Error("the refusal outlived its window, so a conflict that cleared would never be noticed")
	}
	if kind, planned := kindOf(
		func() []ndp.Claim {
			c, _, _ := planClaims(d.state.Claims, []ndp.ProxyEntry{e}, nil, at.Add(proxyConflictRecheck+time.Second))
			return c
		}(), e); !planned || kind != ndp.ClaimScreened {
		t.Errorf("after the window: kind = %v, planned = %v, want screened", kind, planned)
	}
}

func TestBurnIsPermanent(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	e := ndp.ProxyEntry{Addr: "2001:db8:ff02::1", Uplink: "eth1"}
	d.recordConflict(e, "test")

	d.clearConflict(e)
	if d.state.Claims.conflictRemembered(e, time.Now()) {
		t.Error("clearConflict left the re-probe suppression in place")
	}

	if !d.state.Claims.isBurned(e) {
		t.Fatal("clearConflict un-burned the address; it would be speculated on again")
	}
	if kind, planned := kindOf(d.planFor(e), e); !planned || kind != ndp.ClaimScreened {
		t.Error("a previously contested address was queued for a speculative claim")
	}
}

func TestBurnSurvivesRenumbering(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	old := ndp.ProxyEntry{Addr: "2001:db8:ff02::1", Uplink: "eth1"}
	renumbered := ndp.ProxyEntry{Addr: "2001:db8:ff09::1", Uplink: "eth1"}
	d.recordConflict(old, "test")

	if !d.state.Claims.isBurned(renumbered) {
		t.Error("the conflict history was lost across a renumber")
	}
	if kind, planned := kindOf(d.planFor(renumbered), renumbered); !planned || kind != ndp.ClaimScreened {
		t.Error("the renumbered address was queued for a speculative claim")
	}

	for _, e := range []ndp.ProxyEntry{
		{Addr: "2001:db8:ff09::99", Uplink: "eth1"},
		{Addr: "2001:db8:ff09::1", Uplink: "eth3"},
	} {
		if d.state.Claims.isBurned(e) {
			t.Errorf("%v was burned by an unrelated conflict", e)
		}
	}
}

func TestClaimIsNotProbedTwiceAtOnce(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	e := ndp.ProxyEntry{Addr: "2001:db8:ff02::10", Uplink: "eth1"}
	if kind, planned := kindOf(d.planFor(e), e); !planned || kind != ndp.ClaimSpeculative {
		t.Fatalf("first plan: kind = %v, planned = %v, want speculative", kind, planned)
	}
	d.state.Claims.Probing[e] = probing(ndp.ClaimSpeculative)

	if _, planned := kindOf(d.planFor(e), e); planned {
		t.Error("a second probe was planned while one was outstanding")
	}

	d.handleClaimVerified(ClaimVerified{Entry: e, Kind: ndp.ClaimSpeculative})
	if _, planned := kindOf(d.planFor(e), e); !planned {
		t.Error("the address stayed blocked after its probe finished")
	}
}

func TestContestedClaimIsBurnedByTheVerdict(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	e := ndp.ProxyEntry{Addr: "2001:db8:ff02::1", Uplink: "eth1"}
	d.state.Claims.Probing[e] = probing(ndp.ClaimScreened)

	d.handleClaimVerified(ClaimVerified{Entry: e, Kind: ndp.ClaimScreened, Contested: true})

	if _, outstanding := d.state.Claims.Probing[e]; outstanding {
		t.Error("the claim was left outstanding after its verdict")
	}
	if !d.state.Claims.isBurned(e) {
		t.Error("a contested address was not burned, so it would be claimed on spec next time")
	}
	if !d.state.Claims.conflictRemembered(e, time.Now()) {
		t.Error("the refusal was not remembered, so the link would be re-probed on the next poll")
	}
}

func TestVerdictForAForgottenClaimIsDiscarded(t *testing.T) {
	t.Parallel()
	d := newTestReconciler(t)

	e := ndp.ProxyEntry{Addr: "2001:db8:ff02::1", Uplink: "eth1"}

	d.handleClaimVerified(ClaimVerified{Entry: e, Kind: ndp.ClaimScreened, Contested: true})

	if d.state.Claims.isBurned(e) {
		t.Error("a verdict for a claim that was no longer outstanding was applied")
	}
}

func TestAClaimWithNoVerdictIsSweptSoItCanBeProbedAgain(t *testing.T) {
	t.Parallel()

	d := newTestReconciler(t)
	e := ndp.ProxyEntry{Addr: "2001:db8:ff02::10", Uplink: "eth1"}

	now := time.Now()
	d.state.Claims.Probing[e] = inflight{Kind: ndp.ClaimSpeculative, Deadline: now.Add(claimGiveUp)}

	d.sweepInFlightClaims(now)
	if _, outstanding := d.state.Claims.Probing[e]; !outstanding {
		t.Fatal("a claim still inside its window was swept")
	}

	d.sweepInFlightClaims(now.Add(claimGiveUp))

	if _, outstanding := d.state.Claims.Probing[e]; outstanding {
		t.Fatal("a claim whose verdict never came is still in flight")
	}
	if got := d.planFor(e); len(got) != 1 {
		t.Errorf("after the sweep the entry was offered %d times, want it probed again", len(got))
	}
}

func TestAVerdictIsAnActionThatDeliversTheEvent(t *testing.T) {
	t.Parallel()

	events := actor.NewMailbox[Event](1)
	e := ndp.ProxyEntry{Addr: "2001:db8:ff02::10", Uplink: "eth1"}

	a := Verdicts(events)(e, ndp.ClaimScreened, true)

	if a.Tool != command.ToolClaimVerdict {
		t.Errorf("tool is %q, want %q", a.Tool, command.ToolClaimVerdict)
	}
	if got, want := a.Args, []string{e.Addr, e.Uplink, "contested"}; !slices.Equal(got, want) {
		t.Errorf("args are %v, want %v", got, want)
	}
	if _, err := a.Write(); err != nil {
		t.Fatalf("delivering the verdict: %v", err)
	}
	if events.Len() != 1 {
		t.Fatalf("%d verdicts landed, want 1", events.Len())
	}
}
