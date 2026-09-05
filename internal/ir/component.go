package ir

import (
	"fmt"
	"strings"

	"facet/internal/ast"
)

// Component call-site expansion — the mechanism behind reference parameters and
// component children.
//
// A component's value parameters are bound to *values* at render time, which is
// enough for what a fragment shows and not enough for what it is made of. The
// three things a reusable control is parameterized by are not values at all:
//
//	bind      names a state cell         (a text field, select, toggle, upload)
//	->        names an action            (a submit button, a pending/failed wrapper)
//	children  are a node tree            (a card, a panel, a stack, a split)
//
// Each is an identity the compiler must resolve — which cell, which action, which
// nodes — and none of them survives being turned into a runtime value. So they
// are bound where they are known: at the call site. A component that declares a
// reference parameter or contains a `slot` is a *template*; each `use` of it is
// expanded into its own IR component with the caller's names substituted in and
// the caller's children spliced at the slot.
//
// That keeps the whole feature inside the compiler. The IR a template expands to
// is an ordinary component with ordinary value parameters, so no renderer learns
// a new node kind, no name is resolved twice, and every check a hand-written
// component gets — placement of a bound cell, arity of a called action, the route
// a link points at — is the same check, run against concrete names.
//
// A component with neither a reference parameter nor a slot is untouched: it is
// still lowered once and shared by every call site.

// isTemplate reports whether a component must be expanded per call site rather
// than lowered once and shared.
func (e *env) isTemplate(name string) bool {
	cm, ok := e.compAST[name]
	if !ok {
		return false
	}
	return e.compSlot[name] || hasRefParam(cm.Params)
}

func hasRefParam(ps []ast.Param) bool {
	for _, p := range ps {
		if p.Ref != ast.RefValue {
			return true
		}
	}
	return false
}

// valueParams returns the parameters that survive expansion — the ones still
// bound to a value at render time. Reference parameters are substituted into the
// body and carry nothing to the runtime.
func valueParams(ps []ast.Param) []ast.Param {
	var out []ast.Param
	for _, p := range ps {
		if p.Ref == ast.RefValue {
			out = append(out, p)
		}
	}
	return out
}

// bareRef returns the name of an expression that is exactly one identifier. A
// reference argument must be one: it names a declaration, and there is nothing to
// evaluate.
func bareRef(ex ast.Expr) (string, bool) {
	r, ok := ex.(ast.Ref)
	return r.Name, ok
}

// countSlots counts the `slot` nodes in a node tree.
func countSlots(in []ast.Node) int {
	n := 0
	for _, node := range in {
		switch t := node.(type) {
		case ast.Slot:
			n++
		case ast.Modified:
			n += countSlots([]ast.Node{t.Inner})
		case ast.Box:
			n += countSlots(t.Children)
		case ast.Row:
			n += countSlots(t.Children)
		case ast.For:
			n += countSlots(t.Body)
		case ast.If:
			n += countSlots(t.Body)
		case ast.Overlay:
			n += countSlots(t.Body)
		case ast.Form:
			n += countSlots(t.Body)
		case ast.Use:
			n += countSlots(t.Body)
		case ast.Tabs:
			for _, tb := range t.Tabs {
				n += countSlots(tb.Body)
			}
		case ast.Match:
			for _, cs := range t.Cases {
				n += countSlots(cs.Body)
			}
			n += countSlots(t.Else)
		}
	}
	return n
}

