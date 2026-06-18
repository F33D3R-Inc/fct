package codegen

import (
	"encoding/json"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

// Brick 2 of docs/REACTIVITY.md: the compiler records the client reactive graph
// in the manifest — the state signals (with initial values) and, per `{state}`
// interpolation, which signals feed it. These pin that recording; the Tier-1
// updater that consumes them is a later brick.

type manifestState struct {
	Facets []struct {
		Name  string `json:"name"`
		State []struct {
			Name, Type, Init string
		} `json:"state"`
		Bindings []struct {
			ID      string   `json:"id"`
			Signals []string `json:"signals"`
			Expr    string   `json:"expr"`
			Node    string   `json:"node"`
		} `json:"bindings"`
	} `json:"facets"`
}

func compileSrc(t *testing.T, src string) manifestState {
	t.Helper()
	facets, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var m manifestState
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	return m
}

func TestManifestRecordsStateAndBindings(t *testing.T) {
	m := compileSrc(t, `facet Counter:
    state:
        count: int = 0
        liked = false
    looks:
        <button>{count}</button>
        <span>{liked}</span>
`)
	if len(m.Facets) != 1 {
		t.Fatalf("facets = %d, want 1", len(m.Facets))
	}
	f := m.Facets[0]

	// state: both signals recorded with their initial values.
	if len(f.State) != 2 {
		t.Fatalf("state entries = %d, want 2: %+v", len(f.State), f.State)
	}
	if f.State[0].Name != "count" || f.State[0].Type != "int" || f.State[0].Init != "0" {
		t.Errorf("state[0] = %+v, want {count int 0}", f.State[0])
	}
	if f.State[1].Name != "liked" || f.State[1].Init != "false" {
		t.Errorf("state[1] = %+v, want {liked _ false}", f.State[1])
	}

	// bindings: one per state-driven interpolation, ids in document order.
	if len(f.Bindings) != 2 {
		t.Fatalf("bindings = %d, want 2: %+v", len(f.Bindings), f.Bindings)
	}
	b0 := f.Bindings[0]
	if b0.ID != "b0" || b0.Expr != "count" || b0.Node != "text" ||
		len(b0.Signals) != 1 || b0.Signals[0] != "count" {
		t.Errorf("binding[0] = %+v, want {b0 [count] count text}", b0)
	}
	b1 := f.Bindings[1]
	if b1.ID != "b1" || len(b1.Signals) != 1 || b1.Signals[0] != "liked" {
		t.Errorf("binding[1] = %+v, want {b1 [liked] liked text}", b1)
	}
}

// An interpolation that references no state signal is not a client binding — it
// stays a server-rendered hole.
func TestNonStateInterpolationIsNotBound(t *testing.T) {
	m := compileSrc(t, `facet Counter:
    what:
        title: string
    state:
        count = 0
    looks:
        <h1>{title}</h1>
        <button>{count}</button>
`)
	f := m.Facets[0]
	if len(f.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1 (only {count}): %+v", len(f.Bindings), f.Bindings)
	}
	if f.Bindings[0].Expr != "count" {
		t.Errorf("bound the wrong interpolation: %+v", f.Bindings[0])
	}
}

// An expression mixing a signal with a what: field is NOT a live binding in
// Brick 4: the client can't evaluate it without the server field, so it stays
// server-rendered (exposing what: fields client-side is a later brick). Only
// pure-signal interpolations become live bindings.
func TestMixedSignalExprIsNotLiveBinding(t *testing.T) {
	m := compileSrc(t, `facet Counter:
    what:
        base: int
    state:
        count = 0
    looks:
        <span>{base + count}</span>
        <span>{count}</span>
`)
	f := m.Facets[0]
	if len(f.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1 (only the pure {count}): %+v", len(f.Bindings), f.Bindings)
	}
	if f.Bindings[0].Expr != "count" {
		t.Errorf("bound the wrong interpolation: %+v", f.Bindings[0])
	}
}
