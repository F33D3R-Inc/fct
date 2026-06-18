package parser

import "testing"

// Brick 3 of docs/REACTIVITY.md: the `actions:` block — named, reusable client
// handlers that mutate state signals. These pin the language surface; checks and
// codegen live in internal/codegen.

func TestParseActionsBlock(t *testing.T) {
	src := `facet Counter:
    state:
        count = 0
        liked = false
    actions:
        bump:
            count = count + 1
        toggle:
            liked = !liked
            count = count + 1
    looks:
        <button on:click="bump">{count}</button>
`
	facets, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	f := facets[0]
	if len(f.Actions) != 2 {
		t.Fatalf("want 2 actions, got %d", len(f.Actions))
	}
	bump := f.Actions[0]
	if bump.Name != "bump" || len(bump.Mutations) != 1 ||
		bump.Mutations[0].Target != "count" || bump.Mutations[0].Expr != "count + 1" {
		t.Errorf("bump = %+v", bump)
	}
	toggle := f.Actions[1]
	if toggle.Name != "toggle" || len(toggle.Mutations) != 2 {
		t.Fatalf("toggle = %+v", toggle)
	}
	if toggle.Mutations[0].Target != "liked" || toggle.Mutations[0].Expr != "!liked" {
		t.Errorf("toggle.mut[0] = %+v", toggle.Mutations[0])
	}
}

func TestActionAssignmentSplitsAtAssignNotComparison(t *testing.T) {
	src := `facet Counter:
    state:
        flag = false
    actions:
        sync:
            flag = count == 0
    looks:
        <button on:click="sync">x</button>
`
	facets, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	a := facets[0].Actions[0].Mutations[0]
	if a.Target != "flag" || a.Expr != "count == 0" {
		t.Errorf("assign = %+v, want {flag, count == 0}", a)
	}
}

func TestEmptyActionRejected(t *testing.T) {
	src := `facet Counter:
    state:
        count = 0
    actions:
        noop:
    looks:
        <button>x</button>
`
	if _, err := Parse(src); err == nil {
		t.Fatal("expected an error: action with no mutations")
	}
}

func TestActionsRejectedOnClientRenderedKind(t *testing.T) {
	src := `vault DM:
    actions:
        open:
            shown = true
    decrypt:
        <p>{plaintext}</p>
`
	if _, err := Parse(src); err == nil {
		t.Fatal("expected an error: actions: on a vault")
	}
}
