package codegen

import (
	"bytes"
	"encoding/json"
	"html/template"
	"strings"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

// Brick 3 of docs/REACTIVITY.md: named client actions + DOM event wiring, typed
// and checked, recorded in the manifest. The Tier-1 runtime that runs them is a
// later brick.

type manifestActions struct {
	Facets []struct {
		Actions []struct {
			Name      string                          `json:"name"`
			Mutations []struct{ Target, Expr string } `json:"mutations"`
		} `json:"actions"`
		Handlers []struct {
			ID, Event, Action string
		} `json:"handlers"`
	} `json:"facets"`
}

func TestManifestRecordsActionsAndHandlers(t *testing.T) {
	facets, err := parser.Parse(`facet Counter:
    state:
        count = 0
        liked = false
    actions:
        bump:
            count = count + 1
        toggle:
            liked = !liked
    looks:
        <button on:click="bump">{count}</button>
        <button on:click='toggle'>like</button>
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var m manifestActions
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	f := m.Facets[0]

	if len(f.Actions) != 2 {
		t.Fatalf("actions = %d, want 2: %+v", len(f.Actions), f.Actions)
	}
	if f.Actions[0].Name != "bump" || f.Actions[0].Mutations[0].Target != "count" ||
		f.Actions[0].Mutations[0].Expr != "count + 1" {
		t.Errorf("action[0] = %+v", f.Actions[0])
	}

	// Handlers: ids in document order, both quote styles recognised.
	if len(f.Handlers) != 2 {
		t.Fatalf("handlers = %d, want 2: %+v", len(f.Handlers), f.Handlers)
	}
	if f.Handlers[0].ID != "h0" || f.Handlers[0].Event != "click" || f.Handlers[0].Action != "bump" {
		t.Errorf("handler[0] = %+v, want {h0 click bump}", f.Handlers[0])
	}
	if f.Handlers[1].ID != "h1" || f.Handlers[1].Action != "toggle" {
		t.Errorf("handler[1] = %+v, want {h1 _ toggle}", f.Handlers[1])
	}
}

// The load-bearing test: an `on:click="bump"` wiring is rewritten to the inert
// data attribute `data-fa-on-click="bump"` (Brick 4) and must still parse and
// EXECUTE as a valid Go html/template, with the attribute preserved (html/template
// must not strip or JS-escape it).
func TestHandlerAttributeSurvivesTemplate(t *testing.T) {
	facets, err := parser.Parse(`facet Counter:
    state:
        count = 0
    actions:
        bump:
            count = count + 1
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
	src := out.Templates["Counter"]
	tmpl, err := template.New("Counter").Parse(src)
	if err != nil {
		t.Fatalf("generated template did not parse as html/template: %v\n---\n%s", err, src)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{"Count": 0}); err != nil {
		t.Fatalf("execute: %v\n---\n%s", err, src)
	}
	if !strings.Contains(buf.String(), `data-fa-on-click="bump"`) {
		t.Errorf("rendered HTML lost the handler attribute:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), `on:click`) {
		t.Errorf("raw on: wiring should have been rewritten:\n%s", buf.String())
	}
}

// The wedge (Brick 4): a counter compiles to a coherent reactive unit — the bound
// interpolation becomes a marked span painted with the signal's initial value, the
// handler wiring becomes a delegatable data attribute, and the manifest binding id
// matches the span so the runtime can patch exactly that node.
func TestCounterWedgeCompiles(t *testing.T) {
	facets, err := parser.Parse(`facet Counter:
    state:
        count = 0
    actions:
        bump:
            count = count + 1
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

	// Template: marked span with initial paint "0", handler rewritten to data attr.
	tmplSrc := out.Templates["Counter"]
	for _, want := range []string{
		`data-fa-on-click="bump"`,
		`<span data-fa-bind="b0">`,
	} {
		if !strings.Contains(tmplSrc, want) {
			t.Errorf("template missing %q\n---\n%s", want, tmplSrc)
		}
	}
	// It executes and paints the initial value with no server data.
	tmpl, err := template.New("Counter").Parse(tmplSrc)
	if err != nil {
		t.Fatalf("template parse: %v\n%s", err, tmplSrc)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), `<span data-fa-bind="b0">0</span>`) {
		t.Errorf("initial paint wrong:\n%s", buf.String())
	}

	// Manifest: binding id b0 matches the span; action mutates the signal.
	var m manifestActions
	var mb manifestState
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Manifest, &mb); err != nil {
		t.Fatal(err)
	}
	if len(mb.Facets[0].Bindings) != 1 || mb.Facets[0].Bindings[0].ID != "b0" {
		t.Fatalf("binding id mismatch: %+v", mb.Facets[0].Bindings)
	}
	if len(m.Facets[0].Actions) != 1 || m.Facets[0].Actions[0].Mutations[0].Target != "count" {
		t.Fatalf("action wrong: %+v", m.Facets[0].Actions)
	}
}

func generateErr(t *testing.T, src string) error {
	t.Helper()
	facets, err := parser.Parse(src)
	if err != nil {
		return err
	}
	_, err = Generate(facets)
	return err
}

func TestActionCannotMutateWhatField(t *testing.T) {
	err := generateErr(t, `facet Counter:
    what:
        total: int
    state:
        count = 0
    actions:
        bad:
            total = 5
    looks:
        <button on:click="bad">{count}</button>
`)
	if err == nil || !strings.Contains(err.Error(), "what: field") {
		t.Fatalf("want a what:-field error, got: %v", err)
	}
}

func TestActionCannotMutateUndeclaredSignal(t *testing.T) {
	err := generateErr(t, `facet Counter:
    state:
        count = 0
    actions:
        bad:
            ghost = 1
    looks:
        <button on:click="bad">{count}</button>
`)
	if err == nil || !strings.Contains(err.Error(), "undeclared signal") {
		t.Fatalf("want an undeclared-signal error, got: %v", err)
	}
}

func TestHandlerMustReferenceDeclaredAction(t *testing.T) {
	err := generateErr(t, `facet Counter:
    state:
        count = 0
    actions:
        bump:
            count = count + 1
    looks:
        <button on:click="nope">{count}</button>
`)
	if err == nil || !strings.Contains(err.Error(), "undeclared action") {
		t.Fatalf("want an undeclared-action error, got: %v", err)
	}
}

func TestDuplicateActionRejected(t *testing.T) {
	err := generateErr(t, `facet Counter:
    state:
        count = 0
    actions:
        bump:
            count = count + 1
        bump:
            count = count + 2
    looks:
        <button on:click="bump">{count}</button>
`)
	if err == nil || !strings.Contains(err.Error(), "duplicate action") {
		t.Fatalf("want a duplicate-action error, got: %v", err)
	}
}
