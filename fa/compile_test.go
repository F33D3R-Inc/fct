package fa

import (
	"os"
	"strings"
	"testing"
)

func TestCompileAndRender(t *testing.T) {
	src, err := os.ReadFile("../examples/like_button.fct")
	if err != nil {
		t.Skipf("example missing: %v", err)
	}
	c, err := Compile(string(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(c.Manifest) == 0 {
		t.Error("empty manifest")
	}
	h, err := c.Render("LikeButton", map[string]any{
		"Post": map[string]any{"ID": "p9"}, "Count": 2, "Liked": true,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(h)
	if !strings.Contains(got, `data-facet-id="LikeButton:post:p9"`) {
		t.Errorf("render missing concrete facet-id:\n%s", got)
	}
	if !strings.Contains(got, "post-action--like active") || !strings.Contains(got, `fill="currentColor"`) {
		t.Errorf("liked state not rendered:\n%s", got)
	}
}

func TestRenderUnknownFacet(t *testing.T) {
	c, err := Compile("facet A:\n    looks:\n        <p>hi</p>\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Render("Nope", nil); err == nil {
		t.Error("expected error for unknown facet")
	}
}
