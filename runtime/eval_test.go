package runtime

import (
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

// The server evaluator and the client evaluator (assets/facet.js) must agree.
// This pins the Go side; the shapes here mirror the JS switch exactly.
func TestEval(t *testing.T) {
	store := map[string]any{"a": 3, "b": 4, "name": "ada", "on": true}
	bin := func(op string, l, r *ir.Expr) *ir.Expr { return &ir.Expr{Kind: "bin", Op: op, L: l, R: r} }
	ref := func(n string) *ir.Expr { return &ir.Expr{Kind: "ref", Name: n} }
	lit := func(v any, vt string) *ir.Expr { return &ir.Expr{Kind: "lit", Val: v, VType: vt} }

	cases := []struct {
		expr *ir.Expr
		want any
	}{
		{bin("+", ref("a"), ref("b")), 7},
		{bin("*", ref("a"), lit(2, "int")), 6},
		{bin(">", ref("b"), ref("a")), true},
		{bin("==", ref("name"), lit("ada", "text")), true},
		{bin("+", lit("hi ", "text"), ref("name")), "hi ada"},
		{&ir.Expr{Kind: "un", Op: "!", X: ref("on")}, false},
		{bin("&&", ref("on"), bin("<", ref("a"), ref("b"))), true},
	}
	for i, c := range cases {
		if got := eval(c.expr, store); !sameValue(got, c.want) {
			t.Errorf("case %d: got %#v, want %#v", i, got, c.want)
		}
	}
}

// Member access and entity lookup over a collection of records.
func TestEvalGetAndEntityLookup(t *testing.T) {
	rows := []any{
		record{"id": 1, "author": "ada", "likes": 3},
		record{"id": 2, "author": "alan", "likes": 7},
	}
	scope := map[string]any{"Post": rows, "p": rows[0]}

	get := &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "author"}
	if got := eval(get, scope); got != "ada" {
		t.Errorf("p.author: got %v want ada", got)
	}
	eget := &ir.Expr{Kind: "eget", Name: "Post", Field: "likes", Key: &ir.Expr{Kind: "lit", Val: 2, VType: "int"}}
	if got := eval(eget, scope); got != 7 {
		t.Errorf("Post(2).likes: got %v want 7", got)
	}
}

// Aggregates fold an entity collection: count is the row count, sum totals a
// field. The Go fold here must match the JS one in assets/facet.js.
func TestEvalAggregates(t *testing.T) {
	rows := []any{
		record{"id": 1, "likes": 3},
		record{"id": 2, "likes": 7},
		record{"id": 3, "likes": 2},
	}
	scope := map[string]any{"Post": rows}

	count := &ir.Expr{Kind: "agg", Op: "count", Name: "Post"}
	if got := eval(count, scope); got != 3 {
		t.Errorf("count(Post): got %v want 3", got)
	}
	sum := &ir.Expr{Kind: "agg", Op: "sum", Name: "Post", Field: "likes"}
	if got := eval(sum, scope); got != 12 {
		t.Errorf("sum(Post.likes): got %v want 12", got)
	}
	// empty / missing collection folds to zero, not a panic.
	if got := eval(count, map[string]any{}); got != 0 {
		t.Errorf("count of missing collection: got %v want 0", got)
	}
}

// Filtered aggregates scope a count/exists to the rows a predicate accepts, with
// the item variable bound to each row. This is what powers per-tweet like counts
// and the per-viewer "have I liked this?" check. The Go fold must match facet.js.
func TestEvalFilteredAggregates(t *testing.T) {
	likes := []any{
		record{"id": 1, "tweet": 10, "user": "ada"},
		record{"id": 2, "tweet": 10, "user": "bob"},
		record{"id": 3, "tweet": 20, "user": "ada"},
	}
	// item var `l` bound per row; outer `t` (the current tweet) read from scope.
	scope := map[string]any{"Like": likes, "t": record{"id": 10}}

	// count(l in Like where l.tweet == t.id) == 2
	lTweet := &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "l"}, Field: "tweet"}
	tID := &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "t"}, Field: "id"}
	whereTweet := &ir.Expr{Kind: "bin", Op: "==", L: lTweet, R: tID}
	count := &ir.Expr{Kind: "agg", Op: "count", Name: "Like", Var: "l", Where: whereTweet}
	if got := eval(count, scope); got != 2 {
		t.Errorf("count(l in Like where l.tweet == t.id): got %v want 2", got)
	}

	// exists(l in Like where l.tweet == t.id && l.user == "ada") == true
	lUser := &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "l"}, Field: "user"}
	mine := &ir.Expr{Kind: "bin", Op: "&&", L: whereTweet,
		R: &ir.Expr{Kind: "bin", Op: "==", L: lUser, R: &ir.Expr{Kind: "lit", Val: "ada", VType: "text"}}}
	exists := &ir.Expr{Kind: "agg", Op: "exists", Name: "Like", Var: "l", Where: mine}
	if got := eval(exists, scope); got != true {
		t.Errorf("exists(... && l.user == ada): got %v want true", got)
	}

	// no match -> exists false, count 0
	scope["t"] = record{"id": 99}
	if got := eval(exists, scope); got != false {
		t.Errorf("exists with no match: got %v want false", got)
	}
	if got := eval(count, scope); got != 0 {
		t.Errorf("count with no match: got %v want 0", got)
	}

	// the item variable must not leak into the outer scope after evaluation.
	if _, leaked := scope["l"]; leaked {
		t.Errorf("item var l leaked into scope after agg eval")
	}
}

