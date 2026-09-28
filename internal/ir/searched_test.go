package ir_test

import (
	"reflect"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

// SearchedFields finds each text field a query tests case-insensitively by
// substring — through an aggregate in an action, a derive, a view's `for` —
// and nothing else: an unlowered test, a non-text field, a derive
// parameter that is not an entity row.
func TestSearchedFields(t *testing.T) {
	g, err := compile.String(`app S:
    entity Post:
        id: int
        body: text
        title: text
        slug: text
        n: int
    entity Person:
        id: int
        handle: text
        bio: text
    derive hits(q: text): int = count(p in Post where contains(lower(p.body), lower(q)))
    derive muted(body: text, w: text): bool = contains(lower(body), lower(w))
    action find(q: text) -> int:
        return count(x in Person where contains(lower(x.handle), lower(q)) && contains(x.bio, q))
    view Home at "/":
        for p in Post where contains(lower(p.title), "!") && contains(p.slug, "a"):
            text "{p.body}"
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := ir.SearchedFields(g)
	want := map[string][]string{"Post": {"body", "title"}, "Person": {"handle"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("searched = %v, want %v", got, want)
	}
}
