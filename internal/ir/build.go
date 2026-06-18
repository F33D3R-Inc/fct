package ir

import (
	"fmt"
	"sort"
	"strings"

	"facet/internal/ast"
)

// BuildError is a semantic (post-parse) compile error.
type BuildError struct {
	Line int
	Msg  string
}

func (e *BuildError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	}
	return e.Msg
}

// env is the name environment used to validate references and compute deps.
type env struct {
	states       map[string]string          // name -> placement
	entities     map[string]bool            // entity names
	entityFields map[string]map[string]bool // entity -> field set (incl id)
	inline       map[string]*Expr           // policy/derive name -> lowered expr, inlined at every use
	policySet    map[string]bool            // policy names only (gating via `requires`)
}

// Build lowers an ast.App to the IR. This is where the placement calculus runs:
//
//   - Entities are durable, shared, persisted — always authoritative (server).
//   - State is authoritative by default (server); `@client` opts a cell into the
//     ephemeral client domain.
//   - An action runs on the server iff it writes any authoritative cell (an
//     entity, or a server-placed state); otherwise it runs on the client with
//     zero round-trip. The author never says where an action runs.
//   - Soundness: a server action may not read client-only state (the authority
//     cannot see ephemeral client state) — a compile error.
//   - Policies gate actions (`requires`) and are enforced on the server.
//
// It also extracts the dependency graph: trackable interpolations and dynamic
// regions (lists/ifs) record the state/collection names they read, so a
// mutation refreshes exactly the affected regions.
func Build(app *ast.App) (*IR, error) {
	out := &IR{App: app.Name, DepGraph: map[string][]string{}}
	e := &env{states: map[string]string{}, entities: map[string]bool{}, entityFields: map[string]map[string]bool{}, inline: map[string]*Expr{}, policySet: map[string]bool{}}

	// 1. Entities.
	entSeen := map[string]int{}
	for _, ent := range app.Entities {
		if prev, ok := entSeen[ent.Name]; ok {
			return nil, &BuildError{ent.Line, fmt.Sprintf("entity %q redeclared (first at line %d)", ent.Name, prev)}
		}
		entSeen[ent.Name] = ent.Line
		e.entities[ent.Name] = true
		e.entityFields[ent.Name] = map[string]bool{}
		ei := Entity{Name: ent.Name}
		for _, f := range ent.Fields {
			ei.Fields = append(ei.Fields, Field{Name: f.Name, Type: f.Type})
			e.entityFields[ent.Name][f.Name] = true
		}
		out.Entities = append(out.Entities, ei)
	}
	// Validate relation field types now that every entity name is known (so a
	// field may reference an entity declared later). A relation is stored as the
	// referenced row's id; you read across it with a nested lookup, e.g.
	// `User(Message(id).to).name`.
	for _, ent := range app.Entities {
		for _, f := range ent.Fields {
			if !isPrimitive(f.Type) && !e.entities[f.Type] {
				return nil, &BuildError{f.Line, fmt.Sprintf(
					"field %q has unknown type %q (use int, text, bool, or an entity name)", f.Name, f.Type)}
			}
		}
	}

	// 2. States + placement.
	stSeen := map[string]int{}
	for _, s := range app.States {
		if prev, ok := stSeen[s.Name]; ok {
			return nil, &BuildError{s.Line, fmt.Sprintf("state %q redeclared (first at line %d)", s.Name, prev)}
		}
		if e.entities[s.Name] {
			return nil, &BuildError{s.Line, fmt.Sprintf("state %q collides with an entity name", s.Name)}
		}
		stSeen[s.Name] = s.Line
		p := Server
		if s.Placement == ast.PlaceClient {
			p = Client
		}
		e.states[s.Name] = p
		out.States = append(out.States, State{Name: s.Name, Type: s.Type, Placement: p, Init: lower(s.Default, nil)})
	}

	// 3. Policies (predicates over actor + state/entities; no params). Lowered and
	// stored for inlining into `requires` and view `if` conditions.
	polSeen := map[string]int{}
	for _, p := range app.Policies {
		if prev, ok := polSeen[p.Name]; ok {
			return nil, &BuildError{p.Line, fmt.Sprintf("policy %q redeclared (first at line %d)", p.Name, prev)}
		}
		if err := e.checkName(p.Name, p.Line, "policy"); err != nil {
			return nil, err
		}
		polSeen[p.Name] = p.Line
		if err := e.checkPure(p.Expr, withActor(nil), p.Line, "a policy"); err != nil {
			return nil, err
		}
		lowered := lower(p.Expr, e.inline)
		e.inline[p.Name] = lowered
		e.policySet[p.Name] = true
		out.Policies = append(out.Policies, Policy{Name: p.Name, Expr: lowered})
	}

	// 3b. Derives (named computed values). Inlined like policies: each is lowered
	// with every earlier policy/derive already substituted, so the IR a derive
	// carries — and every place it is read — is fully resolved to base cells. A
	// derive is pure and read-only, so it has no placement of its own; its deps
	// drive dependency tracking wherever it is used.
	derSeen := map[string]int{}
	for _, d := range app.Derives {
		if prev, ok := derSeen[d.Name]; ok {
			return nil, &BuildError{d.Line, fmt.Sprintf("derive %q redeclared (first at line %d)", d.Name, prev)}
		}
		if err := e.checkName(d.Name, d.Line, "derive"); err != nil {
			return nil, err
		}
		derSeen[d.Name] = d.Line
		if err := e.checkPure(d.Expr, withActor(nil), d.Line, "a derive"); err != nil {
			return nil, err
		}
		lowered := lower(d.Expr, e.inline)
		e.inline[d.Name] = lowered
		out.Derives = append(out.Derives, Derive{Name: d.Name, Type: d.Type, Expr: lowered, Deps: sortedKeys(e.depsIR(lowered))})
	}

	// 4. Actions: placement (write set), read soundness, requires.
	actSeen := map[string]int{}
	for _, a := range app.Actions {
		if prev, ok := actSeen[a.Name]; ok {
			return nil, &BuildError{a.Line, fmt.Sprintf("action %q redeclared (first at line %d)", a.Name, prev)}
		}
		actSeen[a.Name] = a.Line
		act, err := e.action(a)
		if err != nil {
			return nil, err
		}
		out.Actions = append(out.Actions, act)
	}

	// 4b. Jobs: each schedules a zero-argument, server-placed action. Validated
	// against the actions just built.
	byActionName := map[string]*Action{}
	for i := range out.Actions {
		byActionName[out.Actions[i].Name] = &out.Actions[i]
	}
	jobSeen := map[string]int{}
	for _, j := range app.Jobs {
		if prev, ok := jobSeen[j.Name]; ok {
			return nil, &BuildError{j.Line, fmt.Sprintf("job %q redeclared (first at line %d)", j.Name, prev)}
		}
		jobSeen[j.Name] = j.Line
		act, ok := byActionName[j.Action]
		if !ok {
			return nil, &BuildError{j.Line, fmt.Sprintf("job %q runs unknown action %q", j.Name, j.Action)}
		}
		if act.Placement != Server {
			return nil, &BuildError{j.Line, fmt.Sprintf("job %q runs client-placed action %q; jobs run on the server authority, so the action must be authoritative", j.Name, j.Action)}
		}
		if len(act.Params) != 0 {
			return nil, &BuildError{j.Line, fmt.Sprintf("job %q runs action %q, which takes arguments; a job invokes a zero-argument action", j.Name, j.Action)}
		}
		out.Jobs = append(out.Jobs, Job{Name: j.Name, Action: j.Action, Every: j.Every, OnStart: j.OnStart})
	}

	// 4c. Auth: when enabled, the runtime provides a managed user store and the
	// login/signup/logout actions. Inject them so views can call them and the
	// client treats them as server actions; the runtime supplies the behavior.
	if app.Auth {
		out.Auth = true
		if e.entities[reservedUserEntity] {
			return nil, &BuildError{0, fmt.Sprintf("entity %q is reserved by `auth`", reservedUserEntity)}
		}
		for _, name := range authActionNames {
			if _, ok := byActionName[name]; ok {
				return nil, &BuildError{0, fmt.Sprintf("action %q is reserved by `auth`", name)}
			}
		}
		out.Entities = append(out.Entities, Entity{Name: reservedUserEntity, Fields: []Field{
			{Name: "id", Type: "int"}, {Name: "username", Type: "text"},
			{Name: "password", Type: "text"}, {Name: "role", Type: "text"},
		}})
		out.Actions = append(out.Actions,
			Action{Name: "signup", Placement: Server, Params: []Param{{Name: "username", Type: "text"}, {Name: "password", Type: "text"}}},
			Action{Name: "login", Placement: Server, Params: []Param{{Name: "username", Type: "text"}, {Name: "password", Type: "text"}}},
			Action{Name: "logout", Placement: Server},
		)
	}

	// 5. Views → pages. Each view compiles with its own viewCtx, so binding and
	// region ids are page-local, and is served at its own route.
	byAction := map[string]*Action{}
	for i := range out.Actions {
		byAction[out.Actions[i].Name] = &out.Actions[i]
	}
	pathOf := map[string]string{} // path -> view name
	var allCalls []call
	var allLinks []string
	for i, v := range app.Views {
		pvc := &viewCtx{e: e}
		nodes, err := pvc.nodes(v.Root, scope{})
		if err != nil {
			return nil, err
		}
		path := v.Path
		if path == "" {
			if i == 0 {
				path = "/"
			} else {
				path = "/" + lowerASCII(v.Name)
			}
		}
		if !strings.HasPrefix(path, "/") {
			return nil, &BuildError{v.Line, fmt.Sprintf("view %q route %q must start with `/`", v.Name, path)}
		}
		if prev, ok := pathOf[path]; ok {
			return nil, &BuildError{v.Line, fmt.Sprintf("views %q and %q both map to route %q", v.Name, prev, path)}
		}
		pathOf[path] = v.Name
		page := Page{Name: v.Name, Path: path, View: nodes, Bindings: pvc.bindings, DepGraph: map[string][]string{}}
		for dep, ids := range pvc.deps {
			page.DepGraph[dep] = ids
		}
		out.Pages = append(out.Pages, page)
		allCalls = append(allCalls, pvc.calls...)
		allLinks = append(allLinks, pvc.links...)
	}
	if len(out.Pages) > 0 {
		out.View = out.Pages[0].View
		out.Bindings = out.Pages[0].Bindings
		out.DepGraph = out.Pages[0].DepGraph
	}

	// validate every action a button calls exists and is arity-correct.
	for _, ref := range allCalls {
		act, ok := byAction[ref.name]
		if !ok {
			return nil, &BuildError{0, fmt.Sprintf("button references unknown action %q", ref.name)}
		}
		if len(act.Params) != ref.argc {
			return nil, &BuildError{0, fmt.Sprintf("action %q takes %d argument(s), got %d", ref.name, len(act.Params), ref.argc)}
		}
	}
	// validate every link points at a real route.
	for _, p := range allLinks {
		if _, ok := pathOf[p]; !ok {
			return nil, &BuildError{0, fmt.Sprintf("link to %q, but no view serves that route", p)}
		}
	}
	return out, nil
}

