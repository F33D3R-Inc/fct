package integration

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives this package's tests one temporary root that goes when
// the run ends: TMPDIR points into it, so the facet binary built once per
// process (facetBinary), every t.TempDir, and whatever the engines and
// runtimes the tests start write to their own temp dir live under it. A
// run killed before it could clean up (a -timeout panic, a SIGKILL) leaves
// its root behind; the next run removes every root whose process is gone.
func TestMain(m *testing.M) {
	root, err := testTempRoot("facet-integration-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration tests: temp root:", err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", root)
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}
