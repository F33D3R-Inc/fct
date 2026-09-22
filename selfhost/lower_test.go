package selfhost

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

// lower.fct is the self-hosted port of internal/ir/build.go's lower() (plus
// cloneExpr). This pins it to the real thing: every expression below is
// lowered by the REAL compiler (ir.CompileExpr over a small app graph, or —
// for the proc-only kinds CompileExpr's check refuses outside a proc — the
// `return`/`let` value of a compiled proc body) and json.Marshal'ed, and by
// lower.fct's own lowerSrcJSON / lowerSrcWithInlineJSON over the same
// source; both JSON strings are unmarshaled into ir.Expr and compared with
// reflect.DeepEqual. Since ir_expr.fct's irExprJSON claims to spell exactly
// what encoding/json spells, the raw strings are compared too.

func loadLowerApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("lower.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/lower.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// lowerCasesSrc declares everything the expressions under test refer to:
// an enum (member folding), two entities (egets, aggregates), scalar states
// (refs), zero-arg derives and a policy (inlining), and procs whose bodies
// carry the kinds only a proc may use (index, float, map, struct get).
const lowerCasesSrc = `app LowerCases:
    enum Status: active, closed

    entity Order:
        amount: int
        buyer: text
        status: Status

    entity Line:
        order: int
        qty: int
        price: int

    state n: int = 0
    state name: text = ""
    state flag: bool = false

    derive bigTotal: int = sum(o.amount in Order where o.amount > n)
    derive twice: int = bigTotal * 2

    policy admin:
        role == "admin"

    struct Pt:
        x: int
        y: int

    proc idx(xs: [int]) -> int:
        return xs[0] + xs[len(xs) - 1]

    proc flt() -> float:
        return 2.50 * 0.5 + 10.0

    proc mp(k: text) -> int:
        let m = {"a": 1, "b": 2 + 3}
        return m[k]

    proc pt(p: Pt) -> int:
        return p.x + p.y

    view Home at "/":
        box:
            text "{n}"
`

func lowerCasesGraph(t *testing.T) *ir.IR {
	t.Helper()
	g, err := compile.String(lowerCasesSrc)
	if err != nil {
		t.Fatalf("compile lowerCasesSrc: %v", err)
	}
	return g
}

// goLoweredJSON is the real compiler's answer for an expression read in the
// app's top-level scope (a policy/derive/view expression).
func goLoweredJSON(t *testing.T, g *ir.IR, src string) string {
	t.Helper()
	e, err := ir.CompileExpr(g, src)
	if err != nil {
		t.Fatalf("real compiler refused %q: %v", src, err)
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// goProcStmtJSON is the real compiler's answer for an expression that only a
// proc body may hold: statement `i` of the named proc's body, its Value.
func goProcStmtJSON(t *testing.T, g *ir.IR, proc string, i int) string {
	t.Helper()
	for _, p := range g.Procs {
		if p.Name != proc {
			continue
		}
		if i >= len(p.Body) || p.Body[i].Value == nil {
			t.Fatalf("proc %s has no statement %d with a value", proc, i)
		}
		b, err := json.Marshal(p.Body[i].Value)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Fatalf("proc %s not found in the compiled graph", proc)
	return ""
}

func selfLoweredJSON(t *testing.T, ts *httptest.Server, src, enums string) string {
	t.Helper()
	d := postExprJSON(t, ts, "runLowerSrc", src, enums)
	s, _ := d["lowerResult"].(string)
	return s
}

func selfLoweredInlineJSON(t *testing.T, ts *httptest.Server, src string, names, srcs []string, enums string) string {
	t.Helper()
	d := postExprJSON(t, ts, "runLowerSrcWithInline", src, strings.Join(names, "\n"), strings.Join(srcs, "\n"), enums)
	s, _ := d["lowerResult"].(string)
	return s
}

func assertSameIR(t *testing.T, src, got, want string) {
	t.Helper()
	var ge, we ir.Expr
	if err := json.Unmarshal([]byte(got), &ge); err != nil {
		t.Fatalf("%q: selfhost JSON does not unmarshal into ir.Expr: %v\n  %s", src, err, got)
	}
	if err := json.Unmarshal([]byte(want), &we); err != nil {
		t.Fatalf("%q: real compiler JSON does not unmarshal into ir.Expr: %v\n  %s", src, err, want)
	}
	if !reflect.DeepEqual(ge, we) {
		t.Errorf("%q: selfhost lower() disagrees with the real compiler:\n  got  %s\n  want %s", src, got, want)
		return
	}
	if got != want {
		t.Errorf("%q: same ir.Expr, but the JSON spelling differs:\n  got  %s\n  want %s", src, got, want)
	}
}

const lowerEnums = "Status"

// TestLowerMatchesGoOnTopLevelExpressions covers every kind ir.CompileExpr
// accepts in a top-level scope: literals, refs, nested binary/unary
// precedence, enum folding, entity lookups (nested keys), calls, lists,
// and every aggregate shape (whole collection, bare field, where, sel).
func TestLowerMatchesGoOnTopLevelExpressions(t *testing.T) {
	ts := loadLowerApp(t)
	g := lowerCasesGraph(t)
	cases := []string{
		// literals of each non-proc type
		`42`,
		`007`,
		`true`,
		`false`,
		`"hello"`,
		`"tab\tnew\nline \"quoted\" back\\slash"`,
		`""`,
		// refs (a state, and the runtime identity builtins)
		`n`,
		`actor`,
		`role`,
		// binary / unary precedence
		`1 + 2 * 3`,
		`(1 + 2) * 3`,
		`-n * 2 + 3 > 1 && !flag || flag == false`,
		`n - -1`,
		`name + " " + name`,
		`n >= 1 && n <= 9 || n != 5`,
		// enum member access folds to a text literal
		`Status.active`,
		`Order(1).status == Status.closed`,
		// entity lookups, nested keys, bare lookup
		`Order(1).amount`,
		`Order(Line(1).order).amount`,
		`Order(n + 1).buyer`,
		`Line(Order(Line(n).order).amount).qty`,
		// calls with args (names the self-hosted parser's isCallNameTok knows;
		// `replace`/`min`/`max` in call form are its own parser gap, not lower's)
		`len(name)`,
		`abs(n) + len(trim(name))`,
		`contains(name, "x")`,
		`slice(name, 1, len(name)) + charAt(name, 0)`,
		`split(lower(name), ",")`,
		// list literals
		`[1, 2, n]`,
		`["a", name]`,
		`[]`,
		// aggregates
		`count(Order)`,
		`sum(Order.amount)`,
		`avg(Order.amount)`,
		`count(o in Order where o.amount > n)`,
		`exists(o in Order where o.buyer == actor)`,
		`sum(o.amount in Order where o.status == Status.active)`,
		`sum(l.qty * l.price in Line where l.order == n)`,
		`max(l.qty + 1 in Line)`,
		`min(l.price in Line where l.qty > 0 && l.order == Order(1).id)`,
		`count(o in Order where o.amount > sum(l.price in Line where l.order == o.id))`,
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			want := goLoweredJSON(t, g, src)
			got := selfLoweredJSON(t, ts, src, lowerEnums)
			assertSameIR(t, src, got, want)
		})
	}
}

// TestLowerMatchesGoOnProcOnlyExpressions covers the kinds CompileExpr's
// check refuses outside a proc — index, float literals, map literals, and a
// struct field read — by reading the real compiler's lowering out of a
// compiled proc body instead.
func TestLowerMatchesGoOnProcOnlyExpressions(t *testing.T) {
	ts := loadLowerApp(t)
	g := lowerCasesGraph(t)
	cases := []struct {
		src  string
		proc string
		stmt int
	}{
		{`xs[0] + xs[len(xs) - 1]`, "idx", 0},
		{`2.50 * 0.5 + 10.0`, "flt", 0},
		{`{"a": 1, "b": 2 + 3}`, "mp", 0},
		{`m[k]`, "mp", 1},
		{`p.x + p.y`, "pt", 0},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			want := goProcStmtJSON(t, g, c.proc, c.stmt)
			got := selfLoweredJSON(t, ts, c.src, lowerEnums)
			assertSameIR(t, c.src, got, want)
		})
	}
}

