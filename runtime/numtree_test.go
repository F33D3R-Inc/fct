package runtime

// Arithmetic trees over operands of unknown type (proccompile.go's
// numExpr) evaluate as tagged numbers and box only their root; every node
// must answer exactly what applyBin does for the same two values.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

const numTreeApp = `app A:
    proc mixed() -> text:
        let xs = [7, 2.5, 0, -3, 4.0, "ab", 10]
        let m = {}
        let mut mm = m
        mm["k"] = 5
        mm["f"] = 1.5
        let a = xs[0] * xs[1] + xs[3]
        let b = (xs[0] + xs[6]) / xs[2]
        let c = (xs[0] - xs[3]) % xs[2] + xs[6] % xs[0]
        let d = -(xs[4] * xs[3]) - xs[6] / 4
        let e = xs[6] / 4 * 3.0
        let f = xs[1] % 2 + mm["k"] * mm["f"] - mm["k"] / 2
        let g = xs[5] + xs[0] * 2
        let h = xs[0] * 2 + xs[5]
        let i = (xs[1] + xs[4]) / (xs[2] + 0.0)
        let j = -(xs[0] + xs[3]) * -xs[6]
        return "" + a + "|" + b + "|" + c + "|" + d + "|" + e + "|" + f + "|" + g + "|" + h + "|" + i + "|" + j
    state result: text = ""
    action run:
        let r = do mixed()
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestNumericTreesMatchApplyBin(t *testing.T) {
	g, err := compile.String(numTreeApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	xs := []any{7, 2.5, 0, -3, 4.0, "ab", 10}
	ab := applyBin
	vals := []any{
		ab("+", ab("*", xs[0], xs[1]), xs[3]),
		ab("/", ab("+", xs[0], xs[6]), xs[2]),
		ab("+", ab("%", ab("-", xs[0], xs[3]), xs[2]), ab("%", xs[6], xs[0])),
		ab("-", negate(ab("*", xs[4], xs[3])), ab("/", xs[6], 4)),
		ab("*", ab("/", xs[6], 4), 3.0),
		ab("-", ab("+", ab("%", xs[1], 2), ab("*", 5, 1.5)), ab("/", 5, 2)),
		ab("+", xs[5], ab("*", xs[0], 2)),
		ab("+", ab("*", xs[0], 2), xs[5]),
		ab("/", ab("+", xs[1], xs[4]), ab("+", xs[2], 0.0)),
		ab("*", negate(ab("+", xs[0], xs[3])), negate(xs[6])),
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = toStr(v)
	}
	want := strings.Join(parts, "|")
	deltas := postJSON(t, ts, "run", `{"args":[]}`)
	if got := deltas["result"]; got != want {
		t.Fatalf("result = %v\nwant     %s", got, want)
	}
}
