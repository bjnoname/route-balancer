package command

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync/atomic"
)

type Query struct {
	Tool Tool
	Args []string

	Read func() (any, error)
}

func (q Query) String() string {
	return strings.Join(append([]string{string(q.Tool)}, q.Args...), " ")
}

type Asker interface {
	Ask(q Query) Answer
}

type CountingAsker interface {
	Asker
	Reads() int64
}

type System struct{ n atomic.Int64 }

func NewSystem() *System { return &System{} }

func (s *System) Ask(q Query) Answer {
	s.n.Add(1)

	if q.Read != nil {
		return AnswerOf(q.Read())
	}

	out, err := exec.Command(string(q.Tool), q.Args...).Output()
	return Answer{Out: string(out), Err: err}
}

func (s *System) Reads() int64 { return s.n.Load() }

var ErrNotAsked = errors.New("query was not declared for this observation")

var ErrWrongType = errors.New("query answered with a different type than the reader expected")

type Answer struct {
	Out string

	Val any

	Err error
}

func AnswerOf(v any, err error) Answer {
	if s, ok := v.(string); ok {
		return Answer{Out: s, Err: err}
	}
	return Answer{Val: v, Err: err}
}

type Answers map[string]Answer

func (a Answers) Get(q Query) Answer {
	ans, ok := a[q.String()]
	if !ok {
		slog.Error("Read was not declared for this observation", "query", q.String())
		return Answer{Err: fmt.Errorf("%w: %s", ErrNotAsked, q)}
	}
	return ans
}

func (a Answers) OK(q Query) bool { return a.Get(q).Err == nil }

func ValueOf[T any](a Answers, q Query) (T, error) {
	var zero T

	ans := a.Get(q)
	if ans.Err != nil {
		return zero, ans.Err
	}
	v, ok := ans.Val.(T)
	if !ok {
		return zero, fmt.Errorf("%w: %s answered with %T, want %T", ErrWrongType, q, ans.Val, zero)
	}
	return v, nil
}

type Typed[T any] struct {
	Query

	parse func(string) T
}

func NewTyped[T any](q Query, parse func(string) T) Typed[T] {
	return Typed[T]{Query: q, parse: parse}
}

func (q Typed[T]) Value(a Answers) (T, error) { return q.ValueOf(a.Get(q.Query)) }

func (q Typed[T]) ValueOf(ans Answer) (T, error) {
	var zero T

	if ans.Err != nil {
		return zero, ans.Err
	}

	if ans.Val != nil {
		v, ok := ans.Val.(T)
		if !ok {
			return zero, fmt.Errorf("%w: %s answered with %T, want %T", ErrWrongType, q, ans.Val, zero)
		}
		return v, nil
	}

	if q.parse == nil {
		return zero, fmt.Errorf("%w: %s has no parse", ErrWrongType, q)
	}
	return q.parse(ans.Out), nil
}

func (q Typed[T]) ValueOr(a Answers, fallback T) T {
	v, err := q.Value(a)
	if err != nil {
		return fallback
	}
	return v
}

func Observe(ask Asker, qs []Query) Answers {
	out := make(Answers, len(qs))
	for _, q := range qs {
		key := q.String()
		if _, done := out[key]; done {
			continue
		}
		out[key] = ask.Ask(q)
	}
	return out
}
