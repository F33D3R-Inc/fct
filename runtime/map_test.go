package runtime

import (
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// mapFreqRunApp mirrors internal/compile/map_test.go's mapFreqApp: a real
// frequency-counter algorithm — build a fixed five-value array (proc
// parameters cannot be array/map-typed in this milestone, so the test data
// is built from scratch inside the proc body, exactly like array_test.go's
// arraySumApp does for its own loop+append proof), then count each value's
// occurrences into a map with a single `loop` pass, relying on a missing
// key's nil read (this milestone's chosen semantics, see runtime/eval.go's
// "index" case) to increment a not-yet-seen key with no separate guard.
// `distinctCount` proves the OTHER aggregate shape the task calls out —
// `len(counts)`, the number of distinct keys — over the exact same data,
// live over HTTP, not just compiled.
const mapFreqRunApp = `app A:
    proc freqOf(a: int, b: int, c: int, d: int, e: int, target: int) -> int:
        let xs = [a, b, c, d, e]
        let mut counts = {}
        let mut i = 0
        loop i < len(xs):
            counts[xs[i]] = counts[xs[i]] + 1
            i = i + 1
        return counts[target]
    proc distinctCount(a: int, b: int, c: int, d: int, e: int) -> int:
        let xs = [a, b, c, d, e]
        let mut counts = {}
        let mut i = 0
        loop i < len(xs):
            counts[xs[i]] = counts[xs[i]] + 1
            i = i + 1
        return len(counts)
    state result: int = 0
    action run(a: int, b: int, c: int, d: int, e: int, target: int):
        let r = do freqOf(a, b, c, d, e, target)
        result = r
    action runDistinct(a: int, b: int, c: int, d: int, e: int):
        let r = do distinctCount(a, b, c, d, e)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestMapFreqCounterLive proves the map is actually accumulating counts over
// the wire, not just compiling: over [7, 3, 7, 7, 9], 7 occurs three times,
// 3 and 9 once each, and there are three distinct values. Any of the
// plausible bugs — the map not persisting across loop iterations (each pass
// getting a fresh empty map instead of the same accumulator), the missing-
// key-as-zero idiom actually panicking or returning something `+ 1` can't
// use, or `len` not being wired up for a map — would land on a different
// wrong number, not accidentally the right one.
func TestMapFreqCounterLive(t *testing.T) {
	g, err := compile.String(mapFreqRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "run", `{"args":[7,3,7,7,9,7]}`)
	if got := toInt(deltas["result"]); got != 3 {
		t.Fatalf("freqOf(7) over [7,3,7,7,9] = %v, want 3", deltas["result"])
	}
	deltas = postJSON(t, ts, "run", `{"args":[7,3,7,7,9,3]}`)
	if got := toInt(deltas["result"]); got != 1 {
		t.Fatalf("freqOf(3) over [7,3,7,7,9] = %v, want 1", deltas["result"])
	}
	deltas = postJSON(t, ts, "runDistinct", `{"args":[7,3,7,7,9]}`)
	if got := toInt(deltas["result"]); got != 3 {
		t.Fatalf("distinctCount([7,3,7,7,9]) over the wire = %v, want 3 (7, 3, 9)", deltas["result"])
	}
}

// mapAliasApp is the mutation-semantics proof: a proc-local map is COPIED at
// every new binding (see runtime/eval.go's cloneMapValue), matching this
// language's existing copy-on-assign value model — the same guarantee
// runtime/array_test.go's aliasApp proves for arrays, restated here for
// maps: `let mut m2 = m1` must NOT leave m1 and m2 sharing one backing map,
// so mutating m2 through an index-write must be invisible through m1.
const mapAliasApp = `app A:
    proc aliasTest() -> int:
        let m1 = {"a": 1}
        let mut m2 = m1
        m2["a"] = 999
        return m1["a"]
    state result: int = 0
    action run():
        let r = do aliasTest()
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestMapValueSemanticsLive proves the copy-on-assign decision for maps: if a
// map were reference-typed (Go's default map-assignment aliasing), mutating
// m2["a"] would also change m1["a"] (they would share one backing map) and
// this would return 999 instead of 1. Getting 1 back proves m1's own backing
// map was never touched.
func TestMapValueSemanticsLive(t *testing.T) {
	g, err := compile.String(mapAliasApp)
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
		t.Fatalf("m1[\"a\"] after mutating a `let m2 = m1` copy = %v, want 1 (m1 must be unaffected — maps are copy-on-assign, not aliased)", deltas["result"])
	}
}

