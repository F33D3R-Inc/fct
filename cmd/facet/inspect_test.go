package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/internal/parser"
)

// sampleApp is a small but representative app: auth, an entity, client + server
// state, a server action (uses now()), a policy, and two views. It exercises
// every summary section without needing imports (so compile.String works).
const sampleApp = `app Demo:
    auth

    entity Post:
        id: int
        author: text
        body: text
        created: int

    state draft: text = "" @client
    state pageSize: int = 20

    derive postCount: int = count(Post)

    policy mine(id: int):
        actor == Post(id).author

    action post(body: text):
        add Post { author: actor, body: body, created: now() }

    action remove(id: int):
        requires mine(id)
        remove Post(id)

    view Home at "/":
        box:
            text "{postCount} posts"
            button "post" -> post(draft)

    view One at "/p/:id":
        box:
            text "post"
`

func mustCompile(t *testing.T) *ir.IR {
	t.Helper()
	g, err := compile.String(sampleApp)
	if err != nil {
		t.Fatalf("sample app must compile: %v", err)
	}
	return g
}

func TestBuildInspect_Summary(t *testing.T) {
	g := mustCompile(t)
	r := buildInspect(g)

	if r.App != "Demo" {
		t.Errorf("app = %q, want Demo", r.App)
	}
	if !r.Auth {
		t.Error("auth should be true")
	}
	// `auth` injects the built-in FacetUser entity and signup/login/… actions, so
	// assert on the presence of the author's own declarations, not exact totals.
	if r.Counts["views"] != 2 {
		t.Errorf("views = %d, want 2", r.Counts["views"])
	}
	names := func(get func() []string) map[string]bool {
		m := map[string]bool{}
		for _, n := range get() {
			m[n] = true
		}
		return m
	}
	entNames := names(func() (out []string) {
		for _, e := range r.Entities {
			out = append(out, e.Name)
		}
		return
	})
	if !entNames["Post"] {
		t.Errorf("Post entity missing from %v", entNames)
	}
	var post EntitySummary
	for _, e := range r.Entities {
		if e.Name == "Post" {
			post = e
		}
	}
	if post.Fields != 4 {
		t.Errorf("Post fields = %d, want 4", post.Fields)
	}
	actNames := names(func() (out []string) {
		for _, a := range r.Actions {
			out = append(out, a.Name)
		}
		return
	})
	if !actNames["post"] || !actNames["remove"] {
		t.Errorf("author actions missing from %v", actNames)
	}

	// State placement must survive: draft is @client, pageSize is server.
	placement := map[string]string{}
	for _, s := range r.States {
		placement[s.Name] = s.Placement
	}
	if placement["draft"] != ir.Client {
		t.Errorf("draft placement = %q, want client", placement["draft"])
	}
	if placement["pageSize"] != ir.Server {
		t.Errorf("pageSize placement = %q, want server", placement["pageSize"])
	}

	// A parameterized route must carry its param.
	var one ViewSummary
	for _, v := range r.Views {
		if v.Name == "One" {
			one = v
		}
	}
	if one.Path != "/p/:id" {
		t.Errorf("One path = %q, want /p/:id", one.Path)
	}
	if len(one.Params) != 1 || one.Params[0] != "id" {
		t.Errorf("One params = %v, want [id]", one.Params)
	}
}

func TestBuildInspect_JSONRoundTrips(t *testing.T) {
	g := mustCompile(t)
	r := buildInspect(g)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back InspectReport
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.App != r.App || len(back.Actions) != len(r.Actions) {
		t.Errorf("round-trip mismatch: %+v vs %+v", back, r)
	}
}

func TestWriteInspectText_ContainsSections(t *testing.T) {
	g := mustCompile(t)
	var sb strings.Builder
	writeInspectText(&sb, buildInspect(g))
	out := sb.String()
	for _, want := range []string{"Demo", "ENTITIES", "STATE", "ACTIONS", "VIEWS", "Post", "TOTALS"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect text missing %q; got:\n%s", want, out)
		}
	}
}

func TestMarshalIR_CompactVsIndented(t *testing.T) {
	g := mustCompile(t)
	compact, err := marshalIR(g, true)
	if err != nil {
		t.Fatal(err)
	}
	indented, err := marshalIR(g, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compact), "\n") {
		t.Error("compact IR should be single-line")
	}
	if !strings.Contains(string(indented), "\n  ") {
		t.Error("indented IR should contain two-space indentation")
	}
	// Both must decode to the same graph.
	var a, b ir.IR
	if err := json.Unmarshal(compact, &a); err != nil {
		t.Fatalf("compact decode: %v", err)
	}
	if err := json.Unmarshal(indented, &b); err != nil {
		t.Fatalf("indented decode: %v", err)
	}
	if a.App != b.App || a.App != "Demo" {
		t.Errorf("decoded app mismatch: %q / %q", a.App, b.App)
	}
}

func TestDiagnosticsFor_ParserError(t *testing.T) {
	// A syntactically broken app: missing the leading `app` header.
	_, err := compile.String("entity Post:\n    id: int\n")
	if err == nil {
		t.Fatal("expected a compile error")
	}
	diags := diagnosticsFor("bad.fct", err)
	if len(diags) != 1 {
		t.Fatalf("want 1 diagnostic, got %d", len(diags))
	}
	d := diags[0]
	if d.File != "bad.fct" {
		t.Errorf("file = %q", d.File)
	}
	if d.Severity != "error" {
		t.Errorf("severity = %q", d.Severity)
	}
	if d.Line < 1 {
		t.Errorf("line = %d, want >= 1", d.Line)
	}
	if d.Message == "" {
		t.Error("message should not be empty")
	}
}

