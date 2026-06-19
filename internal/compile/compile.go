// Package compile is the front-to-IR pipeline: source text → ast → IR. It is
// the one entry point both the CLI and the runtime use, so there is exactly one
// definition of what an application means.
//
// An app may be split across files: a file declares `import "other.fct"` lines
// above its `app` header, and File resolves each import relative to the
// importing file and merges every module's declarations into one graph before
// placement. That is how a large app stays many small files, and how a reusable
// "facet" (a module of entities/actions/components/policies) is pulled into
// another app — the same mechanism serves both. Placement still runs once, over
// the merged graph, so a module never has to know whether its pieces land on the
// server or the client.
package compile

import (
	"fmt"
	"os"
	"path/filepath"

	"facet/internal/ast"
	"facet/internal/ir"
	"facet/internal/parser"
	"facet/internal/registry"
)

// String compiles inline Facet source (no file context) to the IR. It is used
// for embedded snippets and tests; because there is no file to resolve imports
// against, a snippet that declares `import` is rejected — use File for that.
func String(src string) (*ir.IR, error) {
	app, err := parser.Parse(src)
	if err != nil {
		return nil, err
	}
	if len(app.Imports) > 0 {
		return nil, fmt.Errorf("import is only supported when compiling from a file (run `facet <command> <file.fct>`)")
	}
	return ir.Build(app)
}

// File compiles a Facet app from a path, resolving and merging every imported
// module first. Imports are de-duplicated (a module pulled in by two files is
// merged once) and import cycles are reported rather than looped.
func File(path string) (*ir.IR, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// The resolver turns every import — local path or remote github.com/ ref —
	// into a file on disk, fetching and pinning remote facets in facet.lock as a
	// side effect. It is rooted at the entry file's directory, where the lock
	// lives. A purely local app never touches the network or writes a lock.
	res, err := registry.New(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	visited := map[string]bool{}
	root, err := loadModule(abs, visited, nil, res)
	if err != nil {
		return nil, err
	}
	if err := res.Save(); err != nil {
		return nil, err
	}
	if err := checkDuplicates(root); err != nil {
		return nil, err
	}
	return ir.Build(root)
}

// loadModule reads, parses, and recursively merges a file and its imports into a
// single ast.App. stack is the chain of files currently being resolved, used to
// detect cycles; visited is the set of modules already merged, used to pull a
// shared module in only once.
func loadModule(abs string, visited map[string]bool, stack []string, res *registry.Resolver) (*ast.App, error) {
	for _, s := range stack {
		if s == abs {
			return nil, fmt.Errorf("import cycle through %s", filepath.Base(abs))
		}
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	app, err := parser.Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(abs), err)
	}
	dir := filepath.Dir(abs)
	for _, imp := range app.Imports {
		// Resolve turns the import string into an absolute local path: a local ref
		// joins against this file's dir; a remote github.com/ ref is fetched and
		// cached first, returning a path inside the cache. Everything below this
		// point is identical for both — a remote facet is just files on disk.
		impAbs, err := res.Resolve(imp, dir)
		if err != nil {
			return nil, err
		}
		if visited[impAbs] {
			continue // already merged via another import path
		}
		visited[impAbs] = true
		sub, err := loadModule(impAbs, visited, append(stack, abs), res)
		if err != nil {
			return nil, err
		}
		mergeInto(app, sub)
	}
	return app, nil
}

// mergeInto folds an imported module's declarations into dst, after dst's own
// (so the root file's first view stays the "/" page). Names are kept as written;
// checkDuplicates validates uniqueness across the merged graph.
func mergeInto(dst, src *ast.App) {
	dst.Auth = dst.Auth || src.Auth
	dst.Entities = append(dst.Entities, src.Entities...)
	dst.Enums = append(dst.Enums, src.Enums...)
	dst.States = append(dst.States, src.States...)
	dst.Derives = append(dst.Derives, src.Derives...)
	dst.Policies = append(dst.Policies, src.Policies...)
	dst.Actions = append(dst.Actions, src.Actions...)
	dst.Jobs = append(dst.Jobs, src.Jobs...)
	dst.Components = append(dst.Components, src.Components...)
	dst.Layouts = append(dst.Layouts, src.Layouts...)
	dst.Views = append(dst.Views, src.Views...)
	dst.Theme = append(dst.Theme, src.Theme...)
}

// checkDuplicates reports the first declaration name that appears twice within a
// kind after merging, so colliding modules fail with a clear message instead of
// a confusing downstream error.
func checkDuplicates(app *ast.App) error {
	dup := func(kind string, names []string) error {
		seen := map[string]bool{}
		for _, n := range names {
			if seen[n] {
				return fmt.Errorf("%s %q is declared more than once (check your imported modules)", kind, n)
			}
			seen[n] = true
		}
		return nil
	}
	var (
		entities, enums, states, derives []string
		policies, actions, jobs          []string
		components, layouts, views       []string
	)
	for _, e := range app.Entities {
		entities = append(entities, e.Name)
	}
	for _, e := range app.Enums {
		enums = append(enums, e.Name)
	}
	for _, s := range app.States {
		states = append(states, s.Name)
	}
	for _, d := range app.Derives {
		derives = append(derives, d.Name)
	}
	for _, p := range app.Policies {
		policies = append(policies, p.Name)
	}
	for _, a := range app.Actions {
		actions = append(actions, a.Name)
	}
	for _, j := range app.Jobs {
		jobs = append(jobs, j.Name)
	}
	for _, c := range app.Components {
		components = append(components, c.Name)
	}
	for _, l := range app.Layouts {
		layouts = append(layouts, l.Name)
	}
	for _, v := range app.Views {
		views = append(views, v.Name)
	}
	for _, check := range []struct {
		kind  string
		names []string
	}{
		{"entity", entities}, {"enum", enums}, {"state", states}, {"derive", derives},
		{"policy", policies}, {"action", actions}, {"job", jobs},
		{"component", components}, {"layout", layouts}, {"view", views},
	} {
		if err := dup(check.kind, check.names); err != nil {
			return err
		}
	}
	return nil
}
