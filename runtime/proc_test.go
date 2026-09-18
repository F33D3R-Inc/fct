package runtime

import (
	"encoding/json"
	"fmt"
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

// ── Bitwise operators, live over HTTP ───────────────────────────────────────

// packRGBRunApp packs three 8-bit channels into one int via shift/or, and
// unpacks the red channel back out via shift/and — a straight-line proc
// exercising `<<`, `>>`, `|` and `&` together, the multi-operator idiom
// (`(r << 16) | (g << 8) | b` / `(packed >> 16) & 0xFF`) the task asked for.
const packRGBRunApp = `app A:
    proc pack(r: int, g: int, b: int) -> int:
        return (r << 16) | (g << 8) | b
    proc unpackRed(v: int) -> int:
        return (v >> 16) & 255
    state packedOut: int = 0
    state redOut: int = 0
    action doPack(r: int, g: int, b: int):
        let p = do pack(r, g, b)
        packedOut = p
    action doUnpack(v: int):
        let red = do unpackRed(v)
        redOut = red
    view Home at "/":
        box:
            text "{packedOut}"
            text "{redOut}"
`

// TestProcBitwisePackUnpackLive is the task's cross-check requirement in its
// most direct form: the expected value is computed in THIS test, using Go's
// own `<<`/`>>`/`|`/`&` on the identical input, and the live HTTP result from
// the compiled .fct proc must match it exactly — not a hardcoded, hand-derived
// "looks plausible" number.
func TestProcBitwisePackUnpackLive(t *testing.T) {
	g, err := compile.String(packRGBRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	r, gr, b := 200, 130, 45
	// Go's own bitwise operators over the identical input — the cross-check.
	wantPacked := (r << 16) | (gr << 8) | b
	wantRed := (wantPacked >> 16) & 0xFF

	deltas := postJSON(t, ts, "doPack", fmt.Sprintf(`{"args":[%d,%d,%d]}`, r, gr, b))
	if got := toInt(deltas["packedOut"]); got != wantPacked {
		t.Fatalf("pack(%d,%d,%d) over the wire = %v, want %d (Go's own (r<<16)|(g<<8)|b)", r, gr, b, deltas["packedOut"], wantPacked)
	}

	deltas = postJSON(t, ts, "doUnpack", fmt.Sprintf(`{"args":[%d]}`, wantPacked))
	if got := toInt(deltas["redOut"]); got != wantRed {
		t.Fatalf("unpackRed(%d) over the wire = %v, want %d (Go's own (packed>>16)&0xFF)", wantPacked, deltas["redOut"], wantRed)
	}
}

// crc8RunApp computes a bit-by-bit CRC-8 (polynomial 0x07, no reflection) over
// a single byte, entirely with `loop`/`if` and bitwise operators — the
// genuinely-needs-iteration algorithm the task asked for as an alternative to
// the straight-line pack/unpack example. It is the textbook software CRC-8
// inner loop: shift the running CRC left one bit at a time, XOR in the
// polynomial whenever the bit shifted out was a 1, masking back to 8 bits
// each pass.
const crc8RunApp = `app A:
    proc crc8(byteVal: int) -> int:
        let mut crc = byteVal & 255
        let mut i = 0
        loop i < 8:
            if (crc & 128) != 0:
                crc = (crc << 1) ^ 7
            else:
                crc = crc << 1
            crc = crc & 255
            i = i + 1
        return crc
    state result: int = 0
    action run(byteVal: int):
        let r = do crc8(byteVal)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// goCRC8 is the reference implementation of the identical bit-by-bit CRC-8
// crc8RunApp's proc computes, written with Go's own bitwise operators — the
// cross-check every live result below is asserted against.
func goCRC8(b int) int {
	crc := b & 0xFF
	for i := 0; i < 8; i++ {
		if crc&0x80 != 0 {
			crc = (crc << 1) ^ 0x07
		} else {
			crc = crc << 1
		}
		crc &= 0xFF
	}
	return crc
}

func TestProcCRC8Live(t *testing.T) {
	g, err := compile.String(crc8RunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, in := range []int{0x00, 0x01, 0xA5, 0xFF, 0x7B} {
		want := goCRC8(in)
		deltas := postJSON(t, ts, "run", fmt.Sprintf(`{"args":[%d]}`, in))
		if got := toInt(deltas["result"]); got != want {
			t.Fatalf("crc8(%#x) over the wire = %v, want %d (Go's own bit-by-bit CRC-8, same input)", in, deltas["result"], want)
		}
	}
}

// bitwiseEdgeApp isolates the two operators whose edge-case behavior this
// task asked to pin down explicitly: `<<` (shift by 0, by a negative amount,
// and by an amount at/beyond the operand's bit width) and `~` (a negative
// operand).
const bitwiseEdgeApp = `app A:
    proc shl(x: int, n: int) -> int:
        return x << n
    proc bnot(x: int) -> int:
        return ~x
    state result: int = 0
    action doShl(x: int, n: int):
        let r = do shl(x, n)
        result = r
    action doNot(x: int):
        let r = do bnot(x)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcBitwiseEdgeCasesLive(t *testing.T) {
	g, err := compile.String(bitwiseEdgeApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Shift by 0 is the identity.
	deltas := postJSON(t, ts, "doShl", `{"args":[1,0]}`)
	if got := toInt(deltas["result"]); got != 1 {
		t.Fatalf("1 << 0 over the wire = %v, want 1", deltas["result"])
	}

	// A shift amount at/beyond the operand's bit width is well-defined in Go
	// (unlike C's undefined behavior for this case) — shifted that many times,
	// every original bit is gone, so the result is 0. Computed via a runtime
	// variable (not a constant expression, which Go would refuse to compile as
	// an overflow) so this is Go's own runtime shift semantics, not a
	// hand-typed number.
	one, big := 1, 100
	wantBig := one << uint(big)
	deltas = postJSON(t, ts, "doShl", `{"args":[1,100]}`)
	if got := toInt(deltas["result"]); got != wantBig {
		t.Fatalf("1 << 100 over the wire = %v, want %d (Go's own semantics for an oversized shift)", deltas["result"], wantBig)
	}

	// A negative shift count is defined here as 0 — the same sentinel this
	// runtime already answers division/modulo by zero with (see applyBin's "/"
	// and "%" cases in runtime/eval.go) — rather than propagating Go's own
	// run-time panic for a negative shift count into the request.
	deltas = postJSON(t, ts, "doShl", `{"args":[1,-5]}`)
	if got := toInt(deltas["result"]); got != 0 {
		t.Fatalf("1 << -5 over the wire = %v, want 0 (a negative shift count is defined as 0, mirroring division by zero)", deltas["result"])
	}

	// Unary ~ on a negative operand: two's complement, ~(-1) == 0.
	deltas = postJSON(t, ts, "doNot", `{"args":[-1]}`)
	if got := toInt(deltas["result"]); got != 0 {
		t.Fatalf("~(-1) over the wire = %v, want 0", deltas["result"])
	}
	// The complementary edge, cross-checked against Go's own ^0: ~0 == -1.
	zero := 0
	wantNot0 := ^zero
	deltas = postJSON(t, ts, "doNot", `{"args":[0]}`)
	if got := toInt(deltas["result"]); got != wantNot0 {
		t.Fatalf("~0 over the wire = %v, want %d (Go's own ^0)", deltas["result"], wantNot0)
	}
}

// procConcatWorkaroundApp is the mechanical rewrite TestProcBodyLiteralBracesAreRefused
// (internal/compile/braces_test.go) proves the compiler now hands back for the exact
// motivating bug: a proc body composing a tag string from mixed int/text locals.
// `+` already stringifies an int operand against a text one (runtime/eval.go's "+"
// case, via toStr) — this is what a real app used instead of
// `"{slot}:{pid}:{score}"` once that silently dropped every brace pair at runtime,
// and this test proves the workaround was correct then and stays correct now that
// the interpolated form is a compile-time error instead of a silent no-op.
const procConcatWorkaroundApp = `app A:
    proc tag(slot: int, pid: int, score: int) -> text:
        return slot + ":" + pid + ":" + score
    state result: text = ""
    action run(slot: int, pid: int, score: int):
        let r = do tag(slot, pid, score)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestProcConcatWorkaroundStillWorksLive is this fix's other half, live: the proc
// body's own `+` concatenation must still compute the right text (this is not new
// behavior — the fix only closes the SILENT-WRONG path, it never touched `+`), and
// the view's own `{result}` interpolation — the position this whole rule exists to
// protect, not to break — must still render that computed value on first paint
// exactly as every other app in this codebase depends on it doing.
func TestProcConcatWorkaroundStillWorksLive(t *testing.T) {
	g, err := compile.String(procConcatWorkaroundApp)
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

	resp, err := client.Post(ts.URL+"/api/run", "application/json", strings.NewReader(`{"args":[3,42,99]}`))
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
	if got, want := fmt.Sprint(out.Deltas["result"]), "3:42:99"; got != want {
		t.Fatalf("proc tag result over the wire = %q, want %q (slot:pid:score via `+`, the mechanical fix for the dropped-interpolation bug)", got, want)
	}

	// Same value, server-rendered page: the view's `{result}` interpolation
	// (an actual, still-working interpolation position) must show it too.
	page, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	body, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "3:42:99") {
		t.Errorf("rendered page should show the proc's computed tag (3:42:99) via the view's {result} interpolation, got: %s", string(body))
	}
}
