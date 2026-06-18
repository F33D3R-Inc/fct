package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/F33D3R-Inc/fct/internal/ast"
)

// Backend is a code-generation target language. The compiler front end —
// parse → AST → semantic checks → the flat render-node stream — is entirely
// target-independent; a Backend supplies only the language-specific lowering.
//
// v0 had exactly one target (Go) baked directly into codegen, justified in
// DECISIONS.md ADR-0001 by "our only backend target today is Go." That ADR names
// its own reversal condition — "we commit to many backend targets (Rust, Node,
// Swift…) and want one portable compiler core" — which is ROADMAP #4. Extracting
// this interface is that reversal: the same compiler core now emits any target.
//
// Three surfaces are language-specific and self-contained:
//
//   - Expr:      an FDL expression  → a target-language expression
//   - FieldName: an FDL field name  → an idiomatic target identifier
//   - Types:     a set of facets    → that language's typed per-facet data decls
//
// A facet's render body (looks:) is the fourth surface. For the Go target it is
// lowered to html/template (genTemplate) and rendered in-process. The non-Go
// targets do NOT re-transpile the body per language: the flat Text/Interp/Ctrl
// stream is already a neutral render program (the same description the manifest
// and the client runtime carry), so a target runtime interprets it directly.
// That neutral render IR is specified separately (see task #6 / docs).
type Backend interface {
	// Name is the fct.toml [compiler] target value this backend serves
	// ("go" | "node" | "python" | "rust").
	Name() string

	// Expr lowers an FDL expression to a target-language expression, given the
	// loop variables currently in scope (a path whose head is in scope is a local
	// binding rather than a field of the facet's data).
	Expr(expr string, scope []string) (string, error)

	// FieldName maps an FDL field name to an idiomatic identifier in the target —
	// user_id → UserID (Go), userId (Node), user_id (Python/Rust).
	FieldName(field string) string

	// Types emits the typed per-facet data declarations for the given module/
	// package name: a struct/interface/dataclass per facet's what: inputs, so apps
	// render with compile-time-checked (or at least documented) data.
	Types(pkg string, facets []*ast.Facet) string

	// TypesFile is the on-disk filename Types writes to (types.go, types.ts, …).
	TypesFile() string
}

// backends is the registry of available code-generation targets, keyed by their
// fct.toml [compiler] target value. Registering a backend here (plus its file)
// is all it takes to add a language; cmd/fct selects from this map.
var backends = map[string]Backend{
	"go": goBackend{},
}

// BackendFor returns the Backend for an fct.toml [compiler] target value. An
// empty target defaults to "go" (the v0 behavior, so existing projects and the
// scaffold keep compiling unchanged). An unknown target is a compile error that
// NAMES the available targets — the project's stated bar is errors that point at
// the fix, not blank output.
func BackendFor(target string) (Backend, error) {
	if target == "" {
		target = "go"
	}
	b, ok := backends[target]
	if !ok {
		return nil, fmt.Errorf("unknown compiler target %q (available: %s)", target, strings.Join(BackendNames(), ", "))
	}
	return b, nil
}

// BackendNames lists the registered target names in sorted order — for error
// messages and `fct` diagnostics.
func BackendNames() []string {
	names := make([]string, 0, len(backends))
	for n := range backends {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ── Go backend (the v0 target) ──────────────────────────────────────────────

// goBackend is the original target: html/template render bodies plus idiomatic
// Go structs. It delegates to the functions codegen has always used (goExpr,
// GoName, GoStructs), so extracting the interface is behavior-preserving — the
// Go path emits byte-for-byte what it did before.
type goBackend struct{}

func (goBackend) Name() string { return "go" }

func (goBackend) Expr(expr string, scope []string) (string, error) {
	return goExpr(expr, scope)
}

func (goBackend) FieldName(field string) string { return GoName(field) }

func (goBackend) Types(pkg string, facets []*ast.Facet) string {
	return GoStructs(pkg, facets)
}

func (goBackend) TypesFile() string { return "types.go" }
