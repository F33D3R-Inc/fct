package runtime

import (
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

// boundsGuardApp is the exact motivating bug from this fix: the extremely
// common "is i in range, AND is the value at i the one I want" idiom. Before
// short-circuit evaluation, `xs[i]` was evaluated even when `i < len(xs)`
// already came back false, which is a hard runtime error (out-of-bounds
// index) rather than a clamp — so calling containsAt(xs, len(xs), target)
// used to blow up instead of cleanly answering false.
//
// Each scenario below gets its OWN action/state pair, deliberately
// defaulted to the OPPOSITE of what a correct call must produce. This
// project's delta protocol (runtime/server.go's "assign" case) only ships a
// state field in the response when its value actually CHANGES
// (`!sameValue(sess[st.Target], v)`); reusing one shared state field across
// several assertions in sequence would make a call that happens to
// reproduce the previous value silently vanish from deltas (indistinguishable
// from "nothing happened"), so distinct fields/defaults keep every assertion
// below unambiguous on its own.
const boundsGuardApp = `app A:
    proc containsAt(xs: [int], i: int, target: int) -> bool:
        if i < len(xs) && xs[i] == target:
            return true
        else:
            return false
    proc outOfRangeOr(xs: [int], i: int, target: int) -> bool:
        if i >= len(xs) || xs[i] == target:
            return true
        else:
            return false
    state andOOB: bool = true
    state andHit: bool = false
    state andMiss: bool = true
    state orOOB: bool = false
    state orHit: bool = false
    state orMiss: bool = true
    action runAndOOB(i: int, target: int):
        let r = do containsAt([10, 20, 30], i, target)
        andOOB = r
    action runAndHit(i: int, target: int):
        let r = do containsAt([10, 20, 30], i, target)
        andHit = r
    action runAndMiss(i: int, target: int):
        let r = do containsAt([10, 20, 30], i, target)
        andMiss = r
    action runOrOOB(i: int, target: int):
        let r = do outOfRangeOr([10, 20, 30], i, target)
        orOOB = r
    action runOrHit(i: int, target: int):
        let r = do outOfRangeOr([10, 20, 30], i, target)
        orHit = r
    action runOrMiss(i: int, target: int):
        let r = do outOfRangeOr([10, 20, 30], i, target)
        orMiss = r
    view Home at "/":
        box:
            text "{andOOB}"
            text "{andHit}"
            text "{andMiss}"
            text "{orOOB}"
            text "{orHit}"
            text "{orMiss}"
`

// TestBoundsGuardAndShortCircuitsLive is the single most important test in
// this fix: i == len(xs) (3, for a 3-element array) is deliberately
// out-of-bounds. Before short-circuiting, `i < len(xs) && xs[i] == target`
// evaluated xs[i] unconditionally and would have thrown a clean-but-fatal
// "array index out of bounds" error over the wire (see
// TestArrayOutOfBoundsIsCleanError for what that used to look like). After
// the fix, `i < len(xs)` is false, so `xs[i]` must never be evaluated at
// all, and the call must cleanly succeed with andOOB = false.
func TestBoundsGuardAndShortCircuitsLive(t *testing.T) {
	g, err := compile.String(boundsGuardApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "runAndOOB", `{"args":[3, 10]}`)
	if v, ok := deltas["andOOB"]; !ok || v != false {
		t.Fatalf("containsAt(xs, i=len(xs), target) = %v (ok=%v), want false with NO error — xs[i] must never be evaluated once i<len(xs) is false", v, ok)
	}

	// Sanity: an in-bounds hit still works normally (both operands legitimately
	// evaluated and both true).
	deltas = postJSON(t, ts, "runAndHit", `{"args":[1, 20]}`)
	if v := deltas["andHit"]; v != true {
		t.Fatalf("containsAt(xs, 1, 20) = %v, want true (20 is xs[1])", v)
	}

	// And an in-bounds miss.
	deltas = postJSON(t, ts, "runAndMiss", `{"args":[1, 999]}`)
	if v := deltas["andMiss"]; v != false {
		t.Fatalf("containsAt(xs, 1, 999) = %v, want false (xs[1] is 20, not 999)", v)
	}
}

// TestBoundsGuardOrShortCircuitsLive is the `||` mirror of the above: the
// guard is phrased as "either i is out of range, or xs[i] is the target" —
// meant to short-circuit past the unsafe read once the left side (i out of
// range) is already true. i == len(xs) is out of bounds, so the left side is
// true and the whole expression must answer true WITHOUT ever evaluating
// xs[i].
func TestBoundsGuardOrShortCircuitsLive(t *testing.T) {
	g, err := compile.String(boundsGuardApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "runOrOOB", `{"args":[3, 10]}`)
	if v, ok := deltas["orOOB"]; !ok || v != true {
		t.Fatalf("outOfRangeOr(xs, i=len(xs), target) = %v (ok=%v), want true with NO error — xs[i] must never be evaluated once i>=len(xs) is true", v, ok)
	}

	// In-bounds, value present: left side false, right side legitimately
	// evaluated and true.
	deltas = postJSON(t, ts, "runOrHit", `{"args":[1, 20]}`)
	if v := deltas["orHit"]; v != true {
		t.Fatalf("outOfRangeOr(xs, 1, 20) = %v, want true (xs[1] == 20)", v)
	}

	// In-bounds, value absent: both sides false.
	deltas = postJSON(t, ts, "runOrMiss", `{"args":[1, 999]}`)
	if v := deltas["orMiss"]; v != false {
		t.Fatalf("outOfRangeOr(xs, 1, 999) = %v, want false (in range, and xs[1] != 999)", v)
	}
}

// differentArrayGuardApp proves the right side of a short-circuited
// `&&`/`||` genuinely does not execute, not merely that it produces the
// right answer, using the approach this task's own verification bar
// suggests: a right-hand index into a SEPARATE, deliberately
// always-out-of-bounds array (ys, length 1, indexed at 999). A proc call is
// its own statement in this language (`do ProcName(...)`), so it cannot
// appear as a sub-expression of `&&`/`||` — a recursive-call-based proof
// isn't expressible here — but an out-of-bounds read is: ys[999] is a hard
// runtime error every single time it is actually evaluated, with no
// dependence on cond, so a clean (non-error) result is only possible if the
// right side was skipped.
const differentArrayGuardApp = `app A:
    proc guardAnd(cond: bool, ys: [int]) -> bool:
        if cond && ys[999] == 1:
            return true
        else:
            return false
    proc guardOr(cond: bool, ys: [int]) -> bool:
        if cond || ys[999] == 1:
            return true
        else:
            return false
    state andResult: bool = true
    state orResult: bool = false
    action runAnd(cond: bool):
        let r = do guardAnd(cond, [42])
        andResult = r
    action runOr(cond: bool):
        let r = do guardOr(cond, [42])
        orResult = r
    view Home at "/":
        box:
            text "{andResult}"
            text "{orResult}"
`

// TestShortCircuitRightSideNeverExecutes drives each guard with the value
// that must skip the always-out-of-bounds right side — false for `&&`
// (nothing can make a false-AND true, so ys[999] must never run), true for
// `||` (nothing can make a true-OR false, so ys[999] must never run either).
// If the right side were still being evaluated despite short-circuiting,
// ys[999] would throw "array index out of bounds" and postJSON (which
// requires a 200) would fail the test right there — a clean 200 with the
// statically-determined answer is only possible because the read never
// happened.
func TestShortCircuitRightSideNeverExecutes(t *testing.T) {
	g, err := compile.String(differentArrayGuardApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "runAnd", `{"args":[false]}`)
	if v := deltas["andResult"]; v != false {
		t.Fatalf("false && ys[999]==1 = %v, want false without ever reading ys[999]", v)
	}

	deltas = postJSON(t, ts, "runOr", `{"args":[true]}`)
	if v := deltas["orResult"]; v != true {
		t.Fatalf("true || ys[999]==1 = %v, want true without ever reading ys[999]", v)
	}

	// The non-short-circuited side of each guard still legitimately runs and
	// still fails the way TestArrayOutOfBoundsIsCleanError expects: cond=true
	// forces `&&` to evaluate ys[999], and cond=false forces `||` to do the
	// same. This confirms the fix is a genuine short-circuit (skip only when
	// already determined), not an accidental "never evaluate the right side
	// of &&/||" regression that would silently paper over real bugs elsewhere.
	resp, err := postRaw(t, ts, "runAnd", `{"args":[true]}`)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("true && ys[999]==1 must still evaluate ys[999] and fail out-of-bounds, got status %d", resp.StatusCode)
	}
	resp, err = postRaw(t, ts, "runOr", `{"args":[false]}`)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("false || ys[999]==1 must still evaluate ys[999] and fail out-of-bounds, got status %d", resp.StatusCode)
	}
}