// componentBinders returns every name a component body binds for itself: its
// value parameters and every loop/aggregate item variable inside it.
//
// A reference argument may not collide with one. Substituting `value` → `draft`
// into a body that also writes `for draft in Post` would silently re-point the
// substituted reads at the loop row, so the collision is refused instead.
func componentBinders(cm *ast.Component) map[string]bool {
	out := map[string]bool{}
	for _, p := range cm.Params {
		if p.Ref == ast.RefValue {
			out[p.Name] = true
		}
	}
	var expr func(ast.Expr)
	expr = func(ex ast.Expr) {
		switch t := ex.(type) {
		case ast.Get:
			expr(t.Obj)
		case ast.EntityGet:
			expr(t.Key)
		case ast.Agg:
			if t.Var != "" {
				out[t.Var] = true
			}
			if t.Where != nil {
				expr(t.Where)
			}
		case ast.Call:
			for _, a := range t.Args {
				expr(a)
			}
		case ast.ListLit:
			for _, el := range t.Elems {
				expr(el)
			}
		case ast.Bin:
			expr(t.L)
			expr(t.R)
		case ast.Un:
			expr(t.X)
		}
	}
	var segs func([]ast.Seg)
	segs = func(ss []ast.Seg) {
		for _, s := range ss {
			if s.Expr != nil {
				expr(s.Expr)
			}
		}
	}
	// A repeating option binds an item variable exactly as a `for` does, and its
	// label and value are expressions like any other. Both choice-list nodes read
	// it here, so neither can forget half of it.
	options := func(opts []ast.Option) {
		for _, o := range opts {
			segs(o.Label)
			expr(o.Val)
			if o.From != nil {
				out[o.From.Var] = true
				expr(o.From.Where)
				expr(o.From.Limit)
			}
		}
	}
	var walk func([]ast.Node)
	walk = func(in []ast.Node) {
		for _, n := range in {
			switch t := n.(type) {
			case ast.Modified:
				segs(t.Class)
				walk([]ast.Node{t.Inner})
			case ast.Box:
				walk(t.Children)
			case ast.Row:
				walk(t.Children)
			case ast.Text:
				segs(t.Segs)
			case ast.Heading:
				expr(t.Level)
				segs(t.Segs)
			case ast.Image:
				segs(t.Segs)
				segs(t.Alt)
			case ast.Video:
				segs(t.Segs)
				segs(t.Alt)
			case ast.Richtext:
				segs(t.Segs)
			case ast.Badge:
				segs(t.Segs)
			case ast.Icon:
				segs(t.Segs)
			case ast.Link:
				segs(t.LabelSegs)
				segs(t.PathSegs)
			case ast.Button:
				segs(t.Label)
				for _, a := range t.Args {
					expr(a)
				}
			case ast.Input:
				segs(t.Placeholder)
			case ast.Control:
				segs(t.Label)
				segs(t.Placeholder)
				options(t.Options)
			case ast.Typeahead:
				segs(t.Placeholder)
			case ast.Upload:
				segs(t.Label)
			case ast.Select:
				options(t.Options)
			case ast.Form:
				segs(t.Submit)
				for _, a := range t.Args {
					expr(a)
				}
				walk(t.Body)
			case ast.For:
				out[t.Var] = true
				if t.Where != nil {
					expr(t.Where)
				}
				if t.Limit != nil {
					expr(t.Limit)
				}
				walk(t.Body)
			case ast.If:
				expr(t.Cond)
				walk(t.Body)
			case ast.Overlay:
				walk(t.Body)
			case ast.Tabs:
				for _, tb := range t.Tabs {
					segs(tb.Label)
					walk(tb.Body)
				}
			case ast.Match:
				expr(t.Expr)
				for _, cs := range t.Cases {
					walk(cs.Body)
				}
				walk(t.Else)
			case ast.Use:
				for _, a := range t.Args {
					expr(a)
				}
				walk(t.Body)
			}
		}
	}
	walk(cm.Root)
	return out
}

// ── substitution ─────────────────────────────────────────────────────────────
//
// Every reference parameter is replaced, throughout the body, by the name the
// call site passed. The rewrite is a copy: a template is expanded once per call
// site, so it must never mutate the AST it expands from — every slice it touches
// is cloned rather than written through.

func rename(s string, m map[string]string) string {
	if v, ok := m[s]; ok {
		return v
	}
	return s
}

func substNodes(in []ast.Node, m map[string]string) []ast.Node {
	if len(in) == 0 {
		return nil
	}
	out := make([]ast.Node, len(in))
	for i, n := range in {
		out[i] = substNode(n, m)
	}
	return out
}

