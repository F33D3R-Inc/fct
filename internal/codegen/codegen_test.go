package codegen

import (
	"bytes"
	"encoding/json"
	"html/template"
	"os"
	"strings"
	"testing"

	"fct.dev/internal/parser"
)

func compileExample(t *testing.T) *Output {
	t.Helper()
	src, err := os.ReadFile("../../examples/like_button.fct")
	if err != nil {
		t.Skipf("example missing: %v", err)
	}
	facets, err := parser.Parse(string(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return out
}

// The load-bearing test: the generated template must be a VALID Go html/template
// and must EXECUTE to correct HTML. If this passes, codegen emits runnable output.
func TestGeneratedTemplateExecutes(t *testing.T) {
	out := compileExample(t)
	src := out.Templates["LikeButton"]
	if src == "" {
		t.Fatal("no template generated for LikeButton")
	}

	tmpl, err := template.New("LikeButton").Parse(src)
	if err != nil {
		t.Fatalf("generated template did not parse as html/template: %v\n---\n%s", err, src)
	}

	render := func(data map[string]any) string {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			t.Fatalf("execute: %v", err)
		}
		return buf.String()
	}

	// liked + count → active class, filled icon, count span, concrete facet-id.
	liked := render(map[string]any{
		"Post":  map[string]any{"ID": "abc123"},
		"Count": 5,
		"Liked": true,
	})
	for _, want := range []string{
		`data-facet-id="LikeButton:post:abc123"`,
		`data-post-id="abc123"`,
		"post-action--like active",               // inline if-liked active
		`fill="currentColor"`,                    // inline if/else → liked branch
		`<span class="btn-like__count">5</span>`, // block if-count body
	} {
		if !strings.Contains(liked, want) {
			t.Errorf("liked render missing %q\n---\n%s", want, liked)
		}
	}

	// not liked + zero count → no active, hollow icon, no count span.
	cold := render(map[string]any{
		"Post":  map[string]any{"ID": "z9"},
		"Count": 0,
		"Liked": false,
	})
	if strings.Contains(cold, "post-action--like active") {
		t.Errorf("cold render should not be active:\n%s", cold)
	}
	if !strings.Contains(cold, `fill="none"`) {
		t.Errorf("cold render should have hollow icon (fill=none):\n%s", cold)
	}
	if strings.Contains(cold, "btn-like__count") {
		t.Errorf("cold render should omit count span:\n%s", cold)
	}
	if !strings.Contains(cold, `data-facet-id="LikeButton:post:z9"`) {
		t.Errorf("cold render missing concrete facet-id:\n%s", cold)
	}
}

func TestManifest(t *testing.T) {
	out := compileExample(t)

	var m struct {
		Facets []struct {
			Name     string `json:"name"`
			FacetID  string `json:"facet_id"`
			Template string `json:"template"`
			When     []struct {
				Events    []string `json:"events"`
				Mutations []struct {
					Op, Target, With string
				} `json:"mutations"`
			} `json:"when"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if len(m.Facets) != 1 {
		t.Fatalf("manifest facets = %d, want 1", len(m.Facets))
	}
	f := m.Facets[0]
	if f.Name != "LikeButton" || f.FacetID != "LikeButton:post:{post.id}" {
		t.Errorf("manifest identity wrong: %+v", f)
	}
	if f.Template != "LikeButton.tmpl.html" {
		t.Errorf("template name = %q", f.Template)
	}
	if len(f.When) != 1 || len(f.When[0].Mutations) != 1 {
		t.Fatalf("when shape wrong: %+v", f.When)
	}
	if len(f.When[0].Events) != 1 || f.When[0].Events[0] != "post.like_toggled" {
		t.Errorf("when events = %v", f.When[0].Events)
	}
	mu := f.When[0].Mutations[0]
	if mu.Op != "replace" || mu.Target != "LikeButton" || mu.With != "event.payload" {
		t.Errorf("mutation = %+v", mu)
	}
}

func TestCompositionGuards(t *testing.T) {
	cases := []struct{ name, src, wantErr string }{
		{
			"self-cycle",
			"facet Loop:\n    looks:\n        <div><Loop /></div>\n",
			"cycle",
		},
		{
			"mutual-cycle",
			"facet A:\n    looks:\n        <div><B /></div>\n" +
				"facet B:\n    looks:\n        <div><A /></div>\n",
			"cycle",
		},
		{
			"unknown-child",
			"facet A:\n    looks:\n        <div><Ghost /></div>\n",
			"unknown child",
		},
		{
			"unknown-prop",
			"facet Avatar:\n    what:\n        user: User\n    looks:\n        <img/>\n" +
				"facet A:\n    looks:\n        <div><Avatar bogus=\"x\" /></div>\n",
			"no field",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facets, err := parser.Parse(tc.src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, err = Generate(facets)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestForLoopCodegen(t *testing.T) {
	src := "" +
		"facet List:\n" +
		"    looks:\n" +
		"        <ul>\n" +
		"        for u in users:\n" +
		"            <li>{u.name}</li>\n" +
		"        </ul>\n"
	facets, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	tmplSrc := out.Templates["List"]
	if !strings.Contains(tmplSrc, "{{range $u := .Users}}") {
		t.Errorf("for did not lower to range: %s", tmplSrc)
	}
	if !strings.Contains(tmplSrc, "{{$u.Name}}") {
		t.Errorf("loop-var access did not lower to $u: %s", tmplSrc)
	}

	tmpl := template.Must(template.New("List").Parse(tmplSrc))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"Users": []map[string]any{{"Name": "ada"}, {"Name": "alan"}},
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "<li>ada</li>") || !strings.Contains(got, "<li>alan</li>") {
		t.Errorf("loop output wrong:\n%s", got)
	}
}
