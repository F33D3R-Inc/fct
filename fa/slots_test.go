package fa

import (
	"os"
	"strings"
	"testing"
)

// TestSlotFilled: a block-form child renders the parent's content (in the
// PARENT's scope) at the child's slot — including a nested child facet.
func TestSlotFilled(t *testing.T) {
	src, err := os.ReadFile("../examples/layout.fct")
	if err != nil {
		t.Skipf("example missing: %v", err)
	}
	c, err := Compile(string(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := c.Render("Page", map[string]any{
		"User": map[string]any{"ID": "42", "Name": "Ada", "URL": "/a.png", "Handle": "ada"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)

	for _, want := range []string{
		`class="card__title">Welcome`,    // child's own data
		`Hello, Ada!`,                    // slot content, evaluated in PARENT scope
		`data-facet-id="Avatar:user:42"`, // a child facet nested inside the slot
		`src="/a.png"`,                   // its prop, passed through from the parent
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "No content.") {
		t.Errorf("default slot content should be replaced when filled:\n%s", got)
	}
}

// TestSlotDefault: using the layout facet with no block content shows the
// slot's default.
func TestSlotDefault(t *testing.T) {
	src, err := os.ReadFile("../examples/layout.fct")
	if err != nil {
		t.Skipf("example missing: %v", err)
	}
	c, err := Compile(string(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// Card has no who:, so plain Render is allowed; no __children → default shows.
	html, err := c.Render("Card", map[string]any{"Title": "Empty"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, "card__title\">Empty") {
		t.Errorf("title missing: %s", got)
	}
	if !strings.Contains(got, "No content.") {
		t.Errorf("expected default slot content when empty:\n%s", got)
	}
}
