package command

import (
	"errors"
	"strings"
	"testing"
)

type countingAsker struct {
	out   map[string]string
	asked []string

	stub bool
}

func (c *countingAsker) Ask(q Query) Answer {
	c.asked = append(c.asked, q.String())

	if q.Read != nil && !c.stub {
		return AnswerOf(q.Read())
	}
	out, ok := c.out[q.String()]
	if !ok {
		return Answer{Err: errors.New("no recorded output")}
	}
	return Answer{Out: out}
}

func (c *countingAsker) count(q string) int {
	n := 0
	for _, asked := range c.asked {
		if asked == q {
			n++
		}
	}
	return n
}

func ipRuleShow() Query { return Query{Tool: ToolIP, Args: []string{"-4", "rule", "show"}} }
func nftList() Query    { return Query{Tool: ToolNFT, Args: []string{"list", "table", "ip6", "t"}} }

func TestObserveRunsEachDistinctQueryOnce(t *testing.T) {
	t.Parallel()

	ask := &countingAsker{out: map[string]string{
		ipRuleShow().String(): "0:\tfrom all lookup local",
	}}

	ans := Observe(ask, []Query{ipRuleShow(), nftList(), ipRuleShow(), ipRuleShow()})

	if got := ask.count(ipRuleShow().String()); got != 1 {
		t.Errorf("policy database read %d times, want 1", got)
	}
	if got := len(ans); got != 2 {
		t.Errorf("recorded %d answers, want 2", got)
	}
	if got := ans.Get(ipRuleShow()).Out; got != "0:\tfrom all lookup local" {
		t.Errorf("Get returned %q", got)
	}
}

func TestAFailedReadKeepsItsError(t *testing.T) {
	t.Parallel()

	ans := Observe(&countingAsker{}, []Query{nftList()})

	if ans.Get(nftList()).Err == nil {
		t.Fatal("a failed read came back without an error")
	}
	if ans.OK(nftList()) {
		t.Error("OK said a failed read succeeded")
	}
	if errors.Is(ans.Get(nftList()).Err, ErrNotAsked) {
		t.Error("a declared query that failed reads as never having been asked")
	}
}

func TestAnUndeclaredQueryIsNotAsked(t *testing.T) {
	t.Parallel()

	ans := Observe(&countingAsker{out: map[string]string{nftList().String(): ""}}, []Query{nftList()})

	got := ans.Get(ipRuleShow())
	if !errors.Is(got.Err, ErrNotAsked) {
		t.Errorf("an undeclared query returned %v, want ErrNotAsked", got.Err)
	}
	if ans.OK(ipRuleShow()) {
		t.Error("OK said an undeclared query succeeded")
	}
}

type link struct {
	Index int
	Name  string
}

func linksQuery(runs *int, links []link) Query {
	return Query{
		Tool: ToolNetlink,
		Args: []string{"link", "show"},
		Read: func() (any, error) {
			*runs++
			return links, nil
		},
	}
}

func TestATextReadIsIndistinguishableFromHavingRunTheTool(t *testing.T) {
	t.Parallel()

	q := Query{
		Tool: ToolSysctl,
		Args: []string{"net.ipv6.conf.eth0.proxy_ndp"},
		Read: func() (any, error) { return "1", nil },
	}

	ans := Observe(&countingAsker{}, []Query{q})

	if got := ans.Get(q).Out; got != "1" {
		t.Errorf("an in-process read landed in Out as %q, want %q", got, "1")
	}
	if !ans.OK(q) {
		t.Error("an in-process read that succeeded reads as failed")
	}
}

func TestAStructuredReadIsHandedBackWithItsType(t *testing.T) {
	t.Parallel()

	runs := 0
	want := []link{{Index: 2, Name: "eth0"}, {Index: 3, Name: "eth1"}}
	q := linksQuery(&runs, want)

	ans := Observe(&countingAsker{}, []Query{q})

	got, err := ValueOf[[]link](ans, q)
	if err != nil {
		t.Fatalf("ValueOf: %v", err)
	}
	if len(got) != 2 || got[0].Name != "eth0" || got[1].Index != 3 {
		t.Errorf("the read came back as %v, want %v", got, want)
	}
}

func TestAStructuredReadAssertedAsTheWrongTypeIsAnError(t *testing.T) {
	t.Parallel()

	runs := 0
	q := linksQuery(&runs, nil)
	ans := Observe(&countingAsker{}, []Query{q})

	if _, err := ValueOf[string](ans, q); !errors.Is(err, ErrWrongType) {
		t.Errorf("asserting the wrong type returned %v, want ErrWrongType", err)
	}
}

func TestAFailedInProcessReadKeepsItsError(t *testing.T) {
	t.Parallel()

	boom := errors.New("netlink: no buffer space")
	q := Query{
		Tool: ToolNetlink,
		Args: []string{"addr", "show"},
		Read: func() (any, error) { return nil, boom },
	}

	ans := Observe(&countingAsker{}, []Query{q})

	if !errors.Is(ans.Get(q).Err, boom) {
		t.Errorf("a failed in-process read came back with %v, want the read's own error", ans.Get(q).Err)
	}
	if _, err := ValueOf[[]link](ans, q); !errors.Is(err, boom) {
		t.Errorf("ValueOf on a failed read returned %v, want the read's own error", err)
	}
}

