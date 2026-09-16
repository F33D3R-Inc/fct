package runtime

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// These pin the pushdown contract (pushdown.go) that survives regardless of
// which store backend is running — region.go's pushable and fqStore's own
// at-rest encoding both depend on it agreeing with itself.

var post = ir.Entity{Name: "Post", Fields: []ir.Field{
	{Name: "id", Type: "int"},
	{Name: "author", Type: "text"},
	{Name: "likes", Type: "int", Index: true},
}}

func TestColumns(t *testing.T) {
	got := columns(post)
	want := []string{"id", "author", "likes"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("columns = %v, want %v", got, want)
	}
}

func TestExprSQL(t *testing.T) {
	// where p.likes > 0
	where := &ir.Expr{Kind: "bin", Op: ">",
		L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "likes"},
		R: &ir.Expr{Kind: "lit", Val: 0, VType: "int"}}
	args := []any{}
	got, err := exprSQL(where, "p", &args)
	if err != nil {
		t.Fatal(err)
	}
	if got != `("likes" > $1)` {
		t.Errorf("exprSQL = %q, want (\"likes\" > $1)", got)
	}
	if len(args) != 1 || args[0] != int64(0) {
		t.Errorf("args = %#v, want [0]", args)
	}

	// == maps to =, != maps to <>, && maps to AND.
	eq := &ir.Expr{Kind: "bin", Op: "&&",
		L: &ir.Expr{Kind: "bin", Op: "==",
			L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "to"},
			R: &ir.Expr{Kind: "lit", Val: 1, VType: "int"}},
		R: &ir.Expr{Kind: "bin", Op: "!=",
			L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "body"},
			R: &ir.Expr{Kind: "lit", Val: "", VType: "text"}}}
	args = []any{}
	got, _ = exprSQL(eq, "p", &args)
	if !strings.Contains(got, `"to" = $1`) || !strings.Contains(got, `"body" <> $2`) || !strings.Contains(got, " AND ") {
		t.Errorf("exprSQL compound = %q", got)
	}

	// a reference it cannot push down is an error, not wrong SQL.
	bad := &ir.Expr{Kind: "ref", Name: "draft"}
	if _, err := exprSQL(bad, "p", &args); err == nil {
		t.Error("exprSQL should reject a non-pushable reference")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := encodeCursor(int64(42), 7)
	ov, id, ok := decodeCursor(c)
	if !ok || id != 7 || toInt(ov) != 42 {
		t.Errorf("cursor round-trip: ov=%v id=%d ok=%v", ov, id, ok)
	}
	// a text order value survives too.
	c = encodeCursor("ada", 3)
	ov, id, ok = decodeCursor(c)
	if !ok || id != 3 || ov != "ada" {
		t.Errorf("text cursor round-trip: ov=%v id=%d ok=%v", ov, id, ok)
	}
	if _, _, ok := decodeCursor("!!!not-base64!!!"); ok {
		t.Error("a malformed cursor should not decode")
	}
}