func substNode(n ast.Node, m map[string]string) ast.Node {
	switch t := n.(type) {
	case ast.Modified:
		t.Class = substSegs(t.Class, m)
		t.Inner = substNode(t.Inner, m)
		return t
	case ast.Box:
		t.Children = substNodes(t.Children, m)
		return t
	case ast.Row:
		t.Children = substNodes(t.Children, m)
		return t
	case ast.Text:
		t.Segs = substSegs(t.Segs, m)
		return t
	case ast.Heading:
		// The level is an expression like any other, so a template that takes its
		// depth through a substituted name resolves it here; a heading that fell
		// through to the default below would keep the caller's un-renamed names and
		// read whatever happened to answer to them.
		t.Level = substExpr(t.Level, m)
		t.Segs = substSegs(t.Segs, m)
		return t
	case ast.Image:
		t.Segs = substSegs(t.Segs, m)
		t.Alt = substSegs(t.Alt, m)
		return t
	case ast.Video:
		t.Segs = substSegs(t.Segs, m)
		t.Alt = substSegs(t.Alt, m)
		return t
	case ast.Richtext:
		t.Segs = substSegs(t.Segs, m)
		return t
	case ast.Badge:
		t.Segs = substSegs(t.Segs, m)
		return t
	case ast.Icon:
		t.Segs = substSegs(t.Segs, m)
		return t
	case ast.Link:
		t.LabelSegs = substSegs(t.LabelSegs, m)
		t.PathSegs = substSegs(t.PathSegs, m)
		return t
	case ast.Button:
		t.Label = substSegs(t.Label, m)
		t.Action = rename(t.Action, m)
		t.Args = substExprs(t.Args, m)
		return t
	case ast.Form:
		t.Action = rename(t.Action, m)
		t.Submit = substSegs(t.Submit, m)
		t.Args = substExprs(t.Args, m)
		t.Body = substNodes(t.Body, m)
		return t
	case ast.Input:
		t.Bind = rename(t.Bind, m)
		t.Placeholder = substSegs(t.Placeholder, m)
		return t
	case ast.Control:
		// The reason controls exist as reference parameters at all: `component
		// CheckboxField(label: text, v: cell bool): checkbox bind v` is a checkbox
		// whose cell the caller names, and this is where that name is resolved.
		t.Bind = rename(t.Bind, m)
		t.Label = substSegs(t.Label, m)
		t.Placeholder = substSegs(t.Placeholder, m)
		t.Options = substOptions(t.Options, m)
		return t
	case ast.Select:
		t.Bind = rename(t.Bind, m)
		t.Options = substOptions(t.Options, m)
		return t
	case ast.Upload:
		t.Bind = rename(t.Bind, m)
		t.Label = substSegs(t.Label, m)
		return t
	case ast.Typeahead:
		t.Bind = rename(t.Bind, m)
		t.Placeholder = substSegs(t.Placeholder, m)
		return t
	case ast.Overlay:
		t.Bind = rename(t.Bind, m)
		t.Body = substNodes(t.Body, m)
		return t
	case ast.Tabs:
		t.Bind = rename(t.Bind, m)
		tabs := make([]ast.Tab, len(t.Tabs))
		for i, tb := range t.Tabs {
			tb.Label = substSegs(tb.Label, m)
			tb.Body = substNodes(tb.Body, m)
			tabs[i] = tb
		}
		t.Tabs = tabs
		return t
	case ast.Match:
		t.Expr = substExpr(t.Expr, m)
		cases := make([]ast.MatchCase, len(t.Cases))
		for i, cs := range t.Cases {
			cs.Body = substNodes(cs.Body, m)
			cases[i] = cs
		}
		t.Cases = cases
		t.Else = substNodes(t.Else, m)
		return t
	case ast.For:
		// The loop variable is a binding occurrence, so it is renamed with the
		// reads of it: an alpha-renaming has to move the binder too, or the body's
		// renamed reads would resolve to nothing.
		t.Var = rename(t.Var, m)
		t.Coll = rename(t.Coll, m)
		t.Where = substExpr(t.Where, m)
		t.Limit = substExpr(t.Limit, m)
		t.Body = substNodes(t.Body, m)
		return t
	case ast.If:
		t.Cond = substExpr(t.Cond, m)
		t.Body = substNodes(t.Body, m)
		return t
	case ast.Use:
		t.Args = substExprs(t.Args, m)
		t.Body = substNodes(t.Body, m)
		return t
	}
	// Slot and SlotRef carry no name that a parameter can stand for.
	return n
}

