package codegen

import (
	"encoding/json"
	"regexp"
	"sort"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

// The client `view` (used to instantiate a facet in the browser inside a reactive
// region or list item) is emitted by emitView, a SEPARATE walk from the server
// template's emitNodes. Their binding ids MUST agree, or the per-instance hydrate
// pass would fill the wrong nodes. These tests pin that invariant: every binding
// id in the manifest appears in the view with the right marker kind, and the view
// introduces no id the manifest does not know.

type viewManifest struct {
	Facets []struct {
		Name     string `json:"name"`
		View     string `json:"view"`
		Bindings []struct {
			ID   string `json:"id"`
			Node string `json:"node"`
		} `json:"bindings"`
	} `json:"facets"`
}

func compileView(t *testing.T, src string) viewManifest {
	t.Helper()
	facets, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var m viewManifest
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return m
}

var (
	bindIDRe     = regexp.MustCompile(`data-fa-bind="(b\d+)"`)
	bindAttrIDRe = regexp.MustCompile(`data-fa-bind-attr="([^"]+)"`)
)

func TestClientViewBindingIDsAlign(t *testing.T) {
	src := `facet Widget:
    state:
        count = 0
        liked = false
    what:
        label = "n=" + count
    looks:
        <div class="w">
            <span class="c">{count}</span>
            <span class="l">{label}</span>
            <button aria-pressed="{liked}" disabled="{liked}">x</button>
        </div>
`
	m := compileView(t, src)
	if len(m.Facets) != 1 {
		t.Fatalf("want 1 facet, got %d", len(m.Facets))
	}
	f := m.Facets[0]
	if f.View == "" {
		t.Fatal("facet has no client view")
	}

	// Partition the manifest binding ids by marker kind.
	wantText := map[string]bool{}
	wantAttr := map[string]bool{}
	for _, b := range f.Bindings {
		if b.Node == "text" {
			wantText[b.ID] = true
		} else {
			wantAttr[b.ID] = true
		}
	}
	if len(wantText) == 0 || len(wantAttr) == 0 {
		t.Fatalf("test needs both text and attr bindings; got text=%v attr=%v", wantText, wantAttr)
	}

	// Collect the ids the view actually carries.
	gotText := map[string]bool{}
	for _, mm := range bindIDRe.FindAllStringSubmatch(f.View, -1) {
		gotText[mm[1]] = true
	}
	gotAttr := map[string]bool{}
	for _, mm := range bindAttrIDRe.FindAllStringSubmatch(f.View, -1) {
		for _, id := range regexp.MustCompile(`\s+`).Split(mm[1], -1) {
			if id != "" {
				gotAttr[id] = true
			}
		}
	}

	if !setEqual(gotText, wantText) {
		t.Errorf("text-binding ids drift: view=%v manifest=%v", keys(gotText), keys(wantText))
	}
	if !setEqual(gotAttr, wantAttr) {
		t.Errorf("attr-binding ids drift: view=%v manifest=%v", keys(gotAttr), keys(wantAttr))
	}
}

// TestClientViewPropHoles checks a child facet's props render as fill holes (so a
// parent can fill them at instantiation) while its own signal bindings stay marked.
func TestClientViewPropHoles(t *testing.T) {
	src := `facet Tweet:
    what:
        author: str
    state:
        liked = false
    looks:
        <article>{author}<button aria-pressed="{liked}">x</button></article>
`
	m := compileView(t, src)
	v := m.Facets[0].View
	if !regexp.MustCompile(`\{author\}`).MatchString(v) {
		t.Errorf("prop should be a fill hole {author}; view=%q", v)
	}
	if !bindAttrIDRe.MatchString(v) {
		t.Errorf("own signal binding should keep its marker; view=%q", v)
	}
}

func setEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
