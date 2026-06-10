package codegen

import (
	"strings"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

// A field referenced in looks: but not declared in what: is a compile error —
// the compiler enforces the facet's data contract (no silent blanks at runtime).
func TestUndeclaredFieldIsCompileError(t *testing.T) {
	src := "" +
		"facet Card:\n" +
		"    what:\n" +
		"        user: User\n" +
		"    looks:\n" +
		"        <p>{usr.name}</p>\n" // typo: usr, not user
	facets, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = Generate(facets)
	if err == nil {
		t.Fatal("expected a compile error for the undeclared field 'usr', got none")
	}
	if !strings.Contains(err.Error(), "usr") || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("error should name the bad field: %v", err)
	}
}

// Declared fields, loop variables, and method calls on declared fields are all
// accepted (no false positives).
func TestValidReferencesAccepted(t *testing.T) {
	src := "" +
		"facet Feed:\n" +
		"    what:\n" +
		"        viewer: User\n" +
		"        page: Page\n" +
		"    looks:\n" +
		"        <div>\n" +
		"        for post in page.posts:\n" +
		"            if viewer.can_view(post):\n" +
		"                <span>{post.body}</span>\n" +
		"        </div>\n"
	facets, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Generate(facets); err != nil {
		t.Fatalf("valid facet rejected: %v", err)
	}
}
