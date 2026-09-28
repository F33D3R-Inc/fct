package runtime

// Float expression trees evaluate natively (proccompile.go's floatExpr):
// every result must be exactly what the boxed path (applyBin, callBuiltin,
// equal) gives — Go's IEEE arithmetic, division by zero included — and the
// errors of an out-of-range index unchanged.

import (
	"fmt"
	"io"
	"math"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

const floatNativeApp = `app A:
    struct P:
        x: float
        ys: [float]
    proc half(v: float) -> float:
        return v / 2.0
    proc series(n: int) -> [float]:
        let mut out = []
        let mut i = 0
        loop i < n:
            out = append(out, toFloat(i) * 0.5 - 1.25)
            i = i + 1
        return out
    proc run(a: float, b: float) -> text:
        let xs = do series(6)
        let p = P{x: a, ys: xs}
        let t1 = a * b + xs[3] * p.x - b / a
        let t2 = -(a - b) * (xs[5] + p.ys[1]) / 3.0
        let t3 = sqrt(a * a + b * b) + exp(a / 10.0) - ln(b + 3.0)
        let t4 = sin(a) * cos(b) + abs(xs[0] - 7.5)
        let t5 = toFloat(len(xs)) * half(b) + a / 0.0
        let t6 = (0.0 - a) / 0.0
        let lt = a * 2.0 < b + 1.0
        let eq = xs[2] == 0.0 - 0.25
        let ne = p.x * 1.0 != a
        let ge = t1 >= t2
        # a float from text and from a proc: evaluated as values
        let big = toFloat("1e20") * 2.0 + half(a)
        let fromText = sqrt(toFloat("16"))
        return "" + big + "|" + fromText + "|" + t1 + "|" + t2 + "|" + t3 + "|" + t4 + "|" + t5 + "|" + t6 + "|" + lt + "|" + eq + "|" + ne + "|" + ge
    proc oob(i: int) -> float:
        let xs = do series(3)
        return xs[i] * 2.0
    state result: text = ""
    action run(a: float, b: float):
        let r = do run(a, b)
        result = r
    action bad(i: int):
        let r = do oob(i)
        result = "" + r
    view Home at "/":
        box:
            text "{result}"
`

func TestFloatExpressionsNative(t *testing.T) {
	g, err := compile.String(floatNativeApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, ab := range [][2]float64{{1.5, -2.25}, {3.0, 0.5}, {-0.75, 4.0}} {
		a, b := ab[0], ab[1]
		xs := make([]float64, 6)
		for i := range xs {
			xs[i] = float64(i)*0.5 - 1.25
		}
		t1 := a*b + xs[3]*a - b/a
		t2 := -(a - b) * (xs[5] + xs[1]) / 3.0
		t3 := math.Sqrt(a*a+b*b) + math.Exp(a/10.0) - math.Log(b+3.0)
		t4 := math.Sin(a)*math.Cos(b) + math.Abs(xs[0]-7.5)
		t5 := float64(len(xs))*(b/2.0) + a/0.0
		t6 := (0.0 - a) / 0.0
		want := strings.Join([]string{toStr(1e20*2.0 + a/2.0), toStr(math.Sqrt(16)), toStr(t1), toStr(t2), toStr(t3), toStr(t4), toStr(t5), toStr(t6),
			fmt.Sprint(a*2.0 < b+1.0), fmt.Sprint(xs[2] == 0.0-0.25), fmt.Sprint(a*1.0 != a), fmt.Sprint(t1 >= t2)}, "|")
		deltas := postJSON(t, ts, "run", fmt.Sprintf(`{"args":[%v,%v]}`, a, b))
		if got := deltas["result"]; got != want {
			t.Fatalf("a=%v b=%v:\n got  %v\n want %s", a, b, got, want)
		}
	}
	r, err := postRaw(t, ts, "bad", `{"args":[5]}`)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), "array index 5 out of bounds (length 3)") {
		t.Fatalf("out-of-range float read: %d %s", r.StatusCode, body)
	}
}
