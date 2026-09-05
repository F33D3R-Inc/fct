package main

import (
	"bytes"
	"strings"
	"testing"

	"facet/internal/compile"
)

// shadowedApp declares a view at /admin — the exact defect the storefront app
// hit: the runtime's generated admin console is registered at that path
// unconditionally (runtime/server.go), so this view is compiled successfully
// but never dispatched to. It also declares a view under /admin/ (prefix
// shadowing) and one at "/", which must NOT be flagged.
const shadowedApp = `app Shadow:
    view Home at "/":
        box:
            text "hi"

    view Admin at "/admin":
        box:
            text "custom admin, never served"

    view AdminSettings at "/admin/settings":
        box:
            text "also never served"
`

// TestRouteShadowing proves `facet routes` marks a view whose path the
// runtime's own fixed endpoints already own, loudly and by name, instead of
// reporting it as an ordinary served route.
func TestRouteShadowing(t *testing.T) {
	g, err := compile.String(shadowedApp)
	if err != nil {
		t.Fatalf("shadowedApp must compile: %v", err)
	}
	routes := buildRoutes(g, false)

	byPath := map[string]RouteEntry{}
	for _, r := range routes {
		byPath[r.Path] = r
	}

	for _, path := range []string{"/admin", "/admin/settings"} {
		r, ok := byPath[path]
		if !ok {
			t.Fatalf("expected a route for %s", path)
		}
		if r.Shadowed == "" {
			t.Errorf("%s: expected Shadowed to name the built-in that wins, got empty", path)
		}
		if !strings.Contains(r.Shadowed, "/admin") {
			t.Errorf("%s: Shadowed = %q, want it to name the /admin built-in", path, r.Shadowed)
		}
	}

	home, ok := byPath["/"]
	if !ok {
		t.Fatal("expected a route for /")
	}
	if home.Shadowed != "" {
		t.Errorf("/ must not be shadowed, got %q", home.Shadowed)
	}

	// The human table must say so loudly, not bury it in a note.
	var buf bytes.Buffer
	writeRoutesText(&buf, RouteReport{App: g.App, Routes: routes})
	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("routes table does not warn about the shadowed route:\n%s", out)
	}
}

// TestCheckRouteShadowing proves the same defect fails `facet doctor` — a dead
// route is a real defect, not merely something `routes` mentions in passing.
func TestCheckRouteShadowing(t *testing.T) {
	g, err := compile.String(shadowedApp)
	if err != nil {
		t.Fatalf("shadowedApp must compile: %v", err)
	}
	checks := checkRouteShadowing(g)
	if len(checks) != 2 {
		t.Fatalf("expected 2 shadowing findings (one per shadowed view), got %d: %+v", len(checks), checks)
	}
	for _, c := range checks {
		if c.State != statusFail {
			t.Errorf("a dead route must be statusFail, got %v (%s)", c.State, c.Detail)
		}
	}

	// A graph with no shadowed views must report nothing at all.
	clean, err := compile.String(sampleApp)
	if err != nil {
		t.Fatalf("sampleApp must compile: %v", err)
	}
	if got := checkRouteShadowing(clean); len(got) != 0 {
		t.Errorf("sampleApp declares no shadowed routes, got %d findings: %+v", len(got), got)
	}
}
