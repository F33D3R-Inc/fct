package codegen

import (
	"encoding/json"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

func TestRenderIR(t *testing.T) {
	src := "facet LikeButton:\n" +
		"    what:\n" +
		"        post: Post\n" +
		"        count: int\n" +
		"        liked: bool\n" +
		"    looks:\n" +
		"        <button data-post-id=\"{post.id}\">\n" +
		"            if liked:\n" +
		"                <b>{count}</b>\n" +
		"        </button>\n" +
		"    when post.like:\n" +
		"        replace LikeButton\n"
	facets, err := parser.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := RenderIR(facets)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Wire   string `json:"wire"`
		Facets []struct {
			Name    string `json:"name"`
			FacetID string `json:"facet_id"`
			Render  []struct {
				Op  string          `json:"op"`
				X   json.RawMessage `json:"x"`
				Var string          `json:"var"`
			} `json:"render"`
			When []struct {
				Events    []string `json:"events"`
				Mutations []struct {
					Op     string `json:"op"`
					Target string `json:"target"`
				} `json:"mutations"`
			} `json:"when"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("render.json is not valid JSON: %v", err)
	}

	if doc.Wire != "1" {
		t.Errorf("wire = %q, want 1", doc.Wire)
	}
	if len(doc.Facets) != 1 {
		t.Fatalf("got %d facets, want 1", len(doc.Facets))
	}
	f := doc.Facets[0]
	if f.Name != "LikeButton" {
		t.Errorf("name = %q", f.Name)
	}
	if f.FacetID != "LikeButton:post:{post.id}" {
		t.Errorf("facet_id = %q", f.FacetID)
	}

	// The op stream must include an `if`, an `expr`, and balanced `end`s.
	var ifs, exprs, ends int
	for _, op := range f.Render {
		switch op.Op {
		case "if":
			ifs++
		case "expr":
			exprs++
		case "end":
			ends++
		}
	}
	if ifs != 1 || ends != 1 {
		t.Errorf("ifs=%d ends=%d, want 1 and 1", ifs, ends)
	}
	if exprs != 2 { // {post.id} and {count}
		t.Errorf("exprs=%d, want 2", exprs)
	}

	// The when: wiring must round-trip the event + replace mutation.
	if len(f.When) != 1 || len(f.When[0].Events) != 1 || f.When[0].Events[0] != "post.like" {
		t.Fatalf("when events = %+v", f.When)
	}
	if m := f.When[0].Mutations; len(m) != 1 || m[0].Op != "replace" || m[0].Target != "LikeButton" {
		t.Errorf("mutations = %+v", f.When[0].Mutations)
	}
}