// TestShortCircuitTruthTablePreservesResult is the correctness-preservation
// check this task calls for: when BOTH operands are safe/pure (the
// overwhelmingly common case), short-circuiting must never change the
// RESULT versus fully evaluating both sides — only skip evaluating the right
// side when the left side already determines the outcome. Exercised at the
// eval() level directly (mirrors runtime/eval_test.go's TestEval style)
// across all four boolean truth-table rows for both `&&` and `||`, and
// cross-checked against applyBin's own (non-short-circuiting) answer so a
// regression that changed the RESULT, not just the evaluation order, would
// be caught too.
func TestShortCircuitTruthTablePreservesResult(t *testing.T) {
	lit := func(v bool) *ir.Expr { return &ir.Expr{Kind: "lit", Val: v, VType: "bool"} }
	bin := func(op string, l, r *ir.Expr) *ir.Expr { return &ir.Expr{Kind: "bin", Op: op, L: l, R: r} }
	scope := map[string]any{}

	for _, op := range []string{"&&", "||"} {
		for _, l := range []bool{false, true} {
			for _, r := range []bool{false, true} {
				want := applyBin(op, l, r).(bool) // ground truth: both sides always evaluated
				got := eval(bin(op, lit(l), lit(r)), scope)
				if got != want {
					t.Errorf("eval(%v %s %v) = %v, want %v (applyBin ground truth)", l, op, r, got, want)
				}
			}
		}
	}
}

