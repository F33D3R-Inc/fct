package compile

import (
	"testing"

	"facet/internal/ir"
)

// Regression tests for two `set` gaps once documented as broken in
// apps/storefront/gaps/*.fct and confirmed fixed in the current parser/build:
// a bulk filtered `set` (set-no-where.fct) and a nested entity lookup as a
// `set` target's key (set-nested-key.fct). Both lock in current behavior at
// the level `TestRemoveFilteredLowering` (remove_test.go) already checks for
// the sibling `remove … where` feature: the compiled IR shape of the action
// body.

func findAction(t *testing.T, g *ir.IR, name string) ir.Action {
	t.Helper()
	for _, a := range g.Actions {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("action %q not found", name)
	return ir.Action{}
}

// GAP 2 (fixed) — `set p in Entity where cond: field = expr` bulk-updates every
// row a predicate accepts in one statement, the filtered addressing mode
// `remove … where` already had. It must lower to a `set` Stmt carrying the item
// variable, the predicate, and a block of field assignments — and no by-id key.
const setWhereApp = `app SW:
    entity Product:
        id: int
        stock: int
    entity CartLine:
        id: int
        product: Product
    action checkout():
        set p in Product where exists(l in CartLine where l.product == p.id):
            stock = p.stock - 1
    view V at "/":
        for p in Product by id:
            text "{p.stock}"
`

func TestSetFilteredLowering(t *testing.T) {
	g, err := String(setWhereApp)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	act := findAction(t, g, "checkout")
	if len(act.Body) != 1 {
		t.Fatalf("want 1 stmt, got %d", len(act.Body))
	}
	st := act.Body[0]
	if st.Op != "set" || st.Entity != "Product" {
		t.Fatalf("want a filtered set on Product, got op=%q entity=%q", st.Op, st.Entity)
	}
	if st.Var != "p" {
		t.Errorf("want item var p, got %q", st.Var)
	}
	if st.Where == nil {
		t.Error("filtered set lost its predicate")
	}
	if st.Key != nil {
		t.Error("a filtered set should carry no by-id key")
	}
	if len(st.Fields) != 1 || st.Fields[0].Name != "stock" {
		t.Fatalf("want one assignment to stock, got %+v", st.Fields)
	}
}

// GAP 4 (fixed) — a `set` target's key is a whole expression, matched by paren
// depth rather than split on the first `)`, so a nested entity lookup —
// `Product(CartLine(lid).product).stock = 0` — parses as one lookup inside
// another, exactly as the identical text already does inside `check`.
const setNestedKeyApp = `app SNK:
    entity Product:
        id: int
        stock: int
    entity CartLine:
        id: int
        product: Product
    action zero(lid: int):
        set Product(CartLine(lid).product).stock = 0
    view V at "/":
        for p in Product by id:
            text "{p.stock}"
`

func TestSetByIDNestedEntityLookupKey(t *testing.T) {
	g, err := String(setNestedKeyApp)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	act := findAction(t, g, "zero")
	if len(act.Body) != 1 {
		t.Fatalf("want 1 stmt, got %d", len(act.Body))
	}
	st := act.Body[0]
	if st.Op != "set" || st.Entity != "Product" || st.Field != "stock" {
		t.Fatalf("want set Product(...).stock, got op=%q entity=%q field=%q", st.Op, st.Entity, st.Field)
	}
	if st.Key == nil {
		t.Fatal("the nested entity lookup key was lost")
	}
	if st.Key.Kind != "eget" || st.Key.Name != "CartLine" || st.Key.Field != "product" {
		t.Fatalf("want the key to be CartLine(lid).product, got %+v", st.Key)
	}
	if st.Key.Key == nil {
		t.Fatal("the inner CartLine(lid) lookup lost its own key")
	}
}