// reservedUserEntity is the runtime-managed users table created when `auth` is on.
const reservedUserEntity = "FacetUser"

// authActionNames are the built-in actions `auth` provides.
var authActionNames = []string{"signup", "login", "logout"}

// lowerASCII lowercases an identifier for a default route.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func (e *env) action(a *ast.Action) (Action, error) {
	act := Action{Name: a.Name}
	loc := map[string]bool{"actor": true, "role": true}
	for _, p := range a.Params {
		act.Params = append(act.Params, Param{Name: p.Name, Type: p.Type})
		loc[p.Name] = true
	}
	for _, r := range a.Requires {
		if !e.policySet[r] {
			if _, isDerive := e.inline[r]; isDerive {
				return Action{}, &BuildError{a.Line, fmt.Sprintf("requires %q, which is a derive, not a policy", r)}
			}
			return Action{}, &BuildError{a.Line, fmt.Sprintf("requires unknown policy %q", r)}
		}
		act.Requires = append(act.Requires, r)
	}

	writes := map[string]bool{} // state names written
	entWrite := false           // any entity mutation
	reads := map[string]bool{}  // state names read (for soundness)
	impure := false             // uses an effectful builtin (now/rand)

	readExpr := func(ex ast.Expr, line int) error {
		if err := e.check(ex, loc, line); err != nil {
			return err
		}
		if hasImpure(ex) {
			impure = true
		}
		for n := range e.depsIR(lower(ex, e.inline)) {
			if _, isState := e.states[n]; isState {
				reads[n] = true
			}
		}
		return nil
	}

	for _, s := range a.Body {
		switch st := s.(type) {
		case ast.Assign:
			p, ok := e.states[st.Target]
			if !ok {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("assignment to unknown state %q", st.Target)}
			}
			_ = p
			if err := readExpr(st.Value, st.Line); err != nil {
				return Action{}, err
			}
			writes[st.Target] = true
			act.Body = append(act.Body, Stmt{Op: "assign", Target: st.Target, Value: lower(st.Value, e.inline)})
		case ast.Add:
			if !e.entities[st.Entity] {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("add to unknown entity %q", st.Entity)}
			}
			entWrite = true
			out := Stmt{Op: "add", Entity: st.Entity}
			for _, fi := range st.Fields {
				if err := readExpr(fi.Expr, st.Line); err != nil {
					return Action{}, err
				}
				out.Fields = append(out.Fields, FieldInit{Name: fi.Name, Expr: lower(fi.Expr, e.inline)})
			}
			act.Body = append(act.Body, out)
		case ast.Set:
			if !e.entities[st.Entity] {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("set on unknown entity %q", st.Entity)}
			}
			entWrite = true
			if err := readExpr(st.Key, st.Line); err != nil {
				return Action{}, err
			}
			if err := readExpr(st.Value, st.Line); err != nil {
				return Action{}, err
			}
			act.Body = append(act.Body, Stmt{Op: "set", Entity: st.Entity, Field: st.Field,
				Key: lower(st.Key, e.inline), Value: lower(st.Value, e.inline)})
		case ast.Remove:
			if !e.entities[st.Entity] {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("remove on unknown entity %q", st.Entity)}
			}
			entWrite = true
			if err := readExpr(st.Key, st.Line); err != nil {
				return Action{}, err
			}
			act.Body = append(act.Body, Stmt{Op: "remove", Entity: st.Entity, Key: lower(st.Key, e.inline)})
		case ast.Clear:
			if !e.entities[st.Entity] {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("clear on unknown entity %q", st.Entity)}
			}
			entWrite = true
			act.Body = append(act.Body, Stmt{Op: "clear", Entity: st.Entity})
		}
	}

	// placement: server iff it writes any authoritative cell OR is impure. An
	// effectful builtin (now/rand) is nondeterministic, so the authority must run
	// it — that way every client sees one agreed result, not its own.
	act.Placement = Client
	if entWrite || impure {
		act.Placement = Server
	}
	for w := range writes {
		if e.states[w] == Server {
			act.Placement = Server
		}
	}
	if act.Placement == Server {
		// Soundness is symmetric: the authority can neither see nor touch ephemeral
		// client state. Reading it is unobservable; writing it is unreachable.
		for r := range reads {
			if e.states[r] == Client {
				return Action{}, &BuildError{a.Line, fmt.Sprintf(
					"action %q runs on the server (it writes authoritative state or uses an effectful builtin) but reads client-only state %q; the authority cannot see ephemeral client state",
					a.Name, r)}
			}
		}
		for w := range writes {
			if e.states[w] == Client {
				return Action{}, &BuildError{a.Line, fmt.Sprintf(
					"action %q runs on the server but writes client-only state %q; the authority cannot reach ephemeral client state",
					a.Name, w)}
			}
		}
	}
	// an action that requires a policy must be authoritative — the gate is the
	// server's job; a client-only action cannot be securely gated.
	if len(act.Requires) > 0 && act.Placement != Server {
		return Action{}, &BuildError{a.Line, fmt.Sprintf(
			"action %q has `requires` but is client-placed; only authoritative (server) actions can be gated", a.Name)}
	}
	act.Writes = sortedKeys(writes)
	act.Reads = sortedKeys(reads)
	return act, nil
}

