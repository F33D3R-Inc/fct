// This file verifies parser.fct's procs against the real, current Go
// implementation they are a subset-port of (internal/parser/parser.go's
// parseProc header logic, parseSignature, and parseCheck's message-boundary
// scan), by compiling and running parser.fct through the same in-memory
// server source_test.go/expr_test.go already use, and comparing against the
// real Go parser.Parse's own ast.Proc/ast.Check output (and, for the check
// condition, parser.ParseExpr's own ast.Expr) for the same input. It also
// verifies — as a separate, generic proof, and again against the actual,
// current, unmodified selfhost/expr.fct — that a NEW proc in a NEW file can
// call an imported file's proc via ordinary `do`, through the compiler's
// real multi-file import/compile.File path. It imports internal/parser,
// internal/ast, and internal/compile read-only — no .go file anywhere in
// the module is modified by this package.
package selfhost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"facet/internal/ast"
	"facet/internal/compile"
	"facet/internal/parser"
	"facet/runtime"
)

// loadParserApp compiles and boots selfhost/parser.fct through the same
// in-memory server pattern loadApp/loadExprApp already use. parser.fct is now
// a driver that imports parser_proc.fct/parser_entity.fct/parser_service.fct
// (the STAGE 1-17 split), so it must go through compile.File — the
// import-aware entry point — exactly like TestCrossFileDoCallComposition
// below already does for its own multi-file fixtures; compile.String
// explicitly rejects any source that declares imports.
func loadParserApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("parser.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/parser.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// postParserJSON mirrors postJSON/postExprJSON exactly, named separately
// only to avoid colliding with those files' own helpers in this shared
// package.
func postParserJSON(t *testing.T, ts *httptest.Server, action string, args ...any) map[string]any {
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

func textOrEmptyP(d map[string]any, key string) string {
	v, ok := d[key]
	if !ok {
		return ""
	}
	return v.(string)
}

func boolOrFalseP(d map[string]any, key string) bool {
	v, ok := d[key]
	if !ok {
		return false
	}
	return v.(bool)
}

func strSliceP(t *testing.T, d map[string]any, key string) []string {
	t.Helper()
	v, ok := d[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: expected []any, got %T (%v)", key, v, v)
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s[%d]: expected string, got %T (%v)", key, i, e, e)
		}
		out[i] = s
	}
	return out
}

func boolSliceP(t *testing.T, d map[string]any, key string) []bool {
	t.Helper()
	v, ok := d[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: expected []any, got %T (%v)", key, v, v)
	}
	out := make([]bool, len(arr))
	for i, e := range arr {
		b, ok := e.(bool)
		if !ok {
			t.Fatalf("%s[%d]: expected bool, got %T (%v)", key, i, e, e)
		}
		out[i] = b
	}
	return out
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func boolSliceEqual(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── parseProc header verification ──
//
// goProcFromSource compiles a full, real `.fct` app via the real,
// unmodified parser.Parse (never compile.String/ir.Build, so the check is
// purely at the parser layer, before any semantic validation the IR would
// add) and returns the one ast.Proc it declares. The body is always a bare
// `return` — parser.Parse's own parseProcBody never cross-checks a return
// statement's value against the proc's declared return type (that is an
// internal/ir/build.go concern), so a body's only real job here is being
// non-empty (parseProc rejects an empty body on its own).
func goProcFromSource(t *testing.T, headerLine string) (*ast.Proc, error) {
	t.Helper()
	src := "app A:\n    " + headerLine + "\n        return\n"
	app, err := parser.Parse(src)
	if err != nil {
		return nil, err
	}
	if len(app.Procs) != 1 {
		t.Fatalf("goProcFromSource(%q): expected exactly one proc, got %d", headerLine, len(app.Procs))
	}
	return app.Procs[0], nil
}

// validProcHeaders are real, varied `proc` declaration headers exercising
// every clause parseProc/parseSignature handles: no params, multiple
// params (plain/list/optional), a return type (plain/list), and a `uses`
// clause with one or several capabilities — plus the no-parens-at-all form
// parseSignature's own `open < 0` branch explicitly allows.
var validProcHeaders = []string{
	"proc Foo:",
	"proc Foo():",
	"proc Bar(x: int):",
	"proc Add(a: int, b: int) -> int:",
	"proc Widget(a: int, b: text) -> Widget:",
	"proc Listy(xs: [int]) -> [int]:",
	"proc Opt(a: int?, b: text?) -> bool:",
	"proc Mixed(a: int, b: [text], c: bool?) -> [int]:",
	"proc UsesOne() uses io.file:",
	"proc UsesMany(a: int) -> int uses io.file, io.net:",
	"proc RetOnly() -> money:",
	"proc RetDate() -> date:",
	"proc RetFloat() -> float:",
	"proc NoRetUses(a: int, b: bool) uses io.net:",
}

// TestProcHeaderMatchesGoParser is the headline verification for this
// task's chosen slice: for each real, valid proc header, parser.fct's own
// end-to-end header procs (invoked live over HTTP) must agree exactly with
// the real, unmodified parser.Parse's own ast.Proc — name, every
// param's name/type/list/optional, the return type's core/list, and the
// `uses` capability list.
func TestProcHeaderMatchesGoParser(t *testing.T) {
	ts := loadParserApp(t)
	for _, header := range validProcHeaders {
		t.Run(header, func(t *testing.T) {
			wantProc, err := goProcFromSource(t, header)
			if err != nil {
				t.Fatalf("real Go parser.Parse rejected a header this test calls valid: %q: %v", header, err)
			}

			d := postParserJSON(t, ts, "runParseProcHeader", header)

			if got := boolOrFalseP(d, "headerValidResult"); !got {
				t.Errorf("headerValidResult = false, want true (real Go parser.Parse accepted %q)", header)
			}
			if got := textOrEmptyP(d, "headerNameResult"); got != wantProc.Name {
				t.Errorf("headerNameResult = %q, want %q", got, wantProc.Name)
			}

			var wantNames, wantTypes []string
			var wantLists, wantOptionals []bool
			for _, p := range wantProc.Params {
				wantNames = append(wantNames, p.Name)
				wantTypes = append(wantTypes, p.Type)
				wantLists = append(wantLists, p.List)
				wantOptionals = append(wantOptionals, p.Optional)
			}
			gotNames := strSliceP(t, d, "paramNamesResult")
			gotTypes := strSliceP(t, d, "paramTypesResult")
			gotLists := boolSliceP(t, d, "paramListsResult")
			gotOptionals := boolSliceP(t, d, "paramOptionalsResult")
			if !strSliceEqual(gotNames, wantNames) {
				t.Errorf("paramNamesResult = %v, want %v", gotNames, wantNames)
			}
			if !strSliceEqual(gotTypes, wantTypes) {
				t.Errorf("paramTypesResult = %v, want %v", gotTypes, wantTypes)
			}
			if !boolSliceEqual(gotLists, wantLists) {
				t.Errorf("paramListsResult = %v, want %v", gotLists, wantLists)
			}
			if !boolSliceEqual(gotOptionals, wantOptionals) {
				t.Errorf("paramOptionalsResult = %v, want %v", gotOptionals, wantOptionals)
			}

			wantHasRet := wantProc.Ret != ""
			if got := boolOrFalseP(d, "hasRetResult"); got != wantHasRet {
				t.Errorf("hasRetResult = %v, want %v (real Go Ret=%q)", got, wantHasRet, wantProc.Ret)
			}
			if got := textOrEmptyP(d, "retCoreResult"); wantHasRet && got != wantProc.Ret {
				t.Errorf("retCoreResult = %q, want %q", got, wantProc.Ret)
			}
			if got := boolOrFalseP(d, "retIsListResult"); wantHasRet && got != wantProc.RetList {
				t.Errorf("retIsListResult = %v, want %v", got, wantProc.RetList)
			}

			gotUses := strSliceP(t, d, "usesResult")
			if !strSliceEqual(gotUses, wantProc.Uses) {
				t.Errorf("usesResult = %v, want %v (real Go Uses)", gotUses, wantProc.Uses)
			}
		})
	}
}

// invalidProcHeaders are real proc headers the actual parseProc/
// parseSignature reject, one per distinct rejection gate: a missing `)`, a
// param missing its `:`, an invalid param name, an invalid param type, a
// missing/invalid return type after a bare arrow, an empty `uses` clause,
// and an invalid capability name.
var invalidProcHeaders = []string{
	"proc Foo(x: int:",                     // missing `)`
	"proc Foo(x) :",                        // param has no `:`
	"proc Foo(1x: int):",                   // invalid param name
	"proc Foo(x: 123):",                    // invalid param type
	"proc Foo() -> :",                      // empty return type
	"proc Foo() -> lowercase_bad_type():",  // not a valid type token (parens make it so)
	"proc Foo() uses :",                    // empty uses clause
	"proc Foo(x: int) uses 123bad:",        // invalid capability name
	"proc Foo(x: int) uses io.file, .bad:", // invalid (empty-part) capability name
}

// TestProcHeaderRejectsInvalidHeadersLikeGoParser proves the honest
// validity signal (headerValid) agrees with the real parser.Parse's own
// accept/reject verdict for genuinely malformed headers, one per real
// rejection gate.
func TestProcHeaderRejectsInvalidHeadersLikeGoParser(t *testing.T) {
	ts := loadParserApp(t)
	for _, header := range invalidProcHeaders {
		t.Run(header, func(t *testing.T) {
			_, err := goProcFromSource(t, header)
			if err == nil {
				t.Fatalf("real Go parser.Parse accepted a header this test calls invalid: %q", header)
			}
			d := postParserJSON(t, ts, "runParseProcHeader", header)
			if got := boolOrFalseP(d, "headerValidResult"); got {
				t.Errorf("headerValidResult = true for %q, want false (real Go parser.Parse rejects this: %v)", header, err)
			}
		})
	}
}

// ── parseCheck message-boundary verification ──
//
// goCheckFromSource compiles a full, real `.fct` app (an action with
// exactly one `check <cond> "<msg>"` body line) via the real, unmodified
// parser.Parse and returns the resulting ast.Check.
func goCheckFromSource(t *testing.T, checkLine string) (ast.Check, error) {
	t.Helper()
	src := "app A:\n    action doThing():\n        " + checkLine + "\n"
	app, err := parser.Parse(src)
	if err != nil {
		return ast.Check{}, err
	}
	if len(app.Actions) != 1 || len(app.Actions[0].Body) != 1 {
		t.Fatalf("goCheckFromSource(%q): expected exactly one action with one body statement", checkLine)
	}
	chk, ok := app.Actions[0].Body[0].(ast.Check)
	if !ok {
		t.Fatalf("goCheckFromSource(%q): body statement is %T, not ast.Check", checkLine, app.Actions[0].Body[0])
	}
	return chk, nil
}

// validCheckConditions are real check statements exercising: a simple
// comparison, a compound boolean condition, a message containing an
// escaped quote, and a condition with parens/arithmetic — genuinely varied
// real Go expr.go grammar the REAL parser.ParseExpr (not expr.fct's own
// subset) must accept, since the reference side here calls the real parser
// directly.
var validCheckStatements = []string{
	`check x > 0 "x must be positive"`,
	`check x > 0 && y < 10 "x and y must be in range"`,
	`check (a + b) * 2 == c "a plus b, doubled, must equal c"`,
	`check name != "" "a message with an escaped \"quote\" inside it"`,
	`check len(items) >= 1 "items must not be empty"`,
}

// TestCheckStatementMatchesGoParser verifies parser.fct's checkStatementValid/
// checkConditionText/checkMessageRaw against the real, unmodified
// parser.Parse's own ast.Check for real check statements — the condition
// span by re-parsing it with the real parser.ParseExpr and comparing the
// resulting ast.Expr to the real ast.Check.Cond with reflect.DeepEqual (so
// the boundary this file finds is proven EXACTLY right, not just
// "close"), and the message span by running Go's own strconv.Unquote over
// the raw span this file extracts and comparing to the real ast.Check.Msg
// (already decoded by the real, unmodified parseCheck/unquote chain).
func TestCheckStatementMatchesGoParser(t *testing.T) {
	ts := loadParserApp(t)
	for _, line := range validCheckStatements {
		t.Run(line, func(t *testing.T) {
			wantChk, err := goCheckFromSource(t, line)
			if err != nil {
				t.Fatalf("real Go parser.Parse rejected a check statement this test calls valid: %q: %v", line, err)
			}

			// s is exactly what parseCheck itself receives:
			// strings.TrimSpace(t[len("check "):]).
			s := strings.TrimSpace(strings.TrimPrefix(line, "check "))
			d := postParserJSON(t, ts, "runCheckStatement", s)

			if got := boolOrFalseP(d, "checkValidResult"); !got {
				t.Fatalf("checkValidResult = false, want true (real Go parser.Parse accepted %q)", line)
			}

			gotCond := textOrEmptyP(d, "checkConditionResult")
			gotCondExpr, err := parser.ParseExpr(gotCond)
			if err != nil {
				t.Fatalf("real Go parser.ParseExpr(%q) (this file's own extracted condition span) failed: %v", gotCond, err)
			}
			if !reflect.DeepEqual(gotCondExpr, wantChk.Cond) {
				t.Errorf("condition span %q re-parses to a DIFFERENT ast.Expr than the real check's own Cond:\n  got  %#v\n  want %#v", gotCond, gotCondExpr, wantChk.Cond)
			}

			gotMsgRaw := textOrEmptyP(d, "checkMessageResult")
			gotMsgDecoded, err := strconv.Unquote(gotMsgRaw)
			if err != nil {
				t.Fatalf("strconv.Unquote(%q) (this file's own extracted raw message span) failed: %v", gotMsgRaw, err)
			}
			if gotMsgDecoded != wantChk.Msg {
				t.Errorf("decoded message = %q, want %q (real Go ast.Check.Msg)", gotMsgDecoded, wantChk.Msg)
			}
		})
	}
}

// invalidCheckStatements are real check statements the actual parseCheck
// rejects: no trailing quoted message at all, and a condition-only message
// with nothing before it.
var invalidCheckStatements = []string{
	`check x > 0`, // no message at all
	`check "just a message with no condition before it"`,
}

func TestCheckStatementRejectsInvalidLikeGoParser(t *testing.T) {
	ts := loadParserApp(t)
	for _, line := range invalidCheckStatements {
		t.Run(line, func(t *testing.T) {
			_, err := goCheckFromSource(t, line)
			if err == nil {
				t.Fatalf("real Go parser.Parse accepted a check statement this test calls invalid: %q", line)
			}
			s := strings.TrimSpace(strings.TrimPrefix(line, "check "))
			d := postParserJSON(t, ts, "runCheckStatement", s)
			if got := boolOrFalseP(d, "checkValidResult"); got {
				t.Errorf("checkValidResult = true for %q, want false (real Go parser.Parse rejects this: %v)", line, err)
			}
		})
	}
}

// ── declKeywordOf (parseDecl's own prefix dispatch) verification ──

// declKeywordCases pairs a real top-level declaration line with the
// keyword parseDecl itself would dispatch on for it — read fresh off
// parseDecl's actual switch (internal/parser/parser.go), not assumed.
var declKeywordCases = map[string]string{
	"entity Post:":               "entity",
	"record Address:":            "record",
	"enum Status:":               "enum",
	"type Money2:":               "type",
	"message Ping:":              "message",
	"component Card(x: int):":    "component",
	"layout Main:":               "layout",
	"theme:":                     "theme",
	"theme dark:":                "theme",
	"theme Retro:":               "theme",
	"css:":                       "css",
	"state count: int = 0":       "state",
	"derive total: int = 0":      "derive",
	"policy admin:":              "policy",
	"action doThing():":          "action",
	"proc helper():":             "proc",
	"job cleanup every 30s -> x": "job",
	"service Payments:":          "service",
	"webhook stripe -> handle":   "webhook",
	"on user.created -> notify":  "on",
	"view Home at \"/\":":        "view",
	"auth":                       "auth",
	"auth:":                      "auth",
	"not a keyword at all":       "",
}

// TestDeclKeywordDispatchMatchesParseDecl verifies declKeywordOf's prefix
// classification against the actual keyword vocabulary parseDecl (parser.go)
// recognizes, confirmed by reading it fresh (see this file's own header,
// STAGE 7) rather than assumed from any summary.
func TestDeclKeywordDispatchMatchesParseDecl(t *testing.T) {
	ts := loadParserApp(t)
	for line, want := range declKeywordCases {
		t.Run(line, func(t *testing.T) {
			d := postParserJSON(t, ts, "runDeclKeyword", line)
			if got := textOrEmptyP(d, "declKeywordResult"); got != want {
				t.Errorf("declKeywordOf(%q) = %q, want %q", line, got, want)
			}
		})
	}
}

// ── Cross-file `do` composition: item 3's first bullet, checked live ──
//
// This task's brief specifically asked whether a NEW proc in a NEW file can
// genuinely call another file's already-working proc as a sub-routine,
// rather than assuming it. The two tests below check this for real, over
// the actual compiler's multi-file import path (internal/compile/
// compile.go's File + mergeInto), not compile.String (which explicitly
// rejects `import` — see compile.go's own doc — so this is the one place in
// this port that must use compile.File instead of the established
// compile.String pattern).

// TestCrossFileDoCallComposition is the generic proof, using two small,
// throwaway fixture files written to a t.TempDir() (never selfhost/ itself,
// so this test has zero coupling to any other file in this package or its
// future edits): a `lib.fct` declaring one plain proc, and a `consumer.fct`
// that imports it and calls it TWICE via ordinary `do`, chaining the first
// call's result into the second — proc-to-proc composition across a file
// boundary, through the real compiler, no different in kind from the
// same-file `do` composition every other file in this port already uses.
func TestCrossFileDoCallComposition(t *testing.T) {
	dir := t.TempDir()
	lib := `app Lib:
    proc double(x: int) -> int:
        return x * 2

    state unused: int = 0

    view LibHome at "/lib":
        box:
            text "{unused}"
`
	consumer := `import "lib.fct"

app Consumer:
    proc quad(x: int) -> int:
        let d = do double(x)
        let d2 = do double(d)
        return d2

    state result: int = 0

    action runQuad(x: int):
        let r = do quad(x)
        result = r

    view Home at "/":
        box:
            text "{result}"
`
	if err := os.WriteFile(filepath.Join(dir, "lib.fct"), []byte(lib), 0o644); err != nil {
		t.Fatal(err)
	}
	consumerPath := filepath.Join(dir, "consumer.fct")
	if err := os.WriteFile(consumerPath, []byte(consumer), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := compile.File(consumerPath)
	if err != nil {
		t.Fatalf("compile.File (cross-file import + do composition): %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	d := postParserJSON(t, ts, "runQuad", 5)
	if got := int(d["result"].(float64)); got != 20 {
		t.Errorf("cross-file do-composed quad(5) = %d, want 20 (5*2*2, via double() imported from a second file)", got)
	}
}

// TestCrossFileDoCallsRealExprFct is the SPECIFIC proof this task's brief
// asked for: not a generic double() stand-in, but the actual, current,
// unmodified selfhost/expr.fct, imported into a brand-new proc in a second
// file, calling its real serializeParsedExpr. This is the exact composition
// a future statement-level port (a `let name = <expr>` or `check <cond>
// "msg"` statement, both of which this file's own procs stop just short of
// full expression parsing on, by design — see this file's header) would
// need. expr.fct now imports expr_tokens.fct/expr_tree.fct (the STAGE 0-2f
// split; expr.fct itself only has the driver/demo layer), so all three are
// copied into the t.TempDir() byte-for-byte (os.ReadFile from the real
// files, os.WriteFile into the temp dir) rather than imported in place from
// selfhost/ itself, so this test's outcome reflects exactly what expr.fct
// provides AT THE TIME THIS TEST RUNS without creating a standing
// dependency of this file's own checked-in test suite on expr.fct's exact
// future proc surface (a concurrent effort is actively extending it in
// parallel with this task). `parseExprArena` (the old int-arena API this
// test used to call) no longer exists — expr.fct's parser now builds real
// `struct ExprNode` values, and serializeParsedExpr is the proc that
// flattens one into an HTTP/state-safe `[text]` list (see expr.fct's own
// serializeExprNode doc for why a struct can't cross that boundary
// directly); this test calls that current API instead of the arena one.
func TestCrossFileDoCallsRealExprFct(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"expr.fct", "expr_tokens.fct", "expr_tree.fct"} {
		raw, err := os.ReadFile(filepath.Join(name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	consumer := `import "expr.fct"

app Consumer2:
    proc serializedLen(src: text) -> int:
        let s = do serializeParsedExpr(src)
        return len(s)

    state result: int = 0

    action runSerializedLen(src: text):
        let r = do serializedLen(src)
        result = r

    view Home2 at "/c2":
        box:
            text "{result}"
`
	consumerPath := filepath.Join(dir, "consumer2.fct")
	if err := os.WriteFile(consumerPath, []byte(consumer), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := compile.File(consumerPath)
	if err != nil {
		t.Fatalf("compile.File (cross-file import of the real, current expr.fct): %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	d := postParserJSON(t, ts, "runSerializedLen", "1 + 2 * 3")
	// "1 + 2 * 3" parses to Bin("+", Lit(1), Bin("*", Lit(2), Lit(3))) — 5
	// nodes (3 int leaves + 2 bins), none carrying pairs/whereClause/sel,
	// each contributing a fixed 9-field header: 5 * 9 = 45.
	if got := int(d["result"].(float64)); got != 45 {
		t.Errorf("cross-file do-call into the real expr.fct's serializeParsedExpr(\"1 + 2 * 3\") flat length = %d, want 45", got)
	}
}
