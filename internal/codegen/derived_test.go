package codegen

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

// Brick 5 of docs/REACTIVITY.md: a computed `what:` field whose roots are all
// signals (or earlier such fields) is client-derived — recomputed in the browser.
// A computed field that touches a plain `what:` input prop stays server-only.

type manifestDerived struct {
	Facets []struct {
		Derived  []struct{ Name, Expr string } `json:"derived"`
		Bindings []struct {
			ID, Expr string
		} `json:"bindings"`
	} `json:"facets"`
}

func compileForDerived(t *testing.T, src string) (*Output, manifestDerived) {
	t.Helper()
	facets, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var m manifestDerived
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	return out, m
}

func TestPureSignalComputedIsClientDerived(t *testing.T) {
	out, m := compileForDerived(t, `facet Poll:
    state:
        yes = 0
        no = 0
    what:
        total = yes + no
    looks:
        <p>{total}</p>
`)
	f := m.Facets[0]

	// total is recorded as a client-derived value.
	if len(f.Derived) != 1 || f.Derived[0].Name != "total" || f.Derived[0].Expr != "yes + no" {
		t.Fatalf("derived = %+v, want [{total, yes + no}]", f.Derived)
	}
	// {total} is a live binding (it reaches signals transitively), and the server
	// bakes the derived's initial value into the first paint via $total.
	if len(f.Bindings) != 1 || f.Bindings[0].Expr != "total" {
		t.Fatalf("bindings = %+v, want one for total", f.Bindings)
	}
	if !strings.Contains(out.Templates["Poll"], `$total := (add 0 0)`) {
		t.Errorf("derived initial-paint definition missing:\n%s", out.Templates["Poll"])
	}
}

// A computed field that mixes a signal with a plain what: field is server-only:
// the client lacks the server value, so it is neither client-derived nor a live
// binding (it stays a server-rendered hole).
func TestMixedComputedIsServerOnly(t *testing.T) {
	_, m := compileForDerived(t, `facet Cart:
    what:
        unit_price: int
        total = unit_price * qty
    state:
        qty = 1
    looks:
        <p>{total}</p>
`)
	f := m.Facets[0]
	if len(f.Derived) != 0 {
		t.Errorf("derived = %+v, want none (total needs a server field)", f.Derived)
	}
	if len(f.Bindings) != 0 {
		t.Errorf("bindings = %+v, want none (total is server-rendered)", f.Bindings)
	}
}

// A chain of derived values resolves in declaration order.
func TestChainedDerived(t *testing.T) {
	_, m := compileForDerived(t, `facet Chain:
    state:
        n = 2
    what:
        doubled = n * 2
        quad = doubled * 2
    looks:
        <p>{quad}</p>
`)
	f := m.Facets[0]
	if len(f.Derived) != 2 || f.Derived[0].Name != "doubled" || f.Derived[1].Name != "quad" {
		t.Fatalf("derived chain = %+v, want [doubled, quad] in order", f.Derived)
	}
}