// ── view lowering ────────────────────────────────────────────────────────────

// scope tracks the local item variables in effect (from enclosing `for`s) and
// whether we are inside a dynamic region (a for/if), where interpolations are
// rendered inline rather than tracked as top-level bindings.
type scope struct {
	locals   map[string]bool
	inRegion bool
}

func (s scope) with(v string) scope {
	m := map[string]bool{}
	for k := range s.locals {
		m[k] = true
	}
	m[v] = true
	return scope{locals: m, inRegion: true}
}

type call struct {
	name string
	argc int
}

type viewCtx struct {
	e          *env
	bindings   []Binding
	deps       map[string][]string // dep -> tracked region ids
	calls      []call
	links      []string // link destination routes (validated against real pages)
	nb, nl, nf int
}

func (c *viewCtx) addDep(dep, id string) {
	if c.deps == nil {
		c.deps = map[string][]string{}
	}
	c.deps[dep] = append(c.deps[dep], id)
}

func (c *viewCtx) nodes(in []ast.Node, sc scope) ([]Node, error) {
	var out []Node
	for _, n := range in {
		switch t := n.(type) {
		case ast.Box:
			kids, err := c.nodes(t.Children, sc)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "box", Children: kids})

		case ast.Text:
			node := Node{Kind: "text"}
			for _, s := range t.Segs {
				if s.Expr == nil {
					node.Segs = append(node.Segs, Seg{Lit: s.Lit})
					continue
				}
				if err := c.e.checkPure(s.Expr, withActor(sc.locals), 0, "a view"); err != nil {
					return nil, err
				}
				if sc.inRegion {
					// item-scope: rendered inline when the enclosing region renders.
					node.Segs = append(node.Segs, Seg{Expr: lower(s.Expr, c.e.inline)})
				} else {
					id := fmt.Sprintf("b%d", c.nb)
					c.nb++
					le := lower(s.Expr, c.e.inline)
					deps := sortedKeys(c.e.depsIR(le))
					c.bindings = append(c.bindings, Binding{ID: id, Expr: le, Deps: deps})
					for _, d := range deps {
						c.addDep(d, id)
					}
					node.Segs = append(node.Segs, Seg{Bind: id})
				}
			}
			out = append(out, node)

		case ast.Button:
			node := Node{Kind: "button", Label: t.Label, Action: t.Action}
			for _, arg := range t.Args {
				if err := c.e.checkPure(arg, withActor(sc.locals), 0, "a view"); err != nil {
					return nil, err
				}
				node.Args = append(node.Args, lower(arg, c.e.inline))
			}
			c.calls = append(c.calls, call{name: t.Action, argc: len(t.Args)})
			out = append(out, node)

		case ast.For:
			if !c.e.entities[t.Coll] && c.e.states[t.Coll] == "" {
				return nil, &BuildError{0, fmt.Sprintf("`for` over unknown collection %q", t.Coll)}
			}
			node := Node{Kind: "list", Var: t.Var, Coll: t.Coll, Order: t.Order, Desc: t.Desc, Limit: t.Limit}
			// `where` filter: a pure predicate over the item var + outer scope.
			if t.Where != nil {
				wlocals := withActor(sc.locals)
				wlocals[t.Var] = true
				if err := c.e.checkPure(t.Where, wlocals, 0, "a `where` filter"); err != nil {
					return nil, err
				}
				node.Where = lower(t.Where, c.e.inline)
			}
			if t.Order != "" {
				if !c.e.entities[t.Coll] {
					return nil, &BuildError{0, fmt.Sprintf("ordering `by %s` requires %q to be an entity", t.Order, t.Coll)}
				}
				if !c.e.entityFields[t.Coll][t.Order] {
					return nil, &BuildError{0, fmt.Sprintf("entity %q has no field %q to order by", t.Coll, t.Order)}
				}
			}
			if !sc.inRegion {
				node.ID = fmt.Sprintf("l%d", c.nl)
				c.nl++
				c.addDep(t.Coll, node.ID)
				// a state the filter reads must also refresh the list when it changes.
				if node.Where != nil {
					for _, d := range sortedKeys(c.e.depsIR(node.Where)) {
						c.addDep(d, node.ID)
					}
				}
			}
			kids, err := c.nodes(t.Body, sc.with(t.Var))
			if err != nil {
				return nil, err
			}
			node.Children = kids
			out = append(out, node)

		case ast.If:
			if err := c.e.checkPure(t.Cond, withActor(sc.locals), 0, "a view"); err != nil {
				return nil, err
			}
			node := Node{Kind: "if", Cond: lower(t.Cond, c.e.inline)}
			if !sc.inRegion {
				node.ID = fmt.Sprintf("f%d", c.nf)
				c.nf++
				for _, d := range sortedKeys(c.e.depsIR(node.Cond)) {
					c.addDep(d, node.ID)
				}
			}
			kids, err := c.nodes(t.Body, scope{locals: sc.locals, inRegion: true})
			if err != nil {
				return nil, err
			}
			node.Children = kids
			out = append(out, node)

		case ast.Input:
			p, ok := c.e.states[t.Bind]
			if !ok {
				return nil, &BuildError{0, fmt.Sprintf("input binds unknown state %q", t.Bind)}
			}
			if p != Client {
				return nil, &BuildError{0, fmt.Sprintf("input binds %q, which is authoritative; two-way input requires a @client state", t.Bind)}
			}
			id := fmt.Sprintf("b%d", c.nb)
			c.nb++
			c.addDep(t.Bind, id)
			out = append(out, Node{Kind: "input", Bind: t.Bind, Placeholder: t.Placeholder, ID: id})

		case ast.Link:
			if t.Path == "" || !strings.HasPrefix(t.Path, "/") {
				return nil, &BuildError{0, fmt.Sprintf("link path %q must start with `/`", t.Path)}
			}
			c.links = append(c.links, t.Path)
			out = append(out, Node{Kind: "link", Label: t.Label, Path: t.Path})
		}
	}
	return out, nil
}

