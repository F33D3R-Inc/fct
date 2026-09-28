package selfhost

import (
	"os"
	"testing"
)

// fqReferenceEnv opts a test run into the Rust FacetQL — the parity
// reference the fct engine (fqserver.fct, what `facet facetql` ships) is held
// to. Nothing in the toolchain runs the Rust engine; only a parity test does,
// and only when asked: FACETQL_REFERENCE=rust.
const fqReferenceEnv = "FACETQL_REFERENCE"

// fqRustReference skips the calling test unless the Rust reference is opted
// into. Every helper that builds or starts the Rust engine calls it first.
func fqRustReference(t testing.TB) {
	t.Helper()
	if os.Getenv(fqReferenceEnv) != "rust" {
		t.Skip("parity against the Rust reference engine is opt-in: set " + fqReferenceEnv + "=rust")
	}
}