// mapMissingKeyApp is the missing-key proving example: reading an absent key
// must answer a clean zero-value (nil, coerced to 0 for this proc's `int`
// return type), not a runtime error — the deliberate opposite of an array's
// out-of-bounds read (runtime/array_test.go's TestArrayOutOfBoundsIsCleanError),
// justified in runtime/eval.go's "index" case: a hash map's entire reason to
// exist is answering "is this key here", so a caller building one up must be
// able to read an as-yet-absent key with no guard.
const mapMissingKeyApp = `app A:
    proc lookup(key: text) -> int:
        let m = {"a": 1, "b": 2}
        return m[key]
    state result: int = 0
    action run(key: text):
        let r = do lookup(key)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestMapMissingKeyIsZeroValueNotErrorLive proves the missing-key decision
// two ways in one test: a present key ("a") answers its real value, and an
// absent key ("z") answers 0 — via postJSON, which itself fails the test on
// any non-200 response, so a passing test IS the proof this never became a
// clean (or unclean) error the way an out-of-bounds array read deliberately
// does.
func TestMapMissingKeyIsZeroValueNotErrorLive(t *testing.T) {
	g, err := compile.String(mapMissingKeyApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "run", `{"args":["a"]}`)
	if got := toInt(deltas["result"]); got != 1 {
		t.Fatalf(`lookup("a") over the wire = %v, want 1 (a present key)`, deltas["result"])
	}
	deltas = postJSON(t, ts, "run", `{"args":["z"]}`)
	if got := toInt(deltas["result"]); got != 0 {
		t.Fatalf(`lookup("z") over the wire = %v, want 0 (an absent key, answered as a clean zero value, not an error — postJSON would have failed this test on any non-200 response)`, deltas["result"])
	}
}

// mapBadKeyRuntimeApp is the key-type-restriction runtime backstop: reading
// `xs[0]` (an array element) has no statically-known type in this shallow
// checker (internal/ir/build.go's inferProcType has no case for ast.Index),
// so `m[xs[0]] = 1` compiles — the restriction can only be proven wrong once
// the bool inside xs is actually read out — and must fail as a clean runtime
// error (runtime/eval.go's mapKey) once it is, the same way an out-of-bounds
// array index does: a proper 4xx/5xx, not a Go panic reaching the HTTP layer
// and not a silently wrong answer. See internal/compile/map_test.go's
// TestMapDeclarationErrors for the (more common) compile-time half of this
// same restriction, where the key's bad type IS statically provable.
const mapBadKeyRuntimeApp = `app A:
    proc badKey(bad: bool) -> int:
        if bad:
            let xs = [true]
            let mut m = {}
            m[xs[0]] = 1
            return 0
        else:
            return 1
    state result: int = 0
    action run(bad: bool):
        let r = do badKey(bad)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestMapKeyTypeRestrictionRuntimeLive proves the runtime backstop: a bool
// value flowing into a map key through a path the compile-time checker
// cannot see through (an array-index read of unknown element type) still
// ends the request as a clean error naming the problem, and the server
// keeps working afterward — the same shape runtime/array_test.go's
// TestArrayOutOfBoundsIsCleanError proves for an out-of-bounds array index.
func TestMapKeyTypeRestrictionRuntimeLive(t *testing.T) {
	g, err := compile.String(mapBadKeyRuntimeApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := postRaw(t, ts, "run", `{"args":[true]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("a bool map key returned %d, want a 4xx/5xx error status", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "map key must be int or text") {
		t.Errorf("error body should name the map key restriction, got: %s", body)
	}

	// The server must still be alive and answering normally after the failed
	// call — proof this was a handled error, not a crash that took the
	// process (or this connection's goroutine) down with it. `bad: false`
	// takes badKey's other, harmless branch.
	deltas := postJSON(t, ts, "run", `{"args":[false]}`)
	if got := toInt(deltas["result"]); got != 1 {
		t.Fatalf("server did not recover cleanly after the bad-map-key call: badKey(false) = %v, want 1", deltas["result"])
	}
}
