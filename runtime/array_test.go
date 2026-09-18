package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// postRaw POSTs body to an action endpoint and returns the raw response,
// whatever its status — unlike proc_test.go's postJSON, which requires 200
// and fails the test otherwise. Used only for the out-of-bounds test below,
// which deliberately expects a non-2xx response.
func postRaw(t *testing.T, ts *httptest.Server, action, body string) (*http.Response, error) {
	t.Helper()
	return http.Post(ts.URL+"/api/"+action, "application/json", strings.NewReader(body))
}

// readBody drains and returns resp's body as a string.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// arraySumRunApp mirrors internal/compile/array_test.go's arraySumApp: a real
// two-pass array algorithm — build via `loop` + `append` (functional, each
// call returns a new array), then sum back via `len` + an indexed read in a
// second `loop` — proven live over HTTP, not just compiled.
const arraySumRunApp = `app A:
    proc sumSquares(n: int) -> int:
        let mut xs = []
        let mut i = 1
        loop i <= n:
            xs = append(xs, i * i)
            i = i + 1
        let mut total = 0
        let mut j = 0
        loop j < len(xs):
            total = total + xs[j]
            j = j + 1
        return total
    state result: int = 0
    action run(n: int):
        let r = do sumSquares(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestArraySumLive(t *testing.T) {
	g, err := compile.String(arraySumRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1^2 + 2^2 + 3^2 + 4^2 + 5^2 = 1+4+9+16+25 = 55. Wrong on almost any
	// plausible bug: an off-by-one in the build loop, `append` mutating in
	// place instead of growing, or an off-by-one in the index-read loop would
	// all land on a different wrong number, not accidentally 55.
	deltas := postJSON(t, ts, "run", `{"args":[5]}`)
	if got := toInt(deltas["result"]); got != 55 {
		t.Fatalf("sumSquares(5) over the wire = %v, want 55 (built via loop+append, summed via len+index read)", deltas["result"])
	}

	// n=0: the build loop never runs (1 <= 0 is false), so xs stays the empty
	// array the literal produced — len(xs) must be 0, not a stale/garbage
	// length, so the sum loop also never runs and total stays 0.
	deltas = postJSON(t, ts, "run", `{"args":[0]}`)
	if got := toInt(deltas["result"]); got != 0 {
		t.Fatalf("sumSquares(0) over the wire = %v, want 0 (empty array literal, both loops skip)", deltas["result"])
	}
}

// aliasApp is the mutation-semantics proof: a proc-local array is COPIED at
// every new binding (see runtime/eval.go's cloneArrayValue), matching this
// language's existing copy-on-read value model (an action's state cells are
// never shared references either) rather than Go's default slice-aliasing
// assignment. `let mut ys = xs` must NOT leave ys and xs sharing a backing
// array: mutating ys through an index-write must be invisible through xs.
const aliasApp = `app A:
    proc aliasTest() -> int:
        let xs = [1, 2, 3]
        let mut ys = xs
        ys[0] = 999
        return xs[0]
    state result: int = 0
    action run():
        let r = do aliasTest()
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestArrayValueSemanticsLive proves the copy-on-assign decision: if arrays
// were reference-typed (Go's default slice-assignment behavior), mutating
// ys[0] would also change xs[0] (they'd share one backing array) and this
// would return 999 instead of 1. Getting 1 back proves xs's own backing
// array was never touched.
func TestArrayValueSemanticsLive(t *testing.T) {
	g, err := compile.String(aliasApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "run", `{"args":[]}`)
	if got := toInt(deltas["result"]); got != 1 {
		t.Fatalf("xs[0] after mutating a `let ys = xs` copy = %v, want 1 (xs must be unaffected — arrays are copy-on-assign, not aliased)", deltas["result"])
	}
}

// oobApp indexes an array with a caller-supplied, out-of-bounds index — the
// one thing that can never be ruled out at compile time (bounds are
// data-dependent), so it must fail as a clean, catchable runtime error.
const oobApp = `app A:
    proc pick(n: int) -> int:
        let xs = [10, 20, 30]
        return xs[n]
    state result: int = 0
    action run(n: int):
        let r = do pick(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestArrayOutOfBoundsIsCleanError proves an out-of-bounds index ends the
// request as a proper error response — not a Go panic reaching the HTTP
// layer (which would crash the handler goroutine or hang the client) and not
// a silently wrong answer (e.g. 0 or nil masquerading as success). The
// server must also still be alive and answering afterward.
func TestArrayOutOfBoundsIsCleanError(t *testing.T) {
	g, err := compile.String(oobApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := postRaw(t, ts, "run", `{"args":[5]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("out-of-bounds access returned %d, want a 4xx/5xx error status", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "out of bounds") {
		t.Errorf("error body should mention the out-of-bounds index, got: %s", body)
	}

	// The server must still be alive and answering normally after the failed
	// call — proof this was a handled error, not a crash that took the
	// process (or this connection's goroutine) down with it.
	deltas := postJSON(t, ts, "run", `{"args":[1]}`)
	if got := toInt(deltas["result"]); got != 20 {
		t.Fatalf("server did not recover cleanly after the out-of-bounds call: pick(1) = %v, want 20", deltas["result"])
	}
}
