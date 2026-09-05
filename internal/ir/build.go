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
	states        map[string]string              // name -> placement
	entities      map[string]bool                // entity names
	entityFields  map[string]map[string]bool     // entity -> field set (incl id)
	queriedFields map[string]map[string]bool     // entity -> fields a `where`/`by`/relation reads at all
	indexFields   map[string]map[string]bool     // entity -> fields whose use an index can actually serve
	inline        map[string]*Expr               // zero-arg policy/derive name -> lowered expr, inlined at every use
	inlineType    map[string]vtype               // the same names -> the type they resolve to (a derive's declared type, a policy's bool)
	policySet     map[string]bool                // policy names (gating via `requires`)
	policyParams  map[string][]Param             // policy name -> its parameters (row-level policies)
	enums         map[string][]string            // enum name -> ordered member values
	components    map[string][]ast.Param         // component name -> its parameters (a reference parameter carries its Ref kind)
	compAST       map[string]*ast.Component      // component name -> its source, for call-site expansion of templates
	compSlot      map[string]bool                // component names whose body contains a `slot` (so a `use` may pass children)
	compDeps      map[string]map[string]bool     // component name -> the state/entity names its body reads (for use-site refresh)
	compRegions   map[string]map[string][]string // component name -> the dependency edges its own regions/inputs need, folded into every page that uses it
	special       []Component                    // per-call-site expansions of template components, appended to the IR
	specialCalls  []call                         // action references made inside expansions, validated with the rest
	specialLinks  []linkRef                      // link destinations inside expansions, route-checked with the rest
	specStack     []string                       // components currently being expanded, to refuse recursion
	nspec         int                            // expansions minted so far; names them and namespaces their region ids
	stateTypes    map[string]string              // state name -> its (core/element) type, for enum-defaulted selects
	stateList     map[string]bool                // state names that are `[T]` list cells, for `for x in <list>`
	services      map[string]map[string]int      // service name -> op name -> parameter count, for checking `call`
	serviceRets   map[string]map[string]opRet    // service name -> op name -> return type, for binding `let x = call …`
	private       map[string]bool                // @private state names — server-only, non-renderable
	entFieldEnum  map[string]map[string]string   // entity -> field -> enum name (only enum-typed fields), for `match` exhaustiveness
	entFieldType  map[string]map[string]string   // entity -> field -> stored type core (enum fields read as "text"), for typing a data-driven option's value
	records       map[string]map[string]recField // record name -> field name -> its type, for `let`-bound field access
	entE2E        map[string]map[string]bool     // entity -> field -> true for @e2e (sealed) fields, for render-marking and the seal dataflow
	locRecords    map[string]recBind             // record-typed action locals (a `let` bind) -> the record bound, for `v.field` checking (reset per action)
	actionSet     map[string]bool                // action names, for validating pending()/failed() targets
}

// opRet is a service operation's declared return type ("" core = no return).
type opRet struct {
	ret  string
	list bool
}

// recField is one record field's resolved type, for checking a `v.field` access.
type recField struct {
	typ  string
	list bool
}

// recBind is what a record-typed local (`let v = call …`) is bound to: a record
// name and whether the bind is a list of that record.
type recBind struct {
	rec  string
	list bool
}

// markQueried records that a `where`, `by`, or relation reads entity.field at
// all — anywhere, at any depth, including buried inside a call.
//
// This is the set the @secret check runs against, and it has to stay this wide:
// an encrypted column stores ciphertext, so *reading* it in a query is the error,
// regardless of whether the read is one an index could have served.
func (e *env) markQueried(entity, field string) {
	if e.queriedFields[entity] == nil {
		e.queriedFields[entity] = map[string]bool{}
	}
	e.queriedFields[entity][field] = true
}

