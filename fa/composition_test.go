package fa

import (
	"os"
	"strings"
	"testing"
)

// TestComposition proves child-facet composition: ProfileHeader composes Avatar
// and FollowButton, and the rendered output nests their data-facet-ids.
func TestComposition(t *testing.T) {
	src, err := os.ReadFile("../examples/composition.fct")
	if err != nil {
		t.Skipf("example missing: %v", err)
	}
	c, err := Compile(string(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	user := map[string]any{"ID": "42", "Name": "Ada", "Handle": "ada", "URL": "/a.png"}
	html, err := c.Render("ProfileHeader", map[string]any{"User": user, "Following": false})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)

	for _, id := range []string{
		`data-facet-id="ProfileHeader:user:42"`,
		`data-facet-id="Avatar:user:42"`,
		`data-facet-id="FollowButton:target:42"`,
	} {
		if !strings.Contains(got, id) {
			t.Errorf("missing nested %s\n---\n%s", id, got)
		}
	}
	if !strings.Contains(got, `src="/a.png"`) {
		t.Errorf("child prop not passed through (avatar src)\n%s", got)
	}
	if !strings.Contains(got, "@ada") || !strings.Contains(got, "Follow") {
		t.Errorf("child content not rendered\n%s", got)
	}
}

// TestSurgicalChildRender proves a sub-facet can be re-rendered in isolation —
// exactly what a surgical update pushes — without leaking parent/sibling content.
func TestSurgicalChildRender(t *testing.T) {
	src, err := os.ReadFile("../examples/composition.fct")
	if err != nil {
		t.Skipf("example missing: %v", err)
	}
	c, err := Compile(string(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	btn, err := c.Render("FollowButton", map[string]any{
		"Target": map[string]any{"ID": "42"}, "Following": true,
	})
	if err != nil {
		t.Fatalf("render child: %v", err)
	}
	s := string(btn)
	if !strings.Contains(s, `data-facet-id="FollowButton:target:42"`) {
		t.Errorf("child id missing:\n%s", s)
	}
	if !strings.Contains(s, "Following") {
		t.Errorf("child state not rendered:\n%s", s)
	}
	if strings.Contains(s, "<img") || strings.Contains(s, "profile-header") {
		t.Errorf("surgical child render leaked parent content:\n%s", s)
	}
}