// TestShortCircuitTruthTablePreservesResultInFrame is the same
// correctness-preservation check, but through evalInFrame (the proc-body
// evaluator) instead of eval() (the action/view evaluator) — the two must
// never disagree, and both must match applyBin's ground truth when nothing
// is actually short-circuited away.
func TestShortCircuitTruthTablePreservesResultInFrame(t *testing.T) {
	lit := func(v bool) *ir.Expr { return &ir.Expr{Kind: "lit", Val: v, VType: "bool"} }
	bin := func(op string, l, r *ir.Expr) *ir.Expr { return &ir.Expr{Kind: "bin", Op: op, L: l, R: r} }
	fr := &frame{vars: map[string]any{}}
	s := &Server{}

	for _, op := range []string{"&&", "||"} {
		for _, l := range []bool{false, true} {
			for _, r := range []bool{false, true} {
				want := applyBin(op, l, r).(bool)
				got, err := s.evalInFrame(bin(op, lit(l), lit(r)), fr)
				if err != nil {
					t.Fatalf("evalInFrame(%v %s %v): unexpected error %v", l, op, r, err)
				}
				if got != want {
					t.Errorf("evalInFrame(%v %s %v) = %v, want %v (applyBin ground truth)", l, op, r, got, want)
				}
			}
		}
	}
}
