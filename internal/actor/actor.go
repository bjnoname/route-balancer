package actor

import (
	"context"
	"log/slog"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
)

type system interface {
	command.CountingAsker
	command.Runner
}

type Observer interface {
	Observe(qs []command.Query) command.Answers
}

type Behaviour interface {
	Step(ctx context.Context, look Observer) ([]command.Action, bool)
}

type core struct {
	name string

	stopClock func()

	ctx    context.Context
	cancel context.CancelFunc

	sys system

	body Behaviour

	children []*Child

	done chan struct{}
}

func newCore(ctx context.Context, name string, sys system) *core {
	ctx, cancel := context.WithCancel(ctx)

	return &core{
		name:      name,
		stopClock: func() {},
		ctx:       ctx,
		cancel:    cancel,
		sys:       sys,
		done:      make(chan struct{}),
	}
}

func (a *core) Observe(qs []command.Query) command.Answers {
	return command.Observe(a.sys, qs)
}

func (a *core) Start() { go a.run() }

func (a *core) Stop() { a.cancel() }

func (a *core) Wait() { <-a.done }

func (a *core) run() {
	if a.body == nil {
		panic("actor " + a.name + " was run with no behaviour attached")
	}

	defer close(a.done)
	defer a.reap()
	defer a.cancel()
	defer a.stopClock()

	slog.Debug(a.name + " started")

	for {
		before := a.sys.Reads()

		acts, more := a.body.Step(a.ctx, a)
		a.sys.RunAll(acts)

		slog.Debug("Batch complete", "loop", a.name,
			"kernel_reads", a.sys.Reads()-before, "kernel_writes", len(acts))

		if !more {
			return
		}
	}
}

type Child struct{ *core }

func (a *core) NewChild(name string, every time.Duration, build func(tick <-chan time.Time) Behaviour) *Child {
	c := &Child{core: newCore(a.ctx, name, command.NewSystem())}
	c.body = build(c.clock(every))

	return c
}

func (a *core) clock(every time.Duration) <-chan time.Time {
	if every <= 0 {
		return nil
	}
	t := time.NewTicker(every)
	a.stopClock = t.Stop

	return t.C
}

func (a *core) Adopt(c *Child) {
	live := a.children[:0]
	for _, kid := range a.children {
		select {
		case <-kid.done:
		default:
			live = append(live, kid)
		}
	}
	a.children = append(live, c)

	c.Start()
}

func (a *core) reap() {
	for _, c := range a.children {
		c.Stop()
	}
	for _, c := range a.children {
		c.Wait()
		slog.Debug("Child stopped", "loop", a.name, "child", c.name)
	}
}

type Actor[M any] struct {
	*core

	in *Mailbox[M]
}

func New[M any](ctx context.Context, name string, in *Mailbox[M], build func(*Actor[M]) Behaviour) *Actor[M] {
	return newWith(ctx, name, in, command.NewSystem(), build)
}

func newWith[M any](
	ctx context.Context,
	name string,
	in *Mailbox[M],
	sys system,
	build func(*Actor[M]) Behaviour,
) *Actor[M] {
	a := &Actor[M]{core: newCore(ctx, name, sys), in: in}
	a.body = build(a)

	return a
}

func (a *Actor[M]) Inbox() <-chan M { return a.in.receiver() }

func NewWith[M any](
	ctx context.Context,
	name string,
	in *Mailbox[M],
	sys system,
	build func(*Actor[M]) Behaviour,
) *Actor[M] {
	return newWith(ctx, name, in, sys, build)
}
