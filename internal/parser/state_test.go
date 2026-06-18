package parser

import "testing"

// Brick 1 of docs/REACTIVITY.md: the `state:` block — local client reactive
// values (signals). These tests pin the language surface; bindings/codegen are
// later bricks.

func TestParseStateBlock(t *testing.T) {
	src := `facet Counter:
    state:
        count: int = 0
        liked = false
    looks:
        <button>{count}</button>
`
	facets, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	f := facets[0]
	if len(f.State) != 2 {
		t.Fatalf("want 2 state fields, got %d", len(f.State))
	}
	if f.State[0].Name != "count" || f.State[0].Type != "int" || f.State[0].Expr != "0" {
		t.Errorf("state[0] = %+v, want {count int 0}", f.State[0])
	}
	// Type is optional — inferred later from the initial value.
	if f.State[1].Name != "liked" || f.State[1].Type != "" || f.State[1].Expr != "false" {
		t.Errorf("state[1] = %+v, want {liked \"\" false}", f.State[1])
	}
}

func TestStateRequiresInitialValue(t *testing.T) {
	src := `facet Counter:
    state:
        count: int
    looks:
        <button>{count}</button>
`
	if _, err := Parse(src); err == nil {
		t.Fatal("expected an error: state field without an initial value")
	}
}

func TestStateRejectedOnClientRenderedKind(t *testing.T) {
	src := `vault DM:
    state:
        open = false
    decrypt:
        <p>{plaintext}</p>
`
	if _, err := Parse(src); err == nil {
		t.Fatal("expected an error: state: on a vault (no server-rendered body to bind)")
	}
}
