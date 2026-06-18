package codegen

import (
	"strings"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

func TestBackendRegistry(t *testing.T) {
	for _, name := range []string{"go", "node", "python", "rust"} {
		b, err := BackendFor(name)
		if err != nil {
			t.Fatalf("BackendFor(%q): %v", name, err)
		}
		if b.Name() != name {
			t.Errorf("BackendFor(%q).Name() = %q", name, b.Name())
		}
	}
	// Empty target defaults to Go (preserves v0 behavior).
	if b, err := BackendFor(""); err != nil || b.Name() != "go" {
		t.Errorf(`BackendFor("") = %v, %v; want go backend`, b, err)
	}
	// Unknown target errors and names the available set.
	_, err := BackendFor("cobol")
	if err == nil || !strings.Contains(err.Error(), "node") {
		t.Errorf("BackendFor(cobol) error = %v; want one naming available targets", err)
	}
}

func TestBackendExpr(t *testing.T) {
	cases := []struct {
		in             string
		scope          []string
		node, py, rust string
	}{
		{in: "count > 100", node: "data.count > 100", py: "data.count > 100", rust: "data.count > 100"},
		{in: `role == "admin"`, node: `data.role == "admin"`, py: `data.role == "admin"`, rust: `data.role == "admin"`},
		{in: "a && b", node: "data.a && data.b", py: "data.a and data.b", rust: "data.a && data.b"},
		{in: "a || b", node: "data.a || data.b", py: "data.a or data.b", rust: "data.a || data.b"},
		{in: "!liked", node: "!data.liked", py: "not data.liked", rust: "!data.liked"},
		{in: "likes + 1", node: "data.likes + 1", py: "data.likes + 1", rust: "data.likes + 1"},
		{in: "a - b * c", node: "data.a - (data.b * data.c)", py: "data.a - (data.b * data.c)", rust: "data.a - (data.b * data.c)"},
		{in: "(a || b) && c", node: "(data.a || data.b) && data.c", py: "(data.a or data.b) and data.c", rust: "(data.a || data.b) && data.c"},
		{in: "viewer.can_view(post)", node: "data.viewer.canView(data.post)", py: "data.viewer.can_view(data.post)", rust: "data.viewer.can_view(data.post)"},
		{in: "post.id", node: "data.post.id", py: "data.post.id", rust: "data.post.id"},
		{in: "user_id", node: "data.userId", py: "data.user_id", rust: "data.user_id"},
		{in: `u.name == "ada"`, scope: []string{"u"}, node: `u.name == "ada"`, py: `u.name == "ada"`, rust: `u.name == "ada"`},
		{in: "true", node: "true", py: "True", rust: "true"},
	}
	node, _ := BackendFor("node")
	py, _ := BackendFor("python")
	rust, _ := BackendFor("rust")
	for _, tc := range cases {
		check := func(b Backend, want string) {
			got, err := b.Expr(tc.in, tc.scope)
			if err != nil {
				t.Errorf("%s %q: %v", b.Name(), tc.in, err)
				return
			}
			if got != want {
				t.Errorf("%s %q\n  got  %q\n  want %q", b.Name(), tc.in, got, want)
			}
		}
		check(node, tc.node)
		check(py, tc.py)
		check(rust, tc.rust)
	}
}

func TestBackendExprErrors(t *testing.T) {
	node, _ := BackendFor("node")
	for _, in := range []string{"count >", "a &&", "(a", "1 +", "@bad"} {
		if _, err := node.Expr(in, nil); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestBackendTypes(t *testing.T) {
	src := "facet LikeButton:\n    what:\n        post: Post\n        like_count: int\n        liked: bool\n    looks:\n        <b>x</b>\n"
	facets, err := parser.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string][]string{
		"node":   {"export interface LikeButtonData {", "post: Post;", "likeCount: number;", "liked: boolean;"},
		"python": {"@dataclass", "class LikeButtonData:", "post: Post", "like_count: int", "liked: bool"},
		"rust":   {"pub struct LikeButtonData {", "pub post: Post,", "pub like_count: i64,", "pub liked: bool,"},
	}
	for name, want := range wants {
		b, _ := BackendFor(name)
		got := b.Types("app", facets)
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s Types missing %q\n---\n%s", name, w, got)
			}
		}
	}
}
