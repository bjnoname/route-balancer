package nft

import (
	"fmt"

	"github.com/bjnoname/route-balancer/internal/command"
)

type Table struct {
	Family string
	Name   string

	ApplyOrder  int
	RemoveOrder int
}

func (t Table) args(verb ...string) []string {
	return append(verb, t.Family, t.Name)
}

func Query[T any](t Table, parse func(string) T) command.Typed[T] {
	return command.NewTyped(
		command.Query{Tool: command.ToolNFT, Args: t.args("list", "table")},
		parse,
	)
}

type Installed[T any] struct {
	Present bool
	Rules   T
}

func Read[T any](t Table, parse func(string) T, a command.Answers) Installed[T] {
	rules, err := Query(t, parse).Value(a)
	if err != nil {
		return Installed[T]{}
	}
	return Installed[T]{Present: true, Rules: rules}
}

func Write(t Table, ruleset string) command.Action {
	return command.Action{
		Tool:   command.ToolNFT,
		Args:   []string{"-f", "-"},
		Stdin:  ruleset,
		OnFail: command.FailWarn,
		Order:  t.ApplyOrder,
	}
}

func Delete(t Table) command.Action {
	return command.Action{
		Tool:   command.ToolNFT,
		Args:   t.args("delete", "table"),
		OnFail: command.FailIgnore,
		Order:  t.RemoveOrder,
	}
}

func Apply(t Table, ruleset string) []command.Action {
	if ruleset == "" {
		return []command.Action{Delete(t)}
	}
	return []command.Action{Write(t, ruleset)}
}

func Ruleset(t Table, body string) string {
	return fmt.Sprintf("table %s %s { }\ndelete table %s %s\ntable %s %s {\n%s}\n",
		t.Family, t.Name, t.Family, t.Name, t.Family, t.Name, body)
}

func SameSet[K comparable](want, got map[K]struct{}) bool {
	if len(want) != len(got) {
		return false
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			return false
		}
	}
	return true
}
