package codegen

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

// Brick 6 of docs/REACTIVITY.md: a `for v in <signal>` over a list signal lifts to
// an <fa-for> host plus a client item template the runtime reconciles by key.

func TestReactiveListExtracted(t *testing.T) {
	facets, err := parser.Parse(`facet Todo:
    state:
        items = []
    actions:
        add:
            items = items + ["x"]
    looks:
        <ul>
            for item in items:
                <li>{item}</li>
        </ul>
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Template: the loop is replaced by an empty fa-for host; no server {{range}}.
	tmplSrc := out.Templates["Todo"]
	if !strings.Contains(tmplSrc, `<fa-for data-fa-list="l0"`) {
		t.Errorf("missing fa-for host:\n%s", tmplSrc)
	}
	if strings.Contains(tmplSrc, "range") {
		t.Errorf("reactive list should not emit a server range:\n%s", tmplSrc)
	}

	var m struct {
		Facets []struct {
			Lists []struct{ ID, Signal, Var, Item string } `json:"lists"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	l := m.Facets[0].Lists
	if len(l) != 1 || l[0].ID != "l0" || l[0].Signal != "items" || l[0].Var != "item" {
		t.Fatalf("list entry = %+v", l)
	}
	if !strings.Contains(l[0].Item, "{item}") {
		t.Errorf("item template should carry the {item} hole: %q", l[0].Item)
	}
}

// A `for` over server data (a what: field) is left as a normal server range.
func TestServerLoopNotReactive(t *testing.T) {
	facets, err := parser.Parse(`facet List:
    what:
        rows: Row
    looks:
        <ul>
            for r in rows:
                <li>{r.name}</li>
        </ul>
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(out.Templates["List"], "range") {
		t.Errorf("server loop should emit a range:\n%s", out.Templates["List"])
	}
	var m struct {
		Facets []struct {
			Lists []struct{ ID string } `json:"lists"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Facets[0].Lists) != 0 {
		t.Errorf("server loop should record no reactive list: %+v", m.Facets[0].Lists)
	}
}
