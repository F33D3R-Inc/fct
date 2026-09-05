package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The one thing `facet build --release` must never get wrong: what it writes has
// to RUN — as an application, on a machine with no source tree, no facet on
// PATH, and no Go toolchain. That claim cannot be tested by inspecting a struct;
// it is a claim about an executable file, so this test produces the real artifact
// and executes it.
//
// The shape mirrors new_test.go's contract for the scaffolder: build the real
// thing with the real code path, then hand it to the thing that will actually
// consume it.
func TestReleaseArtifactRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes a 15MB artifact")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no Go toolchain to build the base binary with (an end user needs none; this test does)")
	}

	work := t.TempDir()

	// The base binary: what a user downloads. Everything after this point is what
	// that user's machine can do with nothing else installed.
	facet := filepath.Join(work, "facet")
	build := exec.Command(goTool, "build", "-o", facet, "facet/cmd/facet")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the toolchain: %v\n%s", err, out)
	}

	// A real project, scaffolded by the real scaffolder.
	project := filepath.Join(work, "blog")
	tmpl, _ := findTemplate("app")
	if err := scaffold(tmpl, project, "", false); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	artifact := filepath.Join(work, "dist", "blog")
	out, err := runIn(project, facet, "build", "--release", tmpl.Entry, "-o", artifact)
	if err != nil {
		t.Fatalf("facet build --release: %v\n%s", err, out)
	}
	fi, err := os.Stat(artifact)
	if err != nil {
		t.Fatalf("no artifact was produced: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatalf("%s is not executable (mode %v)", artifact, fi.Mode())
	}

	// The proof: run it somewhere that contains nothing else. No .fct, no
	// facet.json, no facet.lock, no facet binary — if the artifact needed any of
	// them, it would fail here.
	empty := t.TempDir()
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Fatalf("the empty directory is not empty")
	}

	version, err := runIn(empty, artifact, "version")
	if err != nil {
		t.Fatalf("the artifact does not run: %v\n%s", err, version)
	}
	if !strings.Contains(version, "Blog") {
		t.Errorf("`version` does not identify the app it carries: %q", version)
	}

	// It carries the whole graph, not just a name: the route table it prints with
	// no source present is the one the compiler derived.
	routes, err := runIn(empty, artifact, "routes")
	if err != nil {
		t.Fatalf("the artifact cannot report its routes: %v\n%s", err, routes)
	}
	for _, want := range []string{"/drafts", "/entry/:id", "/api/Entry"} {
		if !strings.Contains(routes, want) {
			t.Errorf("route %s is missing from the artifact's route table:\n%s", want, routes)
		}
	}

	// And it is the app, not the toolchain: a copy of the facet binary with a
	// bundle appended must not still offer to scaffold projects.
	if help, _ := runIn(empty, artifact, "new"); strings.Contains(help, "scaffold a new project") {
		t.Errorf("the artifact still behaves as the toolchain:\n%s", help)
	}
}

// runIn runs a command in dir and returns its combined output.
func runIn(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	// A stray FACET_* in the test runner's environment must not decide what the
	// artifact does; it is asked only about itself.
	cmd.Env = []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// repoRoot is the module directory — this package's parent's parent.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd)) // …/fct/cmd/facet -> …/fct
}