// TestLowerInlinesDerivesAndPoliciesLikeGo: a reference to a zero-arg
// policy or a derive is replaced by that name's own lowered expression —
// the real compiler does it through envFromIR's inline table, the port
// through lowerSrcWithInlineJSON's parallel name/source lists, which lowers
// each inline source in turn (so `twice`, which reads `bigTotal`, carries
// bigTotal's expansion inside its own).
func TestLowerInlinesDerivesAndPoliciesLikeGo(t *testing.T) {
	ts := loadLowerApp(t)
	g := lowerCasesGraph(t)
	names := []string{"bigTotal", "twice", "admin"}
	srcs := []string{
		`sum(o.amount in Order where o.amount > n)`,
		`bigTotal * 2`,
		`role == "admin"`,
	}
	cases := []string{
		`bigTotal`,
		`bigTotal + 1`,
		`twice`,
		`twice > bigTotal`,
		`admin`,
		`admin && flag`,
		`[bigTotal, twice]`,
		`count(o in Order where o.amount > bigTotal)`,
		`len(name) + n`, // nothing to inline: unchanged
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			want := goLoweredJSON(t, g, src)
			got := selfLoweredInlineJSON(t, ts, src, names, srcs, lowerEnums)
			assertSameIR(t, src, got, want)
		})
	}
	// Without the table in scope the same names stay plain refs — the
	// substitution really is the inline table's doing, not the parser's.
	got := selfLoweredJSON(t, ts, `bigTotal + 1`, lowerEnums)
	var e ir.Expr
	if err := json.Unmarshal([]byte(got), &e); err != nil {
		t.Fatal(err)
	}
	if e.Kind != "bin" || e.L == nil || e.L.Kind != "ref" || e.L.Name != "bigTotal" {
		t.Errorf("with no inline table, bigTotal should lower to a plain ref, got %s", got)
	}
}

// TestLowerCloneIsDeep: the substituted copy shares nothing with the table
// entry — inlining the same derive twice in one expression yields two
// structurally equal, independently serialized subtrees, exactly what
// cloneExpr guarantees (and the real compiler produces).
func TestLowerCloneIsDeep(t *testing.T) {
	ts := loadLowerApp(t)
	g := lowerCasesGraph(t)
	src := `bigTotal + bigTotal`
	want := goLoweredJSON(t, g, src)
	got := selfLoweredInlineJSON(t, ts, src, []string{"bigTotal"}, []string{`sum(o.amount in Order where o.amount > n)`}, lowerEnums)
	assertSameIR(t, src, got, want)
}