// ── reference checking + free names ──────────────────────────────────────────

// check validates that every free name in ex is a known state, entity, local,
// or the builtin `actor`, and that every aggregate and builtin call is
// well-formed.
func (e *env) check(ex ast.Expr, locals map[string]bool, line int) error {
	if err := e.checkBuiltins(ex, line); err != nil {
		return err
	}
	for n := range freeNames(ex) {
		if locals[n] || isBuiltinRef(n) {
			continue
		}
		if _, ok := e.states[n]; ok {
			continue
		}
		if e.entities[n] {
			continue
		}
		if _, ok := e.inline[n]; ok { // a policy/derive name used as a value; inlined by lower()
			continue
		}
		return &BuildError{line, fmt.Sprintf("unknown reference %q", n)}
	}
	return nil
}

// checkPure is check plus the guarantee that ex is side-effect-free: it rejects
// the effectful builtins (now/rand). Pure contexts — derives, policies, views —
// may run on any client, so they must be deterministic.
func (e *env) checkPure(ex ast.Expr, locals map[string]bool, line int, ctx string) error {
	if err := e.check(ex, locals, line); err != nil {
		return err
	}
	if hasImpure(ex) {
		return &BuildError{line, fmt.Sprintf(
			"%s cannot use an effectful builtin (now/rand); it must be pure so it can run on any client. Compute it in an action instead", ctx)}
	}
	return nil
}

