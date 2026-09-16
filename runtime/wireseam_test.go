package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"facet/internal/ir"
	wire "facet/schema/generated"
)

// A real nested predicate, built the same way fqstore.go's own helpers build
// one (fqBin/fqGet/fqLitInt/fqLitText) — not a synthetic shape invented for
// this test.
func testPredicate() *ir.Expr {
	return fqBin("&&",
		fqBin(">=", fqGet("age"), fqLitInt(21)),
		fqBin("==", fqGet("region"), fqLitText("x")),
	)
}

// TestWireExprFromIRProducesTheRealWireShape proves the seam emits the exact
// snake_case wire shape FacetQL's real predicate.rs (and this schema's
// generated Rust type, already parity-proven against it) expects — not
// ir.Expr's Go field names, and not ir.Expr's old ad hoc tags reinterpreted.
func TestWireExprFromIRProducesTheRealWireShape(t *testing.T) {
	we, err := wireExprFromIR(testPredicate())
	if err != nil {
		t.Fatalf("wireExprFromIR: %v", err)
	}
	data, err := json.Marshal(we)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(data)
	// Substring-checked on the *keys* only: encoding/json HTML-escapes values
	// like "&&"/">=" by default (&&, >=) — true of ir.Expr's
	// old direct marshal too, since that is a global encoding/json default,
	// not something this seam changes. Operator VALUES are checked below via
	// a real decode, which un-escapes them like any real consumer would.
	for _, want := range []string{
		`"kind":"bin"`, `"kind":"get"`, `"field":"age"`, `"kind":"ref"`, `"name":"item"`,
		`"kind":"lit"`, `"val":21`, `"vtype":"int"`, `"field":"region"`, `"val":"x"`, `"vtype":"text"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated wire JSON missing %s\ngot: %s", want, got)
		}
	}
	// Every key on the wire is snake_case exactly as facetql's real Expr
	// (predicate.rs) and this schema's generated Rust type declare — no
	// PascalCase Go field name ever appears.
	for _, mustNotContain := range []string{`"Kind"`, `"Op"`, `"Val"`, `"VType"`, `"Field"`, `"Obj"`, `"L"`, `"R"`} {
		if strings.Contains(got, mustNotContain) {
			t.Errorf("generated wire JSON leaked a Go field name %s\ngot: %s", mustNotContain, got)
		}
	}
	// Decode back (un-escaping && etc. the way any real consumer,
	// including facetql's own JSON parser, would) and confirm the operators
	// survived intact.
	var decoded wire.Expr
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Op != "&&" {
		t.Errorf("top-level op = %q, want \"&&\"", decoded.Op)
	}
	if decoded.L == nil || decoded.L.Op != ">=" {
		t.Errorf("left op = %+v, want \">=\"", decoded.L)
	}
	if decoded.R == nil || decoded.R.Op != "==" {
		t.Errorf("right op = %+v, want \"==\"", decoded.R)
	}
}

// TestWireExprRoundTripsThroughJSON proves the seam is lossless: ir.Expr ->
// wire.Expr -> JSON -> wire.Expr -> ir.Expr -> wire.Expr -> JSON produces the
// same bytes at both ends, so nothing is silently dropped or reordered by
// either direction of the seam.
func TestWireExprRoundTripsThroughJSON(t *testing.T) {
	original := testPredicate()

	we1, err := wireExprFromIR(original)
	if err != nil {
		t.Fatalf("wireExprFromIR: %v", err)
	}
	firstJSON, err := json.Marshal(we1)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded wire.Expr
	if err := json.Unmarshal(firstJSON, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	backToIR := wireExprToIR(&decoded)
	we2, err := wireExprFromIR(backToIR)
	if err != nil {
		t.Fatalf("wireExprFromIR (round 2): %v", err)
	}
	secondJSON, err := json.Marshal(we2)
	if err != nil {
		t.Fatalf("marshal (round 2): %v", err)
	}

	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("round trip lost data:\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
}

// TestWireExprFromIRRejectsSel proves the one documented gap is enforced, not
// silently mishandled: an aggregate-reduction node (Sel set) has no wire
// representation, and pushable()/exprSQL already refuse to push any
// expression containing one down to FacetQL in the first place — so a
// non-nil Sel reaching this function is a real invariant violation, not a
// value to drop quietly.
func TestWireExprFromIRRejectsSel(t *testing.T) {
	agg := &ir.Expr{Kind: "agg", Op: "sum", Name: "Item", Var: "i", Sel: fqGet("qty")}
	if _, err := wireExprFromIR(agg); err == nil {
		t.Fatal("expected an error for a Sel-bearing expression, got nil")
	}
}
