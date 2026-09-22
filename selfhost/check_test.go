// This file pins selfhost/check.fct — the self-hosted port of
// internal/ir/build.go's check()/checkPure() expression checker — to the
// REAL compiler: every expression below is placed into a tiny app (in a
// derive, an action's `check`, or a view's `text "{…}"`) and compiled with
// compile.String, and the real compiler's accept/reject and, on reject, its
// diagnostic text are compared against what check.fct's exprCheck/
// exprCheckPure report for the same expression in the same scope. It
// imports internal/compile and runtime read-only — no .go file anywhere in
// the module is modified by this package.
package selfhost

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// loadCheckApp compiles and boots selfhost/check.fct through the same
// in-memory server expr_test.go's loadExprApp uses — via compile.File, the
// import-aware entry point, since check.fct imports expr.fct.
func loadCheckApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("check.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/check.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// postCheckJSON mirrors expr_test.go's postExprJSON (same request/response
// shape), named separately only to avoid colliding with the other helpers
// of that shape in this shared package.
func postCheckJSON(t *testing.T, ts *httptest.Server, action string, args ...any) map[string]any {
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

// The fixture app every case is compiled into. Exactly one slot carries the
// case's expression; the others keep a known-good default. Its scope is what
// the port is handed as flat name lists (checkScope, below): two state
// cells, one entity with three fields, one enum, one derive and one policy
// (the two `inline` names), and two actions.
const checkFixtureTemplate = `app CheckProbe:
    entity Post:
        id: int
        author: text
        likes: int

    state count: int = 0
    state draft: text = "" @client

    enum Status: active, closed

    derive total: int = count + 1

    policy admin:
        actor == "admin"

    action save(n: int):
        count = n

    action probe(x: int):
        check %s "probe"
        count = x

    derive probeD: %s = %s

    view Main:
        box:
            text "{%s}"
            for p in Post:
                text "{%s}"
`

// checkScope is the fixture's own declarations as the flat lists check.fct
// takes — the same tables build.go's env carries for the fixture.
var checkScope = struct {
	states, entities, entityFields, enums, inlines, actions string
}{
	states:       "count,draft",
	entities:     "Post",
	entityFields: "Post.id,Post.author,Post.likes",
	enums:        "Status",
	inlines:      "total,admin",
	actions:      "save,probe",
}

// checkCase: one expression in one position. pos is "derive" (typ is the
// derive's declared type, "int" when blank), "check" (the `probe` action's
// guard; its parameter `x` is the one local), "view" (the top-level text
// node; no locals) or "rowview" (the text node inside `for p in Post`; `p`
// is the one local). want is "" for an expression the real compiler must
// accept, else a distinctive substring of the diagnostic BOTH the real
// compiler and the port must report.
type checkCase struct {
	expr, pos, typ, want string
}

func checkFixture(c checkCase) string {
	chk, der, typ, view, row := "x > 0", "count", "int", "count", "p.likes"
	switch c.pos {
	case "check":
		chk = c.expr
	case "derive":
		der = c.expr
		if c.typ != "" {
			typ = c.typ
		}
	case "view":
		view = c.expr
	case "rowview":
		row = c.expr
	default:
		panic("unknown position " + c.pos)
	}
	return fmt.Sprintf(checkFixtureTemplate, chk, typ, der, view, row)
}

// portDiag runs the case through check.fct's driver with the scope build.go
// would hand check()/checkPure() at that position.
func portDiag(t *testing.T, ts *httptest.Server, c checkCase) string {
	t.Helper()
	position, locals := c.pos, ""
	switch c.pos {
	case "check":
		locals = "x"
	case "rowview":
		position, locals = "view", "p"
	}
	d := postCheckJSON(t, ts, "runExprCheck", c.expr, position, locals,
		checkScope.states, checkScope.entities, checkScope.entityFields, checkScope.enums, checkScope.inlines)
	got, ok := d["checkResult"]
	if !ok {
		t.Fatalf("runExprCheck(%q) returned no checkResult delta: %v", c.expr, d)
	}
	s, _ := got.(string)
	return s
}

var checkCases = []checkCase{
	// ---- accepted: names, scope, inline names, builtins, aggregates ----
	{"count + 1", "derive", "int", ""},
	{`actor == "admin"`, "derive", "bool", ""},
	{`role == "admin" && tenantRole == ""`, "derive", "bool", ""},
	{`verified && session != "" && tenant == 0`, "derive", "bool", ""},
	{"route", "view", "", ""},
	{"count + total", "view", "", ""},
	{"p.likes * 2", "rowview", "", ""},
	{"p.author", "rowview", "", ""},
	{"x > count", "check", "", ""},
	{`draft != ""`, "check", "", ""},
	{"Post(x).likes > 0", "check", "", ""},
	{"total + 1", "derive", "int", ""},
	{"admin", "derive", "bool", ""},
	{"!admin || count == 1", "derive", "bool", ""},
	{"-count", "derive", "int", ""},
	{`Status.active == "active"`, "derive", "bool", ""},
	{"count(Post)", "derive", "int", ""},
	{"count(p in Post where p.likes > 2)", "derive", "int", ""},
	{"sum(Post.likes)", "derive", "int", ""},
	{"sum(p.likes * 2 in Post)", "derive", "int", ""},
	{"min(Post.likes)", "derive", "int", ""},
	{"exists(p in Post where p.likes > 2)", "derive", "bool", ""},
	{"Post(1).likes + Post(2).id", "derive", "int", ""},
	{"slice(draft, 0, 1) + upper(draft)", "derive", "text", ""},
	{"lower(trim(draft))", "derive", "text", ""},
	{"abs(count) + toInt(draft)", "derive", "int", ""},
	{`contains(draft, "a")`, "derive", "bool", ""},
	{"len([count, 2])", "derive", "int", ""},
	{`"n={nothing}"`, "derive", "text", ""}, // braces around an unbound name are literal text
	{`"{ border: none }"`, "derive", "text", ""},

	// ---- rejected: unknown reference ----
	{"nope + 1", "derive", "int", `unknown reference "nope"`},
	{"route", "derive", "text", `unknown reference "route"`}, // a render-only name outside a render
	{"y > 0", "check", "", `unknown reference "y"`},
	{"nope", "view", "", `unknown reference "nope"`},
	{"count(p in Post where q.likes > 2)", "derive", "int", `unknown reference "q"`},
	{"len([count, zz])", "derive", "int", `unknown reference "zz"`},

	// ---- rejected: builtin arity ----
	{`len("a", "b")`, "derive", "int", "len takes 1 argument(s), got 2"},
	{"contains(draft)", "derive", "bool", "contains takes 2 argument(s), got 1"},
	{`slice(draft, 1)`, "derive", "text", "slice takes 3 argument(s), got 2"},
	{"now(1)", "derive", "int", "now() takes no arguments"},
	{"rand()", "derive", "int", "rand(n) takes exactly one argument (an exclusive upper bound)"},
	{`readFile("a", "b")`, "derive", "text", "readFile(...) takes exactly one argument"},
	{`writeFile("u")`, "derive", "text", "writeFile(...) takes exactly two arguments"},
	{"listen()", "derive", "int", "listen(...) takes exactly one argument"},
	{"readBytes(1)", "derive", "int", "readBytes(...) takes exactly two arguments"},
	{"channel(1)", "derive", "int", "channel() takes no arguments"},
	{"recv()", "derive", "int", "recv(ch) takes exactly one argument (the channel)"},
	{"send(count)", "derive", "int", "send(ch, value) takes exactly two arguments"},

	// ---- rejected: aggregate rules ----
	{"count(draft)", "derive", "int", `count(...) needs an entity collection; "draft" is not an entity`},
	{"sum(Post.nope)", "derive", "int", `entity "Post" has no field "nope" to sum`},
	{"exists(Post)", "derive", "bool", "exists needs a filtered form: exists(x in Post where <cond>)"},
	{"Post(1).nope", "derive", "int", `entity "Post" has no field "nope" (in ` + "`Post(…).nope`" + ")"},

	// ---- rejected: only inside a proc ----
	{"count & 1", "view", "", `"&" is only available inside a proc`},
	{"count | 1", "derive", "int", `"|" is only available inside a proc`},
	{"~count", "derive", "int", "`~` is only available inside a proc"},
	{"[1, 2][0]", "derive", "int", "array/map indexing (`x[i]`) is only available inside a proc"},
	{`{"a": 1}`, "derive", "int", "a map literal (`{...}`) is only available inside a proc"},
	{"1.5", "derive", "int", "a float literal is only available inside a proc"},
	{"count + 1.5", "view", "", "a float literal is only available inside a proc"},
	{"toFloat(count)", "derive", "int", "toFloat(...) is only available inside a proc"},
	{`readFile("x")`, "derive", "text", "readFile(...) is only available inside a proc that declares `uses io.file`"},
	{`httpPost("u", "b")`, "derive", "text", "httpPost(...) is only available inside a proc that declares `uses io.net`"},
	{"channel()", "derive", "int", "channel(...) is only available inside a proc body"},
	{"listen(80)", "derive", "int", "listen(...) is only available inside a daemon body"},

	// ---- rejected: checkPure's effectful-builtin and print refusals ----
	{"now()", "derive", "int", "a derive cannot use an effectful builtin (now/rand)"},
	{"now() > 0", "check", "", "a check cannot use an effectful builtin (now/rand)"},
	{"rand(3)", "view", "", "a view cannot use an effectful builtin (now/rand)"},
	{"print(count)", "derive", "int", "a derive cannot call print(...)"},

	// ---- rejected: a text literal whose braces would have interpolated ----
	{`"n={count}"`, "derive", "text", "a text literal does not interpolate, so \"n={count}\" renders `{count}` as literal text — write the value as an expression: \"n=\" + count"},
	{`"/x/{x}" != ""`, "check", "", "so \"/x/{x}\" renders `{x}` as literal text — write the value as an expression: \"/x/\" + x"},
}

// TestCheckAgainstCompiler is the cross-check itself: for every case the
// real compiler and check.fct must agree on accept/reject, and on reject
// both diagnostics must carry the case's distinctive substring.
func TestCheckAgainstCompiler(t *testing.T) {
	ts := loadCheckApp(t)
	accepted, rejected := 0, 0
	for _, c := range checkCases {
		if c.want == "" {
			accepted++
		} else {
			rejected++
		}
		t.Run(c.pos+"/"+c.expr, func(t *testing.T) {
			_, err := compile.String(checkFixture(c))
			got := portDiag(t, ts, c)
			if c.want == "" {
				if err != nil {
					t.Fatalf("real compiler rejected %q in a %s: %v", c.expr, c.pos, err)
				}
				if got != "" {
					t.Fatalf("port rejected %q in a %s, which the real compiler accepts: %s", c.expr, c.pos, got)
				}
				return
			}
			if err == nil {
				t.Fatalf("real compiler accepted %q in a %s; expected a diagnostic containing %q", c.expr, c.pos, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("real compiler's diagnostic for %q lacks %q:\n  %v", c.expr, c.want, err)
			}
			if got == "" {
				t.Fatalf("port accepted %q in a %s; the real compiler says: %v", c.expr, c.pos, err)
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("port's diagnostic for %q lacks %q:\n  port: %s\n  real: %v", c.expr, c.want, got, err)
			}
		})
	}
	if len(checkCases) < 25 || accepted < 12 || rejected < 12 {
		t.Fatalf("case table too thin: %d cases (%d accepted, %d rejected)", len(checkCases), accepted, rejected)
	}
}

// TestCheckActState pins the ActState arm — pending()/failed() name an
// action, dirty()/touched() name a state cell — which check.fct ports as
// actStateCheck over the (op, target) pair, because expr_tree.fct's grammar
// builds no ActState node for exprCheck to reach (see check.fct's header).
// The real compiler sees each read in the fixture's `check` position.
func TestCheckActState(t *testing.T) {
	ts := loadCheckApp(t)
	cases := []struct{ op, target, want string }{
		{"pending", "save", ""},
		{"failed", "probe", ""},
		{"dirty", "draft", ""},
		{"touched", "count", ""},
		{"pending", "nope", `pending(nope) names an unknown action "nope"`},
		{"failed", "count", `failed(count) names an unknown action "count"`},
		{"dirty", "nope", `dirty(nope) names an unknown state cell "nope"`},
		{"touched", "save", `touched(save) names an unknown state cell "save"`},
	}
	for _, c := range cases {
		t.Run(c.op+"/"+c.target, func(t *testing.T) {
			expr := c.op + "(" + c.target + ")"
			_, err := compile.String(checkFixture(checkCase{expr: expr, pos: "check"}))
			d := postCheckJSON(t, ts, "runActStateCheck", c.op, c.target, checkScope.actions, checkScope.states)
			got, _ := d["actStateResult"].(string)
			if c.want == "" {
				if err != nil {
					t.Fatalf("real compiler rejected %q: %v", expr, err)
				}
				if got != "" {
					t.Fatalf("port rejected %q, which the real compiler accepts: %s", expr, got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("real compiler's result for %q lacks %q: %v", expr, c.want, err)
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("port's diagnostic for %q lacks %q: %s", expr, c.want, got)
			}
		})
	}
}

// TestCheckScopeWidening pins withActor/viewScope: the same expression is
// checked through the raw scope the driver builds for each position, so a
// render-only name resolves in a view and nowhere else, and the session's
// identity names resolve everywhere.
func TestCheckScopeWidening(t *testing.T) {
	ts := loadCheckApp(t)
	run := func(expr, position, locals string) string {
		d := postCheckJSON(t, ts, "runExprCheck", expr, position, locals,
			checkScope.states, checkScope.entities, checkScope.entityFields, checkScope.enums, checkScope.inlines)
		s, _ := d["checkResult"].(string)
		return s
	}
	if got := run("route", "view", ""); got != "" {
		t.Errorf("route in a view: %s", got)
	}
	if got := run("route", "action", ""); !strings.Contains(got, `unknown reference "route"`) {
		t.Errorf("route in an action body: %q", got)
	}
	if got := run("actor + session", "action", ""); got != "" {
		t.Errorf("identity names in an action body: %s", got)
	}
	if got := run("now()", "action", ""); got != "" {
		t.Errorf("now() in an action body is check(), not checkPure(): %s", got)
	}
	if got := run("n + 1", "action", "n"); got != "" {
		t.Errorf("an action parameter: %s", got)
	}
	if got := run("", "derive", ""); !strings.Contains(got, "cannot parse") {
		t.Errorf("an empty expression must report the port's own parse diagnostic: %q", got)
	}
}
