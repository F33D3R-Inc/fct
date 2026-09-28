package selfhost

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives this package's tests one temporary root that goes when
// the run ends. The helpers that build a toolchain binary once per process
// (fqServerFacet and its kind) make a directory with os.MkdirTemp("", …)
// that no test owns, so nothing removed it: every run of the package left
// ~20 MB per helper in the system temp directory until it filled. With
// TMPDIR pointing here, those directories, every t.TempDir, and whatever
// the engines and runtimes the tests start write to their own temp dir all
// live under one root, removed after the last test. A run killed before
// it could remove its root leaves it behind; the next run removes every
// root whose process is gone (testTempRoot).
func TestMain(m *testing.M) {
	root, err := testTempRoot("selfhost-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "selfhost tests: temp root:", err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", root)
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}
