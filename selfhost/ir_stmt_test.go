package selfhost

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

// ir_stmt.fct's irStmtJSON must spell an IRStmt exactly as encoding/json
// spells the real ir.Stmt. The demo builds a whole-cart checkout loop — a
// `for` carrying a check, a by-id set, a bound add and an if/else — and the
// Go side builds the identical ir.Stmt; the two strings must match byte for
// byte.
func TestIRStmtJSONMatchesEncodingJSON(t *testing.T) {
	g, err := compile.File(filepath.Join("ir_stmt.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/ir_stmt.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	d := postExprJSON(t, ts, "runIRStmtDemo")
	got, _ := d["irStmtDemo"].(string)

	ref := func(n string) *ir.Expr { return &ir.Expr{Kind: "ref", Name: n} }
	get := func(o *ir.Expr, f string) *ir.Expr { return &ir.Expr{Kind: "get", Obj: o, Field: f} }
	lit := func(v int) *ir.Expr { return &ir.Expr{Kind: "lit", Val: v, VType: "int"} }
	l := ref("l")
	qty := get(l, "qty")
	product := get(l, "product")
	stockOf := &ir.Expr{Kind: "eget", Name: "Product", Key: product, Field: "stock"}
	want := ir.Stmt{Op: "for", Entity: "CartLine", Var: "l", Order: "id",
		Where: &ir.Expr{Kind: "bin", Op: "==", L: get(l, "owner"), R: ref("actor")},
		Body: []ir.Stmt{
			{Op: "check", Value: &ir.Expr{Kind: "bin", Op: ">=", L: stockOf, R: qty}, Msg: "sold out"},
			{Op: "set", Entity: "Product", Field: "stock", Key: product, Value: &ir.Expr{Kind: "bin", Op: "-", L: stockOf, R: qty}},
			{Op: "add", Entity: "Order", Fields: []ir.FieldInit{{Name: "buyer", Expr: ref("actor")}, {Name: "qty", Expr: qty}}, Bind: "oid"},
			{Op: "if", Value: &ir.Expr{Kind: "bin", Op: ">", L: qty, R: lit(10)},
				Body: []ir.Stmt{{Op: "assign", Target: "lastOrder", Value: ref("oid")}},
				Else: []ir.Stmt{{Op: "assign", Target: "lastOrder", Value: lit(0)}}},
		}}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(wantJSON) {
		t.Errorf("byte-for-byte mismatch:\n  got  %s\n  want %s", got, wantJSON)
	}
}