// substOptions rewrites a choice list.
//
// An option's value expression is substituted like any other expression. A
// repeating option's range is a *binder*: its item variable is renamed along with
// the reads of it, exactly as ast.For's loop variable is and for the same reason
// — an alpha-renaming that moves the reads without the binder leaves the reads
// resolving to nothing. A component that takes `v: cell text` and offers choices
// from a collection depends on both halves happening here.
//
// The range is copied rather than written through, like every other slice this
// file touches: a template is expanded once per call site and must never mutate
// the AST it expands from.
func substOptions(in []ast.Option, m map[string]string) []ast.Option {
	if len(in) == 0 {
		return nil
	}
	out := make([]ast.Option, len(in))
	for i, o := range in {
		o.Label = substSegs(o.Label, m)
		o.Val = substExpr(o.Val, m)
		if o.From != nil {
			rg := *o.From
			rg.Var = rename(rg.Var, m)
			rg.Coll = rename(rg.Coll, m)
			rg.Where = substExpr(rg.Where, m)
			rg.Limit = substExpr(rg.Limit, m)
			o.From = &rg
		}
		out[i] = o
	}
	return out
}

func substSegs(in []ast.Seg, m map[string]string) []ast.Seg {
	if len(in) == 0 {
		return nil
	}
	out := make([]ast.Seg, len(in))
	for i, s := range in {
		s.Expr = substExpr(s.Expr, m)
		out[i] = s
	}
	return out
}

func substExprs(in []ast.Expr, m map[string]string) []ast.Expr {
	if len(in) == 0 {
		return nil
	}
	out := make([]ast.Expr, len(in))
	for i, ex := range in {
		out[i] = substExpr(ex, m)
	}
	return out
}

func substExpr(ex ast.Expr, m map[string]string) ast.Expr {
	if ex == nil {
		return nil
	}
	switch t := ex.(type) {
	case ast.Ref:
		t.Name = rename(t.Name, m)
		return t
	case ast.ActState:
		t.Action = rename(t.Action, m)
		return t
	case ast.Get:
		t.Obj = substExpr(t.Obj, m)
		return t
	case ast.EntityGet:
		t.Key = substExpr(t.Key, m)
		return t
	case ast.Agg:
		// `count(x in Post where …)` binds x for its predicate — a binder, renamed
		// with its reads for the same reason a loop variable is.
		t.Var = rename(t.Var, m)
		t.Coll = rename(t.Coll, m)
		t.Where = substExpr(t.Where, m)
		return t
	case ast.Call:
		t.Args = substExprs(t.Args, m)
		return t
	case ast.ListLit:
		t.Elems = substExprs(t.Elems, m)
		return t
	case ast.Bin:
		t.L = substExpr(t.L, m)
		t.R = substExpr(t.R, m)
		return t
	case ast.Un:
		t.X = substExpr(t.X, m)
		return t
	}
	return ex // Lit
}

// ── the names spliced children mention ───────────────────────────────────────

// mentionedNames collects every name a tree of already-lowered children refers
// to: the identifiers whose meaning was fixed by the scope the children were
// written in. A component that splices them must not bind any of these names for
// itself, or the child's name would resolve to the component's value instead of
// the author's.
//
// It over-approximates on purpose — a name a child binds for itself (its own
// `for` variable) is collected too, and a name that only looks like it could
// collide costs nothing but a longer name in one expansion's IR. Missing one, by
// contrast, is the bug this exists to prevent, so every place a renderer resolves
// a name against the scope is read here: interpolations, conditions, filters,
// limits, arguments, the collection a list walks, the cell a control binds, and
// the action a button calls.
func mentionedNames(in []Node, out map[string]bool) {
	for _, n := range in {
		for _, segs := range n.SegLists() {
			for _, sg := range segs {
				mentionedExpr(sg.Expr, out)
			}
		}
		mentionedExpr(n.Cond, out)
		mentionedExpr(n.Where, out)
		mentionedExpr(n.Limit, out)
		mentionedExpr(n.Val, out)
		for _, a := range n.Args {
			mentionedExpr(a, out)
		}
		for _, name := range []string{n.Coll, n.Bind, n.Action, n.Var} {
			if name != "" {
				out[name] = true
			}
		}
		mentionedNames(n.Children, out)
	}
}

