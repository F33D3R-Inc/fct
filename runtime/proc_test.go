package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// procRunApp mirrors internal/compile/proc_test.go's procApp: a proc that
// builds a running total across a few `let mut` reassignments — real mutation,
// not just a single bind — called from an action via `do`, with the result
// flowing into a state cell a view renders. This is the live, end-to-end half
// of Milestone 1's proof: the compile test checks the IR shape, this checks the
// runtime actually computes and delivers the value.
const procRunApp = `app A:
    proc sum3(a: int, b: int, c: int) -> int:
        let mut total = a
        total = total + b
        total = total + c
        return total
    state result: int = 0
    action compute(x: int, y: int, z: int):
        let r = do sum3(x, y, z)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestProcComputesMutableLocalsLive proves the mutation is real, live, not just
// parsed: `let mut total = a`, then two reassignments, must return a + b + c —
// asserted both via the action's wire deltas (the API path a client patches
// its DOM from) and via the server-rendered page (the SSR path first paint
// uses), so the value is shown to actually reach the client both ways.
func TestProcComputesMutableLocalsLive(t *testing.T) {
	g, err := compile.String(procRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Post(ts.URL+"/api/compute", "application/json", strings.NewReader(`{"args":[1,2,3]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compute returned %d", resp.StatusCode)
	}
	var out struct {
		OK     bool           `json:"ok"`
		Deltas map[string]any `json:"deltas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("compute did not report ok: %+v", out)
	}
	// 1 + 2 + 3 = 6, computed entirely inside the proc's mutable running total —
	// not 1 (a param passthrough) or 3 (only the last reassignment applied).
	if got := toInt(out.Deltas["result"]); got != 6 {
		t.Fatalf("proc result over the wire = %v, want 6 (1+2+3 via a mutable running total)", out.Deltas["result"])
	}

	// The same value reaches first paint (server-rendered HTML), the other half
	// of "the client sees it": a fresh request with the same session cookie
	// re-renders the page from the now-updated state.
	page, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	bodyBytes, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	if !strings.Contains(body, "6") {
		t.Errorf("rendered page should show the proc's computed result (6), got: %s", body)
	}
}

// procIsolationApp deliberately names a proc-local the same as an app state
// cell (`total`) — the shape that would silently collide if a proc shared the
// action/session's flat scope map instead of getting its own frame.
const procIsolationApp = `app A:
    state total: int = 999
    state result: int = 0
    proc addOne(a: int) -> int:
        let mut total = a
        total = total + 1
        return total
    action bump(x: int):
        let r = do addOne(x)
        result = r
    view Home at "/":
        box:
            text "{result}"
            text "{total}"
`

// TestProcLocalsDoNotLeakIntoActionScope is the isolation proof the milestone
// asks for: a proc's own `total` local must never alias, overwrite, or leak
// (via wire deltas) the app's actual `total` state cell, even though the two
// share a name. The action calling the proc writes only `result`; `total`
// stays exactly what it was before the call, and only `result` ever appears
// as a delta.
func TestProcLocalsDoNotLeakIntoActionScope(t *testing.T) {
	g, err := compile.String(procIsolationApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Post(ts.URL+"/api/bump", "application/json", strings.NewReader(`{"args":[5]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bump returned %d", resp.StatusCode)
	}
	var out struct {
		OK     bool           `json:"ok"`
		Deltas map[string]any `json:"deltas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if got := toInt(out.Deltas["result"]); got != 6 {
		t.Fatalf("result over the wire = %v, want 6 (5+1, computed inside the proc's own frame)", out.Deltas["result"])
	}
	// The proc's local scratch variable — also named "total" — must never have
	// been written into the wire deltas: the action itself never assigns to the
	// state cell "total", so no delta for it should exist at all.
	if v, ok := out.Deltas["total"]; ok {
		t.Fatalf("proc-local %q leaked into the action's wire deltas: %v", "total", v)
	}

	// And server-side, the actual "total" state cell must be untouched — still
	// 999, not 6 and not 5 — proving the proc's frame is a genuinely separate
	// namespace from the session state map, not merely hidden from the wire.
	srv.mu.Lock()
	var sessTotal any
	for _, ses := range srv.sessions {
		if v, ok := ses.state["total"]; ok {
			sessTotal = v
		}
	}
	srv.mu.Unlock()
	if got := toInt(sessTotal); got != 999 {
		t.Fatalf("app state %q was mutated by the proc's same-named local: got %v, want 999 (untouched)", "total", sessTotal)
	}
}

// A proc calling another proc (both unconditionally server-executed, straight
// line only) composes the same way service calls to `do` are documented to.
const procCallsProcApp = `app A:
    proc double(a: int) -> int:
        let mut r = a
        r = r + a
        return r
    proc quad(a: int) -> int:
        let x = do double(a)
        let y = do double(x)
        return y
    state result: int = 0
    action run(x: int):
        let r = do quad(x)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcCallingProcLive(t *testing.T) {
	g, err := compile.String(procCallsProcApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/run", "application/json", strings.NewReader(`{"args":[3]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Deltas map[string]any `json:"deltas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// quad(3) = double(double(3)) = double(6) = 12.
	if got := toInt(out.Deltas["result"]); got != 12 {
		t.Fatalf("nested proc calls = %v, want 12 (double(double(3)))", out.Deltas["result"])
	}
}

// ── Milestone 2: loop/if control flow, live over HTTP ───────────────────────

// postJSON POSTs body to an action endpoint and decodes its wire deltas — the
// shared shape every Milestone 2 live test below asserts against.
func postJSON(t *testing.T, ts *httptest.Server, action, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/"+action, "application/json", strings.NewReader(body))
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

// factorialApp is the self-recursion proof: `fact` calls itself via `do fact(…)`
// (no cycle detection needed or wanted — the compiler proved the recursion
// terminates no more than a human reviewing it would; an author who writes
// infinite recursion gets a stack overflow, called out as acceptable since
// Milestone 1). Its if/else is also this milestone's return-completeness rule
// in its simplest form: both branches return, so the whole `if` is the proc's
// last (and only) statement.
const factorialApp = `app A:
    proc fact(n: int) -> int:
        if n <= 1:
            return 1
        else:
            let m = n - 1
            let r = do fact(m)
            return n * r
    state result: int = 0
    action run(n: int):
        let r = do fact(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcRecursiveFactorialLive(t *testing.T) {
	g, err := compile.String(factorialApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "run", `{"args":[5]}`)
	// 5! = 120, computed entirely by the proc calling itself.
	if got := toInt(deltas["result"]); got != 120 {
		t.Fatalf("fact(5) over the wire = %v, want 120", deltas["result"])
	}
}

// cartTotalApp is the canonical iterate-and-accumulate case this milestone
// exists to unblock — the literal scenario behind apps/storefront's
// action-no-loop gap: a checkout that must apply a per-row write (here, adding
// one line's price to a running total) across every row of a cart, which a
// straight-line action body has no way to express at all. `n` stands in for
// the cart's row count (Milestone 2 still has no array/list value type
// reachable from a proc parameter — see ROADMAP.md — so the count-based loop
// the task's own guidance calls out is the most faithful reproduction
// available today) and `price` the per-row amount charged; a real checkout
// proc would instead read each row's own price, but the accumulation shape —
// `let mut total` mutated once per pass through a `loop`, read back only after
// it exits — is identical.
const cartTotalApp = `app A:
    proc cartTotal(n: int, price: int) -> int:
        let mut total = 0
        let mut i = 0
        loop i < n:
            total = total + price
            i = i + 1
        return total
    state result: int = 0
    action checkout(n: int, price: int):
        let t = do cartTotal(n, price)
        result = t
    view Home at "/":
        box:
            text "{result}"
`

func TestProcLoopAccumulatesLive(t *testing.T) {
	g, err := compile.String(cartTotalApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 4 cart lines at 25 each = 100 — the `let mut total` accumulated inside the
	// loop, not a single reassignment (which would leave it at 25).
	deltas := postJSON(t, ts, "checkout", `{"args":[4,25]}`)
	if got := toInt(deltas["result"]); got != 100 {
		t.Fatalf("cartTotal(4, 25) over the wire = %v, want 100 (4 rows accumulated, not just the last one)", deltas["result"])
	}
}

// classifyApp is the if/else branch-selection proof: `classify` returns one of
// two computed text results depending on its condition. Asserted on BOTH sides
// of the branch over two separate calls, so a bug that always takes one arm
// (or hard-codes the value) cannot pass by accident.
const classifyApp = `app A:
    proc classify(x: int, limit: int) -> text:
        if x > limit:
            return "high"
        else:
            return "low"
    state result: text = ""
    action run(x: int, limit: int):
        let r = do classify(x, limit)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcIfElseBranchingLive(t *testing.T) {
	g, err := compile.String(classifyApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if got := postJSON(t, ts, "run", `{"args":[10,5]}`)["result"]; got != "high" {
		t.Fatalf("classify(10, limit 5) = %v, want \"high\"", got)
	}
	if got := postJSON(t, ts, "run", `{"args":[2,5]}`)["result"]; got != "low" {
		t.Fatalf("classify(2, limit 5) = %v, want \"low\"", got)
	}
}

// breakApp sums 1, 2, 3, … and stops the instant the running total would pass
// limit, via `break` — proving the loop actually stops (rather than only
// skipping the rest of one pass, which `continue` would do). Without `break`
// working, summing 1..n for n=100 would be 5050, not the small early-stopped
// value asserted below.
const breakApp = `app A:
    proc sumUntilOver(n: int, limit: int) -> int:
        let mut total = 0
        let mut i = 1
        loop i <= n:
            if total + i > limit:
                break
            total = total + i
            i = i + 1
        return total
    state result: int = 0
    action run(n: int, limit: int):
        let r = do sumUntilOver(n, limit)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcBreakLive(t *testing.T) {
	g, err := compile.String(breakApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1+2+3+4 = 10 (not over 10); adding 5 would make 15 > 10, so break fires
	// before that addition. n=100 makes the unbroken sum (5050) impossible to
	// confuse with the correct, early-stopped answer.
	deltas := postJSON(t, ts, "run", `{"args":[100,10]}`)
	if got := toInt(deltas["result"]); got != 10 {
		t.Fatalf("sumUntilOver(100, limit 10) over the wire = %v, want 10 (break must stop the loop early)", deltas["result"])
	}
}

// continueApp sums 1..n while skipping exactly one value via `continue` —
// proving continue abandons only the current pass (the skipped value never
// joins the total) without stopping the loop the way `break` does.
const continueApp = `app A:
    proc sumSkipping(n: int, skip: int) -> int:
        let mut total = 0
        let mut i = 1
        loop i <= n:
            if i == skip:
                i = i + 1
                continue
            total = total + i
            i = i + 1
        return total
    state result: int = 0
    action run(n: int, skip: int):
        let r = do sumSkipping(n, skip)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcContinueLive(t *testing.T) {
	g, err := compile.String(continueApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// sum(1..5) = 15; skipping 3 leaves 12. If `continue` didn't actually skip
	// the accumulation (e.g. it behaved like a no-op), this would be 15 instead.
	deltas := postJSON(t, ts, "run", `{"args":[5,3]}`)
	if got := toInt(deltas["result"]); got != 12 {
		t.Fatalf("sumSkipping(5, skip 3) over the wire = %v, want 12 (15 minus the skipped 3)", deltas["result"])
	}
}

// nestedReturnApp is the "return from anywhere" proof: `return` sits inside an
// `if` inside a `loop`, two levels of nesting deep. The running total crosses
// `limit` partway through a loop that runs up to n=1000 times; a correct
// implementation unwinds out of both the if and the loop the instant that
// happens and answers with the iteration count at that moment. A broken
// implementation — one where `return` only escaped its innermost block instead
// of the whole proc — would instead let the loop run to completion (the `if`
// keeps being true every subsequent pass, but a swallowed "return" would not
// stop iteration) and answer with the trailing `return -1` instead: the two
// outcomes cannot be confused with each other.
const nestedReturnApp = `app A:
    proc firstOver(n: int, limit: int) -> int:
        let mut total = 0
        let mut i = 0
        loop i < n:
            total = total + i
            if total > limit:
                return i
            i = i + 1
        return -1
    state result: int = 0
    action run(n: int, limit: int):
        let r = do firstOver(n, limit)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcNestedReturnUnwindsLive(t *testing.T) {
	g, err := compile.String(nestedReturnApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// total: 0,1,3,6,10,15 at i=0..5; 15 is the first value > 10, at i=5.
	deltas := postJSON(t, ts, "run", `{"args":[1000,10]}`)
	if got := toInt(deltas["result"]); got != 5 {
		t.Fatalf("firstOver(1000, limit 10) over the wire = %v, want 5 (return must unwind out of the if AND the loop, not fall through to -1)", deltas["result"])
	}
}
