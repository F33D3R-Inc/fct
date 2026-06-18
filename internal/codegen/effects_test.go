package codegen

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

// Brick 7 of docs/REACTIVITY.md: an `effects:` block runs a named action when its
// dependency signals change. Validated and recorded in the manifest.

func TestEffectsRecorded(t *testing.T) {
	facets, err := parser.Parse(`facet Tracker:
    state:
        count = 0
        history = []
    actions:
        bump:
            count = count + 1
        record:
            history = history + [count]
    effects:
        on count: record
    looks:
        <button on:click="bump">{count}</button>
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var m struct {
		Facets []struct {
			Effects []struct {
				Deps   []string `json:"deps"`
				Action string   `json:"action"`
			} `json:"effects"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	e := m.Facets[0].Effects
	if len(e) != 1 || e[0].Action != "record" || len(e[0].Deps) != 1 || e[0].Deps[0] != "count" {
		t.Fatalf("effects = %+v, want [{[count] record}]", e)
	}
}

func TestEffectUndeclaredActionRejected(t *testing.T) {
	_, err := parser.Parse(`facet T:
    state:
        count = 0
    effects:
        on count: nope
    looks:
        <p>{count}</p>
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	facets, _ := parser.Parse(`facet T:
    state:
        count = 0
    effects:
        on count: nope
    looks:
        <p>{count}</p>
`)
	if _, err := Generate(facets); err == nil || !strings.Contains(err.Error(), "undeclared action") {
		t.Fatalf("want undeclared-action error, got: %v", err)
	}
}

func TestEffectNonSignalDepRejected(t *testing.T) {
	facets, err := parser.Parse(`facet T:
    what:
        title: string
    state:
        count = 0
    actions:
        bump:
            count = count + 1
    effects:
        on title: bump
    looks:
        <p>{count}</p>
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Generate(facets); err == nil || !strings.Contains(err.Error(), "not a state signal") {
		t.Fatalf("want non-signal-dep error, got: %v", err)
	}
}