func TestDiagnosticsFor_SemanticError(t *testing.T) {
	// Compiles as a parse but fails IR build: references an unknown name.
	_, err := compile.String("app X:\n    derive n: int = count(Nope)\n")
	if err == nil {
		t.Fatal("expected a build error")
	}
	diags := diagnosticsFor("x.fct", err)
	if len(diags) != 1 || diags[0].Line < 1 {
		t.Fatalf("bad diagnostics: %+v", diags)
	}
}

func TestErrLocation_TypedErrors(t *testing.T) {
	// parser.Error carries an explicit line that must survive errors.As even when
	// wrapped the way compile.File wraps parse errors.
	pe := &parser.Error{Line: 7, Msg: "boom"}
	wrapped := errors.New("app.fct: " + pe.Error())
	// Direct typed error:
	if line, msg := errLocation(pe); line != 7 || msg != "boom" {
		t.Errorf("typed parser.Error: got line %d msg %q", line, msg)
	}
	// Fallback string parsing ("line N: msg"):
	if line, _ := errLocation(errors.New("line 12: nope")); line != 12 {
		t.Errorf("fallback line parse: got %d, want 12", line)
	}
	_ = wrapped

	// ir.BuildError via errors.As:
	be := &ir.BuildError{Line: 3, Msg: "bad ref"}
	if line, msg := errLocation(be); line != 3 || msg != "bad ref" {
		t.Errorf("typed ir.BuildError: got line %d msg %q", line, msg)
	}
}

func TestHasFlag(t *testing.T) {
	if !hasFlag([]string{"app.fct", "--json"}, "--json", "-json") {
		t.Error("should find --json")
	}
	if hasFlag([]string{"app.fct"}, "--json") {
		t.Error("should not find --json")
	}
	if hasFlag([]string{"--jsonx"}, "--json") {
		t.Error("must match verbatim, not prefix")
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote — enough to assert on the shape of a command's output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// writeTempApp writes src to a .fct file in a temp dir and returns its path.
func writeTempApp(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "app.fct")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCmdCheck_OKAndJSON(t *testing.T) {
	file := writeTempApp(t, sampleApp)

	// Human form: exit 0.
	if code := captureCode(t, func() int { return cmdCheck([]string{file}) }); code != 0 {
		t.Errorf("check exit = %d, want 0", code)
	}
	// JSON form: {ok:true, diagnostics:[]}.
	var out string
	code := captureCodeOut(t, &out, func() int { return cmdCheck([]string{file, "--json"}) })
	if code != 0 {
		t.Fatalf("check --json exit = %d, want 0", code)
	}
	var res CheckResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("check --json not valid JSON: %v\n%s", err, out)
	}
	if !res.OK || len(res.Diagnostics) != 0 {
		t.Errorf("check --json result = %+v", res)
	}
}

func TestCmdCheck_FailJSON(t *testing.T) {
	file := writeTempApp(t, "app X:\n    derive n: int = count(Nope)\n")
	var out string
	code := captureCodeOut(t, &out, func() int { return cmdCheck([]string{file, "--json"}) })
	if code != 1 {
		t.Fatalf("failing check exit = %d, want 1", code)
	}
	var res CheckResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if res.OK || len(res.Diagnostics) != 1 || res.Diagnostics[0].Line < 1 {
		t.Errorf("failing check result = %+v", res)
	}
}

func TestCmdCheck_NoFile(t *testing.T) {
	if code := cmdCheck([]string{"--json"}); code != 2 {
		t.Errorf("check without file exit = %d, want 2", code)
	}
}

func TestCmdIR_EmitsJSON(t *testing.T) {
	file := writeTempApp(t, sampleApp)
	var out string
	code := captureCodeOut(t, &out, func() int { return cmdIR([]string{file, "--compact"}) })
	if code != 0 {
		t.Fatalf("ir exit = %d, want 0", code)
	}
	var g ir.IR
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &g); err != nil {
		t.Fatalf("ir output not valid IR JSON: %v", err)
	}
	if g.App != "Demo" {
		t.Errorf("ir app = %q, want Demo", g.App)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Error("--compact should be single-line")
	}
}

func TestCmdInspect_JSON(t *testing.T) {
	file := writeTempApp(t, sampleApp)
	var out string
	code := captureCodeOut(t, &out, func() int { return cmdInspect([]string{file, "--json"}) })
	if code != 0 {
		t.Fatalf("inspect exit = %d, want 0", code)
	}
	var r InspectReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("inspect --json not valid: %v\n%s", err, out)
	}
	if r.App != "Demo" || r.Counts["views"] != 2 {
		t.Errorf("inspect report = %+v", r)
	}
}

// captureCode runs a command func, discarding stdout, and returns its exit code.
func captureCode(t *testing.T, fn func() int) int {
	t.Helper()
	var code int
	captureStdout(t, func() { code = fn() })
	return code
}

// captureCodeOut runs a command func, capturing stdout into *out, and returns the code.
func captureCodeOut(t *testing.T, out *string, fn func() int) int {
	t.Helper()
	var code int
	*out = captureStdout(t, func() { code = fn() })
	return code
}

func TestFirstNonFlag(t *testing.T) {
	if got := firstNonFlag([]string{"--json", "app.fct", "--compact"}); got != "app.fct" {
		t.Errorf("firstNonFlag = %q, want app.fct", got)
	}
	if got := firstNonFlag([]string{"--json"}); got != "" {
		t.Errorf("firstNonFlag = %q, want empty", got)
	}
}
