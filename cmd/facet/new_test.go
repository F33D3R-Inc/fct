package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/registry"
)

// The one thing `facet new` must never get wrong: what it writes has to
// compile. Every template is scaffolded into a temp directory and handed to the
// real compiler — the same call `facet build` makes — so a template edited into
// source the toolchain rejects fails here rather than in a user's first minute.
func TestTemplatesCompile(t *testing.T) {
	for _, tmpl := range templates {
		t.Run(tmpl.Name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "myproject")
			if err := scaffold(tmpl, dir, "", false); err != nil {
				t.Fatalf("scaffold: %v", err)
			}
			entry := filepath.Join(dir, tmpl.Entry)
			graph, err := compile.File(entry)
			if err != nil {
				t.Fatalf("%s does not compile: %v", tmpl.Entry, err)
			}
			if graph.App == "" {
				t.Error("compiled graph has no app name")
			}

			// Every project carries a manifest pinned to the toolchain that wrote
			// it, and a .gitignore that keeps the secret out of the repo and the
			// lock in it.
			b, err := os.ReadFile(filepath.Join(dir, "facet.json"))
			if err != nil {
				t.Fatalf("facet.json: %v", err)
			}
			m, err := registry.ParseManifest(b)
			if err != nil {
				t.Fatalf("facet.json: %v", err)
			}
			if want := ">=" + registry.ToolchainVersion; m.Facet != want {
				t.Errorf("manifest pins facet %q, want %q", m.Facet, want)
			}
			if m.Main != tmpl.Entry {
				t.Errorf("manifest main is %q, want %q", m.Main, tmpl.Entry)
			}
			if err := registry.CheckToolchainRange(m.Facet, m.Name); err != nil {
				t.Errorf("the manifest this toolchain wrote does not satisfy this toolchain: %v", err)
			}
			ignore, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
			if err != nil {
				t.Fatalf(".gitignore: %v", err)
			}
			if !hasLine(string(ignore), ".env") {
				t.Error(".gitignore does not ignore .env")
			}
			if hasLine(string(ignore), "facet.lock") {
				t.Error(".gitignore ignores facet.lock — it must be committed")
			}
		})
	}
}

// The app template ships fixtures and behavior tests beside the app; they are
// worth nothing if they do not match the entity the template declares.
func TestAppTemplateFixturesAreValidJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blog")
	tmpl, _ := findTemplate("app")
	if err := scaffold(tmpl, dir, "", false); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	for _, name := range []string{"app.seed.json", "app.test.json", "facet.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
		}
	}
}

// A directory name is not an identifier; the app block needs one.
func TestAppIdent(t *testing.T) {
	cases := map[string]string{
		"blog":         "Blog",
		"my-blog":      "MyBlog",
		"my_blog 2":    "MyBlog2",
		"2048":         "App2048",
		"----":         "App",
		"alreadyCamel": "AlreadyCamel",
	}
	for in, want := range cases {
		if got := appIdent(in); got != want {
			t.Errorf("appIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

// hasLine reports whether text contains line as a whole, trimmed line.
func hasLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// `facet new .` must target the current directory rather than always writing
// into a fresh subdirectory — the gap the storefront and journal apps both
// hit ("it scaffolds into ./<name>/ with no way to target the current
// directory").
func TestScaffoldCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(orig)

	tmpl, _ := findTemplate("app")
	if err := scaffold(tmpl, ".", "", false); err != nil {
		t.Fatalf("scaffold into \".\": %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, tmpl.Entry)); err != nil {
		t.Fatalf("expected %s written into the current directory: %v", tmpl.Entry, err)
	}
	// The app identifier comes from the current directory's own name, not the
	// literal ".".
	graph, err := compile.File(filepath.Join(dir, tmpl.Entry))
	if err != nil {
		t.Fatalf("scaffolded project does not compile: %v", err)
	}
	if graph.App == "" || graph.App == "App" {
		t.Errorf("app identifier should derive from %q, got %q", filepath.Base(dir), graph.App)
	}
}

// Scaffolding must never clobber files that are already there unless forced —
// standard for this class of tool, and required now that "." can already
// contain unrelated files.
func TestScaffoldRefusesToClobber(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	tmpl, _ := findTemplate("app")
	if err := scaffold(tmpl, dir, "", false); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}
	if err := scaffold(tmpl, dir, "", false); err == nil {
		t.Fatal("expected a conflict error scaffolding into a directory that already has the template's files")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Errorf("conflict error should mention --force, got: %v", err)
	}
	// --force overwrites the same files without complaint.
	if err := scaffold(tmpl, dir, "", true); err != nil {
		t.Errorf("scaffold with force=true should overwrite, got: %v", err)
	}
}

// findSiblingFacets is what makes --facets honest: it must find a real local
// checkout by its manifest identity (never by directory name alone) and
// compute the relative import the generated app.fct actually needs.
func TestFindSiblingFacets(t *testing.T) {
	root := t.TempDir()
	facetsDir := filepath.Join(root, "facets")
	if err := os.MkdirAll(facetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"github.com/F33D3R-Inc/facets","version":"0.4.0","main":"","facet":">=1.31.0"}`
	if err := os.WriteFile(filepath.Join(facetsDir, "facet.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// A monorepo layout: apps live two levels under the checkout, matching
	// apps/journal and apps/storefront relative to the real facets/ sibling.
	projectDir := filepath.Join(root, "apps", "myapp")

	rel, version, ok := findSiblingFacets(projectDir)
	if !ok {
		t.Fatalf("expected to find the sibling facets/ checkout under %s", root)
	}
	if version != "0.4.0" {
		t.Errorf("version = %q, want %q", version, "0.4.0")
	}
	if want := "../../facets"; rel != want {
		t.Errorf("relative import = %q, want %q", rel, want)
	}

	// A directory merely named "facets" with no matching manifest must not be
	// mistaken for the library.
	decoy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(decoy, "facets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "facets", "facet.json"), []byte(`{"name":"github.com/someone/else"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := findSiblingFacets(filepath.Join(decoy, "apps", "myapp")); ok {
		t.Error("a facets/ directory with an unrelated manifest name must not match")
	}

	// No facets/ checkout anywhere nearby: not found, not guessed.
	if _, _, ok := findSiblingFacets(filepath.Join(t.TempDir(), "apps", "myapp")); ok {
		t.Error("expected no match with no facets/ checkout present")
	}
}