func TestTheIdentityOfAReadIsItsToolAndArgs(t *testing.T) {
	t.Parallel()

	runs := 0
	links := []link{{Index: 2, Name: "eth0"}}

	ans := Observe(&countingAsker{}, []Query{
		linksQuery(&runs, links),
		linksQuery(&runs, links),
		linksQuery(&runs, links),
	})

	if runs != 1 {
		t.Errorf("the read ran %d times, want 1 — Observe deduplicates on Tool and Args", runs)
	}
	if len(ans) != 1 {
		t.Errorf("recorded %d answers, want 1", len(ans))
	}
}

func TestAStubAnswersAReadWithoutRunningIt(t *testing.T) {
	t.Parallel()

	runs := 0
	q := linksQuery(&runs, []link{{Index: 2, Name: "eth0"}})

	ask := &countingAsker{out: map[string]string{q.String(): ""}}
	ask.stub = true

	Observe(ask, []Query{q})

	if runs != 0 {
		t.Errorf("the stub ran the real read %d times, want 0 — a read is stubbed by name", runs)
	}
	if got := ask.count(q.String()); got != 1 {
		t.Errorf("the stub was asked %d times, want 1", got)
	}
}

func TestATypedQueryParsesTheToolOutputItNames(t *testing.T) {
	t.Parallel()

	q := NewTyped(
		Query{Tool: ToolIP, Args: []string{"-4", "route", "show", "default"}},
		func(out string) int { return len(strings.Fields(out)) },
	)

	ans := Observe(&countingAsker{out: map[string]string{q.String(): "default via 10.0.0.1"}}, []Query{q.Query})

	got, err := q.Value(ans)
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if got != 3 {
		t.Errorf("the answer parsed to %d, want %d", got, 3)
	}
}

func TestWrappingAQueryDoesNotChangeItsIdentity(t *testing.T) {
	t.Parallel()

	plain := Query{Tool: ToolIP, Args: []string{"-4", "rule", "show"}}
	q := NewTyped(plain, func(out string) []string { return strings.Split(out, "\n") })

	if q.String() != plain.String() {
		t.Errorf("wrapping renamed the query to %q, want %q", q, plain)
	}

	ans := Observe(&countingAsker{out: map[string]string{plain.String(): "0: from all lookup local"}}, []Query{q.Query})
	if _, err := q.Value(ans); err != nil {
		t.Errorf("the wrapper could not read what its own plan declared: %v", err)
	}
}

func TestAFailedReadNeverReachesTheParse(t *testing.T) {
	t.Parallel()

	parsed := false
	q := NewTyped(
		Query{Tool: ToolNFT, Args: []string{"list", "table", "ip6", "gone"}},
		func(out string) map[string]bool { parsed = true; return map[string]bool{} },
	)

	ans := Observe(&countingAsker{out: map[string]string{}}, []Query{q.Query})

	got, err := q.Value(ans)
	if err == nil {
		t.Fatal("a failed read came back without an error, so an empty table and a failed look are the same fact")
	}
	if parsed {
		t.Error("the parse ran on a failed read")
	}
	if got != nil {
		t.Errorf("a failed read returned %v, want the zero value", got)
	}
	if q.ValueOr(ans, nil) != nil {
		t.Error("ValueOr on a failed read did not fall back")
	}
}

func TestOneIdentityCanCarryTwoParses(t *testing.T) {
	t.Parallel()

	plain := Query{Tool: ToolIP, Args: []string{"-4", "route", "show", "default"}}
	lines := NewTyped(plain, func(out string) int { return len(strings.Split(out, "\n")) })
	words := NewTyped(plain, func(out string) int { return len(strings.Fields(out)) })

	asker := &countingAsker{out: map[string]string{plain.String(): "default via 10.0.0.1\ndefault via 10.0.1.1"}}
	ans := Observe(asker, []Query{lines.Query, words.Query})

	if n := asker.count(plain.String()); n != 1 {
		t.Errorf("two views over one identity cost %d reads, want 1", n)
	}

	gotLines, err := lines.Value(ans)
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	gotWords, err := words.Value(ans)
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if gotLines != 2 || gotWords != 6 {
		t.Errorf("the two views read %d lines and %d words, want 2 and 6", gotLines, gotWords)
	}
}

func TestATypedQueryAnsweredByAStructuredReadSkipsTheParse(t *testing.T) {
	t.Parallel()

	runs := 0
	want := []link{{Index: 2, Name: "eth0"}}
	q := NewTyped(linksQuery(&runs, want), func(out string) []link {
		t.Error("the parse ran on an answer that already carried its value")
		return nil
	})

	ans := Observe(&countingAsker{}, []Query{q.Query})

	got, err := q.Value(ans)
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if len(got) != 1 || got[0].Name != "eth0" {
		t.Errorf("the read came back as %v, want %v", got, want)
	}
}
