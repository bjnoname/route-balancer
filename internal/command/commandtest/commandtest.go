package commandtest

import (
	"fmt"
	"strings"
	"sync"

	"github.com/bjnoname/route-balancer/internal/command"
)

type Asker struct {
	Out map[string]string

	Vals map[string]any

	mu    sync.Mutex
	Asked []string
}

func New(out map[string]string) *Asker { return &Asker{Out: out} }

func (a *Asker) Ask(q command.Query) command.Answer {
	a.mu.Lock()
	a.Asked = append(a.Asked, q.String())
	a.mu.Unlock()

	if v, ok := a.Vals[q.String()]; ok {
		return command.Answer{Val: v}
	}
	out, ok := a.Out[q.String()]
	if !ok {
		return command.Answer{Err: fmt.Errorf("commandtest: no recorded output for %q", q)}
	}
	return command.Answer{Out: out}
}

func (a *Asker) Answers(qs ...command.Query) command.Answers {
	return command.Observe(a, qs)
}

func Recorded(out map[string]string) command.Answers {
	a := command.Answers{}
	for k, v := range out {
		a[k] = command.Answer{Out: v}
	}
	return a
}

func (a *Asker) Count(query string) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	n := 0
	for _, q := range a.Asked {
		if q == query {
			n++
		}
	}
	return n
}

func (a *Asker) Reads() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return int64(len(a.Asked))
}

type Runner struct {
	Fail func(a command.Action) error

	mu  sync.Mutex
	Ran []command.Action
}

func NewRunner() *Runner { return &Runner{} }

func (r *Runner) Run(a command.Action) (string, error) {
	r.mu.Lock()
	r.Ran = append(r.Ran, a)
	r.mu.Unlock()

	if r.Fail != nil {
		return "", r.Fail(a)
	}
	return "", nil
}

func (r *Runner) RunAll(acts []command.Action) {
	for _, a := range command.Ordered(acts) {
		_, _ = r.Run(a)
	}
}

func (r *Runner) Commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.Ran))
	for _, a := range r.Ran {
		out = append(out, strings.Join(append([]string{string(a.Tool)}, a.Args...), " "))
	}
	return out
}
