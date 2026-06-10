package fa

import (
	"strings"
	"testing"
)

// TestRicherExpressionsRender exercises comparisons, method calls, arithmetic,
// and string equality end-to-end through Compile + Render.
func TestRicherExpressionsRender(t *testing.T) {
	src := "" +
		"facet Card:\n" +
		"    what:\n" +
		"        post: Post\n" +
		"        viewer: Viewer\n" +
		"    looks:\n" +
		"        <div>\n" +
		"            if post.likes > 100:\n" +
		"                <span class=\"hot\">hot</span>\n" +
		"            if viewer.can_view(post):\n" +
		"                <p>{post.title} — {post.likes} likes (+1 = {post.likes + 1})</p>\n" +
		"            if viewer.role == \"admin\" && post.deletable:\n" +
		"                <button>delete</button>\n" +
		"        </div>\n"
	c, err := Compile(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	render := func(likes int, role string, canView, deletable bool) string {
		h, err := c.Render("Card", map[string]any{
			"Post": map[string]any{"ID": "1", "Title": "Hi", "Likes": likes, "Deletable": deletable},
			"Viewer": map[string]any{
				"Role":    role,
				"CanView": func(p any) bool { return canView },
			},
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return string(h)
	}

	hot := render(150, "admin", true, true)
	if !strings.Contains(hot, ">hot<") {
		t.Errorf("likes 150 > 100 should show hot:\n%s", hot)
	}
	if !strings.Contains(hot, "Hi — 150 likes (+1 = 151)") {
		t.Errorf("call gate + arithmetic wrong:\n%s", hot)
	}
	if !strings.Contains(hot, "<button>delete</button>") {
		t.Errorf("admin && deletable should show delete:\n%s", hot)
	}

	cold := render(5, "user", false, false)
	if strings.Contains(cold, ">hot<") {
		t.Errorf("likes 5 should NOT be hot:\n%s", cold)
	}
	if strings.Contains(cold, "150") || strings.Contains(cold, "Hi —") {
		t.Errorf("can_view false should hide the body:\n%s", cold)
	}
	if strings.Contains(cold, "delete") {
		t.Errorf("non-admin should NOT see delete:\n%s", cold)
	}
}
