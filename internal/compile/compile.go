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
	if len(app.CSSFiles) > 0 {
		return nil, fmt.Errorf("css from \"...\" is only supported when compiling from a file (run `facet <command> <file.fct>`)")
	}
	return ir.Build(app)
}

// File compiles a Facet app from a path, resolving every imported module first.
// Imports are de-duplicated (a module pulled in by two files is loaded once) and
// import cycles are reported rather than looped.
//
// Two composition models share this entry point. A plain `app` (with optional
// `app` imports) is flat-merged into one graph, the original behavior. A layered
// build — a `playground` baseplate that mounts a `wireframe` and snaps `ui`/`data`
// facets into its typed sockets — is composited by `compose`. Both produce one
// ast.App that placement runs over exactly once.
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
	// Check the entry project's own facet.json — if its `facet` range is not
	// met, fail now with one clear message rather than falling into parsing
	// syntax this toolchain predates and failing with a cascade of confusing
	// parse errors instead.
	if err := res.CheckToolchain(); err != nil {
		return nil, err
	}
	visited := map[string]bool{}
	facets, err := collectModules(abs, visited, nil, res)
	if err != nil {
		return nil, err
	}
	if err := res.Save(); err != nil {
		return nil, err
	}

	var root *ast.App
	if isLayered(facets) {
		// Typed bricks: snap them together into one surface.
		if root, err = compose(facets); err != nil {
			return nil, err
		}
	} else {
		// Plain apps: flat-merge every module's declarations into the entry graph.
		root = facets[0]
		for _, m := range facets[1:] {
			mergeInto(root, m)
		}
	}
	if err := checkDuplicates(root); err != nil {
		return nil, err
	}
	return ir.Build(root)
}

// collectModules reads, parses, and recursively gathers a file and its imports
// into a flat, de-duplicated list with the entry file first (the same depth-first
// order the legacy merge used). stack is the chain of files currently being
// resolved, used to detect cycles; visited pulls a shared module in only once.
func collectModules(abs string, visited map[string]bool, stack []string, res *registry.Resolver) ([]*ast.App, error) {
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
	// `css from "styles.css"` is the external-file counterpart of an inline
	// `css:` block: a sibling stylesheet on disk, referenced by the same quoted
	// path convention `import` uses. Resolved here (not in the parser, which has
	// no file-system access) relative to this file's own directory — never
	// fetched remotely, since a stylesheet is never a shared module — and its raw
	// content is folded into app.CSS exactly like an inline block, so every
	// downstream pass (placement, codegen) sees one stylesheet string regardless
	// of which spelling the author used.
	for _, ref := range app.CSSFiles {
		cssAbs := ref.Path
		if !filepath.IsAbs(cssAbs) {
			cssAbs = filepath.Join(dir, ref.Path)
		}
		cssSrc, err := os.ReadFile(cssAbs)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: css from %q: %w", filepath.Base(abs), ref.Line, ref.Path, err)
		}
		app.CSS = joinStylesheets(app.CSS, string(cssSrc))
	}
	// `asset from "path"` is css from's general counterpart: any file type,
	// resolved the same way (relative to this file's own directory) but found
	// by walking the whole parsed file rather than one collected list — see
	// resolveAssets' own doc for why.
	if err := resolveAssets(app, dir); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(abs), err)
	}
	list := []*ast.App{app}
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
			continue // already collected via another import path
		}
		visited[impAbs] = true
		sub, err := collectModules(impAbs, visited, append(stack, abs), res)
		if err != nil {
			return nil, err
		}
		list = append(list, sub...)
	}
	return list, nil
}

// mergeInto folds an imported module's declarations into dst, after dst's own
// (so the root file's first view stays the "/" page). Names are kept as written;
// checkDuplicates validates uniqueness across the merged graph.
func mergeInto(dst, src *ast.App) {
	dst.Auth = dst.Auth || src.Auth
	dst.Entities = append(dst.Entities, src.Entities...)
	dst.Records = append(dst.Records, src.Records...)
	dst.Structs = append(dst.Structs, src.Structs...)
	dst.Enums = append(dst.Enums, src.Enums...)
	dst.Types = append(dst.Types, src.Types...)
	dst.Messages = append(dst.Messages, src.Messages...)
	dst.States = append(dst.States, src.States...)
	dst.Derives = append(dst.Derives, src.Derives...)
	dst.Policies = append(dst.Policies, src.Policies...)
	dst.Actions = append(dst.Actions, src.Actions...)
	dst.Procs = append(dst.Procs, src.Procs...)
	dst.Jobs = append(dst.Jobs, src.Jobs...)
	dst.Daemons = append(dst.Daemons, src.Daemons...)
	dst.Components = append(dst.Components, src.Components...)
	dst.Layouts = append(dst.Layouts, src.Layouts...)
	dst.Views = append(dst.Views, src.Views...)
	dst.Services = append(dst.Services, src.Services...)
	dst.Files = append(dst.Files, src.Files...)
	dst.Theme = append(dst.Theme, src.Theme...)

	// A facet's stylesheet ships with the facet, exactly like its theme
	// variables directly above. Without this an imported atom could declare a
	// `css:` block that compiled, validated, and then never reached the page —
	// silently, because a missing rule looks like a specificity problem rather
	// than a missing file. That made a component with any layout of its own
	// undistributable: it rendered correctly only in whichever host app happened
	// to carry a copy of its rules.
	dst.CSS = joinStylesheets(dst.CSS, src.CSS)

	// Binary/static assets a facet bundled travel with it for the same reason
	// its stylesheet does, directly above.
	mergeAssets(dst, src)
}

// joinStylesheets concatenates two stylesheet fragments. Mirrors the parser's
// own joiner, which is unexported; the rule it encodes — a newline between
// fragments, and no leading newline when the first is empty — has to hold here
// too, because these fragments are concatenated across module boundaries where
// a stray blank line is the only thing distinguishing one facet's rules from
// another's in a diagnostic.
func joinStylesheets(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n" + b
	}
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
		entities, records, structs, enums, states, derives []string
		policies, actions, procs, jobs, daemons            []string
		components, layouts, views                         []string
	)
	for _, e := range app.Entities {
		entities = append(entities, e.Name)
	}
	for _, r := range app.Records {
		records = append(records, r.Name)
	}
	for _, s := range app.Structs {
		structs = append(structs, s.Name)
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
	for _, p := range app.Procs {
		procs = append(procs, p.Name)
	}
	for _, j := range app.Jobs {
		jobs = append(jobs, j.Name)
	}
	for _, d := range app.Daemons {
		daemons = append(daemons, d.Name)
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
		{"entity", entities}, {"record", records}, {"struct", structs},
		{"enum", enums}, {"state", states}, {"derive", derives},
		{"policy", policies}, {"action", actions}, {"proc", procs}, {"job", jobs},
		{"daemon", daemons}, {"component", components}, {"layout", layouts}, {"view", views},
	} {
		if err := dup(check.kind, check.names); err != nil {
			return err
		}
	}
	return nil
}
