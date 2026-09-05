package main

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	fctast "facet/internal/ast"
)

// `facet lang` — the actual shape of the language right now, read from the
// compiler's own tables and source instead of restated in a document that goes
// stale. wiki/Language-Reference.md still documents v1.3.0 while the compiler
// ships 1.31.0, and the storefront app learned the real language from the
// scaffold templates instead of the wiki — this exists so there is a truthful
// place to ask that isn't "read the templates and guess."
//
// Nothing here is hand-listed:
//
//	controls   read directly from facet/internal/ast.Controls, the very map
//	           the parser dispatches on and both renderers key off — importing
//	           it is the whole implementation, so it cannot drift from what
//	           the compiler does.
//	nodes      the view node kinds, derived by parsing internal/ast's own
//	           source and collecting every `func (X) node() {}` receiver — the
//	           literal mechanism ast.Node uses to mark a type as a view node.
//	builtins   the invocable builtin functions, derived by parsing
//	           internal/parser's source and reading the case labels out of
//	           isBuiltinCall, the single function the parser calls to decide.
//	modifiers  every `@name` annotation keyword, derived by scanning the same
//	           parsed source for string literals of that shape (comments are
//	           not literals, so prose mentioning "@required" is never picked
//	           up as a false positive).
//
// The source-derived sections need the compiler's own .go source on disk next
// to this binary — true for a checkout built with `go build ./cmd/...`, not
// necessarily true of a binary copied elsewhere. When it cannot be found this
// says so plainly and still prints the control table, which needs no source
// file at all. Silently falling back to a remembered list would be exactly the
// staleness this command exists to avoid.
func cmdLang(args []string) int {
	fmt.Println("facet — the language, derived from the compiler's own tables (not a document)")

	writeControls(os.Stdout)

	src, err := compilerSourceDir()
	if err != nil {
		fmt.Printf("\n(node kinds, builtins and modifiers need the compiler's own .go source on disk, "+
			"and it could not be found: %s — showing only what facet/internal/ast exports directly.)\n", err)
		return 0
	}

	astFiles, err := parseGoDir(filepath.Join(src, "internal", "ast"))
	if err != nil {
		fmt.Printf("\n(could not parse internal/ast: %s)\n", err)
		return 0
	}
	parserFiles, err := parseGoDir(filepath.Join(src, "internal", "parser"))
	if err != nil {
		fmt.Printf("\n(could not parse internal/parser: %s)\n", err)
		return 0
	}

	writeNodeKinds(os.Stdout, astFiles)
	writeBuiltins(os.Stdout, parserFiles)
	writeModifiers(os.Stdout, append(astFiles, parserFiles...))
	return 0
}

// compilerSourceDir locates the fct module root (the directory whose
// internal/ast and internal/parser this binary was built from), so the
// source-derived sections below can parse the compiler's own .go files.
//
// runtime.Caller(0) is the path THIS file was compiled from, recorded by the
// compiler at build time — valid on the machine that built the binary, which
// is exactly the case `go build ./cmd/...` and `go test ./cmd/...` verify. It
// is not valid for a binary copied to a different machine or checkout, which
// is why every caller here treats a miss as "unavailable," never as "assume a
// fixed list."
func compilerSourceDir() (string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("no caller info embedded in this binary")
	}
	// self is cmd/facet/lang.go; the module root is two directories up.
	root := filepath.Dir(filepath.Dir(filepath.Dir(self)))
	if _, err := os.Stat(filepath.Join(root, "internal", "ast")); err != nil {
		return "", fmt.Errorf("no internal/ast next to %s", self)
	}
	return root, nil
}

// parseGoDir parses every non-test .go file in dir and returns their ASTs.
func parseGoDir(dir string) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := goparser.ParseFile(fset, filepath.Join(dir, name), nil, goparser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// writeControls prints the two-way controls straight from facet/internal/ast.
// Controls — the single map the parser, the lowering pass and both renderers
// all key off — so this table is exactly what the compiler does, not a
// snapshot of it.
func writeControls(w *os.File) {
	fmt.Fprintln(w, "\nCONTROLS (facet/internal/ast.Controls — the parser dispatches on this map)")
	kinds := make([]string, 0, len(fctast.Controls))
	for k := range fctast.Controls {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		spec := fctast.Controls[k]
		cell := spec.Cell
		if cell == "" {
			cell = "(decided by its options)"
		}
		fmt.Fprintf(w, "  %-10s binds a %-24s %s\n", k, cell, spec.Rule)
	}
}

// nodeReceiver returns the receiver type name of a `func (X) node() {}` or
// `func (X) node() {}`-shaped declaration, and whether decl is one.
func nodeReceiver(decl *ast.FuncDecl) (string, bool) {
	if decl.Name.Name != "node" || decl.Recv == nil || len(decl.Recv.List) != 1 {
		return "", false
	}
	t := decl.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name, true
	}
	return "", false
}

