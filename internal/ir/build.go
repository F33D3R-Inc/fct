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
	states       map[string]string            // name -> placement
	entities     map[string]bool              // entity names
	entityFields map[string]map[string]bool   // entity -> field set (incl id)
	indexFields  map[string]map[string]bool   // entity -> fields the compiler saw queried (build a DB index)
	inline       map[string]*Expr             // zero-arg policy/derive name -> lowered expr, inlined at every use
	policySet    map[string]bool              // policy names (gating via `requires`)
	policyParams map[string][]Param           // policy name -> its parameters (row-level policies)
	enums        map[string][]string          // enum name -> ordered member values
	components   map[string][]Param           // component name -> its parameters
	compDeps     map[string]map[string]bool   // component name -> the state/entity names its body reads (for use-site refresh)
	stateTypes   map[string]string            // state name -> its (core/element) type, for enum-defaulted selects
	services     map[string]map[string]int    // service name -> op name -> parameter count, for checking `call`
	serviceRets  map[string]map[string]opRet  // service name -> op name -> return type, for binding `let x = call …`
	private      map[string]bool              // @private state names — server-only, non-renderable
	entFieldEnum map[string]map[string]string // entity -> field -> enum name (only enum-typed fields), for `match` exhaustiveness
}

// opRet is a service operation's declared return type ("" core = no return).
type opRet struct {
	ret  string
	list bool
}

