package selfhost

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

// ir_expr.fct's irExprJSON must spell an IRExpr exactly as encoding/json
// spells the real ir.Expr — same keys, same order, same omissions — because
// every other self-host port that produces IR is compared to the Go compiler
// through that string. The demo action builds `n + 1`; the Go side builds the
// identical ir.Expr and both are decoded back into ir.Expr for comparison,
// while the raw strings are also compared byte for byte.
func TestIRExprJSONMatchesEncodingJSON(t *testing.T) {
	g, err := compile.File(filepath.Join("ir_expr.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/ir_expr.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	d := postExprJSON(t, ts, "runIRDemo")
	got, _ := d["irDemo"].(string)

	want := &ir.Expr{Kind: "bin", Op: "+",
		L: &ir.Expr{Kind: "ref", Name: "n"},
		R: &ir.Expr{Kind: "lit", Val: 1, VType: "int"}}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(wantJSON) {
		t.Errorf("byte-for-byte mismatch:\n  got  %s\n  want %s", got, wantJSON)
	}
	var back ir.Expr
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("the port's JSON does not decode as ir.Expr: %v", err)
	}
	// Val decodes as float64 through interface{}; normalise before comparing.
	back.R.Val = int(back.R.Val.(float64))
	if !reflect.DeepEqual(&back, want) {
		t.Errorf("decoded IR differs: %+v vs %+v", back, want)
	}
}
