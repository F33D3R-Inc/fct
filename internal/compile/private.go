package compile

import (
	"fmt"
	"hash/fnv"

	"facet/internal/ast"
)

// manglePrivate rewrites every `private` proc/view declared in app so its
// name (and, for a proc, every same-file call to it) is unique to this file
// before modules get merged together (mergeInto/checkDuplicates, below in
// compile.go). Without this, two files that each declare their own
// file-local helper under the same name — a debug entry view, an internal
// parsing helper, anything not meant to be part of a module's public
// surface — can never be imported side by side: the flat, unnamespaced
// merge would report a collision even though neither file's author ever
// meant the two to be the same declaration. Mangling only the declarations
// the author explicitly marked `private` keeps every existing, unmarked
// declaration's name exactly as written — this is purely additive and
// changes no behavior for a file that declares no `private` decl.
//
// Called once per file, right after that file is parsed (collectModules,
// below), so the rewrite always runs before this file's declarations ever
// reach mergeInto — a private name never leaks into another file's view of
// the graph.
func manglePrivate(app *ast.App, abs string) {
	suffix := fileSuffix(abs)
	renamed := map[string]string{} // old proc name -> mangled name, this file only

	for _, p := range app.Procs {
		if !p.Private {
			continue
		}
		mangled := p.Name + "~" + suffix
		renamed[p.Name] = mangled
		p.Name = mangled
	}
	for _, v := range app.Views {
		if v.Private {
			// A view's name carries no other same-file reference — routing
			// is by Path, never by Name (confirmed: nothing in ir/build.go
			// or compile/compose.go looks a view up by name) — so mangling
			// the declaration itself is the whole fix, no call-site rewrite.
			v.Name = v.Name + "~" + suffix
		}
	}
	if len(renamed) == 0 {
		return
	}
	// Every place a proc is invoked by name within this same file's AST needs
	// the identical rewrite, or a private proc's own callers would still
	// resolve to the pre-mangle name and fail to find it once mergeInto
	// combines this file's declarations with another's. A proc is called
	// from exactly two statement shapes — Do and Spawn — never from an
	// expression (ast.Call is builtins only), so only statement bodies need
	// walking, not every Expr in the file.
	for _, p := range app.Procs {
		rewriteProcCalls(p.Body, renamed)
	}
	for _, a := range app.Actions {
		rewriteProcCalls(a.Body, renamed)
	}
	for _, d := range app.Daemons {
		rewriteProcCalls(d.Body, renamed)
	}
}

// rewriteProcCalls walks a statement list, recursing into Loop/IfStmt bodies
// (the only nesting a proc/action/daemon body has), rewriting every Do/Spawn
// that names a proc in renamed to its mangled name. ast.Stmt values are
// stored by value in a []Stmt (see parser.go's ast.Do{}/ast.Spawn{}
// construction), so a rewritten Do/Spawn is written back to body[i]
// explicitly; Loop.Body/IfStmt.Then/IfStmt.Else are themselves slices, whose
// backing array is shared even through a by-value copy of the enclosing
// Loop/IfStmt, so recursing into them needs no such write-back.
func rewriteProcCalls(body []ast.Stmt, renamed map[string]string) {
	for i, s := range body {
		switch st := s.(type) {
		case ast.Do:
			if m, ok := renamed[st.Proc]; ok {
				st.Proc = m
				body[i] = st
			}
		case ast.Spawn:
			if m, ok := renamed[st.Proc]; ok {
				st.Proc = m
				body[i] = st
			}
		case ast.Loop:
			rewriteProcCalls(st.Body, renamed)
		case ast.IfStmt:
			rewriteProcCalls(st.Then, renamed)
			rewriteProcCalls(st.Else, renamed)
		}
	}
}

// fileSuffix derives a short, stable tag from a file's absolute path for
// mangling its private declarations. The same file always mangles to the
// same suffix (matters if a module is ever visited twice); not
// cryptographic — collision resistance only needs to hold across the
// handful of files one build actually imports, not adversarially.
func fileSuffix(abs string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(abs))
	return fmt.Sprintf("%08x", h.Sum32())
}
