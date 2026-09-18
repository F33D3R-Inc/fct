// This file records, as small standalone `.fct` apps compiled and run live
// (the same technique source_test.go and expr_test.go use), the answers to
// three composition questions the expr.fct port needed settled BEFORE any
// real tokenizer/parser code was written — see expr.fct's own header for
// the full write-up of why each one matters. Kept here permanently (not as
// throwaway scratch work) because each is a real, non-obvious fact about
// this language's proc semantics that the next person porting more of
// internal/parser will also need to know, and a live, compiling test is a
// more durable record of it than a comment asserting it.
package selfhost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// loadAppSrc compiles and boots an inline `.fct` app source string (rather
// than a file on disk, unlike loadApp/loadExprApp) — these tests each need
// their own small, throwaway app, not the shared source.fct/expr.fct this
// package otherwise exercises.
func loadAppSrc(t *testing.T, src string) *httptest.Server {
	t.Helper()
	g, err := compile.String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// postJSONSrc mirrors source_test.go's postJSON exactly, named separately
// to avoid colliding with that file's (and expr_test.go's) own helper of
// the same shape in this shared package.
func postJSONSrc(t *testing.T, ts *httptest.Server, action string, args ...any) map[string]any {
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
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned %d: %s", action, resp.StatusCode, b)
	}
	var out struct {
		OK     bool           `json:"ok"`
		Deltas map[string]any `json:"deltas"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("%s did not report ok: %s", action, b)
	}
	return out.Deltas
}

// TestInOperatorWorksOnTextArrayLiterals confirms `c in [...]` is a valid,
// working proc expression for TEXT elements (not just int/entity
// membership) — this is expr.fct's whole strategy for character-class
// tests (isDigitChar/isIdentStartChar), needed because there is no
// isDigit/isAlpha builtin and no lexicographic `<`/`<=` on text (every
// comparison operator in this language coerces through toInt/toFloat; see
// expr.fct's STAGE 0 doc).
func TestInOperatorWorksOnTextArrayLiterals(t *testing.T) {
	src := `app Scratch:
    proc isDigit(c: text) -> bool:
        return c in ["0", "1", "2", "3", "4", "5", "6", "7", "8", "9"]

    state result: bool = false

    action run(c: text):
        let r = do isDigit(c)
        result = r

    view Home at "/":
        box:
            text "{result}"
`
	ts := loadAppSrc(t, src)
	d := postJSONSrc(t, ts, "run", "5")
	if got, _ := d["result"].(bool); !got {
		t.Errorf("isDigit(\"5\") = %v, want true", d["result"])
	}
	d = postJSONSrc(t, ts, "run", "x")
	if got, ok := d["result"].(bool); ok && got {
		t.Errorf("isDigit(\"x\") = %v, want false", d["result"])
	}
}

// TestMutualRecursionBetweenProcs confirms two procs may call each other
// regardless of declaration order — internal/ir/build.go registers every
// proc's signature in one pass before building any body (see its own
// comment at the Procs section: "a proc may call another declared later in
// source order"), so a forward reference like isEven -> isOdd -> isEven
// resolves correctly. expr.fct's factorEnd/termEnd/exprEnd (and
// factorNodes/termNodes/exprNodes) rely on exactly this: exprEnd calls
// termEnd calls factorEnd calls exprEnd again for a parenthesized
// sub-expression, a genuine three-proc cycle, not just two.
func TestMutualRecursionBetweenProcs(t *testing.T) {
	src := `app Scratch:
    proc isEven(n: int) -> bool:
        if n == 0:
            return true
        let r = do isOdd(n - 1)
        return r

    proc isOdd(n: int) -> bool:
        if n == 0:
            return false
        let r = do isEven(n - 1)
        return r

    state result: bool = false

    action run(n: int):
        let r = do isEven(n)
        result = r

    view Home at "/":
        box:
            text "{result}"
`
	ts := loadAppSrc(t, src)
	d := postJSONSrc(t, ts, "run", 10)
	if got, _ := d["result"].(bool); !got {
		t.Errorf("isEven(10) = %v, want true", d["result"])
	}
	d = postJSONSrc(t, ts, "run", 7)
	if got, ok := d["result"].(bool); ok && got {
		t.Errorf("isEven(7) = %v, want false", d["result"])
	}
}

// TestSharedArrayThreadsAcrossSingleRecursiveChain is the headline question
// this porting task specifically flagged as a likely blocker: can a
// recursive proc build up ONE shared, growing array across nested
// recursive calls, purely through return-value threading (no mutable
// shared state — a proc parameter/return value is bound BY COPY, per
// LANGUAGE.md's proc section and runtime/eval.go's cloneArrayValue)?
//
// buildChain(remaining, arena) calls itself `remaining` times, each call
// appending one element to the array the PREVIOUS call actually returned
// (never a fresh `[]`) and handing the longer array down to the next call.
// If copy-on-call semantics broke this pattern, the result would be a
// 1-element list (each call's append lost when its copy went out of
// scope) instead of a 5-element one.
func TestSharedArrayThreadsAcrossSingleRecursiveChain(t *testing.T) {
	src := `app Scratch:
    proc buildChain(remaining: int, arena: [int]) -> [int]:
        if remaining == 0:
            return arena
        let mut a = arena
        a = append(a, 1)
        let r = do buildChain(remaining - 1, a)
        return r

    state result: [int] = []

    action run(n: int):
        let r = do buildChain(n, [])
        result = r

    view Home at "/":
        box:
            text "{result}"
`
	ts := loadAppSrc(t, src)
	d := postJSONSrc(t, ts, "run", 5)
	raw, ok := d["result"].([]any)
	if !ok || len(raw) != 5 {
		t.Fatalf("buildChain(5) result = %#v, want a 5-element list", d["result"])
	}
}

// TestSharedArrayThreadsAcrossTwoRecursiveCallsPerFrame is the harder
// version of the same question, and the one expr.fct's Bin{L,R} nodes
// actually depend on: a proc that recurses TWICE per call, threading the
// arena the FIRST recursive call returns into the SECOND recursive call's
// own input (so the second call's new nodes are appended after the
// first's, never colliding with or overwriting them) — exactly the shape
// "build the left subexpression's nodes, then build the right
// subexpression's nodes starting from whatever arena the left call handed
// back" needs.
//
// buildBinTree(depth, arena) appends a "self" marker (2) preorder, then
// recurses left (depth-1) then right (depth-1), threading the arena
// through both. depth=2 should visit 7 nodes total (1 + 2*(1+2)) in a
// specific, hand-checkable preorder sequence.
func TestSharedArrayThreadsAcrossTwoRecursiveCallsPerFrame(t *testing.T) {
	src := `app Scratch:
    proc buildBinTree(depth: int, arena: [int]) -> [int]:
        if depth == 0:
            let mut a = arena
            a = append(a, 1)
            return a
        let mut a = arena
        a = append(a, 2)
        let afterLeft = do buildBinTree(depth - 1, a)
        let afterRight = do buildBinTree(depth - 1, afterLeft)
        return afterRight

    state result: [int] = []

    action run(depth: int):
        let r = do buildBinTree(depth, [])
        result = r

    view Home at "/":
        box:
            text "{result}"
`
	ts := loadAppSrc(t, src)
	d := postJSONSrc(t, ts, "run", 2)
	raw, ok := d["result"].([]any)
	if !ok {
		t.Fatalf("buildBinTree(2) result = %#v, want a list", d["result"])
	}
	if len(raw) != 7 {
		t.Fatalf("buildBinTree(2) length = %d, want 7 (%v)", len(raw), raw)
	}
	want := []float64{2, 2, 1, 1, 2, 1, 1} // preorder: root, left(2,1,1), right(2,1,1)
	for i, w := range want {
		if raw[i].(float64) != w {
			t.Errorf("result[%d] = %v, want %v (full: %v)", i, raw[i], w, raw)
		}
	}
}
