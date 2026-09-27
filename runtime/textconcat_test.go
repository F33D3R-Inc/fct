package runtime

// A `+` chain headed by a text literal is one concatenation in the proc
// engine (proccompile.go's textConcat); this pins that it renders exactly
// as applyBin's step-by-step text + toStr(right) does for every operand
// kind, and that a failing operand still fails the whole expression.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

const textConcatApp = `app A:
    struct P:
        a: int
    proc render(n: int, f: float, ok: bool, s: text) -> text:
        let xs = [1, "two", 3]
        let m = {}
        let p = P{a: 4}
        let bs = textToBytes("hi")
        let two = "" + n
        let chain = "line " + n + ": " + s + " " + ok + " " + f + " " + xs + " " + p.a + " " + len(bs) + "|" + two + (n + 1) + "" + n * 2
        let list = [1] + [2]
        let sum = n + n + 1
        return chain + "#" + len(list) + "#" + sum
    proc failing(xs: [int], i: int) -> text:
        return "at " + i + " = " + xs[i]
    state result: text = ""
    action run(n: int, f: float, ok: bool, s: text):
        let r = do render(n, f, ok, s)
        result = r
    action fail(i: int):
        let r = do failing([7], i)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestTextConcatChainMatchesStepwise(t *testing.T) {
	g, err := compile.String(textConcatApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	deltas := postJSON(t, ts, "run", `{"args":[70000,2.5,true,"héllo"]}`)
	// Each part renders as toStr would: the int, the text, the bool's word,
	// the float, the list's rendering, the struct field, the byte count —
	// and `two + (n + 1)`: the parenthesized int sum, then `+ "" + n * 2`.
	want := "line 70000: héllo true 2.5 " + toStr([]any{1, "two", 3}) + " 4 2|7000070001140000#2#140001"
	if got := deltas["result"]; got != want {
		t.Fatalf("result = %v\nwant     %s", got, want)
	}
	resp, err := http.Post(ts.URL+"/api/fail", "application/json", strings.NewReader(`{"args":[3]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an out-of-bounds operand inside a text chain must fail the action")
	}
}