func mentionedExpr(x *Expr, out map[string]bool) {
	if x == nil {
		return
	}
	if x.Name != "" {
		out[x.Name] = true
	}
	if x.Var != "" {
		out[x.Var] = true
	}
	mentionedExpr(x.Obj, out)
	mentionedExpr(x.Key, out)
	mentionedExpr(x.Where, out)
	mentionedExpr(x.L, out)
	mentionedExpr(x.R, out)
	mentionedExpr(x.X, out)
	for _, a := range x.Args {
		mentionedExpr(a, out)
	}
}

// ── expansion ────────────────────────────────────────────────────────────────

// specialize lowers one call site of a template component into its own IR
// component and returns that component's name plus the dependency edges its body
// registered, which the caller folds into the page's graph (the expansion's
// regions and two-way inputs live on the caller's page, so that is where they
// must be reachable from).
//
// Each call site gets its own expansion rather than a shared one, because the
// region and input identifiers inside it are page addresses: two call sites that
// shared them would refresh each other's controls.
func (e *env) specialize(cm *ast.Component, subst map[string]string, kids []Node, line int) (string, error) {
	for _, n := range e.specStack {
		if n == cm.Name {
			return "", &BuildError{line, fmt.Sprintf(
				"component %q is recursive (%s); a component with a `slot` or a reference parameter is expanded at its call site, so it cannot use itself",
				cm.Name, strings.Join(append(append([]string{}, e.specStack...), cm.Name), " → "))}
		}
	}
	binders := componentBinders(cm)
	if len(subst) > 0 {
		for param, arg := range subst {
			if binders[param] {
				return "", &BuildError{line, fmt.Sprintf(
					"component %q binds its own name %q (a loop variable or another parameter), so its reference parameter %q cannot be resolved — rename one of them",
					cm.Name, param, param)}
			}
			if binders[arg] {
				return "", &BuildError{line, fmt.Sprintf(
					"cannot pass %q to component %q: its body binds a name %q of its own, which would shadow it — rename the loop variable or parameter inside %s",
					arg, cm.Name, arg, cm.Name)}
			}
		}
	}

	e.specStack = append(e.specStack, cm.Name)
	defer func() { e.specStack = e.specStack[:len(e.specStack)-1] }()

	e.nspec++
	name := fmt.Sprintf("%s#%d", cm.Name, e.nspec)

	// Capture-avoiding expansion.
	//
	// The children a `use` passes are written by the caller, so every name in them
	// is a name of the caller's — resolved in the caller's scope, exactly as if the
	// block had been written inline at the `use`. But they are spliced into this
	// component's body, and a component body renders in a scope that adds the
	// component's own bindings to the caller's. A parameter or loop variable of the
	// *callee* with the same spelling would therefore shadow the caller's name and
	// silently re-point the child at a value the child's author never saw: lexical
	// scoping turning into dynamic scoping at the splice.
	//
	// The remedy is the one every capture-avoiding substitution uses: move the
	// binder, not the free name. Whenever an expansion splices foreign children,
	// every name this component body binds for itself is renamed apart, to a name
	// carrying this expansion's number. `$` cannot occur in an identifier, so the
	// renamed binders collide with nothing the author wrote — at any depth, since
	// each expansion gets its own number, and children that travel two levels
	// through a `slot` pass two bodies whose binders are both renamed apart.
	//
	// Expansions with no children need no renaming, and keep the author's names in
	// their IR and their diagnostics.
	m := subst
	if len(kids) > 0 {
		mentioned := map[string]bool{}
		mentionedNames(kids, mentioned)
		apart := map[string]string{}
		for b := range binders {
			if mentioned[b] {
				apart[b] = fmt.Sprintf("%s$c%d", b, e.nspec)
			}
		}
		if len(apart) > 0 {
			// The two renamings compose into one pass: a reference parameter is
			// never a binder (the check above refuses that), so their names are
			// disjoint and neither can be applied to the other's result.
			for k, v := range subst {
				apart[k] = v
			}
			m = apart
		}
	}

	locals := map[string]bool{}
	vp := valueParams(cm.Params)
	for i := range vp {
		vp[i].Name = rename(vp[i].Name, m)
		locals[vp[i].Name] = true
	}
	// The expansion's own value parameters, under whatever names this expansion
	// renamed them to, so an argument this body passes on to a nested `use` is
	// typed against the parameter it lands in.
	vtypes := e.paramTypes(vp)
	// pfx keeps every identifier this expansion mints distinct from every other
	// expansion's and from the page's own, since they all land in one namespace.
	svc := &viewCtx{e: e, pfx: fmt.Sprintf("c%d.", e.nspec), slotOK: true, slot: kids, origin: fmt.Sprintf("component %q", cm.Name)}
	nodes, err := svc.nodes(substNodes(cm.Root, m), scope{locals: locals, inRegion: true, varTypes: vtypes})
	if err != nil {
		return "", err
	}
	e.specialCalls = append(e.specialCalls, svc.calls...)
	e.specialLinks = append(e.specialLinks, svc.links...)

	deps := e.nodeDeps(nodes)
	for _, p := range vp {
		delete(deps, p.Name)
	}
	e.compDeps[name] = deps
	e.compRegions[name] = svc.deps
	e.special = append(e.special, Component{Name: name, Params: irParams(vp), View: nodes})
	return name, nil
}