// checkBuiltins validates aggregates (must range over a real entity; `sum` needs
// an existing field) and builtin calls (known name, correct arity).
func (e *env) checkBuiltins(ex ast.Expr, line int) error {
	switch t := ex.(type) {
	case ast.Agg:
		if !e.entities[t.Coll] {
			return &BuildError{line, fmt.Sprintf("%s(...) needs an entity collection; %q is not an entity", t.Op, t.Coll)}
		}
		if t.Op == "sum" && !e.entityFields[t.Coll][t.Field] {
			return &BuildError{line, fmt.Sprintf("entity %q has no field %q to sum", t.Coll, t.Field)}
		}
	case ast.Call:
		switch t.Name {
		case "now":
			if len(t.Args) != 0 {
				return &BuildError{line, "now() takes no arguments"}
			}
		case "rand":
			if len(t.Args) != 1 {
				return &BuildError{line, "rand(n) takes exactly one argument (an exclusive upper bound)"}
			}
		default:
			return &BuildError{line, fmt.Sprintf("unknown builtin %q", t.Name)}
		}
		for _, a := range t.Args {
			if err := e.checkBuiltins(a, line); err != nil {
				return err
			}
		}
	case ast.Get:
		return e.checkBuiltins(t.Obj, line)
	case ast.EntityGet:
		return e.checkBuiltins(t.Key, line)
	case ast.Bin:
		if err := e.checkBuiltins(t.L, line); err != nil {
			return err
		}
		return e.checkBuiltins(t.R, line)
	case ast.Un:
		return e.checkBuiltins(t.X, line)
	}
	return nil
}

