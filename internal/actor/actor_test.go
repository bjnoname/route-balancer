package actor

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
	"github.com/bjnoname/route-balancer/internal/command/commandtest"
)

type stubSystem struct {
	*commandtest.Asker
	*commandtest.Runner
}

func newStubSystem(out map[string]string) *stubSystem {
	return &stubSystem{Asker: commandtest.New(out), Runner: commandtest.NewRunner()}
}

type scriptedStep struct {
	acts []command.Action
	more bool
}

type scriptedBehaviour struct {
	script []scriptedStep
	taken  int
}

func (b *scriptedBehaviour) Step(context.Context, Observer) ([]command.Action, bool) {
	s := b.script[b.taken]
	b.taken++
	return s.acts, s.more
}

func says(tool string, more bool) scriptedStep {
	return scriptedStep{acts: []command.Action{{Tool: command.Tool(tool)}}, more: more}
}

func scriptedActor(steps ...scriptedStep) (*Actor[struct{}], *scriptedBehaviour, *stubSystem) {
	sys := newStubSystem(nil)
	b := &scriptedBehaviour{script: steps}

	a := newWith(context.Background(), "Scripted actor", NewMailbox[struct{}](0), sys,
		func(*Actor[struct{}]) Behaviour { return b })

	return a, b, sys
}

func TestActorRunsTheFinalPlanBeforeStopping(t *testing.T) {
	t.Parallel()

	a, _, sys := scriptedActor(says("first", true), says("cleanup", false))

	a.run()

	if got, want := sys.Commands(), []string{"first", "cleanup"}; !slices.Equal(got, want) {
		t.Errorf("the actor ran %v, want %v", got, want)
	}
}

func TestActorAsksForNothingAfterFalse(t *testing.T) {
	t.Parallel()

	a, b, _ := scriptedActor(says("last", false))

	a.run()

	if b.taken != 1 {
		t.Errorf("the actor took %d steps, want 1", b.taken)
	}
}

func TestActorToleratesAnEmptyPlan(t *testing.T) {
	t.Parallel()

	a, _, sys := scriptedActor(scriptedStep{acts: nil, more: false})

	a.run()

	if got := sys.Commands(); len(got) != 0 {
		t.Errorf("the actor ran %v on an empty plan, want nothing", got)
	}
}

func TestWaitReleasesOnlyAfterTheFinalBatchHasRun(t *testing.T) {
	t.Parallel()

	a, _, sys := scriptedActor(says("cleanup", false))

	a.Start()

	done := make(chan struct{})
	go func() {
		a.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not release when the actor finished")
	}

	if got, want := sys.Commands(), []string{"cleanup"}; !slices.Equal(got, want) {
		t.Errorf("the actor had run %v by the time wait released, want %v", got, want)
	}
}

type waitingBehaviour struct{ linger time.Duration }

func (b *waitingBehaviour) Step(ctx context.Context, _ Observer) ([]command.Action, bool) {
	<-ctx.Done()
	time.Sleep(b.linger)
	return []command.Action{{Tool: command.Tool("child-cleanup")}}, false
}

func childActor(b Behaviour) (*Child, *stubSystem) {
	sys := newStubSystem(nil)

	c := &Child{core: newCore(context.Background(), "Child", sys)}
	c.body = b

	return c, sys
}

func adopts(parent *core, c *Child) command.Action {
	return command.Action{
		Tool:  command.Tool("adopt"),
		Write: func() (string, error) { parent.Adopt(c); return "", nil },
	}
}

func TestAParentStopsAndJoinsItsChildren(t *testing.T) {
	t.Parallel()

	kid, kidSys := childActor(&waitingBehaviour{linger: 50 * time.Millisecond})

	parent := newCore(context.Background(), "Parent", command.NewSystem())
	parent.body = &scriptedBehaviour{script: []scriptedStep{
		{acts: []command.Action{adopts(parent, kid)}, more: true},
		{acts: nil, more: false},
	}}

	parent.run()

	if got, want := kidSys.Commands(), []string{"child-cleanup"}; !slices.Equal(got, want) {
		t.Errorf("by the time the parent finished, the child had run %v, want %v", got, want)
	}
}

func TestAdoptDropsChildrenThatHaveFinished(t *testing.T) {
	t.Parallel()

	parent := newCore(context.Background(), "Parent", newStubSystem(nil))

	finished, _ := childActor(&scriptedBehaviour{script: []scriptedStep{says("once", false)}})
	parent.Adopt(finished)
	finished.Wait()

	running, _ := childActor(&waitingBehaviour{})
	parent.Adopt(running)
	t.Cleanup(func() { running.Stop(); running.Wait() })

	if len(parent.children) != 1 || parent.children[0] != running {
		t.Errorf("the parent holds %d children, want only the one still running", len(parent.children))
	}
}