// checkTemplate lowers a template component's body once against synthetic
// references, so a component nobody has used yet is still checked for everything
// that does not depend on which cell or action it is handed — the same guarantee
// an ordinary component gets from being lowered in place.
//
// The expansion is thrown away: it runs against a copy of the environment, so
// neither its IR nor its diagnostics-bearing call and link lists reach the build.
// (Those two are validated at every real call site, where the names are real.)
func (e *env) checkTemplate(cm *ast.Component) error {
	sandbox := *e
	sandbox.states = copyStrMap(e.states)
	sandbox.stateTypes = copyStrMap(e.stateTypes)
	sandbox.stateList = copyBoolMap(e.stateList)
	sandbox.actionSet = copyBoolMap(e.actionSet)
	sandbox.compDeps = map[string]map[string]bool{}
	for k, v := range e.compDeps {
		sandbox.compDeps[k] = v
	}
	sandbox.compRegions = map[string]map[string][]string{}
	for k, v := range e.compRegions {
		sandbox.compRegions[k] = v
	}
	sandbox.special, sandbox.specialCalls, sandbox.specialLinks, sandbox.specStack = nil, nil, nil, nil

	subst := map[string]string{}
	for _, p := range cm.Params {
		switch p.Ref {
		case ast.RefCell:
			// `$` cannot occur in an identifier, so a synthetic name can never
			// collide with one the author wrote.
			syn := "cell$" + cm.Name + "$" + p.Name
			sandbox.states[syn] = Client
			sandbox.stateTypes[syn] = p.Type
			sandbox.stateList[syn] = p.List
			subst[p.Name] = syn
		case ast.RefAction:
			syn := "action$" + cm.Name + "$" + p.Name
			sandbox.actionSet[syn] = true
			subst[p.Name] = syn
		}
	}
	_, err := sandbox.specialize(cm, subst, nil, cm.Line)
	return err
}

func copyStrMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// refNoun names what a reference parameter must be handed, for its error.
func refNoun(ref string) string {
	if ref == ast.RefAction {
		return "action"
	}
	return "state cell"
}

// typeLabel renders a cell's declared type the way the author wrote it.
func typeLabel(core string, list bool) string {
	if core == "" {
		return "not a state cell"
	}
	if list {
		return "[" + core + "]"
	}
	return core
}