// markIndex records that entity.field is filtered, ordered, or a relation, so the
// store builds an index for it.
func (e *env) markIndex(entity, field string) {
	if e.indexFields[entity] == nil {
		e.indexFields[entity] = map[string]bool{}
	}
	e.indexFields[entity][field] = true
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
	e := &env{states: map[string]string{}, entities: map[string]bool{}, entityFields: map[string]map[string]bool{}, indexFields: map[string]map[string]bool{}, inline: map[string]*Expr{}, policySet: map[string]bool{}, policyParams: map[string][]Param{}, enums: map[string][]string{}, components: map[string][]Param{}, compDeps: map[string]map[string]bool{}, stateTypes: map[string]string{}, services: map[string]map[string]int{}, serviceRets: map[string]map[string]opRet{}, private: map[string]bool{}, entFieldEnum: map[string]map[string]string{}}

	// 0. Enums: closed text types. Collected first so field/state/param types and
	// `Enum.member` literals resolve while everything else is built.
	enumSeen := map[string]int{}
	for _, en := range app.Enums {
		if prev, ok := enumSeen[en.Name]; ok {
			return nil, &BuildError{en.Line, fmt.Sprintf("enum %q redeclared (first at line %d)", en.Name, prev)}
		}
		enumSeen[en.Name] = en.Line
		valSeen := map[string]bool{}
		for _, v := range en.Values {
			if valSeen[v] {
				return nil, &BuildError{en.Line, fmt.Sprintf("enum %q has duplicate value %q", en.Name, v)}
			}
			valSeen[v] = true
		}
		e.enums[en.Name] = en.Values
		out.Enums = append(out.Enums, Enum{Name: en.Name, Values: en.Values})
	}

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
			if f.Secret && f.Name == "id" {
				return nil, &BuildError{f.Line, "the id field cannot be @secret"}
			}
			fld := Field{Name: f.Name, Type: f.Type, Secret: f.Secret, Optional: f.Optional}
			// An enum-typed field is stored as text, tagged with its enum so the API and
			// the client can validate/render the closed set.
			if _, isEnum := e.enums[f.Type]; isEnum {
				fld.Type = "text"
				fld.Enum = f.Type
				if e.entFieldEnum[ent.Name] == nil {
					e.entFieldEnum[ent.Name] = map[string]string{}
				}
				e.entFieldEnum[ent.Name][f.Name] = f.Type
			}
			ei.Fields = append(ei.Fields, fld)
			e.entityFields[ent.Name][f.Name] = true
		}
		out.Entities = append(out.Entities, ei)
	}
	// Validate relation field types now that every entity name is known (so a
	// field may reference an entity declared later). A relation is stored as the
	// referenced row's id; you read across it with a nested lookup, e.g.
	// `User(Message(id).to).name`.
	for ei := range out.Entities {
		for fi := range out.Entities[ei].Fields {
			f := &out.Entities[ei].Fields[fi]
			if isPrimitive(f.Type) {
				continue
			}
			if !e.entities[f.Type] {
				return nil, &BuildError{0, fmt.Sprintf(
					"field %q has unknown type %q (use int, text, bool, or an entity name)", f.Name, f.Type)}
			}
			if f.Secret {
				return nil, &BuildError{0, fmt.Sprintf(
					"relation field %q cannot be @secret; a foreign key stores the referenced row's id, which must be queryable", f.Name)}
			}
			// A relation: the column stores the referenced row's id (a foreign key).
			// It is always indexed — reverse lookups (a user's posts) and cascade
			// deletes both walk it.
			f.Ref = f.Type
			e.markIndex(out.Entities[ei].Name, f.Name)
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
		// The element/core type must be a primitive or a declared enum.
		core := s.Type
		if s.List {
			core = s.Elem
		}
		if !isPrimitive(core) {
			if _, isEnum := e.enums[core]; !isEnum {
				return nil, &BuildError{s.Line, fmt.Sprintf("state %q has unknown type %q", s.Name, core)}
			}
		}
		p := Server
		if s.Placement == ast.PlaceClient {
			p = Client
		}
		// @private is authoritative (server) for placement, plus server-only: it is
		// never shipped to a client and never renderable. Tracked so the view checker
		// rejects interpolating it and the runtime strips it from client payloads.
		private := s.Placement == ast.PlacePrivate
		if private {
			e.private[s.Name] = true
		}
		if err := e.checkBuiltins(s.Default, s.Line); err != nil {
			return nil, err
		}
		e.states[s.Name] = p
		e.stateTypes[s.Name] = core
		out.States = append(out.States, State{Name: s.Name, Type: s.Type, Elem: s.Elem, List: s.List, Optional: s.Optional, Placement: p, Private: private, Init: e.low(s.Default)})
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
		// The body sees the actor identity plus the policy's own parameters (for a
		// row-level check like `actor == Post(id).author`).
		plocals := withActor(nil)
		pseen := map[string]bool{}
		for _, pp := range p.Params {
			if pseen[pp.Name] {
				return nil, &BuildError{p.Line, fmt.Sprintf("policy %q has duplicate parameter %q", p.Name, pp.Name)}
			}
			pseen[pp.Name] = true
			plocals[pp.Name] = true
		}
		if err := e.checkPure(p.Expr, plocals, p.Line, "a policy"); err != nil {
			return nil, err
		}
		lowered := e.low(p.Expr)
		e.policySet[p.Name] = true
		e.policyParams[p.Name] = irParams(p.Params)
		// Only a zero-parameter policy resolves to a value, so only it can be inlined
		// (into a view `if`, or another policy/derive). A row-level policy is a gate,
		// reachable only through `requires name(args)`.
		if len(p.Params) == 0 {
			e.inline[p.Name] = lowered
		}
		out.Policies = append(out.Policies, Policy{Name: p.Name, Params: irParams(p.Params), Expr: lowered})
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
		lowered := e.low(d.Expr)
		e.inline[d.Name] = lowered
		out.Derives = append(out.Derives, Derive{Name: d.Name, Type: d.Type, Expr: lowered, Deps: sortedKeys(e.depsIR(lowered))})
	}

	// 3c. Theme: each `name "value"` becomes a CSS custom property (--fa-<name>).
	// Carried on the IR so first paint and the client both apply one style source.
	if len(app.Theme) > 0 {
		out.Theme = map[string]string{}
		for _, tv := range app.Theme {
			out.Theme[tv.Name] = tv.Value
		}
	}

	// 3d. Components: reusable view fragments. Names + parameters are registered in
	// one pass (so a component may `use` another declared later, and arity checks
	// resolve), then each body is lowered. A component is pure projection rendered
	// inline at its call site, so its interpolations and nested regions are inlined
	// (no page-local binding ids); a top-level `use` is refreshed as a whole region
	// keyed on the union of its argument deps and the globals its body reads.
	compSeen := map[string]int{}
	for _, cm := range app.Components {
		if prev, ok := compSeen[cm.Name]; ok {
			return nil, &BuildError{cm.Line, fmt.Sprintf("component %q redeclared (first at line %d)", cm.Name, prev)}
		}
		compSeen[cm.Name] = cm.Line
		if e.entities[cm.Name] {
			return nil, &BuildError{cm.Line, fmt.Sprintf("component %q collides with an entity name", cm.Name)}
		}
		pseen := map[string]bool{}
		for _, p := range cm.Params {
			if pseen[p.Name] {
				return nil, &BuildError{cm.Line, fmt.Sprintf("component %q has duplicate parameter %q", cm.Name, p.Name)}
			}
			pseen[p.Name] = true
		}
		e.components[cm.Name] = irParams(cm.Params)
	}
	var compCalls []call
	var compLinks []string
	for _, cm := range app.Components {
		locals := map[string]bool{}
		for _, p := range cm.Params {
			locals[p.Name] = true
		}
		cvc := &viewCtx{e: e}
		// inRegion: a component body renders inline at the call site, so it carries no
		// page-local binding/region ids of its own.
		nodes, err := cvc.nodes(cm.Root, scope{locals: locals, inRegion: true})
		if err != nil {
			return nil, err
		}
		compCalls = append(compCalls, cvc.calls...)
		compLinks = append(compLinks, cvc.links...)
		// the globals (state/entity) the body reads, minus the bound parameters; a
		// `use` of this component refreshes when any of these change.
		deps := e.nodeDeps(nodes)
		for _, p := range cm.Params {
			delete(deps, p.Name)
		}
		e.compDeps[cm.Name] = deps
		out.Components = append(out.Components, Component{Name: cm.Name, Params: irParams(cm.Params), View: nodes})
	}

	// 3d′. Services: external brains an action may `call`. Each op's parameter
	// count is recorded so a call site is checked at compile time; the URL + param
	// names flow to the runtime, which posts to them. A call is an effect, so it
	// pins its action to the server — never reachable directly from a client.
	for _, sv := range app.Services {
		if _, dup := e.services[sv.Name]; dup {
			return nil, &BuildError{sv.Line, fmt.Sprintf("service %q redeclared", sv.Name)}
		}
		ops := map[string]int{}
		rets := map[string]opRet{}
		irsv := Service{Name: sv.Name, URL: sv.URL}
		for _, op := range sv.Ops {
			if _, dup := ops[op.Name]; dup {
				return nil, &BuildError{op.Line, fmt.Sprintf("service %q declares operation %q twice", sv.Name, op.Name)}
			}
			ops[op.Name] = len(op.Params)
			rets[op.Name] = opRet{ret: op.Ret, list: op.RetList}
			var pnames []string
			for _, p := range op.Params {
				pnames = append(pnames, p.Name)
			}
			irsv.Ops = append(irsv.Ops, ServiceOp{Name: op.Name, Params: pnames, Ret: op.Ret, RetList: op.RetList})
		}
		e.services[sv.Name] = ops
		e.serviceRets[sv.Name] = rets
		out.Services = append(out.Services, irsv)
	}

	// 3e. Layouts: page chrome with one `slot` where the routed view is injected.
	// Kept as raw node trees and inlined into each view that opts in (`in Main`),
	// so a page compiles to a single tree and the runtimes need no layout concept.
	layouts := map[string]*ast.Layout{}
	laySeen := map[string]int{}
	for _, ly := range app.Layouts {
		if prev, ok := laySeen[ly.Name]; ok {
			return nil, &BuildError{ly.Line, fmt.Sprintf("layout %q redeclared (first at line %d)", ly.Name, prev)}
		}
		laySeen[ly.Name] = ly.Line
		layouts[ly.Name] = ly
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
		for _, a := range authActions() {
			if _, ok := byActionName[a.Name]; ok {
				return nil, &BuildError{0, fmt.Sprintf("action %q is reserved by `auth`", a.Name)}
			}
		}
		// The managed user table. password/tokens are stored hashed; the TOTP secret
		// is encrypted at rest (@secret). The whole table is hidden from the API/SSE.
		out.Entities = append(out.Entities, Entity{Name: reservedUserEntity, Fields: []Field{
			{Name: "id", Type: "int"}, {Name: "username", Type: "text"},
			{Name: "password", Type: "text"}, {Name: "role", Type: "text"},
			{Name: "email", Type: "text"}, {Name: "verified", Type: "bool"},
			{Name: "verifyToken", Type: "text"}, {Name: "resetToken", Type: "text"},
			{Name: "resetExpires", Type: "int"},
			{Name: "mfaSecret", Type: "text", Secret: true}, {Name: "mfaEnabled", Type: "bool"},
		}})
		out.Actions = append(out.Actions, authActions()...)
	}

	// 5. Views → pages. Each view compiles with its own viewCtx, so binding and
	// region ids are page-local, and is served at its own route.
	byAction := map[string]*Action{}
	for i := range out.Actions {
		byAction[out.Actions[i].Name] = &out.Actions[i]
	}
	// 4d. Webhooks: inbound endpoints external systems POST to. Each names an
	// existing action the runtime runs (with system authority) after verifying an
	// HMAC over the raw body. The path must be unique and must not collide with a
	// route the runtime already owns, so a webhook can never shadow the app's API.
	hookSeen := map[string]int{}
	for _, wh := range app.Webhooks {
		if _, ok := byAction[wh.Action]; !ok {
			return nil, &BuildError{wh.Line, fmt.Sprintf("webhook %q targets unknown action %q", wh.Path, wh.Action)}
		}
		if prev, ok := hookSeen[wh.Path]; ok {
			return nil, &BuildError{wh.Line, fmt.Sprintf("webhook path %q redeclared (first at line %d)", wh.Path, prev)}
		}
		if reservedWebhookPath(wh.Path) {
			return nil, &BuildError{wh.Line, fmt.Sprintf("webhook path %q collides with a route the runtime reserves", wh.Path)}
		}
		hookSeen[wh.Path] = wh.Line
		out.Webhooks = append(out.Webhooks, Webhook{Path: wh.Path, Action: wh.Action, Secret: wh.Secret})
	}

	// 4e. Triggers: `on <action> -> <reaction>`. When the source action completes,
	// the runtime runs the reaction — a zero-arg, server-placed action, like a job's
	// target. The reaction must exist and be authoritative; an edge whose source is
	// unknown can never fire. The trigger graph must be acyclic so reactions always
	// terminate — a cycle is a compile error naming the loop.
	trigEdges := map[string][]string{} // source action -> reaction actions
	trigSeen := map[string]bool{}
	for _, tr := range app.Triggers {
		src, ok := byAction[tr.On]
		if !ok {
			return nil, &BuildError{tr.Line, fmt.Sprintf("trigger `on %s` names unknown action %q", tr.On, tr.On)}
		}
		_ = src
		react, ok := byAction[tr.Action]
		if !ok {
			return nil, &BuildError{tr.Line, fmt.Sprintf("trigger reaction %q is not a defined action", tr.Action)}
		}
		if react.Placement != Server {
			return nil, &BuildError{tr.Line, fmt.Sprintf("trigger reaction %q is client-placed; a reaction runs on the server authority, so it must be authoritative", tr.Action)}
		}
		if len(react.Params) != 0 {
			return nil, &BuildError{tr.Line, fmt.Sprintf("trigger reaction %q takes arguments; a reaction runs a zero-argument action", tr.Action)}
		}
		key := tr.On + " -> " + tr.Action
		if trigSeen[key] {
			return nil, &BuildError{tr.Line, fmt.Sprintf("trigger `on %s -> %s` redeclared", tr.On, tr.Action)}
		}
		trigSeen[key] = true
		trigEdges[tr.On] = append(trigEdges[tr.On], tr.Action)
		out.Triggers = append(out.Triggers, Trigger{On: tr.On, Action: tr.Action})
	}
	if cyc := triggerCycle(trigEdges); cyc != "" {
		return nil, &BuildError{app.Line, fmt.Sprintf("trigger cycle: %s — reactions would never terminate", cyc)}
	}

	pathOf := map[string]string{} // path -> view name
	allCalls := compCalls
	allLinks := compLinks
	for i, v := range app.Views {
		// Route parameters (`/post/:id`) are in scope as text locals, bound from the
		// matched URL at render time.
		locals := map[string]bool{}
		for _, p := range v.Params {
			locals[p] = true
		}
		// A `requires` guard names a zero-argument policy the authority enforces
		// before rendering the route (and the client uses to hide links to it).
		if v.Requires != "" {
			params, ok := e.policyParams[v.Requires]
			if !ok {
				return nil, &BuildError{v.Line, fmt.Sprintf("view %q requires unknown policy %q", v.Name, v.Requires)}
			}
			if len(params) != 0 {
				return nil, &BuildError{v.Line, fmt.Sprintf("view %q route guard %q is row-level; a route guard must be a zero-argument policy", v.Name, v.Requires)}
			}
		}
		// `in Layout` wraps the view in shared chrome by inlining the view's nodes at
		// the layout's `slot`, producing one tree.
		root := v.Root
		if v.Layout != "" {
			ly, ok := layouts[v.Layout]
			if !ok {
				return nil, &BuildError{v.Line, fmt.Sprintf("view %q uses unknown layout %q", v.Name, v.Layout)}
			}
			root = inlineLayout(ly.Root, v.Root)
		}
		pvc := &viewCtx{e: e}
		nodes, err := pvc.nodes(root, scope{locals: locals})
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
		page := Page{Name: v.Name, Path: path, Params: v.Params, Requires: v.Requires, Screen: v.Screen, View: nodes, Bindings: pvc.bindings, DepGraph: map[string][]string{}}
		for dep, ids := range pvc.deps {
			page.DepGraph[dep] = ids
		}
		out.Pages = append(out.Pages, page)
		allCalls = append(allCalls, pvc.calls...)
		allLinks = append(allLinks, pvc.links...)
	}
	for i := range out.Pages {
		out.Routes = append(out.Routes, Route{Path: out.Pages[i].Path, Requires: out.Pages[i].Requires})
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
	// validate every link points at a real route. A link may target a concrete
	// path of a dynamic route (`/post/5` against `/post/:id`), so match against the
	// route patterns, not just the static paths.
	for _, p := range allLinks {
		matched := false
		for i := range out.Pages {
			if routeMatches(out.Pages[i].Path, p) {
				matched = true
				break
			}
		}
		if !matched {
			return nil, &BuildError{0, fmt.Sprintf("link to %q, but no view serves that route", p)}
		}
	}

	// Stamp the index flags the compiler accumulated (relations + every filtered or
	// ordered field) onto the entity fields, so the store knows what to index.
	for ei := range out.Entities {
		idx := e.indexFields[out.Entities[ei].Name]
		for fi := range out.Entities[ei].Fields {
			f := &out.Entities[ei].Fields[fi]
			if idx[f.Name] {
				// An encrypted column stores ciphertext, so it cannot be filtered,
				// ordered, or indexed in SQL — only read back into memory.
				if f.Secret {
					return nil, &BuildError{0, fmt.Sprintf(
						"field %q is @secret and cannot be used in a `where`, `by`, or relation; it is encrypted at rest", f.Name)}
				}
				f.Index = true
			}
		}
	}
	return out, nil
}

// itemFields returns the names of the loop item's fields a lowered predicate
// reads — every `get` whose object is the item variable (e.g. `p.likes` in a
// `where p.likes > 0`). These are the columns a pushed-down query filters on.
func itemFields(le *Expr, itemVar string) map[string]bool {
	out := map[string]bool{}
	var walk func(*Expr)
	walk = func(x *Expr) {
		if x == nil {
			return
		}
		if x.Kind == "get" && x.Obj != nil && x.Obj.Kind == "ref" && x.Obj.Name == itemVar {
			out[x.Field] = true
		}
		walk(x.Obj)
		walk(x.Key)
		walk(x.L)
		walk(x.R)
		walk(x.X)
		for _, a := range x.Args {
			walk(a)
		}
	}
	walk(le)
	return out
}

// reservedUserEntity is the runtime-managed users table created when `auth` is on.
const reservedUserEntity = "FacetUser"

// authActions are the built-in server actions `auth` provides — identity
// (signup/login/logout), RBAC management (setRole), and account lifecycle
// (password reset, email verification, MFA enrollment + second factor). The
// runtime supplies their behavior (runtime/auth.go); they are injected here so
// views can call them, the API advertises them, and their names are reserved.
func authActions() []Action {
	text := func(names ...string) []Param {
		ps := make([]Param, len(names))
		for i, n := range names {
			ps[i] = Param{Name: n, Type: "text"}
		}
		return ps
	}
	specs := []Action{
		{Name: "signup", Params: text("username", "password")},
		{Name: "login", Params: text("username", "password")},
		{Name: "logout"},
		{Name: "setRole", Params: text("username", "role")},
		{Name: "requestReset", Params: text("username")},
		{Name: "resetPassword", Params: text("username", "token", "password")},
		{Name: "verifyEmail", Params: text("token")},
		{Name: "enableMFA"},
		{Name: "confirmMFA", Params: text("code")},
		{Name: "loginMFA", Params: text("username", "code")},
	}
	for i := range specs {
		specs[i].Placement = Server
	}
	return specs
}

// inlineLayout produces a single node tree by substituting a view's nodes for
// the `slot` marker in a layout's tree, recursing through container nodes. The
// layout's chrome surrounds the routed view, and the result needs no runtime
// layout concept.
func inlineLayout(layout []ast.Node, view []ast.Node) []ast.Node {
	var out []ast.Node
	for _, n := range layout {
		switch t := n.(type) {
		case ast.Slot:
			out = append(out, view...)
		case ast.Box:
			out = append(out, ast.Box{Children: inlineLayout(t.Children, view)})
		case ast.Row:
			out = append(out, ast.Row{Children: inlineLayout(t.Children, view)})
		case ast.If:
			out = append(out, ast.If{Cond: t.Cond, Body: inlineLayout(t.Body, view)})
		case ast.For:
			t.Body = inlineLayout(t.Body, view)
			out = append(out, t)
		default:
			out = append(out, n)
		}
	}
	return out
}

// routeMatches reports whether a concrete path satisfies a route pattern, where a
// `:param` segment matches any single non-empty segment.
func routeMatches(pattern, path string) bool {
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	cs := strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) != len(cs) {
		return false
	}
	for i := range ps {
		if strings.HasPrefix(ps[i], ":") {
			if cs[i] == "" {
				return false
			}
			continue
		}
		if ps[i] != cs[i] {
			return false
		}
	}
	return true
}

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
	loc := map[string]bool{"actor": true, "role": true, "verified": true, "tenant": true, "tenantRole": true}
	for _, p := range a.Params {
		act.Params = append(act.Params, Param{Name: p.Name, Type: p.Type})
		loc[p.Name] = true
	}
	for _, r := range a.Requires {
		params, ok := e.policyParams[r.Name]
		if !ok {
			if _, isDerive := e.inline[r.Name]; isDerive && !e.policySet[r.Name] {
				return Action{}, &BuildError{a.Line, fmt.Sprintf("requires %q, which is a derive, not a policy", r.Name)}
			}
			return Action{}, &BuildError{a.Line, fmt.Sprintf("requires unknown policy %q", r.Name)}
		}
		if len(r.Args) != len(params) {
			return Action{}, &BuildError{a.Line, fmt.Sprintf(
				"policy %q takes %d argument(s), got %d", r.Name, len(params), len(r.Args))}
		}
		req := Require{Name: r.Name}
		for _, arg := range r.Args {
			// A gate argument is an expression over the action's params and the actor;
			// it must be pure (the gate runs on the authority, deterministically).
			if err := e.checkPure(arg, loc, a.Line, "a requires argument"); err != nil {
				return Action{}, err
			}
			req.Args = append(req.Args, e.low(arg))
		}
		act.Requires = append(act.Requires, req)
	}

	writes := map[string]bool{} // state names written
	entWrite := false           // any entity mutation
	reads := map[string]bool{}  // state names read (for soundness)
	impure := false             // uses an effectful builtin (now/rand)
	callsService := false       // calls an external service (an effect)
	establishesID := false      // sets the session identity (`establish`)
	mutated := false            // a state/entity mutation has run — checks/lets must precede it

	readExpr := func(ex ast.Expr, line int) error {
		if err := e.check(ex, loc, line); err != nil {
			return err
		}
		if hasImpure(ex) {
			impure = true
		}
		for n := range e.depsIR(e.low(ex)) {
			if _, isState := e.states[n]; isState {
				reads[n] = true
			}
		}
		return nil
	}

	// Validation (`check`) and request→response binds (`let`) must come before any
	// mutation, so a failed check or a brain error aborts with nothing committed and
	// no partial in-memory write to unwind.
	mustValidateFirst := func(kind string, line int) error {
		if mutated {
			return &BuildError{line, fmt.Sprintf("%s must come before any mutation (add/set/remove/clear/assign/establish), so a failure rolls back nothing — move it above the first mutation", kind)}
		}
		return nil
	}

	for _, s := range a.Body {
		switch st := s.(type) {
		case ast.Check:
			if err := mustValidateFirst("a check", st.Line); err != nil {
				return Action{}, err
			}
			if err := e.checkPure(st.Cond, loc, st.Line, "a check"); err != nil {
				return Action{}, err
			}
			act.Body = append(act.Body, Stmt{Op: "check", Value: e.low(st.Cond), Msg: st.Msg})
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
			mutated = true
			act.Body = append(act.Body, Stmt{Op: "assign", Target: st.Target, Value: e.low(st.Value)})
		case ast.Add:
			if !e.entities[st.Entity] {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("add to unknown entity %q", st.Entity)}
			}
			entWrite = true
			mutated = true
			out := Stmt{Op: "add", Entity: st.Entity}
			for _, fi := range st.Fields {
				if err := readExpr(fi.Expr, st.Line); err != nil {
					return Action{}, err
				}
				out.Fields = append(out.Fields, FieldInit{Name: fi.Name, Expr: e.low(fi.Expr)})
			}
			act.Body = append(act.Body, out)
		case ast.Set:
			if !e.entities[st.Entity] {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("set on unknown entity %q", st.Entity)}
			}
			entWrite = true
			mutated = true
			if err := readExpr(st.Key, st.Line); err != nil {
				return Action{}, err
			}
			if err := readExpr(st.Value, st.Line); err != nil {
				return Action{}, err
			}
			act.Body = append(act.Body, Stmt{Op: "set", Entity: st.Entity, Field: st.Field,
				Key: e.low(st.Key), Value: e.low(st.Value)})
		case ast.Remove:
			if !e.entities[st.Entity] {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("remove on unknown entity %q", st.Entity)}
			}
			entWrite = true
			mutated = true
			if st.Where != nil {
				// Filtered delete: a pure predicate over the item var + action scope.
				wl := map[string]bool{st.Var: true}
				for k := range loc {
					wl[k] = true
				}
				if err := e.checkPure(st.Where, wl, st.Line, "a `remove … where` filter"); err != nil {
					return Action{}, err
				}
				lw := e.low(st.Where)
				// track state reads for soundness (the authority can't read @client state)
				for n := range e.depsIR(lw) {
					if _, isState := e.states[n]; isState {
						reads[n] = true
					}
				}
				act.Body = append(act.Body, Stmt{Op: "remove", Entity: st.Entity, Var: st.Var, Where: lw})
			} else {
				if err := readExpr(st.Key, st.Line); err != nil {
					return Action{}, err
				}
				act.Body = append(act.Body, Stmt{Op: "remove", Entity: st.Entity, Key: e.low(st.Key)})
			}
		case ast.Clear:
			if !e.entities[st.Entity] {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("clear on unknown entity %q", st.Entity)}
			}
			entWrite = true
			mutated = true
			act.Body = append(act.Body, Stmt{Op: "clear", Entity: st.Entity})
		case ast.ServiceCall:
			ops, ok := e.services[st.Service]
			if !ok {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("call to unknown service %q", st.Service)}
			}
			argc, ok := ops[st.Op]
			if !ok {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("service %q has no operation %q", st.Service, st.Op)}
			}
			if len(st.Args) != argc {
				return Action{}, &BuildError{st.Line, fmt.Sprintf("%s.%s expects %d argument(s), got %d", st.Service, st.Op, argc, len(st.Args))}
			}
			callsService = true
			cs := Stmt{Op: "call", Service: st.Service, Field: st.Op}
			for _, arg := range st.Args {
				if err := readExpr(arg, st.Line); err != nil {
					return Action{}, err
				}
				cs.Args = append(cs.Args, e.low(arg))
			}
			// Request→response: `let x = call …` binds the typed result into a local
			// so the rest of the body can use it (e.g. assign it into a state cell).
			if st.Bind != "" {
				if err := mustValidateFirst("a `let` bind", st.Line); err != nil {
					return Action{}, err
				}
				ret := e.serviceRets[st.Service][st.Op]
				if ret.ret == "" {
					return Action{}, &BuildError{st.Line, fmt.Sprintf("%s.%s returns nothing — declare a return type (`%s(…) -> Type`) to bind it", st.Service, st.Op, st.Op)}
				}
				if loc[st.Bind] {
					return Action{}, &BuildError{st.Line, fmt.Sprintf("%q is already in scope — pick another name for the bound result", st.Bind)}
				}
				loc[st.Bind] = true // visible to the rest of the action body
				cs.Bind = st.Bind
				cs.Ret = ret.ret
				cs.RetList = ret.list
			}
			act.Body = append(act.Body, cs)
		case ast.Establish:
			// Adopt a custom session identity. Setting who you are is the authority's
			// job, so it forces server placement; the actor/role exprs are reads.
			establishesID = true
			mutated = true
			if err := readExpr(st.Actor, st.Line); err != nil {
				return Action{}, err
			}
			// actor/role become the renderable session identity, so they cannot be a
			// @private value — that would copy the secret key into a renderable slot.
			if err := e.checkNoPrivate(st.Actor); err != nil {
				return Action{}, &BuildError{st.Line, "establish actor sets the renderable identity, so it cannot be a @private value — establish the handle and key policies on the @private UUID instead"}
			}
			es := Stmt{Op: "establish", Value: e.low(st.Actor)}
			if st.Role != nil {
				if err := readExpr(st.Role, st.Line); err != nil {
					return Action{}, err
				}
				if err := e.checkNoPrivate(st.Role); err != nil {
					return Action{}, &BuildError{st.Line, "establish role cannot be a @private value"}
				}
				es.Role = e.low(st.Role)
			}
			act.Body = append(act.Body, es)
		}
	}

	// placement: server iff it writes any authoritative cell OR is impure. An
	// effectful builtin (now/rand) is nondeterministic, so the authority must run
	// it — that way every client sees one agreed result, not its own.
	act.Placement = Client
	act.Reason = "only touches @client state, so it runs in the browser with no round-trip"
	switch {
	case entWrite:
		act.Placement = Server
		act.Reason = "writes durable entity data — the authority owns the database"
	case impure:
		act.Placement = Server
		act.Reason = "uses an effectful builtin (now/rand) — the authority owns nondeterminism, so every client sees one agreed result"
	case callsService:
		act.Placement = Server
		act.Reason = "calls an external service — egress routes through the authority, never the client"
	case establishesID:
		act.Placement = Server
		act.Reason = "establishes the session identity — only the authority may set who you are"
	}
	if act.Placement == Client {
		for _, w := range sortedKeys(writes) {
			if e.states[w] == Server {
				act.Placement = Server
				act.Reason = fmt.Sprintf("writes authoritative state %q — only the authority may change it", w)
				break
			}
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
	locals    map[string]bool
	inRegion  bool
	itemTypes map[string]string // loop/item variable -> the entity it ranges over (for `match` enum resolution)
}

func (s scope) with(v string) scope {
	m := map[string]bool{}
	for k := range s.locals {
		m[k] = true
	}
	m[v] = true
	it := map[string]string{}
	for k, val := range s.itemTypes {
		it[k] = val
	}
	return scope{locals: m, inRegion: true, itemTypes: it}
}

// matchEnum resolves the enum type of a `match` subject, for exhaustiveness — a
// state cell (`match mode:`) or an entity item field (`match post.kind:`). Returns
// "" when the subject is not enum-typed (then the match must have an `else`).
func (c *viewCtx) matchEnum(e ast.Expr, sc scope) string {
	switch t := e.(type) {
	case ast.Ref:
		if typ := c.e.stateTypes[t.Name]; typ != "" {
			if _, ok := c.e.enums[typ]; ok {
				return typ
			}
		}
	case ast.Get:
		if r, ok := t.Obj.(ast.Ref); ok {
			if ent, ok := sc.itemTypes[r.Name]; ok {
				if fields, ok := c.e.entFieldEnum[ent]; ok {
					return fields[t.Field] // "" if not an enum field
				}
			}
		}
	}
	return ""
}

func enumHas(members []string, v string) bool {
	for _, m := range members {
		if m == v {
			return true
		}
	}
	return false
}

type call struct {
	name string
	argc int
}

type viewCtx struct {
	e              *env
	bindings       []Binding
	deps           map[string][]string // dep -> tracked region ids
	calls          []call
	links          []string // link destination routes (validated against real pages)
	nb, nl, nf, nu int
}

func (c *viewCtx) addDep(dep, id string) {
	if c.deps == nil {
		c.deps = map[string][]string{}
	}
	c.deps[dep] = append(c.deps[dep], id)
}

// lowerSegs lowers interpolated segments shared by text, button labels, and image
// URLs. A pure expression segment renders inline inside a region (a `for` row) or,
// at the top level, becomes a reactive binding the client recomputes on change.
func (c *viewCtx) lowerSegs(segs []ast.Seg, sc scope) ([]Seg, error) {
	var out []Seg
	for _, s := range segs {
		if s.Expr == nil {
			out = append(out, Seg{Lit: s.Lit})
			continue
		}
		if err := c.e.checkPure(s.Expr, withActor(sc.locals), 0, "a view"); err != nil {
			return nil, err
		}
		if err := c.e.checkNoPrivate(s.Expr); err != nil {
			return nil, err
		}
		if sc.inRegion {
			out = append(out, Seg{Expr: c.e.low(s.Expr)})
		} else {
			id := fmt.Sprintf("b%d", c.nb)
			c.nb++
			le := c.e.low(s.Expr)
			deps := sortedKeys(c.e.depsIR(le))
			c.bindings = append(c.bindings, Binding{ID: id, Expr: le, Deps: deps})
			for _, d := range deps {
				c.addDep(d, id)
			}
			out = append(out, Seg{Bind: id})
		}
	}
	return out, nil
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

		case ast.Row:
			kids, err := c.nodes(t.Children, sc)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "row", Children: kids})

		case ast.Text:
			segs, err := c.lowerSegs(t.Segs, sc)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "text", Segs: segs})

		case ast.Image:
			segs, err := c.lowerSegs(t.Segs, sc)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "image", Segs: segs})

		case ast.Icon:
			out = append(out, Node{Kind: "icon", Name: t.Name})

		case ast.Video:
			segs, err := c.lowerSegs(t.Segs, sc)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "video", Segs: segs})

		case ast.Richtext:
			segs, err := c.lowerSegs(t.Segs, sc)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "richtext", Segs: segs})

		case ast.Badge:
			segs, err := c.lowerSegs(t.Segs, sc)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "badge", Segs: segs})

		case ast.Tabs:
			p, ok := c.e.states[t.Bind]
			if !ok {
				return nil, &BuildError{t.Line, fmt.Sprintf("tabs binds unknown state %q", t.Bind)}
			}
			if p != Client {
				return nil, &BuildError{t.Line, fmt.Sprintf("tabs binds %q, which is authoritative; switching tabs is local, so it requires a @client state", t.Bind)}
			}
			node := Node{Kind: "tabs", Bind: t.Bind}
			if !sc.inRegion {
				node.ID = fmt.Sprintf("t%d", c.nf)
				c.nf++
				c.addDep(t.Bind, node.ID)
			}
			for _, tb := range t.Tabs {
				kids, err := c.nodes(tb.Body, scope{locals: sc.locals, inRegion: true})
				if err != nil {
					return nil, err
				}
				node.Children = append(node.Children, Node{Kind: "tab", Label: tb.Label, Value: tb.Value, Children: kids})
			}
			// A tab body's reads (its feed list, counts) refresh the whole control too.
			if node.ID != "" {
				for _, d := range sortedKeys(c.e.nodeDeps(node.Children)) {
					c.addDep(d, node.ID)
				}
			}
			out = append(out, node)

		case ast.Match:
			if err := c.e.checkPure(t.Expr, withActor(sc.locals), t.Line, "a `match`"); err != nil {
				return nil, err
			}
			enumName := c.matchEnum(t.Expr, sc)
			node := Node{Kind: "match", Cond: c.e.low(t.Expr)}
			seen := map[string]bool{}
			for _, cs := range t.Cases {
				if seen[cs.Value] {
					return nil, &BuildError{t.Line, fmt.Sprintf("duplicate match case %q", cs.Value)}
				}
				seen[cs.Value] = true
				if enumName != "" && !enumHas(c.e.enums[enumName], cs.Value) {
					return nil, &BuildError{t.Line, fmt.Sprintf("enum %q has no member %q", enumName, cs.Value)}
				}
				kids, err := c.nodes(cs.Body, scope{locals: sc.locals, inRegion: true, itemTypes: sc.itemTypes})
				if err != nil {
					return nil, err
				}
				node.Children = append(node.Children, Node{Kind: "case", Value: cs.Value, Children: kids})
			}
			if t.Else != nil {
				kids, err := c.nodes(t.Else, scope{locals: sc.locals, inRegion: true, itemTypes: sc.itemTypes})
				if err != nil {
					return nil, err
				}
				node.Children = append(node.Children, Node{Kind: "else", Children: kids})
			}
			// Exhaustiveness: an enum-typed match must cover every member or have an
			// `else`; an open-typed match must have an `else` (we can't prove coverage).
			if t.Else == nil {
				if enumName != "" {
					var missing []string
					for _, m := range c.e.enums[enumName] {
						if !seen[m] {
							missing = append(missing, m)
						}
					}
					if len(missing) > 0 {
						return nil, &BuildError{t.Line, fmt.Sprintf(
							"match on enum %q is not exhaustive: missing %s (add the case(s) or an `else`)", enumName, strings.Join(missing, ", "))}
					}
				} else {
					return nil, &BuildError{t.Line,
						"match must be exhaustive: add an `else` branch (the matched value's type is open, so coverage can't be proven)"}
				}
			}
			if !sc.inRegion {
				node.ID = fmt.Sprintf("m%d", c.nf)
				c.nf++
				for _, d := range sortedKeys(c.e.depsIR(node.Cond)) {
					c.addDep(d, node.ID)
				}
				for _, d := range sortedKeys(c.e.nodeDeps(node.Children)) {
					c.addDep(d, node.ID)
				}
			}
			out = append(out, node)

		case ast.Button:
			segs, err := c.lowerSegs(t.Label, sc)
			if err != nil {
				return nil, err
			}
			node := Node{Kind: "button", Segs: segs, Action: t.Action}
			for _, arg := range t.Args {
				if err := c.e.checkPure(arg, withActor(sc.locals), 0, "a view"); err != nil {
					return nil, err
				}
				node.Args = append(node.Args, c.e.low(arg))
			}
			c.calls = append(c.calls, call{name: t.Action, argc: len(t.Args)})
			out = append(out, node)

		case ast.For:
			if !c.e.entities[t.Coll] && c.e.states[t.Coll] == "" {
				return nil, &BuildError{0, fmt.Sprintf("`for` over unknown collection %q", t.Coll)}
			}
			node := Node{Kind: "list", Var: t.Var, Coll: t.Coll, Order: t.Order, Desc: t.Desc}
			// `limit` may be a literal or a pure expr (e.g. a @client page size for
			// load-more / infinite scroll); evaluated per render, not per row.
			if t.Limit != nil {
				if err := c.e.checkPure(t.Limit, withActor(sc.locals), 0, "a `limit`"); err != nil {
					return nil, err
				}
				node.Limit = c.e.low(t.Limit)
			}
			// `where` filter: a pure predicate over the item var + outer scope.
			if t.Where != nil {
				wlocals := withActor(sc.locals)
				wlocals[t.Var] = true
				if err := c.e.checkPure(t.Where, wlocals, 0, "a `where` filter"); err != nil {
					return nil, err
				}
				node.Where = c.e.low(t.Where)
				// Any of the item's fields the filter touches becomes a candidate index
				// — that is the column the store filters on when it pushes the query down.
				if c.e.entities[t.Coll] {
					for f := range itemFields(node.Where, t.Var) {
						if c.e.entityFields[t.Coll][f] {
							c.e.markIndex(t.Coll, f)
						}
					}
				}
			}
			if t.Order != "" {
				if !c.e.entities[t.Coll] {
					return nil, &BuildError{0, fmt.Sprintf("ordering `by %s` requires %q to be an entity", t.Order, t.Coll)}
				}
				if !c.e.entityFields[t.Coll][t.Order] {
					return nil, &BuildError{0, fmt.Sprintf("entity %q has no field %q to order by", t.Coll, t.Order)}
				}
				c.e.markIndex(t.Coll, t.Order)
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
				// a dynamic limit (load-more page size) refreshes the list when it grows.
				if node.Limit != nil {
					for _, d := range sortedKeys(c.e.depsIR(node.Limit)) {
						c.addDep(d, node.ID)
					}
				}
			}
			child := sc.with(t.Var)
			if c.e.entities[t.Coll] {
				child.itemTypes[t.Var] = t.Coll // so `match item.field` can resolve an enum field
			}
			kids, err := c.nodes(t.Body, child)
			if err != nil {
				return nil, err
			}
			node.Children = kids
			// Reads inside the body — interpolated counts, cross-entity lookups
			// (`User(m.to).name`), per-row `count(...)`/`exists(...)` — refresh the
			// whole list region too, so e.g. a new like updates a per-row count live.
			if node.ID != "" {
				for _, d := range sortedKeys(c.e.nodeDeps(kids)) {
					c.addDep(d, node.ID)
				}
			}
			out = append(out, node)

		case ast.If:
			if err := c.e.checkPure(t.Cond, withActor(sc.locals), 0, "a view"); err != nil {
				return nil, err
			}
			node := Node{Kind: "if", Cond: c.e.low(t.Cond)}
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

		case ast.Select:
			p, ok := c.e.states[t.Bind]
			if !ok {
				return nil, &BuildError{0, fmt.Sprintf("select binds unknown state %q", t.Bind)}
			}
			if p != Client {
				return nil, &BuildError{0, fmt.Sprintf("select binds %q, which is authoritative; two-way input requires a @client state", t.Bind)}
			}
			node := Node{Kind: "select", Bind: t.Bind}
			for _, o := range t.Options {
				node.Options = append(node.Options, Option{Label: o.Label, Value: o.Value})
			}
			// An enum-typed select with no explicit options defaults to the enum members.
			if len(node.Options) == 0 {
				if members, isEnum := c.e.enums[c.e.stateTypes[t.Bind]]; isEnum {
					for _, m := range members {
						node.Options = append(node.Options, Option{Label: m, Value: m})
					}
				} else {
					return nil, &BuildError{0, fmt.Sprintf("select on %q needs options (or a `@client` enum cell to default them)", t.Bind)}
				}
			}
			id := fmt.Sprintf("b%d", c.nb)
			c.nb++
			node.ID = id
			c.addDep(t.Bind, id)
			out = append(out, node)

		case ast.Form:
			node := Node{Kind: "form", Action: t.Action, Label: t.Submit}
			for _, arg := range t.Args {
				if err := c.e.checkPure(arg, withActor(sc.locals), 0, "a view"); err != nil {
					return nil, err
				}
				node.Args = append(node.Args, c.e.low(arg))
			}
			c.calls = append(c.calls, call{name: t.Action, argc: len(t.Args)})
			kids, err := c.nodes(t.Body, sc)
			if err != nil {
				return nil, err
			}
			node.Children = kids
			out = append(out, node)

		case ast.Upload:
			p, ok := c.e.states[t.Bind]
			if !ok {
				return nil, &BuildError{0, fmt.Sprintf("upload binds unknown state %q", t.Bind)}
			}
			if p != Client {
				return nil, &BuildError{0, fmt.Sprintf("upload binds %q, which is authoritative; it must store the URL in a @client state", t.Bind)}
			}
			id := fmt.Sprintf("b%d", c.nb)
			c.nb++
			c.addDep(t.Bind, id)
			out = append(out, Node{Kind: "upload", Bind: t.Bind, Label: t.Label, ID: id})

		case ast.Use:
			params, ok := c.e.components[t.Name]
			if !ok {
				return nil, &BuildError{0, fmt.Sprintf("use of unknown component %q", t.Name)}
			}
			if len(t.Args) != len(params) {
				return nil, &BuildError{0, fmt.Sprintf("component %q takes %d argument(s), got %d", t.Name, len(params), len(t.Args))}
			}
			node := Node{Kind: "use", Name: t.Name}
			deps := map[string]bool{}
			for _, arg := range t.Args {
				if err := c.e.checkPure(arg, withActor(sc.locals), 0, "a view"); err != nil {
					return nil, err
				}
				le := c.e.low(arg)
				node.Args = append(node.Args, le)
				for d := range c.e.depsIR(le) {
					deps[d] = true
				}
			}
			for d := range c.e.compDeps[t.Name] {
				deps[d] = true
			}
			// A top-level `use` is a tracked region: it re-renders whole when any state
			// its arguments or body reads changes. Inside another region it renders
			// inline and refreshes with its parent.
			if !sc.inRegion {
				node.ID = fmt.Sprintf("u%d", c.nu)
				c.nu++
				for _, d := range sortedKeys(deps) {
					c.addDep(d, node.ID)
				}
			}
			out = append(out, node)

		case ast.Slot:
			return nil, &BuildError{0, "`slot` may only appear inside a layout"}
		case ast.SlotRef:
			return nil, &BuildError{0, fmt.Sprintf("`slot %s` may only appear inside a wireframe frame", t.Name)}
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
		if _, ok := e.enums[n]; ok { // an enum name, as the object of a `.member` access (folded by lower())
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
// checkNoPrivate rejects an expression that reads a @private state cell from a
// render position. A @private value is server-only — never shipped, never
// renderable — so interpolating it (text/label/url/badge/richtext/video) would
// leak it into output. It stays available to policies, checks, and action logic.
func (e *env) checkNoPrivate(ex ast.Expr) error {
	if len(e.private) == 0 {
		return nil
	}
	for n := range e.depsIR(e.low(ex)) {
		if e.private[n] {
			return &BuildError{0, fmt.Sprintf(
				"%q is @private (server-only) and cannot be rendered — it can gate logic, key policies, and feed services, but interpolating it would leak it to the client. Render a non-private value (e.g. the handle) instead", n)}
		}
	}
	return nil
}

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
		if t.Op == "exists" && t.Var == "" {
			return &BuildError{line, fmt.Sprintf("exists needs a filtered form: exists(x in %s where <cond>)", t.Coll)}
		}
		if t.Where != nil {
			if err := e.checkBuiltins(t.Where, line); err != nil {
				return err
			}
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
			// A pure standard-library builtin (string/date/math): fixed arity.
			n, ok := pureBuiltinArity(t.Name)
			if !ok {
				return &BuildError{line, fmt.Sprintf("unknown builtin %q", t.Name)}
			}
			if len(t.Args) != n {
				return &BuildError{line, fmt.Sprintf("%s takes %d argument(s), got %d", t.Name, n, len(t.Args))}
			}
		}
		for _, a := range t.Args {
			if err := e.checkBuiltins(a, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := e.checkBuiltins(el, line); err != nil {
				return err
			}
		}
	case ast.Get:
		// Enum member access (`Status.active`) must name a declared member.
		if r, ok := t.Obj.(ast.Ref); ok {
			if members, isEnum := e.enums[r.Name]; isEnum {
				for _, m := range members {
					if m == t.Field {
						return nil
					}
				}
				return &BuildError{line, fmt.Sprintf("enum %q has no member %q", r.Name, t.Field)}
			}
		}
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
// only nondeterministic calls; the standard-library builtins (string/math/date)
// are pure and may appear in any context.
func hasImpure(ex ast.Expr) bool {
	switch t := ex.(type) {
	case ast.Call:
		if t.Name == "now" || t.Name == "rand" {
			return true
		}
		for _, a := range t.Args {
			if hasImpure(a) {
				return true
			}
		}
		return false
	case ast.Get:
		return hasImpure(t.Obj)
	case ast.EntityGet:
		return hasImpure(t.Key)
	case ast.ListLit:
		for _, el := range t.Elems {
			if hasImpure(el) {
				return true
			}
		}
		return false
	case ast.Bin:
		return hasImpure(t.L) || hasImpure(t.R)
	case ast.Un:
		return hasImpure(t.X)
	case ast.Agg:
		return t.Where != nil && hasImpure(t.Where)
	}
	return false
}

// pureBuiltinArity gives the fixed argument count of a pure standard-library
// builtin, and whether the name is one.
func pureBuiltinArity(name string) (int, bool) {
	switch name {
	case "abs", "floor", "round", "money", "len", "upper", "lower", "trim", "year", "month", "day":
		return 1, true
	case "min", "max", "contains":
		return 2, true
	}
	return 0, false
}

func isPrimitive(t string) bool {
	switch t {
	case "int", "text", "bool", "money", "date":
		return true
	}
	return false
}

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
		case "astate":
			// pending()/failed() read per-action client status; a synthetic dep key so
			// the dispatch loop can refresh exactly the regions that show it.
			out["@act:"+x.Name] = true
		case "agg":
			if e.entities[x.Name] {
				out[x.Name] = true
			}
			// The filter may read outer state/entities (e.g. `actor`, another entity);
			// the item variable is a bare ref to neither, so it is ignored naturally.
			walk(x.Where)
		case "call", "list":
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

// nodeDeps collects every state/entity name read anywhere in a node tree (text
// segs, conditions, filters, args). A `use` of a component refreshes when any of
// these change, so its own body's reads matter alongside its argument deps.
func (e *env) nodeDeps(nodes []Node) map[string]bool {
	out := map[string]bool{}
	add := func(x *Expr) {
		for d := range e.depsIR(x) {
			out[d] = true
		}
	}
	var walk func(n Node)
	walk = func(n Node) {
		for _, s := range n.Segs {
			if s.Expr != nil {
				add(s.Expr)
			}
		}
		add(n.Cond)
		add(n.Where)
		for _, a := range n.Args {
			add(a)
		}
		if n.Coll != "" && (e.states[n.Coll] != "" || e.entities[n.Coll]) {
			out[n.Coll] = true
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
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
			// The filter's references are free names too — except the item variable,
			// which the aggregate itself binds.
			if t.Where != nil {
				for n := range freeNames(t.Where) {
					if n != t.Var {
						out[n] = true
					}
				}
			}
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

// low lowers an expression with this environment's inline (policy/derive) and
// enum tables in scope. It is the method every build site uses.
func (e *env) low(ex ast.Expr) *Expr { return lower(ex, e.inline, e.enums) }

// lower converts an ast.Expr to its serializable IR form, inlining any reference
// to a policy or derive name with that name's lowered expression (so the same
// value is computed identically wherever it is read — a server gate, a view
// `if`, or another derivation) and folding enum member access (`Status.active`)
// to its backing text literal.
func lower(ex ast.Expr, inline map[string]*Expr, enums map[string][]string) *Expr {
	switch t := ex.(type) {
	case ast.Lit:
		return &Expr{Kind: "lit", Val: t.Val, VType: t.Kind}
	case ast.ListLit:
		out := &Expr{Kind: "list"}
		for _, el := range t.Elems {
			out.Args = append(out.Args, lower(el, inline, enums))
		}
		return out
	case ast.Ref:
		if inline != nil {
			if p, ok := inline[t.Name]; ok {
				return cloneExpr(p)
			}
		}
		return &Expr{Kind: "ref", Name: t.Name}
	case ast.Get:
		// Enum member access folds to its backing text value at compile time.
		if r, ok := t.Obj.(ast.Ref); ok && enums != nil {
			if _, isEnum := enums[r.Name]; isEnum {
				return &Expr{Kind: "lit", Val: t.Field, VType: "text"}
			}
		}
		return &Expr{Kind: "get", Obj: lower(t.Obj, inline, enums), Field: t.Field}
	case ast.EntityGet:
		return &Expr{Kind: "eget", Name: t.Entity, Key: lower(t.Key, inline, enums), Field: t.Field}
	case ast.ActState:
		return &Expr{Kind: "astate", Op: t.Op, Name: t.Action}
	case ast.Agg:
		a := &Expr{Kind: "agg", Op: t.Op, Name: t.Coll, Field: t.Field, Var: t.Var}
		if t.Where != nil {
			a.Where = lower(t.Where, inline, enums)
		}
		return a
	case ast.Call:
		out := &Expr{Kind: "call", Name: t.Name}
		for _, a := range t.Args {
			out.Args = append(out.Args, lower(a, inline, enums))
		}
		return out
	case ast.Bin:
		return &Expr{Kind: "bin", Op: t.Op, L: lower(t.L, inline, enums), R: lower(t.R, inline, enums)}
	case ast.Un:
		return &Expr{Kind: "un", Op: t.Op, X: lower(t.X, inline, enums)}
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
	c.Where = cloneExpr(e.Where)
	if e.Args != nil {
		c.Args = make([]*Expr, len(e.Args))
		for i, a := range e.Args {
			c.Args[i] = cloneExpr(a)
		}
	}
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
// signed-in user's name (`actor`), role (`role`), verified-email flag
// (`verified`), the active tenant id (`tenant`), or the actor's role within it
// (`tenantRole`). The tenant values are 0/"" unless multi-tenancy is enabled.
func isBuiltinRef(n string) bool {
	switch n {
	case "actor", "role", "verified", "tenant", "tenantRole":
		return true
	}
	return false
}

func withActor(locals map[string]bool) map[string]bool {
	m := map[string]bool{"actor": true, "role": true, "verified": true, "tenant": true, "tenantRole": true}
	for k := range locals {
		m[k] = true
	}
	return m
}

// irParams converts AST parameters to their IR form.
func irParams(ps []ast.Param) []Param {
	if len(ps) == 0 {
		return nil
	}
	out := make([]Param, len(ps))
	for i, p := range ps {
		out[i] = Param{Name: p.Name, Type: p.Type}
	}
	return out
}

// reservedWebhookPath reports whether a webhook path would shadow a route the
// runtime owns (its API, admin, uploads, auth, ops probes, and the built-in
// billing webhook). Keeping these off-limits means a declared webhook can never
// intercept the app's own traffic.
func reservedWebhookPath(p string) bool {
	switch p {
	case "/", "/event", "/live", "/api", "/upload", "/admin", "/facet.js",
		"/healthz", "/readyz", "/metrics", "/billing/webhook":
		return true
	}
	for _, pre := range []string{"/api/", "/uploads/", "/admin/", "/auth/", "/dev/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// triggerCycle reports the first cycle in the trigger graph (edges: source action
// -> reaction) as a readable path like "a -> b -> a", or "" when the graph is
// acyclic. A cycle would let reactions re-fire forever, so the compiler rejects it.
func triggerCycle(edges map[string][]string) string {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	color := map[string]int{}
	var stack []string
	var dfs func(n string) string
	dfs = func(n string) string {
		color[n] = gray
		stack = append(stack, n)
		for _, m := range edges[n] {
			switch color[m] {
			case gray:
				// Found a back-edge: the cycle is the stack from m's first appearance.
				start := 0
				for i, s := range stack {
					if s == m {
						start = i
						break
					}
				}
				return strings.Join(append(append([]string{}, stack[start:]...), m), " -> ")
			case white:
				if c := dfs(m); c != "" {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return ""
	}
	for _, n := range sortedEdgeKeys(edges) {
		if color[n] == white {
			if c := dfs(n); c != "" {
				return c
			}
		}
	}
	return ""
}

// sortedEdgeKeys returns the source nodes of an edge map in a stable order, so
// cycle detection reports the same cycle across runs.
func sortedEdgeKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
