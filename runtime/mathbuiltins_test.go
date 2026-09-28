package runtime

import (
	"math"
	"testing"

	"facet/internal/compile"
)

// exp, ln and sqrt are Go's math.Exp/Log/Sqrt on the authority: in an
// action, a proc and a derive an action evaluates.
func TestTranscendentalBuiltins(t *testing.T) {
	g, err := compile.String(`
app M:
    type R:
        e: float
        l: float
        s: float
        sig: float
        sn: float
        cs: float
    proc sigmoid(x: float) -> float:
        return 1.0 / (1.0 + exp(0.0 - x))
    action calc(x: float) -> R:
        let sg = do sigmoid(x)
        return R{e: exp(x), l: ln(x), s: sqrt(x), sig: sg, sn: sin(x), cs: cos(x)}
    view Home at "/":
        text "hi"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	got, err := srv.RunValue("ada", "member", true, "calc", []any{2.0})
	if err != nil {
		t.Fatal(err)
	}
	r := got.(map[string]any)
	for k, want := range map[string]float64{"e": math.Exp(2), "l": math.Ln2, "s": math.Sqrt2, "sig": 1 / (1 + math.Exp(-2)), "sn": math.Sin(2), "cs": math.Cos(2)} {
		if v := toFloat(r[k]); math.Abs(v-want) > 1e-12 {
			t.Errorf("%s = %v, want %v", k, v, want)
		}
	}
	// The browser has no such function: a client-placed use is refused.
	if _, err := compile.String(`
app M:
    state x: float = 1.0 @client
    view Home at "/":
        text "{exp(x)}"
`); err == nil {
		t.Errorf("exp in a client view must not compile")
	}
}