// markIndex records that entity.field is used in a way an index can serve, so
// the store builds one for it. `id` is never marked: a row's identity is already
// the store's primary order (a Postgres primary key, a FacetQL address), so a
// secondary index over it costs writes and buys no read.
//
// # Why this is a different set from markQueried
//
// They were one map, and conflating them shipped an index nobody could use and
// that a large enough row made fatal. `where contains(lower(t.body), q)` reads
// `body`, so `body` was marked and an index was built over it — but a substring
// search cannot be answered by an ordered index in any store, so the index only
// ever cost writes. On FacetQL it did worse: an index key is bounded, so the
// first post longer than that bound made `create index on Tweet.body` fail, and
// because the store reconciles its indexes at startup, the app stopped booting.
// One ordinary long post permanently took the site down.
//
// So a field earns an index by *how* it is used, not by being mentioned. See
// comparedItemFields.
func (e *env) markIndex(entity, field string) {
	e.markQueried(entity, field)

	if field == "id" {
		return
	}
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
	e := &env{states: map[string]string{}, entities: map[string]bool{}, entityFields: map[string]map[string]bool{}, queriedFields: map[string]map[string]bool{}, indexFields: map[string]map[string]bool{}, inline: map[string]*Expr{}, inlineType: map[string]vtype{}, policySet: map[string]bool{}, policyParams: map[string][]Param{}, enums: map[string][]string{}, components: map[string][]ast.Param{}, compAST: map[string]*ast.Component{}, compSlot: map[string]bool{}, compDeps: map[string]map[string]bool{}, compRegions: map[string]map[string][]string{}, stateTypes: map[string]string{}, stateList: map[string]bool{}, services: map[string]map[string]int{}, serviceRets: map[string]map[string]opRet{}, private: map[string]bool{}, entFieldEnum: map[string]map[string]string{}, entFieldType: map[string]map[string]string{}, records: map[string]map[string]recField{}, entE2E: map[string]map[string]bool{}, actionSet: map[string]bool{}}

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

	// 0b. Records: flat value-object types (the typed shape of a brain's reply).
	// Collected after enums so a record field may be enum-typed. A record is pure
	// data — it can't be named where an entity is expected, only as a service-op
	// return type or the type of a `let`-bound result.
	recSeen := map[string]int{}
	for _, rc := range app.Records {
		if prev, ok := recSeen[rc.Name]; ok {
			return nil, &BuildError{rc.Line, fmt.Sprintf("record %q redeclared (first at line %d)", rc.Name, prev)}
		}
		if _, clash := e.enums[rc.Name]; clash {
			return nil, &BuildError{rc.Line, fmt.Sprintf("record %q clashes with an enum of the same name", rc.Name)}
		}
		recSeen[rc.Name] = rc.Line
		fields := map[string]recField{}
		var irFields []RecordField
		for _, f := range rc.Fields {
			if _, dup := fields[f.Name]; dup {
				return nil, &BuildError{f.Line, fmt.Sprintf("record %q has duplicate field %q", rc.Name, f.Name)}
			}
			// A record is flat: its field is a primitive or an enum, never another
			// record or an entity — so `v.field` is always a single-level, typed read.
			if !isPrimitive(f.Type) {
				if _, isEnum := e.enums[f.Type]; !isEnum {
					return nil, &BuildError{f.Line, fmt.Sprintf("record field %q has type %q — a record field must be a primitive, an enum, or a list of those (records are flat: no nested records or entities)", f.Name, f.Type)}
				}
			}
			fields[f.Name] = recField{typ: f.Type, list: f.List}
			irFields = append(irFields, RecordField{Name: f.Name, Type: f.Type, List: f.List, Optional: f.Optional})
		}
		e.records[rc.Name] = fields
		out.Records = append(out.Records, Record{Name: rc.Name, Fields: irFields})
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
		// Every row has an `id` nobody declares: the store's own identity, an int
		// (runtime/sql.go and Server.entityField spell the same thing). It is the
		// value a data-driven option almost always stores, so the type table has to
		// know it or `option "{c.name}" -> c.id` could not be typed at all.
		e.entFieldType[ent.Name] = map[string]string{"id": "int"}
		ei := Entity{Name: ent.Name, SoftDelete: ent.SoftDelete}
		for _, f := range ent.Fields {
			if f.Secret && f.Name == "id" {
				return nil, &BuildError{f.Line, "the id field cannot be @secret"}
			}
			fld := Field{Name: f.Name, Type: f.Type, Secret: f.Secret, E2E: f.E2E, ReadPolicy: f.ReadPolicy, Optional: f.Optional,
				Unique: f.Unique, Required: f.Required, Min: f.Min, Max: f.Max, Matches: f.Matches}
			if f.Unique {
				e.markIndex(ent.Name, f.Name) // a uniqueness check reads by value; index it
			}
			if f.E2E {
				if e.entE2E[ent.Name] == nil {
					e.entE2E[ent.Name] = map[string]bool{}
				}
				e.entE2E[ent.Name][f.Name] = true
			}
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
			e.entFieldType[ent.Name][f.Name] = fld.Type
		}
		// Soft-delete needs a durable `archived` flag to persist the hidden state. The
		// compiler injects it (reserved), so the author never models it by hand. It is
		// indexed — the load path filters archived rows out of the live working set.
		if ent.SoftDelete {
			if e.entityFields[ent.Name]["archived"] {
				return nil, &BuildError{ent.Line, fmt.Sprintf("entity %q is @softdelete, so `archived` is reserved — rename your field", ent.Name)}
			}
			ei.Fields = append(ei.Fields, Field{Name: "archived", Type: "bool", Index: true})
			e.entityFields[ent.Name]["archived"] = true
			e.entFieldType[ent.Name]["archived"] = "bool"
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
		// A state cell and the render scope share one namespace, so a cell named
		// `route` would be silently overwritten by the path every time a page
		// rendered — the cell would read correctly on the client and wrongly on the
		// server. A local (a `for` variable, a component parameter) named `route`
		// is fine: it shadows, in both renderers, the way any local does.
		if s.Name == "route" {
			return nil, &BuildError{s.Line, "state \"route\" collides with the built-in `route` (the path being rendered) — rename the cell"}
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
		// A default is evaluated before any page exists, so it interpolates against
		// nothing at all.
		if err := e.checkLiteralExpr(s.Default, nil, s.Line); err != nil {
			return nil, err
		}
		e.states[s.Name] = p
		e.stateTypes[s.Name] = core
		if s.List {
			e.stateList[s.Name] = true
		}
		out.States = append(out.States, State{Name: s.Name, Type: s.Type, Elem: s.Elem, List: s.List, Optional: s.Optional, Placement: p, Private: private, Init: e.low(s.Default)})
	}

	// Built-in theme switch. When the app declares any alternate palette (`theme
	// dark:` or a named `theme <name>:`), inject a `@client` text state named
	// `theme` holding the active palette name (""=base/OS). An action assigning it
	// (`theme = "dark"`) is therefore client-placed — the browser flips the
	// `data-theme` attribute with no round-trip — and a view may read `{theme}`. If
	// the author declared their own `theme` state, theirs stands.
	if _, declared := e.states["theme"]; !declared && (len(app.DarkTheme) > 0 || len(app.Themes) > 0) {
		e.states["theme"] = Client
		e.stateTypes["theme"] = "text"
		out.States = append(out.States, State{Name: "theme", Type: "text", Placement: Client, Init: e.low(ast.Lit{Kind: "text", Val: ""})})
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
			e.inlineType[p.Name] = vtype{core: "bool"} // a policy is a predicate
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
		e.inlineType[d.Name] = vtype{core: d.Type}
		out.Derives = append(out.Derives, Derive{Name: d.Name, Type: d.Type, Expr: lowered, Deps: sortedKeys(e.depsIR(lowered))})
	}

	// 3c. Theme: each `name "value"` becomes a CSS custom property (--fa-<name>).
	// Carried on the IR so first paint and the client both apply one style source.
	if len(app.Theme) > 0 {
		out.Theme = map[string]string{}
		for _, tv := range app.Theme {
			if err := e.checkThemeVar(tv); err != nil {
				return nil, err
			}
			out.Theme[tv.Name] = tv.Value
		}
	}
	// `theme dark:` overrides the same tokens under prefers-color-scheme: dark.
	if len(app.DarkTheme) > 0 {
		out.ThemeDark = map[string]string{}
		for _, tv := range app.DarkTheme {
			if err := e.checkThemeVar(tv); err != nil {
				return nil, err
			}
			out.ThemeDark[tv.Name] = tv.Value
		}
	}
	// `theme <name>:` blocks are alternate palettes, each emitted under a
	// `[data-theme="<name>"]` selector. The app switches between them at runtime by
	// assigning the built-in `theme` state, injected below.
	if len(app.Themes) > 0 {
		out.Themes = map[string]map[string]string{}
		for _, nt := range app.Themes {
			tokens, ok := out.Themes[nt.Name]
			if !ok {
				tokens = map[string]string{}
				out.Themes[nt.Name] = tokens
			}
			for _, tv := range nt.Vars {
				if err := e.checkThemeVar(tv); err != nil {
					return nil, err
				}
				tokens[tv.Name] = tv.Value
			}
		}
	}
	// Raw author stylesheet (`css:` blocks). Emitted verbatim into the page after the
	// built-in and theme CSS, so author rules win on equal specificity — the escape
	// hatch for layout the token system can't express (pinned rails, breakpoints).
	out.CSS = app.CSS

	// Pre-register action names so a component body's pending()/failed() resolves an
	// action declared later in source order — components are lowered (3d) before the
	// action pass (4) that normally registers these. Full validation still runs in 4;
	// this only makes the names visible early. Critical for shared component facets
	// (e.g. a ComposeBox whose `pending(post)` names the host's action).
	for _, a := range app.Actions {
		e.actionSet[a.Name] = true
	}
	if app.Auth {
		for _, a := range authActions() {
			e.actionSet[a.Name] = true
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
			// A cell parameter names a state cell, so its declared type is the cell's
			// type and must be one the language has.
			if p.Ref == ast.RefCell && !isPrimitive(p.Type) {
				if _, isEnum := e.enums[p.Type]; !isEnum {
					return nil, &BuildError{cm.Line, fmt.Sprintf("component %q parameter %q is `cell %s`, but %q is not a state type (a primitive or an enum)", cm.Name, p.Name, p.Type, p.Type)}
				}
			}
		}
		// A component has at most one `slot`: its children are one tree, and two
		// slots would silently render them twice.
		if n := countSlots(cm.Root); n > 1 {
			return nil, &BuildError{cm.Line, fmt.Sprintf("component %q has %d `slot`s; a component has at most one, since its children are one tree", cm.Name, n)}
		} else if n == 1 {
			e.compSlot[cm.Name] = true
		}
		e.components[cm.Name] = cm.Params
		e.compAST[cm.Name] = cm
	}
	var compCalls []call
	var compLinks []linkRef
	for _, cm := range app.Components {
		// A template — one with a reference parameter or a `slot` — is not lowered
		// here at all: it has no meaning until a call site says which cell, which
		// action, which children. It is expanded per `use` instead. It is still
		// checked once, against synthetic references, so an unused one is not
		// unexamined.
		if e.isTemplate(cm.Name) {
			if err := e.checkTemplate(cm); err != nil {
				return nil, err
			}
			continue
		}
		locals := map[string]bool{}
		for _, p := range cm.Params {
			locals[p.Name] = true
		}
		// inRegion: a component body renders inline at the call site, so it carries no
		// page-local *binding* ids of its own. It does still mint region and input
		// ids, and those are addresses on whatever page renders it — so they are
		// namespaced per component (they used to duplicate the page's own `b0`/`l0`)
		// and the edges that reach them are folded into each page that uses it.
		cvc := &viewCtx{e: e, pfx: fmt.Sprintf("k%d.", len(e.compRegions)), origin: fmt.Sprintf("component %q", cm.Name)}
		nodes, err := cvc.nodes(cm.Root, scope{locals: locals, inRegion: true, varTypes: e.paramTypes(cm.Params)})
		if err != nil {
			return nil, err
		}
		e.compRegions[cm.Name] = cvc.deps
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
		if err := e.checkLiteral(sv.URL, nil, sv.Line, fmt.Sprintf("service %q's base URL", sv.Name),
			"a service's base URL is fixed at compile time — it is where the authority connects, not something a render decides; put the varying part in the operation's arguments"); err != nil {
			return nil, err
		}
		irsv := Service{Name: sv.Name, URL: sv.URL}
		for _, op := range sv.Ops {
			if _, dup := ops[op.Name]; dup {
				return nil, &BuildError{op.Line, fmt.Sprintf("service %q declares operation %q twice", sv.Name, op.Name)}
			}
			// A declared return type must resolve: a primitive, an enum, or a record
			// (the structured-reply case). A bare capitalized name the parser accepted
			// is only valid here if it names a real record/enum.
			if op.Ret != "" && !isPrimitive(op.Ret) {
				_, isEnum := e.enums[op.Ret]
				_, isRec := e.records[op.Ret]
				if !isEnum && !isRec {
					return nil, &BuildError{op.Line, fmt.Sprintf("%s.%s returns unknown type %q — declare a `record %s: …` for a structured reply, or use a primitive/enum", sv.Name, op.Name, op.Ret, op.Ret)}
				}
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
		e.actionSet[a.Name] = true // for pending()/failed() target validation in views
	}
	// Built-in auth adds its own actions (login/logout/signup/…); register their
	// names too so a view may read pending()/failed() on them.
	if app.Auth {
		for _, a := range authActions() {
			e.actionSet[a.Name] = true
		}
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
		if err := e.checkLiteral(wh.Path, nil, wh.Line, fmt.Sprintf("webhook path %q", wh.Path),
			"a webhook path is a fixed endpoint an external system POSTs to, so it is literal — read the varying part out of the body in the action it targets"); err != nil {
			return nil, err
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

	// Field-level authz: a `@requires(policy)` gate names a zero-argument policy the
	// API evaluates per actor. Validate the policy exists and is parameterless (a
	// row-level policy would need an argument the projection layer can't supply).
	for ei := range out.Entities {
		for _, f := range out.Entities[ei].Fields {
			if f.ReadPolicy == "" {
				continue
			}
			params, ok := e.policyParams[f.ReadPolicy]
			if !ok {
				return nil, &BuildError{0, fmt.Sprintf("field %s.%s @requires unknown policy %q", out.Entities[ei].Name, f.Name, f.ReadPolicy)}
			}
			if len(params) != 0 {
				return nil, &BuildError{0, fmt.Sprintf("field %s.%s @requires row-level policy %q; a field gate must be a zero-argument policy", out.Entities[ei].Name, f.Name, f.ReadPolicy)}
			}
		}
	}

	pathOf := map[string]string{} // path -> view name
	allCalls := compCalls
	allLinks := compLinks
	for i, v := range app.Views {
		// Route parameters (`/post/:id`) are in scope as text locals, bound from the
		// matched URL at render time.
		locals := map[string]bool{}
		rparams := map[string]vtype{}
		for _, p := range v.Params {
			locals[p] = true
			rparams[p] = vtype{core: "text"} // a matched URL segment is text
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
			// ast.SpliceLayout is the same call the parser makes to validate a
			// layout, so the tree spliced here is by construction the one the
			// parser accepted — the splicer cannot reach a slot the check did not
			// see, or refuse one it did.
			spliced, err := ast.SpliceLayout(v.Layout, ly.Root, v.Root)
			if err != nil {
				return nil, &BuildError{v.Line, err.Error()}
			}
			root = spliced
		}
		pvc := &viewCtx{e: e, origin: fmt.Sprintf("view %q", v.Name)}
		nodes, err := pvc.nodes(root, scope{locals: locals, varTypes: rparams})
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
		// A route is declared, not rendered: its dynamic segments are written `:name`
		// and are what *binds* the scope, so a `{…}` here is a segment that will only
		// ever match itself.
		if err := e.checkLiteral(path, locals, v.Line, fmt.Sprintf("view %q's route", v.Name),
			"a route's dynamic segment is written `:name` — `view "+v.Name+" at \"/post/:id\"` — and binds `id` into the page's scope, which is where `{id}` then interpolates"); err != nil {
			return nil, err
		}
		if prev, ok := pathOf[path]; ok {
			return nil, &BuildError{v.Line, fmt.Sprintf("views %q and %q both map to route %q", v.Name, prev, path)}
		}
		pathOf[path] = v.Name
		// Page metadata is rendered once, server-side, so lower it as region (Expr)
		// segments — evaluated against the route scope, never a reactive client bind.
		metaScope := scope{locals: locals, inRegion: true, varTypes: rparams}
		title, err := pvc.lowerSegs(v.TitleSegs, metaScope, false)
		if err != nil {
			return nil, err
		}
		desc, err := pvc.lowerSegs(v.DescSegs, metaScope, false)
		if err != nil {
			return nil, err
		}
		page := Page{Name: v.Name, Path: path, Params: v.Params, Requires: v.Requires, Screen: v.Screen, View: nodes, Bindings: pvc.bindings, DepGraph: map[string][]string{}, Title: title, Desc: desc}
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

	// Every component expansion's action references and link destinations are
	// validated with the rest: an expansion is ordinary view content, and the
	// point of resolving its names at the call site is that the same checks apply.
	allCalls = append(allCalls, e.specialCalls...)
	allLinks = append(allLinks, e.specialLinks...)
	out.Components = append(out.Components, e.special...)

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
	for _, ref := range allLinks {
		matched := false
		for i := range out.Pages {
			if routeMatches(out.Pages[i].Path, ref.path) {
				matched = true
				break
			}
		}
		if !matched {
			// The shape carries a NUL sentinel where an interpolation was, which
			// is unreadable and unquotable; show the `{…}` the author wrote.
			where := ""
			if ref.origin != "" {
				where = " in " + ref.origin
			}
			// A layered build has no top-level `view` to add, so the bare message
			// would name a declaration its author cannot write. Name where a route
			// comes from in the track actually being compiled.
			hint := ""
			if app.Composed {
				hint = " — declare it as a `view` on the `ui`/`data` facet that owns the route, or `mount` a wireframe at it"
			}
			return nil, &BuildError{0, fmt.Sprintf("link to %q%s, but no view serves that route%s",
				strings.ReplaceAll(ref.path, dynamicSegment, "{…}"), where, hint)}
		}
	}

	// validate every `#anchor` destination names an anchor some node declares.
	//
	// This is the fragment half of the guarantee the route check gives the path
	// half, and it exists for the same reason: a link to a place that is not there
	// fails silently. A mistyped route is a page that does not exist and at least
	// 404s; a mistyped fragment is a link that loads the right page and then simply
	// does not move, which is indistinguishable from a working table of contents
	// until someone notices the page never scrolled.
	//
	// Anchors are collected from every page AND every component, whether or not the
	// component is used, because a component is a definition and the check is over
	// what the program declares — the same standard the route check applies. An
	// external destination's fragment belongs to somebody else's document and is
	// not checked.
	anchors := map[string]bool{}
	declared := func(n Node) {
		if n.Anchor != "" {
			anchors[n.Anchor] = true
		}
	}
	for i := range out.Pages {
		walkNodes(out.Pages[i].View, declared)
	}
	for i := range out.Components {
		walkNodes(out.Components[i].View, declared)
	}

	missing := ""
	referenced := func(n Node) {
		if n.Kind != "link" || n.External || missing != "" {
			return
		}
		shape := n.Path
		if len(n.PathSegs) > 0 {
			shape = linkShape(n.PathSegs)
		}
		if _, frag := splitFragment(shape); frag != "" && !anchors[frag] {
			missing = frag
		}
	}
	for i := range out.Pages {
		walkNodes(out.Pages[i].View, referenced)
	}
	for i := range out.Components {
		walkNodes(out.Components[i].View, referenced)
	}
	if missing != "" {
		return nil, &BuildError{0, fmt.Sprintf(
			"link to %q, but no node declares that anchor: write `anchor %q` on the node it should scroll to",
			"#"+missing, missing)}
	}

	// Stamp the index flags the compiler accumulated (relations + every filtered or
	// ordered field) onto the entity fields, so the store knows what to index.
	for ei := range out.Entities {
		queried := e.queriedFields[out.Entities[ei].Name]
		idx := e.indexFields[out.Entities[ei].Name]
		for fi := range out.Entities[ei].Fields {
			f := &out.Entities[ei].Fields[fi]
			// An encrypted column stores ciphertext, so it cannot be filtered,
			// ordered, or indexed — only read back into memory. Any read at all
			// is the error, which is why this asks the wider set.
			if queried[f.Name] && f.Secret {
				return nil, &BuildError{0, fmt.Sprintf(
					"field %q is @secret and cannot be used in a `where`, `by`, or relation; it is encrypted at rest", f.Name)}
			}
			if idx[f.Name] {
				f.Index = true
			}
		}
	}
	return out, nil
}

// itemFields returns the names of the loop item's fields a lowered predicate
// reads — every `get` whose object is the item variable (e.g. `p.likes` in a
// `where p.likes > 0`), at any depth and inside any call.
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

// comparedItemFields returns the loop item's fields the predicate uses in a way
// an ordered index can serve: as a direct operand of a comparison.
//
// The distinction against itemFields is the whole point. `t.author == q` names a
// value the store can seek to. `contains(lower(t.body), q)` also *reads*
// `t.body`, but no ordered index answers a substring search — the store would
// have to decode and test every row either way, so the index is pure write cost.
// FacetQL makes that cost concrete: an index key is bounded, so an index over a
// free-text field is refused the moment one row exceeds the bound, and since
// indexes are reconciled at startup, the app stops booting. Marking only what an
// index can serve is what keeps a large ordinary value from being fatal.
//
// The walk descends through the boolean connectives (`&&`, `||`, `!`) because a
// comparison under any of them is still a comparison the store can seek on — an
// index here is a candidate access path, not a promise that the planner will
// narrow to it. It does not descend into call arguments, which is exactly the
// case above: passing a field to a function makes its value an input to
// arbitrary computation, not a key to look up.
func comparedItemFields(le *Expr, itemVar string) map[string]bool {
	out := map[string]bool{}

	isItemField := func(x *Expr) string {
		if x != nil && x.Kind == "get" && x.Obj != nil &&
			x.Obj.Kind == "ref" && x.Obj.Name == itemVar {
			return x.Field
		}
		return ""
	}

	var walk func(*Expr)
	walk = func(x *Expr) {
		if x == nil {
			return
		}

		switch x.Kind {
		case "bin":
			switch x.Op {
			case "&&", "||":
				walk(x.L)
				walk(x.R)
			case "==", "!=", "<", "<=", ">", ">=":
				if f := isItemField(x.L); f != "" {
					out[f] = true
				}
				if f := isItemField(x.R); f != "" {
					out[f] = true
				}
			}
		case "un":
			if x.Op == "!" {
				walk(x.X)
			}
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

// walkNodes visits every node of a view tree — each node, then its children.
func walkNodes(nodes []Node, fn func(Node)) {
	for i := range nodes {
		fn(nodes[i])
		walkNodes(nodes[i].Children, fn)
	}
}

// dynamicSegment stands for an interpolated path segment during route checking.
// A byte no URL path can contain, so it cannot be confused with a literal.
const dynamicSegment = "\x00"

// interpolated reports whether a segment renders something other than its own
// literal text.
//
// A segment is dynamic in two ways, not one: an `Expr` (an expression over the
// surrounding scope) or a `Bind` (a top-level state cell, which the client
// re-renders in place when the cell changes). Checking only `Expr` treated
// `{actor}` as empty literal text, so `/profile/{actor}` reduced to `/profile/`
// and failed route validation with a message about a route nobody wrote.
func interpolated(s Seg) bool { return s.Expr != nil || s.Bind != "" }

// literalSegs returns the concatenated text when every segment is a literal.
func literalSegs(segs []Seg) (string, bool) {
	var b strings.Builder

	for _, s := range segs {
		if interpolated(s) {
			return "", false
		}
		b.WriteString(s.Lit)
	}

	return b.String(), true
}

// isRouteExpr reports whether a destination is a single interpolation and
// nothing else — the whole route supplied as a value.
//
// "Nothing else" is strict on purpose. A destination with literal text around an
// interpolation is a path template with a hole in it, and a template's holes are
// escaped as data; treating `"{base}/edit"` as a route would silently stop
// escaping the value in the first half. So exactly one dynamic segment, no
// literal text, is the route form, and everything else is a template.
func isRouteExpr(segs []Seg) bool {
	seen := false
	for _, s := range segs {
		if !interpolated(s) {
			if s.Lit != "" {
				return false
			}
			continue
		}
		if seen {
			return false
		}
		seen = true
	}
	return seen
}

// renderShape renders a destination back the way the author roughly wrote it,
// for an error message — `{…}` where an interpolation stands.
func renderShape(segs []Seg) string {
	var b strings.Builder
	for _, s := range segs {
		if interpolated(s) {
			b.WriteString("{…}")
			continue
		}
		b.WriteString(s.Lit)
	}
	return b.String()
}

// linkShape renders a link's destination for route checking, with every
// interpolated run collapsed to one wildcard token.
//
// Checking the shape rather than the text is what keeps the check meaningful
// once destinations can interpolate. Before this, `link "open" -> "/post/{p.id}"`
// was *accepted* — the literal string `{p.id}` matched the `:id` slot as "any
// non-empty segment" — and then shipped verbatim as an href pointing at a page
// that does not exist. It validated precisely because it was broken.
func linkShape(segs []Seg) string {
	var b strings.Builder

	for _, s := range segs {
		if interpolated(s) {
			b.WriteString(dynamicSegment)
			continue
		}
		b.WriteString(s.Lit)
	}

	return b.String()
}

// externalSchemes is the closed set of URI schemes a link destination may name.
//
// It is an allowlist, not a denylist, and that is the entire security argument.
// A denylist of `javascript:` and `data:` is a list of the payloads someone
// already thought of — `vbscript:`, `jar:`, a scheme invented next year, or the
// same word spelled with a stray case or an embedded newline all walk past it.
// Naming the three that navigate somewhere means everything else, known or not,
// is a build failure.
//
// `https` and `http` are the web; `mailto` is here because a contact link is the
// other destination a site actually needs and it cannot be spelled as a path.
var externalSchemes = map[string]bool{"https": true, "http": true, "mailto": true}

// destScheme returns the URI scheme a destination shape begins with, lowercased,
// and whether it has one at all.
//
// RFC 3986 spells a scheme `ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ) ":"` and
// nothing else is one: `/docs`, `#top` and `a/b` have no scheme; `https://x`,
// `mailto:a@b` and `javascript:alert(1)` do. Recognising a scheme the language
// does not accept is the point — it is what turns `javascript:` into an error
// that says so rather than the generic "must start with `/`".
func destScheme(shape string) (string, bool) {
	for i := 0; i < len(shape); i++ {
		c := shape[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			continue
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
			continue
		case c == ':' && i > 0:
			return strings.ToLower(shape[:i]), true
		}
		return "", false
	}
	return "", false
}

// checkExternalDest reports why an external destination is not one the compiler
// will accept, or nil.
//
// The rule it enforces is that the ORIGIN is the author's and only the author's.
// An interpolation may fill part of the path, the query or the fragment — those
// are percent-escaped like any other template hole, so a value cannot climb out
// of the segment it lands in — but the scheme and the authority must be literal
// text in the source. Without that rule `https://{host}/x` would let a row in a
// database choose where the reader's browser goes, which is the same hole the
// route-expression form exists to keep shut, reopened one level up.
func checkExternalDest(scheme, shape string, segs []Seg) *BuildError {
	if scheme == "mailto" {
		if strings.Contains(shape, dynamicSegment) {
			return &BuildError{0, fmt.Sprintf(
				"link path %q interpolates a `mailto:` address: an address is not a path, so there is no segment for a value to be escaped into — write the address literally",
				renderShape(segs))}
		}
		if strings.TrimSpace(shape[len("mailto:"):]) == "" {
			return &BuildError{0, "link path \"mailto:\" has no address"}
		}
		return nil
	}

	rest, ok := strings.CutPrefix(shape, scheme+"://")
	if !ok {
		return &BuildError{0, fmt.Sprintf(
			"link path %q is missing `//` after the scheme: an %s destination is %s://host/path",
			renderShape(segs), scheme, scheme)}
	}

	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if authority == "" {
		return &BuildError{0, fmt.Sprintf("link path %q has no host", renderShape(segs))}
	}
	if strings.Contains(authority, dynamicSegment) {
		return &BuildError{0, fmt.Sprintf(
			"link path %q interpolates its host: an external destination's scheme and host must be literal text the author wrote, so a value can never decide where a reader is sent — interpolate the path instead (e.g. \"%s://%s/{id}\")",
			renderShape(segs), scheme, strings.ReplaceAll(authority, dynamicSegment, "host"))}
	}
	return nil
}

// splitFragment splits a destination into its path and its `#fragment`.
//
// A fragment is a position inside a page, not part of the route, so it comes off
// before the route check — `/docs#install` is a link to `/docs`, and asking the
// route table about `docs#install` finds nothing and reports a route nobody wrote.
func splitFragment(shape string) (path, frag string) {
	if i := strings.IndexByte(shape, '#'); i >= 0 {
		return shape[:i], shape[i+1:]
	}
	return shape, ""
}

// validAnchorName reports whether s is usable as an author-chosen anchor id.
//
// Restricted to the characters an id can carry through a URL fragment, an HTML
// `id` attribute and a CSS selector without any of the three needing to escape
// it. That keeps one spelling of an anchor everywhere it appears, which is what
// makes `#install` in a link and `anchor "install"` on a heading provably the
// same name rather than two strings that usually agree.
func validAnchorName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// routeMatches reports whether a concrete path satisfies a route pattern, where a
// `:param` segment matches any single non-empty segment.
//
// A path segment containing an interpolation matches any pattern segment: its
// value is not known until render, so structure is all that can be checked here.
func routeMatches(pattern, path string) bool {
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	cs := strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) != len(cs) {
		return false
	}
	for i := range ps {
		// An interpolated segment could render as anything, so it satisfies
		// either a parameter slot or a literal one.
		if strings.Contains(cs[i], dynamicSegment) {
			continue
		}
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
	// Record-typed locals (a `let v = call …` whose op returns a record) live for the
	// span of this action build, so a later `v.field` resolves against the record.
	e.locRecords = map[string]recBind{}
	defer func() { e.locRecords = nil }()
	sealParams := map[string]bool{} // params whose value flows into an @e2e field (the client seals them before sending)
	paramSet := map[string]bool{}   // this action's parameter names
	loc := map[string]bool{"actor": true, "role": true, "verified": true, "tenant": true, "tenantRole": true}
	for _, p := range a.Params {
		act.Params = append(act.Params, Param{Name: p.Name, Type: p.Type})
		loc[p.Name] = true
		paramSet[p.Name] = true
	}
	// e2eWrite enforces the sealed-field dataflow at a `field: value` write: an @e2e
	// field can only be written from a bare action parameter, because that is the
	// value the client seals (encrypts) before the request is sent. Anything else
	// would be an expression the authority computes and therefore sees in plaintext,
	// breaking the end-to-end guarantee. It returns whether the field is @e2e.
	e2eWrite := func(entity, field string, val ast.Expr, line int) (bool, error) {
		if !e.entE2E[entity][field] {
			return false, nil
		}
		r, ok := val.(ast.Ref)
		if !ok {
			return true, &BuildError{line, fmt.Sprintf("@e2e field %s.%s must be written straight from an action parameter (the value the client seals); an expression here would be computed by the authority in plaintext", entity, field)}
		}
		if !paramSet[r.Name] {
			return true, &BuildError{line, fmt.Sprintf("@e2e field %s.%s must be written from an action parameter; %q is not one (only a parameter can be sealed on the client before sending)", entity, field, r.Name)}
		}
		sealParams[r.Name] = true
		return true, nil
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

	// readExprIn validates an expression against a named scope and records what it
	// reads: the state cells (for placement soundness) and whether it reached an
	// effectful builtin. Every value an action evaluates goes through it, so the
	// bookkeeping cannot be forgotten at one statement and remembered at another.
	// `locals` is the action's own scope, widened by whatever the statement binds —
	// a filtered write binds its item variable, and nothing else does.
	readExprIn := func(ex ast.Expr, locals map[string]bool, line int) error {
		if err := e.check(ex, locals, line); err != nil {
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
	readExpr := func(ex ast.Expr, line int) error { return readExprIn(ex, loc, line) }

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
			if err := e.checkLiteral(st.Msg, loc, st.Line, "a check message",
				"a check message is literal text the authority sends back when the guard fails — it has no scope to interpolate against, because it is written before the values it would read are known to be valid; "+
					"state the rule instead of the value"); err != nil {
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
				isE2E, err := e2eWrite(st.Entity, fi.Name, fi.Expr, st.Line)
				if err != nil {
					return Action{}, err
				}
				// A sealed value is opaque ciphertext to the authority: don't read it (it
				// is a validated parameter), just carry the ref so the row stores it.
				if !isE2E {
					if err := readExpr(fi.Expr, st.Line); err != nil {
						return Action{}, err
					}
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
			if st.Where != nil {
				// Filtered update: a pure predicate over the item var + action scope,
				// and a block of assignments evaluated against the row it matched. The
				// same shape as `remove … where`, which is why it reuses `Op: "set"`
				// with Where set rather than becoming an opcode of its own — one
				// statement, two addressing modes, exactly as remove has.
				wl := map[string]bool{st.Var: true}
				for k := range loc {
					wl[k] = true
				}
				if err := e.checkPure(st.Where, wl, st.Line, "a `set … where` filter"); err != nil {
					return Action{}, err
				}
				lw := e.low(st.Where)
				for n := range e.depsIR(lw) {
					if _, isState := e.states[n]; isState {
						reads[n] = true
					}
				}
				out := Stmt{Op: "set", Entity: st.Entity, Var: st.Var, Where: lw}

				for _, fi := range st.Fields {
					if !e.entityFields[st.Entity][fi.Name] {
						return Action{}, &BuildError{st.Line, fmt.Sprintf(
							"entity %q has no field %q to set", st.Entity, fi.Name)}
					}
					if e.entE2E[st.Entity][fi.Name] {
						// An @e2e field is sealed on the client from one action parameter, so
						// it has exactly one writable shape and a bulk update is not it: the
						// same ciphertext across every matching row is not the same value.
						return Action{}, &BuildError{st.Line, fmt.Sprintf(
							"@e2e field %s.%s cannot be written by a filtered set — a sealed value is encrypted per row on the client, so it can only be written straight from an action parameter to one row", st.Entity, fi.Name)}
					}
					// The assignment itself is an ordinary action value: it may read the
					// row, the action's parameters and the clock, exactly as the by-id
					// `set` may. Only the *predicate* has to be pure — it is what decides
					// which rows are touched, and a store has to be able to agree.
					if err := readExprIn(fi.Expr, wl, st.Line); err != nil {
						return Action{}, err
					}
					out.Fields = append(out.Fields, FieldInit{Name: fi.Name, Expr: e.low(fi.Expr)})
				}
				act.Body = append(act.Body, out)
				break
			}
			if err := readExpr(st.Key, st.Line); err != nil {
				return Action{}, err
			}
			isE2E, err := e2eWrite(st.Entity, st.Field, st.Value, st.Line)
			if err != nil {
				return Action{}, err
			}
			if !isE2E {
				if err := readExpr(st.Value, st.Line); err != nil {
					return Action{}, err
				}
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
				// If the op returns a record, remember the bind's record type so a later
				// `v.field` is checked (and a list-of-record bind reports that you must
				// iterate it before a field access).
				if _, isRec := e.records[ret.ret]; isRec {
					e.locRecords[st.Bind] = recBind{rec: ret.ret, list: ret.list}
				}
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

	// @e2e dataflow guarantee: a parameter that seals into an @e2e field is
	// ciphertext to the authority. It may therefore appear ONLY as that sealed
	// write — never in a check, policy argument, other field, or service call, all
	// of which the server evaluates and would only see ciphertext. Walk the body
	// once more and reject any other use, then publish the seal set so the client
	// knows which arguments to encrypt before POSTing.
	if len(sealParams) > 0 {
		used := map[string]bool{}
		collect := func(ex ast.Expr) {
			for n := range freeNames(ex) {
				used[n] = true
			}
		}
		for _, s := range a.Body {
			switch st := s.(type) {
			case ast.Check:
				collect(st.Cond)
			case ast.Assign:
				collect(st.Value)
			case ast.Add:
				for _, fi := range st.Fields {
					if e.entE2E[st.Entity][fi.Name] {
						continue // the sealed write itself is allowed
					}
					collect(fi.Expr)
				}
			case ast.Set:
				collect(st.Key)
				if !e.entE2E[st.Entity][st.Field] {
					collect(st.Value)
				}
				collect(st.Where)
				for _, fi := range st.Fields {
					collect(fi.Expr)
				}
			case ast.Remove:
				collect(st.Key)
				collect(st.Where)
			case ast.ServiceCall:
				for _, arg := range st.Args {
					collect(arg)
				}
			case ast.Establish:
				collect(st.Actor)
				collect(st.Role)
			}
		}
		for _, r := range a.Requires {
			for _, arg := range r.Args {
				collect(arg)
			}
		}
		for p := range sealParams {
			if used[p] {
				return Action{}, &BuildError{a.Line, fmt.Sprintf("parameter %q seals into an @e2e field, so the authority only ever holds its ciphertext — it cannot also be read elsewhere in %q (a check, policy, or another write all run on the server). Validate it on the client or model the readable part as a separate parameter", p, a.Name)}
			}
		}
		act.Seal = sortedKeys(sealParams)
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

// scope tracks the locals in effect (route parameters, component parameters, and
// the item variables of enclosing `for`s and repeating options), what each one's
// type is, and whether we are inside a dynamic region (a for/if), where
// interpolations are rendered inline rather than tracked as top-level bindings.
//
// locals and varTypes answer two different questions and both are needed:
// `locals` is "does this name resolve here?", which every name must, and
// `varTypes` is "to what type?", which only the locals whose type the builder can
// prove carry an entry for. A local with no entry is a name that resolves to a
// value of an unknown type — accepted everywhere, like any unknown.
type scope struct {
	locals   map[string]bool
	inRegion bool
	varTypes map[string]vtype // local -> its type (an entity core means a row of that entity)
}

func (s scope) with(v string) scope {
	m := map[string]bool{}
	for k := range s.locals {
		m[k] = true
	}
	m[v] = true
	vt := map[string]vtype{}
	for k, val := range s.varTypes {
		vt[k] = val
	}
	// The new binder shadows whatever it is named after until its own type is
	// recorded, so a stale outer entry can never be read for it.
	delete(vt, v)
	return scope{locals: m, inRegion: true, varTypes: vt}
}

// region is the scope a nested body renders in: the same names and the same
// types, inside a dynamic region.
//
// Every construct that lowers a child body needs exactly this, and each of them
// used to build the literal by hand — which is how `if`, `overlay` and `tabs`
// came to drop the type map while `for` and `match` kept it, so `match p.kind`
// resolved its enum in one nesting and not in another, and an argument read off
// a row was typed at the top of a loop and untyped one `if` deeper. Stating it
// once is what keeps them from drifting apart again.
func (s scope) region() scope {
	return scope{locals: s.locals, inRegion: true, varTypes: s.varTypes}
}

// rowEntity reports the entity a local ranges over, when it is a row of one.
// `match p.kind`, an @e2e field read and a data-driven option's value all ask
// this same question of the same map.
func (c *viewCtx) rowEntity(sc scope, name string) (string, bool) {
	t, ok := sc.varTypes[name]
	if !ok || t.list || !c.e.entities[t.core] {
		return "", false
	}
	return t.core, true
}

// bindRange records the type of the item variable a range binds: a row of the
// entity walked, or an element of the `[T]` list state walked.
func (c *viewCtx) bindRange(sc scope, rg ast.Range) scope {
	child := sc.with(rg.Var)
	switch {
	case c.e.entities[rg.Coll]:
		child.varTypes[rg.Var] = vtype{core: rg.Coll}
	case c.e.stateList[rg.Coll]:
		child.varTypes[rg.Var] = vtype{core: c.e.stateTypes[rg.Coll]}
	}
	return child
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
			if ent, ok := c.rowEntity(sc, r.Name); ok {
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

// linkRef is one internal link destination awaiting route validation: the
// destination's *shape* (interpolated runs already collapsed to a wildcard) and
// the declaration it was written in.
type linkRef struct {
	path   string
	origin string
}

type viewCtx struct {
	e        *env
	bindings []Binding
	deps     map[string][]string // dep -> tracked region ids
	calls    []call
	links    []linkRef // link destination routes (validated against real pages)
	// origin names what the author wrote that holds these links — `view "Home"`,
	// `component "PostCard"` — so an unserved route can say which one wanted it.
	// A link is checked after every page is lowered, by which point the tree it
	// came from is gone, so the answer has to be carried rather than recovered.
	origin string
	// pfx namespaces every region/input id this context mints. A page uses none;
	// each component expansion uses its own, because those ids are addresses on
	// the page that renders it and two expansions must not collide.
	pfx string
	// slot holds the already-lowered children a `use` handed this component, and
	// slotOK says a `slot` is legal here at all (a component body or a layout —
	// never a view).
	slot           []Node
	slotOK         bool
	nb, nl, nf, nu int
}

// id mints a namespaced region/binding identifier.
func (c *viewCtx) id(kind string, n int) string { return fmt.Sprintf("%s%s%d", c.pfx, kind, n) }

func (c *viewCtx) addDep(dep, id string) {
	if c.deps == nil {
		c.deps = map[string][]string{}
	}
	c.deps[dep] = append(c.deps[dep], id)
}

// e2eFieldRead reports whether ex is exactly a read of an @e2e (sealed) entity
// field — either `Entity(key).field` or `item.field` for an item ranging over an
// entity. Such a value is ciphertext; it is opened on the client, never here.
func (c *viewCtx) e2eFieldRead(ex ast.Expr, sc scope) bool {
	switch t := ex.(type) {
	case ast.EntityGet:
		return c.e.entE2E[t.Entity][t.Field]
	case ast.Get:
		if r, ok := t.Obj.(ast.Ref); ok {
			if ent, ok := c.rowEntity(sc, r.Name); ok {
				return c.e.entE2E[ent][t.Field]
			}
		}
	}
	return false
}

// containsE2E reports whether any subexpression of ex reads an @e2e field, so a
// sealed value buried inside a larger expression (a concatenation, a comparison)
// can be rejected: it must stand alone to be opened on the client.
func (c *viewCtx) containsE2E(ex ast.Expr, sc scope) bool {
	if c.e2eFieldRead(ex, sc) {
		return true
	}
	switch t := ex.(type) {
	case ast.Get:
		return c.containsE2E(t.Obj, sc)
	case ast.EntityGet:
		return c.containsE2E(t.Key, sc)
	case ast.Bin:
		return c.containsE2E(t.L, sc) || c.containsE2E(t.R, sc)
	case ast.Un:
		return c.containsE2E(t.X, sc)
	case ast.Call:
		for _, a := range t.Args {
			if c.containsE2E(a, sc) {
				return true
			}
		}
	case ast.Agg:
		if t.Where != nil && c.containsE2E(t.Where, sc) {
			return true
		}
		if t.Sel != nil {
			return c.containsE2E(t.Sel, sc)
		}
	}
	return false
}

// lowerSegs lowers interpolated segments shared by text, button labels, and image
// URLs. A pure expression segment renders inline inside a region (a `for` row) or,
// at the top level, becomes a reactive binding the client recomputes on change.
// openable says whether this context can hold an @e2e value: text/badge/richtext
// render it as a client-opened placeholder; an attribute (image/video src), a
// button label, or server-only page metadata cannot open it, so a sealed read
// there is a compile error.
func (c *viewCtx) lowerSegs(segs []ast.Seg, sc scope, openable bool) ([]Seg, error) {
	var out []Seg
	for _, s := range segs {
		if s.Expr == nil {
			out = append(out, Seg{Lit: s.Lit})
			continue
		}
		if err := c.e.checkPure(s.Expr, viewScope(sc.locals), 0, "a view"); err != nil {
			return nil, err
		}
		if err := c.e.checkNoPrivate(s.Expr); err != nil {
			return nil, err
		}
		e2e := c.containsE2E(s.Expr, sc)
		if e2e {
			if !openable {
				return nil, &BuildError{0, "an @e2e (sealed) value can only be rendered as a text or badge node — it is opened on the client and cannot fill an attribute, a button label, richtext, or page metadata"}
			}
			if !c.e2eFieldRead(s.Expr, sc) {
				return nil, &BuildError{0, "an @e2e value must stand alone in its interpolation (e.g. `{dm.body}`) — it can't be combined with other text or expressions, since the whole ciphertext is opened on the client at once"}
			}
		}
		if sc.inRegion {
			out = append(out, Seg{Expr: c.e.low(s.Expr), E2E: e2e})
		} else {
			id := c.id("b", c.nb)
			c.nb++
			le := c.e.low(s.Expr)
			deps := sortedKeys(c.e.depsIR(le))
			c.bindings = append(c.bindings, Binding{ID: id, Expr: le, Deps: deps})
			for _, d := range deps {
				c.addDep(d, id)
			}
			out = append(out, Seg{Bind: id, E2E: e2e})
		}
	}
	return out, nil
}

// dynamicOptions reports whether a choice list holds anything the compiler cannot
// reduce to a fixed value — a computed value, or a `for` over a collection. It is
// the one test that decides which of the two shapes in Node.Options a control
// lowers into, so both callers ask it rather than each deciding for itself.
func dynamicOptions(opts []ast.Option) bool {
	for _, o := range opts {
		if o.Val != nil || o.From != nil {
			return true
		}
	}
	return false
}

// lowerRange lowers the header every repeating construct shares onto the node
// that repeats over it: which collection, the item variable, and the
// where/by/limit clauses that narrow it.
//
// It is one method for the same reason ast.Range is one type. A `for` node and
// the `for` inside a select's or a radio group's choice list are the same query
// — the same iterable collections, the same pure predicate over the item
// variable, the same @secret and index bookkeeping the store depends on — and a
// second copy of these checks is a second place for `by` to quietly stop marking
// an index, or for a filter to stop being checked for purity.
//
// `line` is where to report a failure; a `for` node passes 0 because ast.For does
// not carry one, an option's range passes the option's own line.
func (c *viewCtx) lowerRange(rg ast.Range, node *Node, sc scope, line int) error {
	// A range walks an entity (rows) or a `[T]` list state cell (its elements).
	// A scalar state cell is not iterable.
	if !c.e.entities[rg.Coll] {
		if c.e.states[rg.Coll] == "" {
			return &BuildError{line, fmt.Sprintf("`for` over unknown collection %q (an entity or a list state)", rg.Coll)}
		}
		if !c.e.stateList[rg.Coll] {
			return &BuildError{line, fmt.Sprintf("`for x in %s` needs a list — %q is a scalar state, not a `[T]` collection", rg.Coll, rg.Coll)}
		}
	}
	node.Var, node.Coll, node.Order, node.Desc = rg.Var, rg.Coll, rg.Order, rg.Desc
	// `limit` may be a literal or a pure expr (e.g. a @client page size for
	// load-more / infinite scroll); evaluated per render, not per row.
	if rg.Limit != nil {
		if err := c.e.checkPure(rg.Limit, viewScope(sc.locals), line, "a `limit`"); err != nil {
			return err
		}
		node.Limit = c.e.low(rg.Limit)
	}
	// `where` filter: a pure predicate over the item var + outer scope.
	if rg.Where != nil {
		wlocals := viewScope(sc.locals)
		wlocals[rg.Var] = true
		if err := c.e.checkPure(rg.Where, wlocals, line, "a `where` filter"); err != nil {
			return err
		}
		node.Where = c.e.low(rg.Where)
		// Two different questions about the same predicate. Every field it reads is
		// a field a @secret column may not appear in. Only the fields it *compares*
		// are ones an index can serve.
		if c.e.entities[rg.Coll] {
			for f := range itemFields(node.Where, rg.Var) {
				if c.e.entityFields[rg.Coll][f] {
					c.e.markQueried(rg.Coll, f)
				}
			}
			for f := range comparedItemFields(node.Where, rg.Var) {
				if c.e.entityFields[rg.Coll][f] {
					c.e.markIndex(rg.Coll, f)
				}
			}
		}
	}
	if rg.Order != "" {
		if !c.e.entities[rg.Coll] {
			return &BuildError{line, fmt.Sprintf("ordering `by %s` requires %q to be an entity", rg.Order, rg.Coll)}
		}
		if !c.e.entityFields[rg.Coll][rg.Order] {
			return &BuildError{line, fmt.Sprintf("entity %q has no field %q to order by", rg.Coll, rg.Order)}
		}
		c.e.markIndex(rg.Coll, rg.Order)
	}
	return nil
}

// lowerOptions lowers the choice list of a select or a radio group — the one
// place either of them learns what its choices are.
//
// A choice list is fixed or it is drawn from data, and the difference is what the
// compiler can still prove about the *value* half of a choice.
//
// A fixed list is unchanged, down to the field it lowers into (Node.Options).
// Every option's value is a literal the compiler holds: an enum-typed cell with
// no options still defaults to that enum's members, a value the author writes is
// still one the enum has, and nothing about that path moved.
//
// A list drawn from data cannot have that. `option "{c.name}" -> c.id` is one
// choice per row of a table nobody has inserted into yet, so its identity does
// not exist at compile time and no amount of checking will make it exist. What
// the compiler still proves is everything *around* the identity:
//
//   - the collection is real and iterable, and its where/by/limit are checked
//     exactly as a `for`'s are (lowerRange) — including that `by <field>` names a
//     field the entity has;
//   - a value written as `c.field` names a field the entity actually has. That is
//     the typo check a literal option gets, kept: a misspelled field would
//     otherwise store the empty string in every row, silently;
//   - that field's declared type is the bound cell's type, so a `text` cell is
//     never quietly filled with row ids;
//   - the value expression is pure, reads no @private cell, and is not a sealed
//     (@e2e) field, whose plaintext this side never holds;
//   - the bound cell is `@client`, is not a list, and is text or int — never an
//     enum, because an enum cell's choices ARE its members and a computed value
//     cannot be proven to be one. Making that a compile error is what keeps enum
//     exhaustiveness sound instead of merely usually true.
//
// What genuinely moves to runtime is one thing: whether the value a row supplies
// is a member of anything. The cell holds what the chosen row stored, and a row
// that disappears leaves a cell holding a value no option offers — the same
// position an `input` bound to a text cell has always been in.
func (c *viewCtx) lowerOptions(kw, bind string, opts []ast.Option, sc scope, line int) ([]Option, []Node, error) {
	cell := c.e.stateTypes[bind]
	members, isEnum := c.e.enums[cell]

	if !dynamicOptions(opts) {
		var flat []Option
		for _, o := range opts {
			label, err := c.lowerSegs(o.Label, sc, false)
			if err != nil {
				return nil, nil, err
			}
			if err := c.checkOptionLit(o, sc); err != nil {
				return nil, nil, err
			}
			flat = append(flat, Option{Label: label, Value: o.Value})
		}
		if len(flat) == 0 {
			// An enum cell already names its own choices, so writing them out again
			// is the thing that drifts.
			if !isEnum {
				return nil, nil, &BuildError{line, fmt.Sprintf("%s on %q needs options (or a `@client` enum cell to default them)", kw, bind)}
			}
			for _, m := range members {
				flat = append(flat, Option{Label: []Seg{{Lit: m}}, Value: m})
			}
		}
		return flat, nil, nil
	}

	if isEnum {
		return nil, nil, &BuildError{line, fmt.Sprintf(
			"%s binds %q, whose type is the enum %q — its choices are that enum's members, and a value computed from data cannot be proven to be one of them "+
				"(that proof is what makes a `match` on %q exhaustive). Bind a text or int cell to draw choices from a collection.",
			kw, bind, cell, bind)}
	}
	if cell != "text" && cell != "int" {
		return nil, nil, &BuildError{line, fmt.Sprintf(
			"%s binds %q, which is %s; a choice drawn from data stores whatever its value expression evaluates to, so the cell must be text or int",
			kw, bind, typeLabel(cell, c.e.stateList[bind]))}
	}

	var kids []Node
	for _, o := range opts {
		n := Node{Kind: "option"}
		osc := sc
		if o.From != nil {
			n.Kind = "options"
			if err := c.lowerRange(*o.From, &n, sc, o.Line); err != nil {
				return nil, nil, err
			}
			osc = c.bindRange(sc, *o.From)
		}
		label, err := c.lowerSegs(o.Label, osc, false)
		if err != nil {
			return nil, nil, err
		}
		n.Label = label
		if o.Val == nil {
			if err := c.checkOptionLit(o, osc); err != nil {
				return nil, nil, err
			}
			n.Value = o.Value
		} else {
			if err := c.e.checkPure(o.Val, viewScope(osc.locals), o.Line, "an option value"); err != nil {
				return nil, nil, err
			}
			if err := c.e.checkNoPrivate(o.Val); err != nil {
				return nil, nil, err
			}
			if c.containsE2E(o.Val, osc) {
				return nil, nil, &BuildError{o.Line,
					"an @e2e (sealed) value cannot be an option's value — the authority holds only its ciphertext, so it is not an identity anything can be selected by"}
			}
			if err := c.checkOptionValue(kw, bind, cell, o, osc); err != nil {
				return nil, nil, err
			}
			n.Val = c.e.low(o.Val)
		}
		kids = append(kids, n)
	}
	return nil, kids, nil
}

// checkOptionValue types a computed option value against the cell it is stored
// in, as far as a language with no general expression typing can.
//
// Two shapes cover what an author writes, and they are exactly the two worth
// checking: a row's field, and a literal. Anything else is left to the render's
// own coercion — it was never a compile-time identity to begin with, and
// pretending to check it would be worse than saying so.
func (c *viewCtx) checkOptionValue(kw, bind, cell string, o ast.Option, sc scope) error {
	switch t := o.Val.(type) {
	case ast.Get:
		r, ok := t.Obj.(ast.Ref)
		if !ok {
			return nil
		}
		ent, ok := c.rowEntity(sc, r.Name)
		if !ok {
			return nil // not a row of a known entity: nothing to resolve the field against
		}
		got, known := c.e.entFieldType[ent][t.Field]
		if !known {
			return &BuildError{o.Line, fmt.Sprintf(
				"entity %q has no field %q, so `option ... -> %s.%s` has nothing to store", ent, t.Field, r.Name, t.Field)}
		}
		if got != cell {
			return &BuildError{o.Line, fmt.Sprintf(
				"%s binds %q, which is %s, but the option value `%s.%s` is %s", kw, bind, cell, r.Name, t.Field, got)}
		}
	case ast.Lit:
		if t.Kind != cell {
			return &BuildError{o.Line, fmt.Sprintf(
				"%s binds %q, which is %s, but the option value is %s", kw, bind, cell, t.Kind)}
		}
	}
	return nil
}

func (c *viewCtx) nodes(in []ast.Node, sc scope) ([]Node, error) {
	var out []Node
	for _, n := range in {
		switch t := n.(type) {
		case ast.Modified:
			// Lower the wrapped node, then stamp the author's modifiers onto the
			// single IR node it produced so the rendered element carries both the
			// built-in `fa-*` class and the escape-hatch attributes.
			inner, err := c.nodes([]ast.Node{t.Inner}, sc)
			if err != nil {
				return nil, err
			}
			classSegs, err := c.lowerSegs(t.Class, sc, false)
			if err != nil {
				return nil, err
			}
			// `class` interpolates and `style` does not, on purpose (see ast.Modified),
			// so a `{…}` written in a `style` is a value that never arrives. It used to
			// arrive as five characters of CSS the browser discarded; now it is an
			// error that names the class-per-value it should have been.
			if err := c.e.checkLiteral(t.Style, viewScope(sc.locals), t.Line, "`style`",
				"`style` is a stylesheet fragment and stays literal, because there is no one-line rule for what a value may safely put in one (`class` has such a rule, which is why `class` interpolates) — "+
					"carry the varying part in a class instead: `class \"…-{expr}\"` plus a `css:` rule per value"); err != nil {
				return nil, err
			}
			// A purely literal class keeps the flat `Class` field — the same split
			// `Path`/`PathSegs` makes, and for the same reason.
			classLit, classIsLit := literalSegs(classSegs)
			for i := range inner {
				switch {
				case len(classSegs) == 0:
				case classIsLit:
					inner[i].Class = classLit
				default:
					inner[i].ClassSegs = classSegs
				}
				if t.Style != "" {
					inner[i].Style = t.Style
				}
				if t.Anchor != "" {
					inner[i].Anchor = t.Anchor
				}
			}
			out = append(out, inner...)
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
			segs, err := c.lowerSegs(t.Segs, sc, true)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "text", Segs: segs})

		case ast.Heading:
			// A heading's words are a text leaf's words — same lowering, same
			// bindings, same @e2e allowance. Only the element it lands in differs,
			// and that is what Level carries.
			lvl, err := c.headingLevel(t, sc)
			if err != nil {
				return nil, err
			}
			segs, err := c.lowerSegs(t.Segs, sc, true)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "heading", Level: lvl, Segs: segs})

		case ast.Image:
			segs, err := c.lowerSegs(t.Segs, sc, false)
			if err != nil {
				return nil, err
			}
			alt, err := c.lowerSegs(t.Alt, sc, false)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "image", Segs: segs, Alt: alt})

		case ast.Icon:
			segs, err := c.lowerSegs(t.Segs, sc, false)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "icon", Segs: segs})

		case ast.Video:
			segs, err := c.lowerSegs(t.Segs, sc, false)
			if err != nil {
				return nil, err
			}
			alt, err := c.lowerSegs(t.Alt, sc, false)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "video", Segs: segs, Alt: alt})

		case ast.Richtext:
			segs, err := c.lowerSegs(t.Segs, sc, false)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "richtext", Segs: segs})

		case ast.Badge:
			segs, err := c.lowerSegs(t.Segs, sc, true)
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
				node.ID = c.id("t", c.nf)
				c.nf++
				c.addDep(t.Bind, node.ID)
			}
			for _, tb := range t.Tabs {
				if err := c.e.checkLiteral(tb.Value, viewScope(sc.locals), t.Line, "a tab value",
					"a tab's value is the identity its bound cell takes when that tab is active, so it is a literal the compiler compares — name the constant the cell holds, and put the varying text in the tab's label, which does interpolate"); err != nil {
					return nil, err
				}
				kids, err := c.nodes(tb.Body, sc.region())
				if err != nil {
					return nil, err
				}
				label, err := c.lowerSegs(tb.Label, sc.region(), false)
				if err != nil {
					return nil, err
				}
				node.Children = append(node.Children, Node{Kind: "tab", Label: label, Value: tb.Value, Children: kids})
			}
			// A tab body's reads (its feed list, counts) refresh the whole control too.
			if node.ID != "" {
				for _, d := range sortedKeys(c.e.nodeDeps(node.Children)) {
					c.addDep(d, node.ID)
				}
			}
			out = append(out, node)

		case ast.Match:
			if err := c.e.checkPure(t.Expr, viewScope(sc.locals), t.Line, "a `match`"); err != nil {
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
				if err := c.e.checkLiteral(cs.Value, viewScope(sc.locals), t.Line, "a `case` value",
					"a case value is a compile-time constant compared against the matched value — it is what enum exhaustiveness is proved against, so it can never be computed at render time; "+
						"put the computed value in the `match` head (`match "+concatForm(cs.Value)+":`) and write the constants in the cases, or branch with `if`"); err != nil {
					return nil, err
				}
				if enumName != "" && !enumHas(c.e.enums[enumName], cs.Value) {
					return nil, &BuildError{t.Line, fmt.Sprintf("enum %q has no member %q", enumName, cs.Value)}
				}
				kids, err := c.nodes(cs.Body, sc.region())
				if err != nil {
					return nil, err
				}
				node.Children = append(node.Children, Node{Kind: "case", Value: cs.Value, Children: kids})
			}
			if t.Else != nil {
				kids, err := c.nodes(t.Else, sc.region())
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
				node.ID = c.id("m", c.nf)
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
			segs, err := c.lowerSegs(t.Label, sc, false)
			if err != nil {
				return nil, err
			}
			node := Node{Kind: "button", Segs: segs, Action: t.Action}
			for _, arg := range t.Args {
				if err := c.e.checkPure(arg, viewScope(sc.locals), t.Line, "a view"); err != nil {
					return nil, err
				}
				node.Args = append(node.Args, c.e.low(arg))
			}
			c.calls = append(c.calls, call{name: t.Action, argc: len(t.Args)})
			out = append(out, node)

		case ast.For:
			node := Node{Kind: "list"}
			if err := c.lowerRange(t.Range, &node, sc, 0); err != nil {
				return nil, err
			}
			if !sc.inRegion {
				node.ID = c.id("l", c.nl)
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
			// so `match item.field` can resolve an enum field, and so an argument
			// read off the row can be typed against the parameter it is passed to
			child := c.bindRange(sc, t.Range)
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
			if err := c.e.checkPure(t.Cond, viewScope(sc.locals), 0, "a view"); err != nil {
				return nil, err
			}
			node := Node{Kind: "if", Cond: c.e.low(t.Cond)}
			if !sc.inRegion {
				node.ID = c.id("f", c.nf)
				c.nf++
				for _, d := range sortedKeys(c.e.depsIR(node.Cond)) {
					c.addDep(d, node.ID)
				}
			}
			kids, err := c.nodes(t.Body, sc.region())
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
			id := c.id("b", c.nb)
			c.nb++
			c.addDep(t.Bind, id)
			ph, err := c.lowerSegs(t.Placeholder, sc, false)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "input", Bind: t.Bind, Placeholder: ph, ID: id})

		// Every control in ast.Controls lowers here, once. A control is a cell
		// plus a way to write it, so what the compiler has to establish is the
		// same four things for all of them — the cell exists, it is `@client`
		// (only a control may write one, and only a client cell may be written
		// without asking the authority), it is not a list, and it holds the type
		// this control can actually produce. The differences between a textarea,
		// a checkbox, a toggle and a radio group are rows in the table, not
		// branches here.
		case ast.Control:
			spec := ast.Controls[t.Kind]
			p, ok := c.e.states[t.Bind]
			if !ok {
				return nil, &BuildError{t.Line, fmt.Sprintf("%s binds unknown state %q", t.Kind, t.Bind)}
			}
			if p != Client {
				return nil, &BuildError{t.Line, fmt.Sprintf("%s binds %q, which is authoritative; two-way input requires a @client state", t.Kind, t.Bind)}
			}
			got, isList := c.e.stateTypes[t.Bind], c.e.stateList[t.Bind]
			_, isEnum := c.e.enums[got]
			// A control writes one value, so it cannot be pointed at a list cell —
			// checked before the type rule below, which would otherwise report
			// `[text]` against `text` as if the element type were the problem.
			if isList {
				return nil, &BuildError{t.Line, fmt.Sprintf("%s binds %q, which is %s; a control writes one value, not a list", t.Kind, t.Bind, typeLabel(got, isList))}
			}
			switch {
			case spec.Cell != "" && got != spec.Cell:
				return nil, &BuildError{t.Line, fmt.Sprintf("%s binds %q, which is %s, but %s", t.Kind, t.Bind, typeLabel(got, isList), spec.Rule)}
			case spec.Cell == "" && dynamicOptions(t.Options):
				// A choice drawn from data stores what its rows store, not text the
				// author wrote, so which cells it may be pointed at is lowerOptions'
				// question — it is the half that knows what the values are.
			case spec.Cell == "" && got != "text" && !isEnum:
				// A control whose value set is its options stores whatever an option
				// says it stores, and an option's value is written as text.
				return nil, &BuildError{t.Line, fmt.Sprintf("%s binds %q, which is %s, but %s", t.Kind, t.Bind, typeLabel(got, isList), spec.Rule)}
			}
			node := Node{Kind: spec.IRKind, Bind: t.Bind, Value: spec.Variant}
			label, err := c.lowerSegs(t.Label, sc, false)
			if err != nil {
				return nil, err
			}
			hint, err := c.lowerSegs(t.Placeholder, sc, false)
			if err != nil {
				return nil, err
			}
			node.Label, node.Placeholder = label, hint
			if spec.Options {
				// Same rule, same function, same errors a `select` gets: a radio group
				// and a dropdown are one choice written two ways.
				flat, kids, err := c.lowerOptions(t.Kind, t.Bind, t.Options, sc, t.Line)
				if err != nil {
					return nil, err
				}
				node.Options, node.Children = flat, kids
			}
			node.ID = c.id("b", c.nb)
			c.nb++
			c.addDep(t.Bind, node.ID)
			c.optionDeps(node, node.Children, sc)
			out = append(out, node)

		case ast.Overlay:
			p, ok := c.e.states[t.Bind]
			if !ok {
				return nil, &BuildError{0, fmt.Sprintf("overlay binds unknown state %q", t.Bind)}
			}
			if p != Client {
				return nil, &BuildError{0, fmt.Sprintf("overlay binds %q, which is authoritative; a modal toggles client-side, so it needs a @client state", t.Bind)}
			}
			if c.e.stateTypes[t.Bind] != "bool" {
				return nil, &BuildError{0, fmt.Sprintf("overlay binds %q, which is not a bool; an overlay is shown while a bool cell is true", t.Bind)}
			}
			node := Node{Kind: "overlay", Bind: t.Bind}
			if !sc.inRegion {
				node.ID = c.id("f", c.nf)
				c.nf++
				c.addDep(t.Bind, node.ID)
			}
			kids, err := c.nodes(t.Body, sc.region())
			if err != nil {
				return nil, err
			}
			node.Children = kids
			out = append(out, node)

		case ast.Typeahead:
			p, ok := c.e.states[t.Bind]
			if !ok {
				return nil, &BuildError{0, fmt.Sprintf("typeahead binds unknown state %q", t.Bind)}
			}
			if p != Client {
				return nil, &BuildError{0, fmt.Sprintf("typeahead binds %q, which is authoritative; it needs a @client text state", t.Bind)}
			}
			if c.e.stateTypes[t.Bind] != "text" {
				return nil, &BuildError{0, fmt.Sprintf("typeahead binds %q, which is not text", t.Bind)}
			}
			if !c.e.entities[t.Entity] {
				return nil, &BuildError{0, fmt.Sprintf("typeahead reads unknown entity %q", t.Entity)}
			}
			if !c.e.entityFields[t.Entity][t.Field] {
				return nil, &BuildError{0, fmt.Sprintf("entity %q has no field %q for typeahead", t.Entity, t.Field)}
			}
			id := c.id("b", c.nb)
			c.nb++
			c.addDep(t.Bind, id)
			ph, err := c.lowerSegs(t.Placeholder, sc, false)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "typeahead", Bind: t.Bind, Coll: t.Entity, Value: t.Field, Placeholder: ph, ID: id})

		case ast.Link:
			labelSegs, err := c.lowerSegs(t.LabelSegs, sc, false)
			if err != nil {
				return nil, err
			}

			pathSegs, err := c.lowerSegs(t.PathSegs, sc, false)
			if err != nil {
				return nil, err
			}

			// A destination is one of exactly four things, and which one is decided
			// by what the author wrote — never by what a value turns out to be.
			//
			// A *path template* starts with a literal `/`: the author wrote the
			// route's shape and interpolations fill segments of it. Its shape is
			// known here, so it is checked here, exactly as before — a link to a
			// route no view serves is still a compile error.
			//
			// A *route expression* is one interpolation and nothing else: the
			// destination is computed, and no amount of static analysis can say what
			// it will be. That is the form a `link`/breadcrumb/pagination component
			// needs, and refusing it is what made the whole navigation category
			// unwritable. The compiler checks what it still can (the expression is
			// pure, non-private and renders as text — already done above) and stops;
			// the obligation moves to the renderers, which resolve the value against
			// this app's routes and render nothing navigable when it is not one.
			// A parameterized destination can therefore still only ever reach a page
			// this app serves, which is the guarantee the static check was giving.
			//
			// An *external URL* is an absolute destination the author wrote whole:
			// `https://github.com/F33D3R-Inc/fct`. Its scheme comes from a closed
			// allowlist, and its scheme and host must be literal source text, so the
			// origin is always something a reader can find by reading the program.
			// Interpolation after the host is a path template like any other and is
			// escaped like one. This deliberately does NOT extend to a route
			// expression: a destination that arrives as data still may only name a
			// route of this app, because the day a runtime value can become an
			// arbitrary anchor is the day a `javascript:` payload in a database row
			// becomes a link. The literal/value split is the whole safety property.
			//
			// An *anchor* is `#install` or `/docs#install`: a position within a page,
			// declared with `anchor "install"` on the node it scrolls to. The path
			// half is route-checked exactly as above and the fragment is checked
			// against the anchors the app declares.
			//
			// Anything in between — a leading interpolation with more path after it —
			// is none of them, and is refused rather than guessed at.
			node := Node{Kind: "link"}

			shape := linkShape(pathSegs)
			scheme, hasScheme := destScheme(shape)

			switch {
			case isRouteExpr(pathSegs):
				node.Route = true

			case hasScheme:
				// An ABSOLUTE URL leaves this app. It is accepted only as text the
				// author wrote — see checkExternalDest — and only for a scheme in the
				// allowlist, so `javascript:` and `data:` are a build failure with
				// their own name in the message rather than the generic path error.
				if !externalSchemes[scheme] {
					return nil, &BuildError{0, fmt.Sprintf(
						"link path %q uses the %q scheme, which is not a destination: a link goes to a path of this app (\"/docs\"), an anchor on a page (\"#install\"), or an external https/http/mailto URL",
						renderShape(pathSegs), scheme)}
				}
				if err := checkExternalDest(scheme, shape, pathSegs); err != nil {
					return nil, err
				}
				node.External = true

			case strings.HasPrefix(shape, "//"):
				// `//host/path` is an absolute URL that inherits the current scheme —
				// it leaves the app while looking exactly like a path, and the route
				// check would read `host` as the first segment of a local route.
				return nil, &BuildError{0, fmt.Sprintf(
					"link path %q is protocol-relative, which leaves this app while looking like a path: write the scheme (\"https:%s\") or a single leading `/`",
					shape, shape)}

			case strings.HasPrefix(shape, "#"), strings.HasPrefix(shape, "/"):
				// A path template, optionally ending at an anchor on the page it names.
				// A bare `#install` is an anchor on the page the link is already on, so
				// there is no path to route-check.
				path, frag := splitFragment(shape)
				if strings.Contains(shape, "#") && !validAnchorName(frag) {
					return nil, &BuildError{0, fmt.Sprintf(
						"link path %q names %q, which is not an anchor name: an anchor is literal text of letters, digits, `-` and `_`, written as `anchor \"install\"` on the node it scrolls to and spelled `#install` here",
						renderShape(pathSegs), strings.ReplaceAll(frag, dynamicSegment, "{…}"))}
				}
				if path != "" {
					c.links = append(c.links, linkRef{path: path, origin: c.origin})
				}

			case strings.HasPrefix(shape, dynamicSegment):
				return nil, &BuildError{0, fmt.Sprintf(
					"link path %q starts with an interpolation but is not one: a destination either starts with a literal `/` (a path the compiler can check, e.g. \"/post/{p.id}\") or is a single interpolation supplying the whole route (e.g. \"{href}\")",
					renderShape(pathSegs))}

			default:
				return nil, &BuildError{0, fmt.Sprintf(
					"link path %q must start with `/`", shape)}
			}

			// A label is only ever displayed, so it is segments like every other
			// node's label — there is no literal form for a consumer to resolve.
			node.Label = labelSegs

			// A link whose *destination* is a pure literal keeps the flat `Path`
			// field. That is not just economy on the wire: it is the whole contract
			// for every consumer that predates interpolation, and a static link
			// must keep resolving for them.
			if lit, ok := literalSegs(pathSegs); ok {
				node.Path = lit
			} else {
				node.PathSegs = pathSegs
			}

			out = append(out, node)

		case ast.Select:
			p, ok := c.e.states[t.Bind]
			if !ok {
				return nil, &BuildError{t.Line, fmt.Sprintf("select binds unknown state %q", t.Bind)}
			}
			if p != Client {
				return nil, &BuildError{t.Line, fmt.Sprintf("select binds %q, which is authoritative; two-way input requires a @client state", t.Bind)}
			}
			node := Node{Kind: "select", Bind: t.Bind}
			flat, kids, err := c.lowerOptions("select", t.Bind, t.Options, sc, t.Line)
			if err != nil {
				return nil, err
			}
			node.Options, node.Children = flat, kids
			node.ID = c.id("b", c.nb)
			c.nb++
			c.addDep(t.Bind, node.ID)
			c.optionDeps(node, kids, sc)
			out = append(out, node)

		case ast.Form:
			submit, err := c.lowerSegs(t.Submit, sc, false)
			if err != nil {
				return nil, err
			}
			node := Node{Kind: "form", Action: t.Action, Label: submit}
			for _, arg := range t.Args {
				if err := c.e.checkPure(arg, viewScope(sc.locals), t.Line, "a view"); err != nil {
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
			id := c.id("b", c.nb)
			c.nb++
			c.addDep(t.Bind, id)
			label, err := c.lowerSegs(t.Label, sc, false)
			if err != nil {
				return nil, err
			}
			out = append(out, Node{Kind: "upload", Bind: t.Bind, Label: label, ID: id})

		case ast.Use:
			params, ok := c.e.components[t.Name]
			if !ok {
				return nil, &BuildError{t.Line, fmt.Sprintf("use of unknown component %q", t.Name)}
			}
			if len(t.Args) != len(params) {
				return nil, &BuildError{t.Line, fmt.Sprintf("component %q takes %d argument(s), got %d", t.Name, len(params), len(t.Args))}
			}
			// Children. A block under a `use` used to be parsed and then dropped, so
			// a wrapper that quietly rendered nothing looked like a working one; it is
			// a compile error now unless the component has a `slot` to put it in.
			if len(t.Body) > 0 && !c.e.compSlot[t.Name] {
				return nil, &BuildError{t.Line, fmt.Sprintf("component %q takes no children — it has no `slot`. Add a `slot` to %s where the block should render, or remove the block", t.Name, t.Name)}
			}
			// Reference arguments name a declaration; value arguments are evaluated.
			// The two are separated here, because only the values survive into the IR
			// — a reference is substituted into the expansion's body.
			subst := map[string]string{}
			var valArgs []ast.Expr
			for i, p := range params {
				if p.Ref == ast.RefValue {
					// A value argument is an expression, so what is checked is its
					// type — the counterpart of the two checks below, which ask what
					// declaration a reference argument names.
					if err := c.e.checkArgType(t, i, p, sc); err != nil {
						return nil, err
					}
					valArgs = append(valArgs, t.Args[i])
					continue
				}
				name, isName := bareRef(t.Args[i])
				if !isName {
					return nil, &BuildError{t.Line, fmt.Sprintf("component %q parameter %q is a reference — pass the name of a %s, not an expression", t.Name, p.Name, refNoun(p.Ref))}
				}
				switch p.Ref {
				case ast.RefCell:
					if _, declared := c.e.states[name]; !declared {
						return nil, &BuildError{t.Line, fmt.Sprintf("component %q parameter %q needs a state cell; %q is not one", t.Name, p.Name, name)}
					}
					if got := c.e.stateTypes[name]; got != p.Type || c.e.stateList[name] != p.List {
						return nil, &BuildError{t.Line, fmt.Sprintf("component %q parameter %q is `cell %s`, but %q is %s", t.Name, p.Name, typeLabel(p.Type, p.List), name, typeLabel(got, c.e.stateList[name]))}
					}
				case ast.RefAction:
					if !c.e.actionSet[name] {
						return nil, &BuildError{t.Line, fmt.Sprintf("component %q parameter %q needs an action; %q is not one", t.Name, p.Name, name)}
					}
				}
				subst[p.Name] = name
			}
			useName := t.Name
			if c.e.isTemplate(t.Name) {
				// The caller's children are lowered in the caller's scope — they are the
				// caller's nodes, and the row variable they read is the caller's — and
				// spliced at the slot as IR. inRegion: they render inside this `use`.
				var kids []Node
				if len(t.Body) > 0 {
					k, err := c.nodes(t.Body, sc.region())
					if err != nil {
						return nil, err
					}
					kids = k
				}
				name, err := c.e.specialize(c.e.compAST[t.Name], subst, kids, t.Line)
				if err != nil {
					return nil, err
				}
				useName = name
			}
			// A component's own regions and two-way inputs are addressed on the page
			// that renders it, so the edges that reach them belong in this page's
			// graph — the component was lowered elsewhere, but it refreshes here.
			for dep, ids := range c.e.compRegions[useName] {
				for _, id := range ids {
					c.addDep(dep, id)
				}
			}
			node := Node{Kind: "use", Name: useName}
			deps := map[string]bool{}
			// A component argument is an expression, not a template — so it is the
			// position this bug class was found in, and the one that can say the most
			// about the fix: it knows the component, the parameter, and the line.
			if err := c.e.checkUseArgs(t, params, viewScope(sc.locals)); err != nil {
				return nil, err
			}
			for _, arg := range valArgs {
				if err := c.e.checkPure(arg, viewScope(sc.locals), t.Line, "a view"); err != nil {
					return nil, err
				}
				le := c.e.low(arg)
				node.Args = append(node.Args, le)
				for d := range c.e.depsIR(le) {
					deps[d] = true
				}
			}
			for d := range c.e.compDeps[useName] {
				deps[d] = true
			}
			// A top-level `use` is a tracked region: it re-renders whole when any state
			// its arguments or body reads changes. Inside another region it renders
			// inline and refreshes with its parent.
			if !sc.inRegion {
				node.ID = c.id("u", c.nu)
				c.nu++
				for _, d := range sortedKeys(deps) {
					c.addDep(d, node.ID)
				}
			}
			out = append(out, node)

		case ast.Slot:
			if !c.slotOK {
				return nil, &BuildError{0, "`slot` may only appear inside a layout or a component"}
			}
			out = append(out, c.slot...)
		case ast.SlotRef:
			return nil, &BuildError{0, fmt.Sprintf("`slot %s` may only appear inside a wireframe frame", t.Name)}
		}
	}
	return out, nil
}

// ── reference checking + free names ──────────────────────────────────────────

// resolves reports whether name is one the surrounding scope defines: a local, a
// builtin, a state cell, an entity, an enum, or an inlinable policy/derive.
//
// It is stated once because two questions rest on it and they must never give
// different answers: "is this reference valid?" (check, below) and "would these
// braces have rendered a value?" (litInterp, in braces.go). A name that resolves
// is a name an interpolation would have printed.
func (e *env) resolves(name string, locals map[string]bool) bool {
	if locals[name] || isBuiltinRef(name) {
		return true
	}
	if _, ok := e.states[name]; ok {
		return true
	}
	if e.entities[name] {
		return true
	}
	if _, ok := e.enums[name]; ok { // an enum name, as the object of a `.member` access (folded by lower())
		return true
	}
	if _, ok := e.inline[name]; ok { // a policy/derive name used as a value; inlined by lower()
		return true
	}
	return false
}

// check validates that every free name in ex is a known state, entity, local,
// or the builtin `actor`, that every aggregate and builtin call is well-formed,
// and that no text literal inside it is a dropped interpolation (see braces.go —
// this is the funnel every expression in the language passes through with its
// scope, so one call covers every expression position at once).
func (e *env) check(ex ast.Expr, locals map[string]bool, line int) error {
	if err := e.checkBuiltins(ex, line); err != nil {
		return err
	}
	for n := range freeNames(ex) {
		if !e.resolves(n, locals) {
			return &BuildError{line, fmt.Sprintf("unknown reference %q", n)}
		}
	}
	return e.checkLiteralExpr(ex, locals, line)
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
		if isFieldAgg(t.Op) && t.Sel == nil && !e.entityFields[t.Coll][t.Field] {
			// The message names the two shapes, because the most likely way to
			// arrive here is a reduced expression the parser could not attach a
			// row variable to — which leaves Field empty and would otherwise
			// report that the entity has no field "".
			if t.Field == "" {
				return &BuildError{line, fmt.Sprintf(
					"%s needs a field of %s to reduce, or an expression over each row: %s(x.field in %s where …)",
					t.Op, t.Coll, t.Op, t.Coll)}
			}
			return &BuildError{line, fmt.Sprintf("entity %q has no field %q to %s", t.Coll, t.Field, t.Op)}
		}
		if t.Sel != nil {
			if err := e.checkBuiltins(t.Sel, line); err != nil {
				return err
			}
		}
		if t.Op == "exists" && t.Var == "" {
			return &BuildError{line, fmt.Sprintf("exists needs a filtered form: exists(x in %s where <cond>)", t.Coll)}
		}
		if t.Where != nil {
			if err := e.checkBuiltins(t.Where, line); err != nil {
				return err
			}
		}
	case ast.ActState:
		// pending()/failed() name an action; dirty()/touched() name a state cell.
		// Validate the target so a typo is a compile error, not a silent false.
		if t.Op == "pending" || t.Op == "failed" {
			if !e.actionSet[t.Action] {
				return &BuildError{line, fmt.Sprintf("%s(%s) names an unknown action %q", t.Op, t.Action, t.Action)}
			}
		} else { // dirty | touched
			if _, ok := e.states[t.Action]; !ok {
				return &BuildError{line, fmt.Sprintf("%s(%s) names an unknown state cell %q", t.Op, t.Action, t.Action)}
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
			// Record field access on a `let`-bound local (`v.score`): the field must
			// exist on the bound record, and the bind must not be a list (iterate first).
			if rb, isRec := e.locRecords[r.Name]; isRec {
				if rb.list {
					return &BuildError{line, fmt.Sprintf("%q is a list of %s — access a field on one element, not the whole list", r.Name, rb.rec)}
				}
				if _, ok := e.records[rb.rec][t.Field]; !ok {
					return &BuildError{line, fmt.Sprintf("record %s has no field %q", rb.rec, t.Field)}
				}
				return nil
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
		return (t.Where != nil && hasImpure(t.Where)) || (t.Sel != nil && hasImpure(t.Sel))
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

// isFieldAgg reports whether an aggregate op reduces a numeric field (and so
// needs one): sum, avg, min, max. count/exists range over rows.
func isFieldAgg(op string) bool {
	switch op {
	case "sum", "avg", "min", "max":
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
			// the dispatch loop can refresh exactly the regions that show it. dirty()/
			// touched() read form-field status keyed on the cell itself, so editing the
			// input (which refreshes that cell) also refreshes what reads them.
			if x.Op == "dirty" || x.Op == "touched" {
				out[x.Name] = true
			} else {
				out["@act:"+x.Name] = true
			}
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
		for _, segs := range n.SegLists() {
			for _, sg := range segs {
				if sg.Expr != nil {
					add(sg.Expr)
				}
			}
		}
		add(n.Cond)
		add(n.Where)
		// A dynamic limit (a load-more page size) and a computed option value are
		// reads like any other: what they read must refresh what renders them.
		add(n.Limit)
		add(n.Val)
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

// optionDeps registers the refresh edges a control whose choices come from data
// needs.
//
// This is the half of the feature that is silently broken when it is left out: a
// dropdown paints correctly and then never changes again, which looks exactly
// like a dropdown that works. A `for` region earns edges from the collection it
// walks and from everything its filter, its limit and its body read; a choice
// list walks a collection for the same reason and reads the same things, so it
// earns exactly the same edges — pointed at the control's own id, because the
// control IS the region its options live in.
//
// Only at the top level, and for the reason a `for` mints a region id only there:
// inside another region there is no single element to re-fill, and that region's
// own refresh already rebuilds this control along with the rest of its row.
func (c *viewCtx) optionDeps(node Node, kids []Node, sc scope) {
	if len(kids) == 0 || sc.inRegion {
		return
	}
	for _, d := range sortedKeys(c.e.nodeDeps(kids)) {
		c.addDep(d, node.ID)
	}
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
			// which the aggregate itself binds. The reduced value reads the row
			// through that same variable, so it is bound there for exactly the same
			// reason; every OTHER name in it has to resolve in the enclosing scope,
			// which is what makes a typo in `sum(l.qty * unitPrice in …)` a compile
			// error naming `unitPrice` rather than a silent zero.
			for _, sub := range [2]ast.Expr{t.Where, t.Sel} {
				if sub == nil {
					continue
				}
				for n := range freeNames(sub) {
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
		if t.Sel != nil {
			// The reduced value, when it is more than one of the row's columns.
			// Exclusive with Field (the parser guarantees it), so a bare
			// `sum(x.amount in …)` lowers exactly as it always did and every
			// program written before this is byte-identical through the compiler.
			a.Sel = lower(t.Sel, inline, enums)
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

// viewScope is withActor plus the names a *render* binds, as opposed to the ones
// the session binds. There is exactly one so far — `route`, the path being
// rendered — and it lives here rather than in isBuiltinRef for a reason worth
// stating: `actor` and `role` are answerable anywhere, including inside an action
// the authority runs with no page in sight, but `route` has an answer only while
// a page is being rendered. Reading it from a policy, a derive or an action body
// would be asking a question that has no answer yet, and would silently produce
// an empty string rather than say so — so those contexts keep withActor and this
// one name is refused there by name resolution, like any other unknown.
func viewScope(locals map[string]bool) map[string]bool {
	m := withActor(locals)
	m["route"] = true
	return m
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