// contains(s, sub) is the substring predicate behind search; it must match the JS
// String.includes in assets/facet.js. Case folding composes via lower().
func TestEvalContains(t *testing.T) {
	lit := func(s string) *ir.Expr { return &ir.Expr{Kind: "lit", Val: s, VType: "text"} }
	call := func(s, sub string) any {
		return eval(&ir.Expr{Kind: "call", Name: "contains", Args: []*ir.Expr{lit(s), lit(sub)}}, map[string]any{})
	}
	if call("hello world", "world") != true {
		t.Error(`contains("hello world","world") should be true`)
	}
	if call("hello", "xyz") != false {
		t.Error(`contains("hello","xyz") should be false`)
	}
}

// The effectful builtins evaluate on the server: now() is a positive unix time,
// rand(n) is bounded to [0, n).
func TestEvalBuiltins(t *testing.T) {
	now := eval(&ir.Expr{Kind: "call", Name: "now"}, map[string]any{})
	if n, ok := now.(int); !ok || n <= 0 {
		t.Errorf("now() should be a positive int, got %#v", now)
	}
	bound := &ir.Expr{Kind: "call", Name: "rand", Args: []*ir.Expr{{Kind: "lit", Val: 5, VType: "int"}}}
	for i := 0; i < 50; i++ {
		v := toInt(eval(bound, map[string]any{}))
		if v < 0 || v >= 5 {
			t.Fatalf("rand(5) out of range: %d", v)
		}
	}
	// rand(0) is defined as 0 (empty range), not a panic.
	zero := &ir.Expr{Kind: "call", Name: "rand", Args: []*ir.Expr{{Kind: "lit", Val: 0, VType: "int"}}}
	if got := eval(zero, map[string]any{}); got != 0 {
		t.Errorf("rand(0) should be 0, got %v", got)
	}
}

// selectRows runs a list's query: filter (where), order (by), cap (limit). This
// pins the Go pipeline; assets/facet.js mirrors it exactly.
func TestSelectRows(t *testing.T) {
	rows := []any{
		record{"id": 1, "likes": 3},
		record{"id": 2, "likes": 0},
		record{"id": 3, "likes": 7},
		record{"id": 4, "likes": 5},
	}
	// where likes > 0, by likes desc, limit 2  ->  [7, 5]
	node := ir.Node{
		Kind: "list", Var: "p", Coll: "Post", Order: "likes", Desc: true,
		Limit: &ir.Expr{Kind: "lit", Val: 2, VType: "int"},
		Where: &ir.Expr{Kind: "bin", Op: ">",
			L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "likes"},
			R: &ir.Expr{Kind: "lit", Val: 0, VType: "int"}},
	}
	got := selectRows(rows, node, map[string]any{"Post": rows})
	if len(got) != 2 {
		t.Fatalf("filter+limit should yield 2 rows, got %d", len(got))
	}
	if toInt(got[0].(record)["likes"]) != 7 || toInt(got[1].(record)["likes"]) != 5 {
		t.Errorf("want likes [7,5], got [%v,%v]", got[0].(record)["likes"], got[1].(record)["likes"])
	}
	// no filter, no order, no limit: rows pass through unchanged.
	if got := selectRows(rows, ir.Node{Kind: "list", Var: "p", Coll: "Post"}, map[string]any{}); len(got) != 4 {
		t.Errorf("unfiltered list should pass all 4 rows, got %d", len(got))
	}
}

// A server action mutates authoritative state and reports the delta; the dep
// graph is what the client uses to patch only the affected bindings.
func TestServerActionDelta(t *testing.T) {
	g, err := compile.String(`
app C:
    state count: int = 0
    action inc:
        count = count + 5
    view M:
        box:
            text "{count}"
`)
	if err != nil {
		t.Fatal(err)
	}
	store := map[string]any{"count": 0}
	var act *ir.Action
	for i := range g.Actions {
		if g.Actions[i].Name == "inc" {
			act = &g.Actions[i]
		}
	}
	if act.Placement != ir.Server {
		t.Fatalf("inc should be server-placed")
	}
	// run the action body the way handleEvent does
	v := eval(act.Body[0].Value, store)
	if v != 5 {
		t.Fatalf("count after inc: got %v want 5", v)
	}
}
