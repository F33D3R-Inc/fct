package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// A relation field's on-delete modifier — `@restrict` or `@setNull` — is a new
// annotation on an entity field, spelled exactly like every other bare field
// flag (`@secret`, `@unique`, ...). Unmarked relations keep the historical,
// sole behavior: cascade. This is the language-level half of the fix for
// apps/storefront/gaps/relation-always-cascades.fct — every entity relation
// used to lower to ON DELETE CASCADE with no way to say otherwise.
const onDeleteApp = `app Shop:
    entity Product:
        id: int
        name: text
    entity CartLine:
        id: int
        product: Product @restrict
        qty: int
    entity Review:
        id: int
        product: Product? @setNull
        body: text
    entity Order:
        id: int
        product: Product
        qty: int
    action addLine(id: int, qty: int):
        add CartLine { product: id, qty: qty }
    view Home at "/":
        box:
            text "{count(Product)}"
`

func TestRelationOnDeleteLowers(t *testing.T) {
	g, err := String(onDeleteApp)
	if err != nil {
		t.Fatalf("a relation with @restrict/@setNull should compile, got: %v", err)
	}
	got := map[string]map[string]string{} // entity -> field -> OnDelete
	for _, e := range g.Entities {
		got[e.Name] = map[string]string{}
		for _, f := range e.Fields {
			if f.IsRelation() {
				got[e.Name][f.Name] = f.OnDelete
			}
		}
	}
	cases := []struct{ entity, field, want string }{
		{"CartLine", "product", "restrict"},
		{"Review", "product", "setNull"},
		{"Order", "product", ""}, // no annotation: the historical default, cascade
	}
	for _, tc := range cases {
		if got := got[tc.entity][tc.field]; got != tc.want {
			t.Errorf("%s.%s.OnDelete = %q, want %q", tc.entity, tc.field, got, tc.want)
		}
	}

	// ir.References is the single derivation the runtime cascades along and the
	// stores declare to their engines — it has to carry the same modifier.
	byField := map[string]ir.Reference{}
	for _, r := range ir.References(g.Entities) {
		byField[r.Entity+"."+r.Field] = r
	}
	if r := byField["CartLine.product"]; r.OnDelete != "restrict" {
		t.Errorf("References()[CartLine.product].OnDelete = %q, want restrict", r.OnDelete)
	}
	if r := byField["Review.product"]; r.OnDelete != "setNull" {
		t.Errorf("References()[Review.product].OnDelete = %q, want setNull", r.OnDelete)
	}
	if r := byField["Order.product"]; r.OnDelete != "" {
		t.Errorf("References()[Order.product].OnDelete = %q, want \"\" (cascade)", r.OnDelete)
	}
}

func TestRelationOnDeleteErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"both @restrict and @setNull",
			`app A:
    entity Product:
        id: int
    entity Line:
        id: int
        product: Product? @restrict @setNull
    view H at "/":
        box:
            text "y"`,
			"cannot be both @restrict and @setNull",
		},
		{
			"@setNull without optional",
			`app A:
    entity Product:
        id: int
    entity Line:
        id: int
        product: Product @setNull
    view H at "/":
        box:
            text "y"`,
			"not optional",
		},
		{
			"@restrict on a non-relation field",
			`app A:
    entity Line:
        id: int
        qty: int @restrict
    view H at "/":
        box:
            text "y"`,
			"not a relation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := String(tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got: %v", tc.want, err)
			}
		})
	}
}