// nodeKinds returns every view node kind: every type that implements
// ast.Node's `node()` marker method, found by walking internal/ast's own
// source for that exact declaration shape — the literal mechanism that makes
// a type a node, per ast.go's own comment on the Node interface.
func nodeKinds(files []*ast.File) []string {
	var kinds []string
	for _, f := range files {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok {
				if name, ok := nodeReceiver(fd); ok {
					kinds = append(kinds, name)
				}
			}
		}
	}
	sort.Strings(kinds)
	return kinds
}

// writeNodeKinds prints the node kinds nodeKinds derives.
func writeNodeKinds(w *os.File, files []*ast.File) {
	kinds := nodeKinds(files)
	fmt.Fprintf(w, "\nNODE KINDS (%d — every type implementing ast.Node, from internal/ast's own `func (X) node() {}` declarations)\n", len(kinds))
	writeWrapped(w, kinds)
}

// builtinNames returns the invocable builtin functions, read out of
// internal/parser's isBuiltinCall — the one function the parser calls to
// decide whether a name in call position is a builtin. `count`, `sum`,
// `avg`, `min`(range) and `max`(range) are deliberately absent: those are
// aggregates over a range, a different grammar position, dispatched
// elsewhere — isBuiltinCall does not claim them, so neither does this.
func builtinNames(files []*ast.File) []string {
	var names []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "isBuiltinCall" {
				return true
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				cc, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, expr := range cc.List {
					if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							names = append(names, s)
						}
					}
				}
				return true
			})
			return true
		})
	}
	sort.Strings(names)
	return names
}

// writeBuiltins prints the builtins builtinNames derives.
func writeBuiltins(w *os.File, files []*ast.File) {
	names := builtinNames(files)
	fmt.Fprintf(w, "\nBUILT-IN FUNCTIONS (%d — internal/parser's isBuiltinCall, the parser's own list)\n", len(names))
	writeWrapped(w, names)
}

// modifierPattern matches an `@name` annotation keyword written as a Go string
// literal in the compiler's source — never a comment, since go/parser only
// produces BasicLit nodes for literals actually in code.
func modifierPattern(s string) bool {
	if len(s) < 2 || s[0] != '@' {
		return false
	}
	for _, r := range s[1:] {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// modifierNames returns every `@name` annotation keyword the compiler's
// source writes as a literal — `@required`, `@client`, `@secret` and the rest
// — read out of the actual string literals in the given files, not retyped
// by hand. Comments are never picked up: go/parser only produces BasicLit
// nodes for literals actually in code.
func modifierNames(files []*ast.File) []string {
	seen := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil && modifierPattern(s) {
				seen[s] = true
			}
			return true
		})
	}
	names := make([]string, 0, len(seen))
	for s := range seen {
		names = append(names, s)
	}
	sort.Strings(names)
	return names
}

// writeModifiers prints the modifiers modifierNames derives.
func writeModifiers(w *os.File, files []*ast.File) {
	names := modifierNames(files)
	fmt.Fprintf(w, "\nMODIFIERS (%d — every `@name` annotation literal found in internal/ast and internal/parser)\n", len(names))
	writeWrapped(w, names)
}

// writeWrapped prints names six to a line, so a hundred-odd node kinds do not
// scroll one per line.
func writeWrapped(w *os.File, names []string) {
	if len(names) == 0 {
		fmt.Fprintln(w, "  (none found)")
		return
	}
	const perLine = 6
	for i := 0; i < len(names); i += perLine {
		end := min(i+perLine, len(names))
		fmt.Fprintln(w, "  "+strings.Join(names[i:end], ", "))
	}
}
