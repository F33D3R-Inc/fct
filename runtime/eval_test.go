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
		Kind: "list", Var: "p", Coll: "Post", Order: "likes", Desc: true, Limit: 2,
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
