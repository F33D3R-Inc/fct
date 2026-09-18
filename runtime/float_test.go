package runtime

import (
	"math"
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

// ── Verification item 1: a real floating-point algorithm, cross-checked ────
// ── against Go's own math, live over HTTP. ──────────────────────────────────

// weightedAverageApp computes a weighted average over five fixed float
// values with five fixed float weights — a real, non-trivial float
// computation (not just "add two floats"): a `loop`-driven accumulation of
// two running float totals (`num`/`den`), a float division, and a final
// `round()` back to int so the result can cross into the action/state layer
// (see LANGUAGE.md's `proc` section: float is proc-only, so the boundary
// value is always an int/text/bool/money/date). Every input is a float
// array literal read by index — proving float array elements, not just bare
// float locals.
//
// This is scaled by 1000000.0 before rounding so the fractional precision of
// the underlying division survives the round-trip through round()'s
// int-typed result — the test asserts on the scaled integer, then divides
// back down in the failure message for readability.
const weightedAverageApp = `app A:
    proc weightedAverage() -> int:
        let xs = [1.5, 2.5, 3.5, 4.5, 5.5]
        let ws = [0.1, 0.15, 0.2, 0.25, 0.3]
        let mut num = 0.0
        let mut den = 0.0
        let mut i = 0
        loop i < len(xs):
            num = num + xs[i] * ws[i]
            den = den + ws[i]
            i = i + 1
        let avg = num / den
        return round(avg * 1000000.0)
    proc dotProduct4() -> int:
        let a = [1.5, 2.0, 3.25, 4.0]
        let b = [2.0, 3.0, 4.0, 0.5]
        let mut sum = 0.0
        let mut i = 0
        loop i < len(a):
            sum = sum + a[i] * b[i]
            i = i + 1
        return round(sum * 1000.0)
    state avgScaled: int = 0
    state dotScaled: int = 0
    action run():
        let a = do weightedAverage()
        let d = do dotProduct4()
        avgScaled = a
        dotScaled = d
    view Home at "/":
        box:
            text "{avgScaled}"
            text "{dotScaled}"
`

// TestFloatWeightedAverageAndDotProductLive is this milestone's actual proof
// of reach: a real float algorithm (a weighted average's running-total
// accumulation over a `loop`, and a 4-element dot product), computed entirely
// inside a proc, delivered over HTTP, and cross-checked bit-for-bit against
// Go's own float64 arithmetic performing the IDENTICAL sequence of operations
// in the IDENTICAL order (floating-point addition is not associative, so
// order matters for an exact-equality check to be meaningful rather than
// approximate). If applyBin's float branches (runtime/eval.go) silently
// truncated through toInt anywhere in the accumulation, or evaluated the
// terms in a different order, this would very likely land on a different
// scaled integer, not accidentally the right one.
func TestFloatWeightedAverageAndDotProductLive(t *testing.T) {
	g, err := compile.String(weightedAverageApp)
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

	// Go's own computation of the identical expression over the identical
	// inputs, in the identical operation order.
	xs := []float64{1.5, 2.5, 3.5, 4.5, 5.5}
	ws := []float64{0.1, 0.15, 0.2, 0.25, 0.3}
	num, den := 0.0, 0.0
	for i := range xs {
		num = num + xs[i]*ws[i]
		den = den + ws[i]
	}
	avg := num / den
	wantAvgScaled := int(math.Round(avg * 1000000.0))

	a := []float64{1.5, 2.0, 3.25, 4.0}
	b := []float64{2.0, 3.0, 4.0, 0.5}
	sum := 0.0
	for i := range a {
		sum = sum + a[i]*b[i]
	}
	wantDotScaled := int(math.Round(sum * 1000.0))

	if got := toInt(deltas["avgScaled"]); got != wantAvgScaled {
		t.Fatalf("weighted average (x1000000) over the wire = %v, want %v (avg = %v vs Go's %v)",
			deltas["avgScaled"], wantAvgScaled, float64(got)/1000000.0, avg)
	}
	if got := toInt(deltas["dotScaled"]); got != wantDotScaled {
		t.Fatalf("dot product (x1000) over the wire = %v, want %v (dot = %v vs Go's %v)",
			deltas["dotScaled"], wantDotScaled, float64(got)/1000.0, sum)
	}
}

// ── Verification item 2: int/float mixing behaves exactly as designed ──────
// (a compile-time error, not a silent promotion). The declaration-error half
// of this proof (the exact error text) lives in
// internal/compile/float_test.go's TestFloatDeclarationErrors; this is the
// live counterpart proving the SAME source genuinely never reaches a running
// server at all — compile.String fails before NewInMemory ever gets called.

const intFloatMixApp = `app A:
    state result: int = 0
    proc bad() -> int:
        let x = 1 + 2.5
        return x
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`

func TestFloatIntMixNeverReachesARunningServer(t *testing.T) {
	if _, err := compile.String(intFloatMixApp); err == nil {
		t.Fatal("want a compile error for `1 + 2.5` (no automatic int/float promotion), got a clean compile")
	}
}

// TestFloatExplicitPromotionViaToFloatLive proves the OTHER half of the
// design decision: since there is no automatic promotion, `toFloat`/`toInt`
// are the real, working conversion path — an int argument converted to
// float, combined with a float computed inside the proc, then converted back
// to int. Two different inputs prove it is genuinely computing, not a
// hardcoded constant.
const toFloatConversionApp = `app A:
    proc scaleByHalf(n: int) -> int:
        let x = toFloat(n)
        let half = x * 0.5
        return round(half * 100.0)
    state result: int = 0
    action run(n: int):
        let r = do scaleByHalf(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestFloatExplicitPromotionViaToFloatLive(t *testing.T) {
	g, err := compile.String(toFloatConversionApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cases := []struct {
		n    int
		want int
	}{
		{7, int(math.Round(float64(7) * 0.5 * 100.0))},   // 350
		{3, int(math.Round(float64(3) * 0.5 * 100.0))},   // 150
		{-4, int(math.Round(float64(-4) * 0.5 * 100.0))}, // -200
	}
	for _, c := range cases {
		deltas := postJSON(t, ts, "run", `{"args":[`+itoa(c.n)+`]}`)
		if got := toInt(deltas["result"]); got != c.want {
			t.Fatalf("scaleByHalf(%d) over the wire = %v, want %d", c.n, deltas["result"], c.want)
		}
	}
}

// ── Verification item 3: floor/round/abs correctness, including a negative ─
// ── and an exact-half case, live over HTTP. ─────────────────────────────────

// floorRoundAbsApp exercises every numeric builtin's float branch in one
// action: floor toward negative infinity (not toward zero — floor(-2.7) is
// -3, not -2), round-half-away-from-zero (this milestone's chosen
// convention: round(2.5) is 3 and round(-2.5) is -3, matching Go's own
// math.Round — NOT round-half-to-even/banker's rounding, which would answer
// 2), a non-half case that should just round to the nearer int, and abs on
// both a float and an int input (proving abs is not float-only).
const floorRoundAbsApp = `app A:
    proc pFloorPos() -> int:
        return floor(2.7)
    proc pFloorNeg() -> int:
        return floor(-2.7)
    proc pRoundHalfUp() -> int:
        return round(2.5)
    proc pRoundHalfDown() -> int:
        return round(-2.5)
    proc pRoundNoHalf() -> int:
        return round(2.4)
    proc pAbsFloat() -> int:
        return round(abs(-3.5) * 10.0)
    proc pAbsInt() -> int:
        return abs(-7)
    state floorPos: int = 0
    state floorNeg: int = 0
    state roundHalfUp: int = 0
    state roundHalfDown: int = 0
    state roundNoHalf: int = 0
    state absFloat: int = 0
    state absInt: int = 0
    action run():
        let a = do pFloorPos()
        let b = do pFloorNeg()
        let c = do pRoundHalfUp()
        let d = do pRoundHalfDown()
        let e = do pRoundNoHalf()
        let f = do pAbsFloat()
        let g = do pAbsInt()
        floorPos = a
        floorNeg = b
        roundHalfUp = c
        roundHalfDown = d
        roundNoHalf = e
        absFloat = f
        absInt = g
    view Home at "/":
        box:
            text "{floorPos}"
`

func TestFloorRoundAbsLive(t *testing.T) {
	g, err := compile.String(floorRoundAbsApp)
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
	want := map[string]int{
		"floorPos":      2,  // floor(2.7)
		"floorNeg":      -3, // floor(-2.7) — toward -infinity, not toward zero
		"roundHalfUp":   3,  // round(2.5) — round-half-away-from-zero
		"roundHalfDown": -3, // round(-2.5)
		"roundNoHalf":   2,  // round(2.4)
		"absFloat":      35, // abs(-3.5) == 3.5, *10 == 35
		"absInt":        7,  // abs(-7) on a plain int, unaffected by float support
	}
	for k, w := range want {
		if got := toInt(deltas[k]); got != w {
			t.Errorf("%s over the wire = %v, want %d", k, deltas[k], w)
		}
	}
}

// ── Verification item 4: float rejected as a map key / bitwise operand ─────
// (the interop restrictions from the task's item 8). The primary proof is
// compile-time (internal/compile/float_test.go's TestFloatDeclarationErrors,
// "a float literal cannot be a map key" / "a bitwise operator refuses a
// float operand") — this is the live counterpart, proving the rejected
// source never reaches a running server.

const floatMapKeyApp = `app A:
    state result: int = 0
    proc bad() -> int:
        let m = {1.5: 1}
        return m[1.5]
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`

const floatBitwiseApp = `app A:
    state result: int = 0
    proc bad(x: float) -> int:
        return 1 & x
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`

func TestFloatRejectedAsMapKeyAndBitwiseOperandNeverReachARunningServer(t *testing.T) {
	if _, err := compile.String(floatMapKeyApp); err == nil {
		t.Fatal("want a compile error for a float map key, got a clean compile")
	}
	if _, err := compile.String(floatBitwiseApp); err == nil {
		t.Fatal("want a compile error for a float bitwise operand, got a clean compile")
	}
}
