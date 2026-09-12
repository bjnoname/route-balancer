package claim

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
	"github.com/bjnoname/route-balancer/internal/ndp"
)

const wan0Neigh = "ip -6 neigh show dev wan0"

type obs struct{ *commandtest.Asker }

func newObs(out map[string]string) obs { return obs{commandtest.New(out)} }

func (o obs) Observe(qs []command.Query) command.Answers {
	return command.Observe(o.Asker, qs)
}

type stubSystem struct {
	*commandtest.Asker
	*commandtest.Runner
}

func claimUplinks() ndp.Options {
	return ndp.Options{Uplinks: []ndp.Uplink{{Name: "wan0"}, {Name: "wan1"}}}
}

func reports(e ndp.ProxyEntry, _ ndp.ClaimKind, contested bool) command.Action {
	outcome := "free"
	if contested {
		outcome = "contested"
	}
	return command.Action{
		Tool: command.ToolClaimVerdict,
		Args: []string{e.Addr, e.Uplink, outcome},
	}
}

func testProber(t *testing.T, out map[string]string) (*prober, obs) {
	t.Helper()

	return newProber(claimUplinks(), make(chan Probe, QueueDepth), reports), newObs(out)
}

func testClaimActor(ctx context.Context, out map[string]string) (*actor.Actor[Probe], *stubSystem, *actor.Mailbox[Probe]) {
	sys := &stubSystem{Asker: commandtest.New(out), Runner: commandtest.NewRunner()}
	in := actor.NewMailbox[Probe](QueueDepth)

	cl := actor.NewWith(ctx, "Claim actor", in, sys, func(self *actor.Actor[Probe]) actor.Behaviour {
		return newProber(claimUplinks(), self.Inbox(), reports)
	})
	actor.Every(ctx, in, pollInterval, func() Probe { return Probe{Poll: true} })

	return cl, sys, in
}

func claimOn(uplink, addr string) ndp.Claim {
	return ndp.Claim{Entry: ndp.ProxyEntry{Addr: addr, Uplink: uplink}}
}

func commands(acts []command.Action) []string {
	r := commandtest.NewRunner()
	r.RunAll(acts)
	return r.Commands()
}

func TestAdmitPurgesBeforeSoliciting(t *testing.T) {
	t.Parallel()

	a, look := testProber(t, map[string]string{
		wan0Neigh: "2001:db8:1::10 lladdr aa:bb:cc:dd:ee:ff STALE\n" +
			"2001:db8:1::99 lladdr aa:bb:cc:dd:ee:11 REACHABLE\n",
	})

	got := commands(a.admit(look, Probe{Claims: []ndp.Claim{
		claimOn("wan0", "2001:db8:1::20"),
		claimOn("wan0", "2001:db8:1::10"),
	}}))

	want := []string{
		"ip -6 neigh del 2001:db8:1::10 dev wan0",
		"solicit 2001:db8:1::10 dev wan0",
		"solicit 2001:db8:1::20 dev wan0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("admit planned\n\t%v\nwant\n\t%v", got, want)
	}
	if len(a.pending) != 2 {
		t.Errorf("%d claims in flight, want 2", len(a.pending))
	}
}

func TestAdmitIgnoresAClaimAlreadyInFlight(t *testing.T) {
	t.Parallel()

	a, look := testProber(t, map[string]string{wan0Neigh: ""})
	batch := Probe{Claims: []ndp.Claim{claimOn("wan0", "2001:db8:1::10")}}

	a.admit(look, batch)
	if got := commands(a.admit(look, batch)); len(got) != 0 {
		t.Errorf("re-admitting an in-flight claim planned %v, want nothing", got)
	}
}

func TestPollRulesOnAContest(t *testing.T) {
	t.Parallel()

	a, look := testProber(t, map[string]string{
		wan0Neigh: "2001:db8:1::10 lladdr aa:bb:cc:dd:ee:ff REACHABLE\n",
	})

	c := claimOn("wan0", "2001:db8:1::10")
	a.pending[c.Entry] = pending{Claim: c, Started: time.Now()}

	got := commands(a.poll(look))
	want := []string{"claim-verdict 2001:db8:1::10 wan0 contested"}
	if !slices.Equal(got, want) {
		t.Errorf("poll planned\n\t%v\nwant\n\t%v", got, want)
	}
	if len(a.pending) != 0 {
		t.Error("a decided claim is still in flight")
	}
}

