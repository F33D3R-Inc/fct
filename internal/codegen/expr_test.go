package codegen

import (
	"strings"
	"testing"

	"fct.dev/internal/parser"
)

func TestExprLowering(t *testing.T) {
	cases := []struct{ in, want string }{
		// paths (unchanged from before)
		{"count", ".Count"},
		{"post.id", ".Post.ID"},
		// comparisons → builtins
		{"count > 100", "(gt .Count 100)"},
		{"count <= 0", "(le .Count 0)"},
		{`role == "admin"`, `(eq .Role "admin")`},
		{"a != b", "(ne .A .B)"},
		// boolean
		{"a && b", "(and .A .B)"},
		{"a || b", "(or .A .B)"},
		{"!liked", "(not .Liked)"},
		// arithmetic
		{"likes + 1", "(add .Likes 1)"},
		{"a - b * c", "(sub .A (mul .B .C))"},
		// calls (Go template `call`, so func-valued data works too)
		{"viewer.can_view(post)", "(call .Viewer.CanView .Post)"},
		{"v.has_liked(post.id)", "(call .V.HasLiked .Post.ID)"},
		{"f()", "(call .F)"},
		// precedence + grouping
		{"a || b && c", "(or .A (and .B .C))"},
		{"(a || b) && c", "(and (or .A .B) .C)"},
		{`liked && count > 0`, "(and .Liked (gt .Count 0))"},
	}
	for _, tc := range cases {
		got, err := goExpr(tc.in, nil)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q\n  got  %q\n  want %q", tc.in, got, tc.want)
		}
	}
}

func TestGoStructs(t *testing.T) {
	facets, err := parser.Parse("facet LikeButton:\n    what:\n        post: Post\n        like_count: int\n        liked: bool\n    looks:\n        <b>x</b>\n")
	if err != nil {
		t.Fatal(err)
	}
	got := GoStructs("app", facets)
	for _, want := range []string{
		"package app",
		"type LikeButtonData struct {",
		"\tPost Post\n",     // custom type by name
		"\tLikeCount int\n", // snake_case → idiomatic
		"\tLiked bool\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("GoStructs missing %q\n---\n%s", want, got)
		}
	}
}

func TestExprLoopVar(t *testing.T) {
	got, err := goExpr("u.name == \"ada\"", []string{"u"})
	if err != nil {
		t.Fatal(err)
	}
	if got != `(eq $u.Name "ada")` {
		t.Fatalf("loop var lowering = %q", got)
	}
}

func TestExprErrors(t *testing.T) {
	for _, in := range []string{"count >", "a &&", "(a", "1 +", "@bad"} {
		if _, err := goExpr(in, nil); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}