// hasImpure reports whether ex invokes an effectful builtin. now/rand are the
// only calls in the language and both are impure, so any Call is impure.
func hasImpure(ex ast.Expr) bool {
	switch t := ex.(type) {
	case ast.Call:
		return true
	case ast.Get:
		return hasImpure(t.Obj)
	case ast.EntityGet:
		return hasImpure(t.Key)
	case ast.Bin:
		return hasImpure(t.L) || hasImpure(t.R)
	case ast.Un:
		return hasImpure(t.X)
	}
	return false
}

func isPrimitive(t string) bool { return t == "int" || t == "text" || t == "bool" }

// checkName rejects a policy/derive name that collides with an existing state,
// entity, policy, or derive, so every global name resolves unambiguously.
func (e *env) checkName(name string, line int, kind string) error {
	if _, ok := e.states[name]; ok {
		return &BuildError{line, fmt.Sprintf("%s %q collides with a state name", kind, name)}
	}
	if e.entities[name] {
		return &BuildError{line, fmt.Sprintf("%s %q collides with an entity name", kind, name)}
	}
	if _, ok := e.inline[name]; ok {
		return &BuildError{line, fmt.Sprintf("%s %q collides with an existing policy or derive", kind, name)}
	}
	return nil
}

// depsIR returns the trackable state/entity names a *lowered* expression reads
// (after policy inlining), so dependency edges reflect the real predicate.
func (e *env) depsIR(le *Expr) map[string]bool {
	out := map[string]bool{}
	var walk func(*Expr)
	walk = func(x *Expr) {
		if x == nil {
			return
		}
		switch x.Kind {
		case "ref":
			if _, ok := e.states[x.Name]; ok {
				out[x.Name] = true
			} else if e.entities[x.Name] {
				out[x.Name] = true
			}
		case "get":
			walk(x.Obj)
		case "eget":
			if e.entities[x.Name] {
				out[x.Name] = true
			}
			walk(x.Key)
		case "agg":
			if e.entities[x.Name] {
				out[x.Name] = true
			}
		case "call":
			for _, a := range x.Args {
				walk(a)
			}
		case "bin":
			walk(x.L)
			walk(x.R)
		case "un":
			walk(x.X)
		}
	}
	walk(le)
	return out
}

