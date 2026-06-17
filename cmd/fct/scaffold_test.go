package main

import (
	"io/fs"
	"strings"
	"testing"
)

// The scaffolded main.go is a contract: it must stay inert. App authors add
// features under app/, never in the entrypoint. These tests fail the build if a
// future edit lets application logic leak back into main.go — that regression is
// exactly what this structure exists to prevent.

func TestScaffoldMainIsInert(t *testing.T) {
	b, err := scaffoldFS.ReadFile("scaffold/main.go.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	// No application wiring belongs in the entrypoint. If any of these appear in
	// main.go, a feature is being grown there instead of in app/.
	banned := []string{
		".On(",          // event handlers
		".Route(",       // routes
		".Mount(",       // server wiring
		"http.",         // net/http
		"os/signal",     // lifecycle
		"CompileDir",    // facet compilation
		"template.HTML", // rendering / state
	}
	for _, tok := range banned {
		if strings.Contains(src, tok) {
			t.Errorf("scaffold main.go must stay inert, but contains %q — that logic belongs in app/", tok)
		}
	}

	// Count real code lines (not comments/blank). The entrypoint should be tiny.
	const maxCodeLines = 12
	code := 0
	for _, ln := range strings.Split(src, "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		code++
	}
	if code > maxCodeLines {
		t.Errorf("scaffold main.go has %d code lines, want <= %d — keep features in app/", code, maxCodeLines)
	}
}

// TestScaffoldHasAppPackage guards the other half of the contract: the place
// features are supposed to live actually ships in the scaffold.
func TestScaffoldHasAppPackage(t *testing.T) {
	want := map[string]bool{
		"scaffold/app/app.go.tmpl":    false,
		"scaffold/app/routes.go.tmpl": false,
		"scaffold/app/like.go.tmpl":   false,
		"scaffold/app/style.go.tmpl":  false,
	}
	err := fs.WalkDir(scaffoldFS, "scaffold/app", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if _, ok := want[path]; ok {
			want[path] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, found := range want {
		if !found {
			t.Errorf("scaffold is missing %s — feature files must ship in app/", path)
		}
	}
}
