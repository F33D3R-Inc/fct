package fa

import (
	"strings"
	"testing"
)

func TestRenderForEnforcesWho(t *testing.T) {
	src := "facet Secret:\n" +
		"    who:\n" +
		"        require: is_admin\n" +
		"        redact user.ssn always\n" +
		"    what:\n" +
		"        user: User\n" +
		"    looks:\n" +
		"        <div>name={user.name}</div>\n"
	c, err := Compile(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	c.Policy("is_admin", func(v View) bool { return v.Identity == "admin" })

	mkData := func() map[string]any {
		return map[string]any{"User": map[string]any{"ID": "1", "Name": "Ada", "Ssn": "SECRET123"}}
	}

	// The unchecked path is refused for a protected facet.
	if _, err := c.Render("Secret", mkData()); err == nil || !strings.Contains(err.Error(), "RenderFor") {
		t.Fatalf("Render on a protected facet must error pointing to RenderFor, got: %v", err)
	}

	// A viewer who fails `require` is forbidden.
	if _, err := c.RenderFor(View{Identity: "guest"}, "Secret", mkData()); err != ErrForbidden {
		t.Fatalf("guest must be forbidden, got: %v", err)
	}

	// An authorized viewer renders — and the redacted field is gone.
	orig := mkData()
	html, err := c.RenderFor(View{Identity: "admin"}, "Secret", orig)
	if err != nil {
		t.Fatalf("admin render: %v", err)
	}
	if !strings.Contains(string(html), "name=Ada") {
		t.Errorf("expected name to render: %s", html)
	}
	if strings.Contains(string(html), "SECRET123") {
		t.Errorf("ssn must be redacted from output: %s", html)
	}
	// The caller's data must NOT have been mutated by redaction.
	if orig["User"].(map[string]any)["Ssn"] != "SECRET123" {
		t.Error("RenderFor mutated the caller's data")
	}
}

func TestRedactUnlessPolicy(t *testing.T) {
	src := "facet Card:\n" +
		"    who:\n" +
		"        redact post.flags unless is_mod\n" +
		"    what:\n" +
		"        post: Post\n" +
		"    looks:\n" +
		"        <div>flags={post.flags}</div>\n"
	c, err := Compile(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	c.Policy("is_mod", func(v View) bool { return v.Identity == "mod" })
	mk := func() map[string]any {
		return map[string]any{"Post": map[string]any{"ID": "1", "Flags": "NSFW"}}
	}

	// non-moderator: flags redacted
	h, err := c.RenderFor(View{Identity: "user"}, "Card", mk())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(h), "NSFW") {
		t.Errorf("flags must be redacted for non-mod: %s", h)
	}
	// moderator: flags visible
	h, err = c.RenderFor(View{Identity: "mod"}, "Card", mk())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(h), "NSFW") {
		t.Errorf("flags must be visible for mod: %s", h)
	}
}

func TestMissingPolicyErrors(t *testing.T) {
	src := "facet X:\n    who:\n        require: nope\n    what:\n        u: User\n    looks:\n        <p>x</p>\n"
	c, err := Compile(src)
	if err != nil {
		t.Fatal(err)
	}
	// No policy registered for "nope" → render errors rather than silently allowing.
	if _, err := c.RenderFor(View{}, "X", map[string]any{}); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected unregistered-policy error, got: %v", err)
	}
}