// freeNames returns every root name an expression references.
func freeNames(ex ast.Expr) map[string]bool {
	out := map[string]bool{}
	var walk func(ast.Expr)
	walk = func(ex ast.Expr) {
		switch t := ex.(type) {
		case ast.Ref:
			out[t.Name] = true
		case ast.Get:
			walk(t.Obj)
		case ast.EntityGet:
			out[t.Entity] = true
			walk(t.Key)
		case ast.Agg:
			out[t.Coll] = true
		case ast.Call:
			for _, a := range t.Args {
				walk(a)
			}
		case ast.Bin:
			walk(t.L)
			walk(t.R)
		case ast.Un:
			walk(t.X)
		}
	}
	walk(ex)
	return out
}

// lower converts an ast.Expr to its serializable IR form, inlining any reference
// to a policy or derive name with that name's lowered expression (so the same
// value is computed identically wherever it is read — a server gate, a view
// `if`, or another derivation).
func lower(ex ast.Expr, inline map[string]*Expr) *Expr {
	switch t := ex.(type) {
	case ast.Lit:
		return &Expr{Kind: "lit", Val: t.Val, VType: t.Kind}
	case ast.Ref:
		if inline != nil {
			if p, ok := inline[t.Name]; ok {
				return cloneExpr(p)
			}
		}
		return &Expr{Kind: "ref", Name: t.Name}
	case ast.Get:
		return &Expr{Kind: "get", Obj: lower(t.Obj, inline), Field: t.Field}
	case ast.EntityGet:
		return &Expr{Kind: "eget", Name: t.Entity, Key: lower(t.Key, inline), Field: t.Field}
	case ast.Agg:
		return &Expr{Kind: "agg", Op: t.Op, Name: t.Coll, Field: t.Field}
	case ast.Call:
		out := &Expr{Kind: "call", Name: t.Name}
		for _, a := range t.Args {
			out.Args = append(out.Args, lower(a, inline))
		}
		return out
	case ast.Bin:
		return &Expr{Kind: "bin", Op: t.Op, L: lower(t.L, inline), R: lower(t.R, inline)}
	case ast.Un:
		return &Expr{Kind: "un", Op: t.Op, X: lower(t.X, inline)}
	}
	return nil
}

func cloneExpr(e *Expr) *Expr {
	if e == nil {
		return nil
	}
	c := *e
	c.L = cloneExpr(e.L)
	c.R = cloneExpr(e.R)
	c.X = cloneExpr(e.X)
	c.Obj = cloneExpr(e.Obj)
	c.Key = cloneExpr(e.Key)
	return &c
}

func locals(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// isBuiltinRef reports whether a name is a runtime-provided identity value: the
// signed-in user's name (`actor`) or role (`role`).
func isBuiltinRef(n string) bool { return n == "actor" || n == "role" }

func withActor(locals map[string]bool) map[string]bool {
	m := map[string]bool{"actor": true, "role": true}
	for k := range locals {
		m[k] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
