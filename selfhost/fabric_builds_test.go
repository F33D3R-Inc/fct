package selfhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The Rust references the fabric tests run, built once per package run into
// one cargo target directory, in one profile:
//
//   - testdata/fabric_check, the harness that links the real fabric crates
//     (fabricCheckBinary);
//   - the fabric workspace's own binaries — `fabricd`, the `fabric` CLI and
//     `facet-protocol` — in a single cargo invocation
//     (fabricWorkspaceBinary).
//
// Each used to be its own build in its own target directory (and the
// binaries in debug beside a release harness), so every dependency the
// crates share — tokio, hyper, reqwest, serde — was compiled once per
// binary, and the first test to ask for each paid for it. Sharing the
// directory and the profile lets cargo reuse every artifact whose
// resolution is the same, and building the workspace binaries together
// compiles the workspace's crates once.

// fabricTarget: FCT_FABRIC_TARGET, or the persistent test cache.
func fabricTarget() string {
	if dir := os.Getenv("FCT_FABRIC_TARGET"); dir != "" {
		return dir
	}
	return fqTestCargoTarget("fct-fabric")
}

type fabricBuild struct {
	once sync.Once
	dir  string // the release directory the binaries land in
	err  string // "skip: ..." when cargo or the checkout is missing
}

func (b *fabricBuild) get(t *testing.T, what string, build func() (string, string)) string {
	t.Helper()
	b.once.Do(func() { b.dir, b.err = build() })
	if strings.HasPrefix(b.err, "skip: ") {
		t.Skip(what + " unavailable: " + strings.TrimPrefix(b.err, "skip: "))
	}
	if b.err != "" {
		t.Fatal(b.err)
	}
	return b.dir
}

func fabricCargoBuild(dir, manifest string, args ...string) (string, string) {
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		return "", "skip: cargo is not installed"
	}
	if _, err := os.Stat(filepath.Join(dir, manifest)); err != nil {
		return "", "skip: no " + filepath.Join(dir, manifest) + " beside fct"
	}
	target := fabricTarget()
	cmd := exec.Command(cargo, append([]string{"build", "--release", "--quiet"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "cargo build in " + dir + " failed: " + err.Error() + "\n" + string(out)
	}
	return filepath.Join(target, "release"), ""
}

var fabricCheckBuild, fabricWorkspaceBuild fabricBuild

// fabricCheckBinary: testdata/fabric_check, built once.
func fabricCheckBinary(t *testing.T) string {
	t.Helper()
	dir := fabricCheckBuild.get(t, "fabric_check", func() (string, string) {
		if _, err := os.Stat("../../fabric/crates/fabric-protocol/Cargo.toml"); err != nil {
			return "", "skip: no fabric checkout beside fct"
		}
		return fabricCargoBuild(filepath.Join("testdata", "fabric_check"), "Cargo.toml")
	})
	return filepath.Join(dir, "fabric_check")
}

// fabricWorkspaceBinary: one of the fabric workspace's binaries (`fabricd`,
// `fabric`, `facet-protocol`), all three built together once (--locked, so
// fabric/Cargo.lock is never rewritten).
func fabricWorkspaceBinary(t *testing.T, name string) string {
	t.Helper()
	dir := fabricWorkspaceBuild.get(t, "the fabric workspace's binaries", func() (string, string) {
		return fabricCargoBuild(filepath.Join("..", "..", "fabric"), "Cargo.toml",
			"--locked", "--bin", "fabricd", "--bin", "fabric", "--bin", "facet-protocol")
	})
	return filepath.Join(dir, name)
}