func TestPollRulesFreeOnlyOnceTheWindowHasRun(t *testing.T) {
	t.Parallel()

	a, look := testProber(t, map[string]string{wan0Neigh: ""})
	c := claimOn("wan0", "2001:db8:1::10")

	a.pending[c.Entry] = pending{Claim: c, Started: time.Now()}
	for range 5 {
		if got := commands(a.poll(look)); len(got) != 0 {
			t.Fatalf("poll ruled %v inside the window, want silence", got)
		}
	}

	a.pending[c.Entry] = pending{Claim: c, Started: time.Now().Add(-ndp.ProbeTimeout)}

	got := commands(a.poll(look))
	want := []string{"claim-verdict 2001:db8:1::10 wan0 free"}
	if !slices.Equal(got, want) {
		t.Errorf("poll planned\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestPollsEveryUplinkInOneTick(t *testing.T) {
	t.Parallel()

	a, look := testProber(t, map[string]string{
		wan0Neigh:                   "",
		"ip -6 neigh show dev wan1": "",
	})

	now := time.Now()
	for _, c := range []ndp.Claim{
		claimOn("wan0", "2001:db8:1::10"),
		claimOn("wan1", "2001:db8:2::10"),
	} {
		a.pending[c.Entry] = pending{Claim: c, Started: now}
	}

	a.poll(look)

	for _, q := range []string{wan0Neigh, "ip -6 neigh show dev wan1"} {
		if got := look.Count(q); got != 1 {
			t.Errorf("%q ran %d times, want 1", q, got)
		}
	}
}

func TestPollIsQuietWithNothingInFlight(t *testing.T) {
	t.Parallel()

	a, look := testProber(t, map[string]string{wan0Neigh: ""})

	if got := commands(a.poll(look)); len(got) != 0 {
		t.Errorf("an idle poll planned %v, want nothing", got)
	}
	if got := look.Reads(); got != 0 {
		t.Errorf("an idle poll read %d times, want none", got)
	}
}

func TestABatchDoesNotPostponeThePoll(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl, sys, in := testClaimActor(ctx, map[string]string{wan0Neigh: ""})
	cl.Start()

	offer := Offer(in)
	batch := []ndp.Claim{claimOn("wan0", "2001:db8:1::10")}

	if _, err := offer(batch).Write(); err != nil {
		t.Fatalf("the first offer failed: %v", err)
	}

	go func() {
		for ctx.Err() == nil {
			_, _ = offer(batch).Write()
			time.Sleep(pollInterval / 5)
		}
	}()

	deadline := time.Now().Add(4 * ndp.ProbeTimeout)
	for time.Now().Before(deadline) {
		if slices.Contains(sys.Commands(), "claim-verdict 2001:db8:1::10 wan0 free") {
			return
		}
		time.Sleep(pollInterval / 2)
	}
	t.Fatal("no verdict was planned: a busy inbox held off the poll")
}

func TestTheLoopStopsWithItsContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	cl, _, _ := testClaimActor(ctx, map[string]string{wan0Neigh: ""})
	cl.Start()

	done := make(chan struct{})
	go func() {
		cl.Wait()
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the actor did not stop when its context was cancelled")
	}
}

func TestTheOfferIsAnActionThatDeliversTheBatch(t *testing.T) {
	t.Parallel()

	in := actor.NewMailbox[Probe](1)
	a := Offer(in)([]ndp.Claim{
		{Entry: ndp.ProxyEntry{Addr: "2001:db8:ff02::20", Uplink: "eth1"}},
		{Entry: ndp.ProxyEntry{Addr: "2001:db8:ff02::10", Uplink: "eth1"}},
	})

	if a.Tool != command.ToolProbeClaim {
		t.Errorf("tool is %q, want %q", a.Tool, command.ToolProbeClaim)
	}
	if len(a.Args) != 2 || a.Args[0] != "2001:db8:ff02::10" || a.Args[1] != "2001:db8:ff02::20" {
		t.Errorf("args are %v, want both addresses in order", a.Args)
	}

	if _, err := a.Write(); err != nil {
		t.Fatalf("the handoff failed against an empty inbox: %v", err)
	}
	if in.Len() != 1 {
		t.Errorf("%d batches landed, want 1", in.Len())
	}
}

func TestAFullInboxFailsTheHandoffRatherThanBlockingTheLoop(t *testing.T) {
	t.Parallel()

	in := actor.NewMailbox[Probe](0)
	a := Offer(in)([]ndp.Claim{{Entry: ndp.ProxyEntry{Addr: "2001:db8:ff02::10", Uplink: "eth1"}}})

	done := make(chan error, 1)
	go func() {
		_, err := a.Write()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a handoff nobody could receive reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handoff blocked on a mailbox nobody was reading")
	}
}

func TestASolicitThatNeverLeftIsNotWaitedOn(t *testing.T) {
	t.Parallel()

	const nowhere = "rb-no-such-device"

	a, look := testProber(t, map[string]string{"ip -6 neigh show dev " + nowhere: ""})
	c := claimOn(nowhere, "2001:db8:1::10")

	acts := a.admit(look, Probe{Claims: []ndp.Claim{c}})
	if len(a.pending) != 1 {
		t.Fatalf("%d claims in flight after admit, want 1", len(a.pending))
	}

	sent := 0
	for _, act := range acts {
		if act.Tool != command.ToolSolicit {
			continue
		}
		sent++
		if _, err := act.Write(); err == nil {
			t.Fatal("soliciting through a device that does not exist reported success")
		}
	}
	if sent != 1 {
		t.Fatalf("admit planned %d solicits, want 1", sent)
	}

	if len(a.pending) != 0 {
		t.Errorf("%d claims still in flight, want none: poll would time this one out and "+
			"report free for an address nobody was ever asked about", len(a.pending))
	}

	if got := commands(a.poll(look)); len(got) != 0 {
		t.Errorf("poll planned %v after a send that never left, want nothing", got)
	}
}
