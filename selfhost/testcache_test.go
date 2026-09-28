package selfhost

import (
	"os"
	"path/filepath"
)

// fqTestCargoTarget is where a test builds a Rust reference binary with
// cargo (CARGO_TARGET_DIR): a persistent directory under the user's cache,
// one per build, reused by every later run. A cargo target is hundreds of
// megabytes and incremental — it belongs in a cache, never in the system
// temp directory, where each copy stayed behind (TestMain now points TMPDIR
// at a per-run root, so a target there would also be rebuilt from nothing
// on every run). Without a user cache directory it falls back to the temp
// directory.
func fqTestCargoTarget(name string) string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return filepath.Join(os.TempDir(), name)
	}
	return filepath.Join(base, "facet-test-targets", name)
}
