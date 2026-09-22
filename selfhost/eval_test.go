package selfhost

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

// loadEvalApp compiles selfhost/eval.fct (and its import chain down to
// expr_tokens.fct) and serves it, exactly like loadExprApp does for expr.fct.
func loadEvalApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("eval.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/eval.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// postEvalJSON POSTs /api/<action> and returns the state deltas.
func postEvalJSON(t *testing.T, ts *httptest.Server, action string, args ...any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/api/"+action, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s returned %d: %s", action, resp.StatusCode, b)
	}
	var out struct {
		OK     bool           `json:"ok"`
		Deltas map[string]any `json:"deltas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("%s did not report ok: %+v", action, out)
	}
	return out.Deltas
}

// scopeVar is one typed cell of the evaluation scope, as both sides see it:
// the real runtime through a `state` cell, the port through its
// `name<TAB>kind<TAB>spelling` spec. A "list" cell spells as comma-joined
// ints (the shape `state xs: [int]` seeds).
type scopeVar struct {
	name, kind string
	val        any
}

func specOf(vars []scopeVar) string {
	var rows []string
	for _, v := range vars {
		rows = append(rows, v.name+"\t"+v.kind+"\t"+spellGo(v.val))
	}
	return strings.Join(rows, "\n")
}

// evalCase is one cross-checked expression. `real` is the source the real
// runtime evaluates. The port evaluates the same source through its parser
// (runEvalSrc) unless `op` is set, in which case it applies `op` to the
// scope refs in `args` through the node-level driver (runEvalNode) — the
// route for operators/builtins expr_tree.fct's grammar subset does not
// parse yet (`%`, `in`, `<<`, `>>`, min/max/replace/slug as calls).
type evalCase struct {
	real string
	op   string
	args string
}

// evalViaPort runs one case through the port's HTTP driver and returns
// (toStr-spelled result, result kind). A cell absent from the deltas kept its
// initial "" (each http.Post is a fresh session).
func evalViaPort(t *testing.T, ts *httptest.Server, c evalCase, spec string) (string, string) {
	t.Helper()
	var d map[string]any
	if c.op != "" {
		d = postEvalJSON(t, ts, "runEvalNode", c.op, c.args, spec)
	} else {
		d = postEvalJSON(t, ts, "runEvalSrc", c.real, spec)
	}
	res, _ := d["evalResult"].(string)
	kind, _ := d["evalKind"].(string)
	return res, kind
}

// spellGo is runtime/eval.go's toStr, restated for the value shapes
// eval()/evalInFrame() produce (toStr itself is unexported).
func spellGo(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) && x == math.Trunc(x) &&
			x >= -9223372036854775808.0 && x < 9223372036854775808.0 {
			return strconv.Itoa(int(x))
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, el := range x {
			parts[i] = spellGo(el)
		}
		return strings.Join(parts, ",")
	case []int:
		parts := make([]string, len(x))
		for i, el := range x {
			parts[i] = strconv.Itoa(el)
		}
		return strings.Join(parts, ",")
	}
	return ""
}

func kindGo(v any) string {
	switch v.(type) {
	case nil:
		return "none"
	case string:
		return "text"
	case bool:
		return "bool"
	case int, int64:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	}
	return "?"
}

// TestEvalPortMatchesRealRuntime cross-checks every case against the REAL
// action/view evaluator (runtime/eval.go's eval, reached through
// ir.CompileExpr + Server.EvalExpr over a tiny app whose `state` cells are
// the scope), comparing both the toStr spelling and the dynamic kind.
func TestEvalPortMatchesRealRuntime(t *testing.T) {
	now := int(time.Now().Unix())
	vars := []scopeVar{
		{"n", "int", 7},
		{"m", "int", -3},
		{"a", "int", 17},
		{"b", "int", 5},
		{"z", "int", 0},
		{"two", "int", 2},
		{"nine", "int", 9},
		{"s", "text", "Hello, World"},
		{"w", "text", "  padded  "},
		{"lo", "text", "l"},
		{"up", "text", "L"},
		{"title", "text", "Hello, World! 2024"},
		{"strTwo", "text", "2"},
		{"flag", "bool", true},
		{"ts", "int", 1700000000}, // 2023-11-14 22:13:20 UTC
		{"tnow", "int", now},
		{"xs", "list", []int{1, 2, 3}},
	}
	app := `app EvalCross:
    state n: int = 7
    state m: int = -3
    state a: int = 17
    state b: int = 5
    state z: int = 0
    state two: int = 2
    state nine: int = 9
    state s: text = "Hello, World"
    state w: text = "  padded  "
    state lo: text = "l"
    state up: text = "L"
    state title: text = "Hello, World! 2024"
    state strTwo: text = "2"
    state flag: bool = true
    state ts: int = 1700000000
    state tnow: int = 0
    state xs: [int] = [1, 2, 3]
    action seed(t: int):
        tnow = t
    view Home at "/":
        box:
            text "{n}"
`
	g, err := compile.String(app)
	if err != nil {
		t.Fatalf("compile scope app: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	if _, err := srv.Run("ada", "member", true, "seed", []any{now}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []evalCase{
		// arithmetic, precedence, negatives
		{real: "1 + 2 * 3"},
		{real: "(1 + 2) * 3"},
		{real: "10 - 4 - 3"},
		{real: "-5 + 2"},
		{real: "2 * -3"},
		{real: "-(n + m)"},
		{real: "n * m + 1"},
		{real: "2 * (n - m) / 4"},
		// integer division / modulo (truncation, x/0 == 0)
		{real: "17 / 5"},
		{real: "-17 / 5"},
		{real: "7 / 0"},
		{real: "a % b", op: "%", args: "a,b"},
		{real: "-a % b", op: "%", args: "negA,b"},
		{real: "n % z", op: "%", args: "n,z"},
		// text concat with ints and bools
		{real: "\"n=\" + n"},
		{real: "n + \" items\""},
		{real: "\"flag:\" + flag"},
		{real: "\"\" + (n > m)"},
		{real: "s + \"!\""},
		{real: "\"a\" + 1 + 2"},
		{real: "1 + 2 + \"a\""},
		{real: "\"tab\\there\" + \"\\n\" + \"q\\\"q\""},
		// every comparison
		{real: "n < m"},
		{real: "n <= 7"},
		{real: "n > m"},
		{real: "n >= 8"},
		{real: "n == 7"},
		{real: "n != 7"},
		{real: "\"abc\" < \"abd\""},
		{real: "\"b\" > \"abc\""},
		{real: "\"10\" < 9"},
		{real: "n == \"7\""},
		{real: "flag == 1"},
		{real: "\"\" == 0"},
		{real: "true == 1"},
		{real: "n != m"},
		// && / || short-circuit, unary !
		{real: "flag && n > 0"},
		{real: "!flag || n < 0"},
		{real: "false && (7 / 0 == 0)"},
		{real: "true || false"},
		{real: "!flag"},
		{real: "!n"},
		{real: "!0"},
		{real: "!\"\""},
		{real: "n && m"},
		{real: "0 || \"\""},
		// pure builtins
		{real: "abs(m)"},
		{real: "abs(n)"},
		{real: "min(n, m)", op: "min", args: "n,m"},
		{real: "max(n, m)", op: "max", args: "n,m"},
		{real: "floor(n)"},
		{real: "round(m)"},
		{real: "money(123456)"},
		{real: "money(-5)"},
		{real: "money(7)"},
		{real: "len(s)"},
		{real: "len(\"\")"},
		{real: "upper(s)"},
		{real: "lower(s)"},
		{real: "trim(w)"},
		{real: "contains(s, \"World\")"},
		{real: "contains(s, \"world\")"},
		{real: "take(s, 5)"},
		{real: "take(s, 100)"},
		{real: "take(s, -1)"},
		{real: "split(s, \", \")"},
		{real: "len(split(s, \"l\"))"},
		{real: "slice(s, 7, 12)"},
		{real: "slice(s, 3, 1)"},
		{real: "slice(s, -4, 99)"},
		{real: "charAt(s, 4)"},
		{real: "charAt(s, 99)"},
		{real: "replace(s, lo, up)", op: "replace", args: "s,lo,up"},
		{real: "slug(title)", op: "slug", args: "title"},
		{real: "year(ts)"},
		{real: "month(ts)"},
		{real: "day(ts)"},
		{real: "ago(ts)"},
		{real: "ago(tnow - 7500)"},
		{real: "ago(tnow - 150)"},
		{real: "ago(tnow)"},
		{real: "compact(999)"},
		{real: "compact(1999)"},
		{real: "compact(604000)"},
		{real: "compact(-2500)"},
		{real: "compact(240100000)"},
		{real: "commas(1234567)"},
		{real: "commas(-999)"},
		{real: "commas(12)"},
		{real: "toMoney(\"12.345\")"},
		{real: "toMoney(\"abc\")"},
		{real: "toInt(\"42\")"},
		{real: "toInt(\"abc\")"},
		{real: "toInt(flag)"},
		{real: "toInt(\"3.9\")"},
		{real: "toInt(\" -8 \")"},
		// list literal + len, membership
		{real: "len([1, 2, 3])"},
		{real: "[1, 2, 3]"},
		{real: "[n, m, s]"},
		{real: "xs"},
		{real: "two in xs", op: "in", args: "two,xs"},
		{real: "strTwo in xs", op: "in", args: "strTwo,xs"},
		{real: "nine in xs", op: "in", args: "nine,xs"},
	}
	if len(cases) < 40 {
		t.Fatalf("need at least 40 cross-checked expressions, have %d", len(cases))
	}

	ts := loadEvalApp(t)
	spec := specOf(append(vars, scopeVar{"negA", "int", -17}))
	for _, c := range cases {
		e, err := ir.CompileExpr(g, c.real)
		if err != nil {
			t.Fatalf("real ir.CompileExpr(%q): %v", c.real, err)
		}
		real := srv.EvalExpr(e, "ada", "member", true)
		wantStr, wantKind := spellGo(real), kindGo(real)
		gotStr, gotKind := evalViaPort(t, ts, c, spec)
		if gotStr != wantStr || gotKind != wantKind {
			t.Errorf("%q: port = %q (%s), real runtime = %q (%s)", c.real, gotStr, gotKind, wantStr, wantKind)
		}
	}
}

// TestEvalPortMatchesRealProcEvaluator covers what the action/view evaluator
// refuses at compile time (checkNoIndex / checkNoBitwise / no float literal
// outside a proc): each case is a real proc body evaluated by evalInFrame
// through a bound `do` call into a text state cell, versus the port
// evaluating the equivalent expression.
func TestEvalPortMatchesRealProcEvaluator(t *testing.T) {
	cases := []struct {
		procBody string   // statements of the proc; must `return` a text
		port     evalCase // what the port evaluates (real is the source form)
	}{
		{"let xs = [10, 20, 30]\n        return \"\" + xs[1]", evalCase{real: "[10, 20, 30][1]"}},
		{"let xs = [\"a\", \"b\", \"c\"]\n        return \"\" + xs[2]", evalCase{real: "[\"a\", \"b\", \"c\"][2]"}},
		{"let xs = [10, 20, 30]\n        return \"\" + (xs[0] + xs[2] * 2)", evalCase{real: "[10, 20, 30][0] + [10, 20, 30][2] * 2"}},
		{"let xs = [4, 5]\n        return \"\" + len(xs)", evalCase{real: "len([4, 5])"}},
		{"return \"\" + (12 & 10)", evalCase{real: "12 & 10"}},
		{"return \"\" + (12 | 3)", evalCase{real: "12 | 3"}},
		{"return \"\" + (12 ^ 10)", evalCase{real: "12 ^ 10"}},
		{"return \"\" + (1 << 4)", evalCase{real: "1 << 4", op: "<<", args: "one,four"}},
		{"return \"\" + (-16 >> 2)", evalCase{real: "-16 >> 2", op: ">>", args: "neg16,two"}},
		{"return \"\" + (1 << -1)", evalCase{real: "1 << -1", op: "<<", args: "one,negOne"}},
		{"return \"\" + (~5)", evalCase{real: "~5"}},
		{"return \"\" + (1.5 + 2.25)", evalCase{real: "1.5 + 2.25"}},
		{"return \"\" + (7.0 / 2.0)", evalCase{real: "7.0 / 2.0"}},
		{"return \"\" + (3.0 * 2.0)", evalCase{real: "3.0 * 2.0"}},
		{"return \"\" + (1.5 < 2.0)", evalCase{real: "1.5 < 2.0"}},
		{"return \"\" + (2.5 == 2.5)", evalCase{real: "2.5 == 2.5"}},
		{"return \"\" + floor(2.7)", evalCase{real: "floor(2.7)"}},
		{"return \"\" + round(2.5)", evalCase{real: "round(2.5)"}},
		{"return \"\" + abs(-1.25)", evalCase{real: "abs(-1.25)"}},
		{"return \"\" + min(1.5, 0.5)", evalCase{real: "min(1.5, 0.5)", op: "min", args: "f15,f05"}},
		{"return \"\" + max(1.5, 0.5)", evalCase{real: "max(1.5, 0.5)", op: "max", args: "f15,f05"}},
		{"return \"\" + (-2.5)", evalCase{real: "-2.5"}},
		{"return \"\" + (0.1 + 0.2)", evalCase{real: "0.1 + 0.2"}},
		{"return \"v=\" + 2.5", evalCase{real: "\"v=\" + 2.5"}},
	}
	spec := specOf([]scopeVar{
		{"one", "int", 1}, {"four", "int", 4}, {"two", "int", 2}, {"neg16", "int", -16}, {"negOne", "int", -1},
		{"f15", "float", 1.5}, {"f05", "float", 0.5},
	})
	var b strings.Builder
	b.WriteString("app EvalProcCross:\n    state procOut: text = \"\"\n")
	for i, c := range cases {
		id := strconv.Itoa(i)
		b.WriteString("    proc pc" + id + "() -> text:\n        " + c.procBody + "\n")
		b.WriteString("    action runPc" + id + "():\n        let r = do pc" + id + "()\n        procOut = r\n")
	}
	b.WriteString("    view Home at \"/\":\n        box:\n            text \"{procOut}\"\n")
	g, err := compile.String(b.String())
	if err != nil {
		t.Fatalf("compile proc app: %v\n%s", err, b.String())
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)

	ts := loadEvalApp(t)
	for i, c := range cases {
		if _, err := srv.Run("ada", "member", true, "runPc"+strconv.Itoa(i), nil); err != nil {
			t.Fatalf("real proc %q: %v", c.port.real, err)
		}
		want, _ := srv.StateValue("procOut").(string)
		got, _ := evalViaPort(t, ts, c.port, spec)
		if got != want {
			t.Errorf("%q: port = %q, real evalInFrame = %q", c.port.real, got, want)
		}
	}
}

// TestEvalPortScopeAndKinds pins the port's own contract where the real
// runtime has no direct counterpart to compare against: the result kind
// for each scope kind, float scope cells (not seedable through an action),
// an unbound name (ir.CompileExpr rejects one before eval() could yield
// nil), and an out-of-bounds index (an error in evalInFrame, none here).
func TestEvalPortScopeAndKinds(t *testing.T) {
	ts := loadEvalApp(t)
	spec := "x\tfloat\t2.5\nk\tint\t4\nname\ttext\tAda\nok\tbool\tfalse"
	cases := []struct{ src, want, kind string }{
		{"x * 2", "5", "float"},
		{"x + k", "6.5", "float"},
		{"k / 3", "1", "int"},
		{"x / 0", "+Inf", "float"},
		{"name + \" \" + k", "Ada 4", "text"},
		{"ok || k", "true", "bool"},
		{"!ok && x > 2", "true", "bool"},
		{"missing", "", "none"},
		{"missing == 0", "true", "bool"},
		{"missing + 1", "1", "int"},
		{"\"x\" + missing", "x", "text"},
		{"[k, x, ok]", "4,2.5,false", "list"},
		{"[1, 2][5]", "", "none"},
		{"toInt(x)", "2", "int"},
		// an unknown name: nil either way in Go (an unbound ref, or an
		// unknown builtin in callBuiltin); ir.CompileExpr rejects it before
		// eval() could run, so only the port can be asked
		{"frobnicate(1)", "", "none"},
	}
	for _, c := range cases {
		got, kind := evalViaPort(t, ts, evalCase{real: c.src}, spec)
		if got != c.want || kind != c.kind {
			t.Errorf("%q: got %q (%s), want %q (%s)", c.src, got, kind, c.want, c.kind)
		}
	}
}
