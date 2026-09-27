package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// `facet dev` reloads when any file the program is compiled from changes —
// an import in a subdirectory or a parent directory, or a `css from` file —
// not only a file beside the root. projectSig is the watcher's fingerprint,
// so it has to move when such a file moves.
func TestDevWatchesImportedModulesAnywhere(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "app", "wireframes"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "lib"), 0o755))
	root := filepath.Join(dir, "app", "site.fct")
	inner := filepath.Join(dir, "app", "wireframes", "shell.fct")
	outer := filepath.Join(dir, "lib", "atoms.fct")
	css := filepath.Join(dir, "lib", "theme.css")
	must(os.WriteFile(root, []byte("import \"wireframes/shell.fct\"\napp A:\n    view Home at \"/\":\n        text \"hi\"\n"), 0o644))
	must(os.WriteFile(inner, []byte("import \"../../lib/atoms.fct\"\napp Shell:\n    component Nav:\n        text \"nav\"\n"), 0o644))
	must(os.WriteFile(outer, []byte("app Atoms:\n    css from \"theme.css\"\n    component Atom:\n        text \"atom\"\n"), 0o644))
	must(os.WriteFile(css, []byte("body{}"), 0o644))
	old := time.Now().Add(-time.Hour)
	for _, f := range []string{root, inner, outer, css} {
		must(os.Chtimes(f, old, old))
	}
	before := projectSig(root)
	for i, f := range []string{inner, outer, css} {
		touched := old.Add(time.Duration(i+1) * time.Minute)
		must(os.Chtimes(f, touched, touched))
		if after := projectSig(root); !after.After(before) {
			t.Fatalf("editing %s did not move the dev signature (%v -> %v)", f, before, after)
		} else {
			before = after
		}
	}
}
