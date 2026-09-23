package ir

import (
	"fmt"
	"net/textproto"
	"sort"
	"strings"

	"facet/internal/ast"
	"facet/internal/parser"
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
// emittedEvent is one `emit [name] Dto{…}` statement, checked against the
// declared streams once they are built.
type emittedEvent struct {
	name, typ string
	line      int
	on        int // how many `on` path values it names
}

type env struct {
	wireTypes        map[string]bool                   // wire `type`/`message` names (SCHEMA_IDL_SCOPE Tier C) — an action may return one
	emittedTypes     map[string]int                    // wire type -> line of an `emit` of it, checked against the streams that carry it
	actLocalTypes    map[string]vtype                  // the action being built: its parameters' and lets' types
	procNames        map[string]bool                   // every declared proc, known before derives are checked
	procArity        map[string]int                    // every declared proc's parameter count
	procDerives      map[string]bool                   // parameterized derives that call a proc: usable only in actions
	inDerive         string                            // the parameterized derive whose body is being checked
	inProc           bool                              // a proc body is being checked
	inDaemon         bool                              // a daemon body is being checked (server-only, like a proc)
	daemonCtxWhy     map[string]string                 // a detached proc that is not daemon-context: the reference that disqualifies it (detachctx.go)
	actParams        map[string]bool                   // the action being built: its parameter names (given(p))
	deriveParamTypes map[string]vtype                  // the parameterized derive being checked: its parameters' types
	entDeriveExprs   map[string]map[string]*Expr       // entity -> its derives' lowered expressions (over "$row")
	wireOptional     map[string]map[string]bool        // wire type -> its optional (`?`) fields
	wireNullable     map[string][]string               // wire type -> its `T or null` fields
	wireFieldTypes   map[string]map[string]vtype       // wire type -> field -> its declared type
	emittedEvents    []emittedEvent                    // every `emit name Dto{…}`: the named event must be one a stream carries with that payload
	unnamedEmits     []emittedEvent                    // every `emit Dto{…}`: no stream may carry Dto under two names
	wireFields       map[string]map[string]bool        // wire type -> field set, for a `Dto{...}` literal's field check
	states           map[string]string                 // name -> placement
	entities         map[string]bool                   // entity names
	entityFields     map[string]map[string]bool        // entity -> field set (incl id)
	entityDerives    map[string]map[string]bool        // entity -> derive-field set (never a stored column; read-only, see ast.Entity.Derives)
	queriedFields    map[string]map[string]bool        // entity -> fields a `where`/`by`/relation reads at all
	indexFields      map[string]map[string]bool        // entity -> fields whose use an index can actually serve
	inline           map[string]*Expr                  // zero-arg policy/derive name -> lowered expr, inlined at every use
	inlineType       map[string]vtype                  // the same names -> the type they resolve to (a derive's declared type, a policy's bool)
	deriveFns        map[string]*deriveFn              // parameterized derive name -> its signature and lowered body, expanded at every call
	policySet        map[string]bool                   // policy names (gating via `requires`)
	policyParams     map[string][]Param                // policy name -> its parameters (row-level policies)
	enums            map[string][]string               // enum name -> ordered member values
	components       map[string][]ast.Param            // component name -> its parameters (a reference parameter carries its Ref kind)
	compAST          map[string]*ast.Component         // component name -> its source, for call-site expansion of templates
	compSlot         map[string]bool                   // component names whose body contains a `slot` (so a `use` may pass children)
	compDeps         map[string]map[string]bool        // component name -> the state/entity names its body reads (for use-site refresh)
	compRegions      map[string]map[string][]string    // component name -> the dependency edges its own regions/inputs need, folded into every page that uses it
	special          []Component                       // per-call-site expansions of template components, appended to the IR
	specialCalls     []call                            // action references made inside expansions, validated with the rest
	specialLinks     []linkRef                         // link destinations inside expansions, route-checked with the rest
	specStack        []string                          // components currently being expanded, to refuse recursion
	nspec            int                               // expansions minted so far; names them and namespaces their region ids
	stateTypes       map[string]string                 // state name -> its (core/element) type, for enum-defaulted selects
	stateList        map[string]bool                   // state names that are `[T]` list cells, for `for x in <list>`
	services         map[string]map[string]int         // service name -> op name -> parameter count, for checking `call`
	serviceRets      map[string]map[string]opRet       // service name -> op name -> return type, for binding `let x = call …`
	private          map[string]bool                   // @private state names — server-only, non-renderable
	entFieldEnum     map[string]map[string]string      // entity -> field -> enum name (only enum-typed fields), for `match` exhaustiveness
	entFieldType     map[string]map[string]string      // entity -> field -> stored type core (enum fields read as "text"), for typing a data-driven option's value
	records          map[string]map[string]recField    // record name -> field name -> its type, for `let`-bound field access
	structs          map[string]map[string]structField // struct name -> field name -> its type, for a proc-local struct literal/field access (see ast.Struct)
	entE2E           map[string]map[string]bool        // entity -> field -> true for @e2e (sealed) fields, for render-marking and the seal dataflow
	locRecords       map[string]recBind                // record-typed action locals (a `let` bind) -> the record bound, for `v.field` checking (reset per action)
	entPassword      map[string]map[string]bool        // entity -> field -> true for @password (one-way hashed) fields, which only verifyPassword may read
	rowLocals        map[string]string                 // row-typed locals of the body being checked (an action's `for` variable or `let r = Entity(k)`, a read clause's $row) -> entity, for refusing a @password read (reset per body)
	actionSet        map[string]bool                   // action names, for validating pending()/failed() targets
	procSigs         map[string]procSig                // proc name -> its signature, for checking `do` (from an action or another proc)
	shareds          map[string]*ast.Shared            // `shared` cell name -> its declaration (see ast.Shared); read/assigned only from proc-shaped bodies
	files            map[string]*ast.File              // file name -> its declaration, for checking a proc's `read`/`write` statements
}

// procSig is a proc's signature: enough to check a `do` call site (arity) and to
// bind/coerce its result (Ret/RetList), mirroring opRet for a service operation.
type procSig struct {
	params  []ast.Param
	ret     string
	retList bool
}

// deriveFn is a parameterized derive: what a call site is checked against (its
// parameters, its declared type) and what it expands to (the lowered body, whose
// parameters are still plain refs — see expandDerive).
type deriveFn struct {
	params []ast.Param
	ret    vtype
	body   *Expr
}

// actionSig is enough of an already-built action's signature to check an
// `act ActionName(args)` call site (arity) inside a daemon body — see
// ast.Act's doc. Only ever populated for a daemon build (procBlock's
// actionSigs parameter is nil while lowering a real proc's body, which is
// what makes `act` a compile error there).
type actionSig struct {
	params    []Param
	placement string
}

// spawnedTask is procBlock's own record of one still-outstanding `spawn`
// (Milestone 5) — the spawned proc's name (for a clear diagnostic) plus its
// signature (so `join`'s optional bind knows what type it's receiving),
// keyed by handle name in pendingSpawns.
type spawnedTask struct {
	proc string
	sig  procSig
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

// structField is one struct field's resolved type, for checking a
// proc-local `Type{...}` literal (every field present, no unknown field, no
// obvious type mismatch) and a `.field` read off a struct-typed proc value —
// recField's exact shape, kept as its own named type since a struct field's
// Type may resolve to ANOTHER struct name (recField's never does: a record
// field is always a primitive or an enum — see ast.Record's doc), which is
// what lets checkStructFieldTypes chase a field access chain like
// `node.left.op` one struct at a time.
type structField struct {
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
	e := &env{states: map[string]string{}, entities: map[string]bool{}, entityFields: map[string]map[string]bool{}, entityDerives: map[string]map[string]bool{}, queriedFields: map[string]map[string]bool{}, indexFields: map[string]map[string]bool{}, inline: map[string]*Expr{}, inlineType: map[string]vtype{}, policySet: map[string]bool{}, policyParams: map[string][]Param{}, enums: map[string][]string{}, components: map[string][]ast.Param{}, compAST: map[string]*ast.Component{}, compSlot: map[string]bool{}, compDeps: map[string]map[string]bool{}, compRegions: map[string]map[string][]string{}, stateTypes: map[string]string{}, stateList: map[string]bool{}, services: map[string]map[string]int{}, serviceRets: map[string]map[string]opRet{}, private: map[string]bool{}, entFieldEnum: map[string]map[string]string{}, entFieldType: map[string]map[string]string{}, records: map[string]map[string]recField{}, structs: map[string]map[string]structField{}, entE2E: map[string]map[string]bool{}, entPassword: map[string]map[string]bool{}, actionSet: map[string]bool{}, procSigs: map[string]procSig{}, files: map[string]*ast.File{}, emittedTypes: map[string]int{}}

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
		out.Enums = append(out.Enums, Enum{Name: en.Name, Values: en.Values, WireNames: en.WireNames})
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

	// 0b2. Structs: proc-local named-field composite types (see ast.Struct's
	// doc — the construct this task adds so a tree/composite value gets real
	// field names instead of a flat `[int]` arena of magic slots). Unlike
	// Record, a struct field may name another struct, including itself (a
	// binary expression's `left: Expr, right: Expr`, or a tree node's
	// `children: [Node]`), so names are collected in a first pass — exactly
	// like Type/Message below — before any field is resolved, so a self- or
	// forward-reference just works.
	structSeen := map[string]int{}
	for _, sc := range app.Structs {
		if prev, ok := structSeen[sc.Name]; ok {
			return nil, &BuildError{sc.Line, fmt.Sprintf("struct %q redeclared (first at line %d)", sc.Name, prev)}
		}
		if _, clash := e.enums[sc.Name]; clash {
			return nil, &BuildError{sc.Line, fmt.Sprintf("struct %q clashes with an enum of the same name", sc.Name)}
		}
		if _, clash := e.records[sc.Name]; clash {
			return nil, &BuildError{sc.Line, fmt.Sprintf("struct %q clashes with a record of the same name", sc.Name)}
		}
		structSeen[sc.Name] = sc.Line
		e.structs[sc.Name] = map[string]structField{} // populated below, once every name is known
	}
	for _, sc := range app.Structs {
		fields := e.structs[sc.Name]
		var irFields []RecordField
		for _, f := range sc.Fields {
			if _, dup := fields[f.Name]; dup {
				return nil, &BuildError{f.Line, fmt.Sprintf("struct %q has duplicate field %q", sc.Name, f.Name)}
			}
			switch {
			case f.Type == "int", f.Type == "text", f.Type == "bool", f.Type == "float":
				// a scalar field
			case e.structs[f.Type] != nil:
				// another declared struct (or this one, self-referentially)
			default:
				return nil, &BuildError{f.Line, fmt.Sprintf(
					"struct %q field %q has type %q — a struct field must be int/text/bool/float, another declared struct, or a list of those", sc.Name, f.Name, f.Type)}
			}
			fields[f.Name] = structField{typ: f.Type, list: f.List}
			irFields = append(irFields, RecordField{Name: f.Name, Type: f.Type, List: f.List})
		}
		out.Structs = append(out.Structs, Struct{Name: sc.Name, Fields: irFields})
	}

	// 0c. Types & Messages: wire-schema-only value types and tagged unions
	// (SCHEMA_IDL_SCOPE.md Tier C). Unlike Records these may reference each
	// other and themselves, so names are collected in a first pass before any
	// field is validated, and a self/mutual reference is resolved rather than
	// rejected — codegen boxes/points at it (WireField.Ref), it never inlines.
	wireNames := map[string]bool{}
	wireSeen := map[string]int{}
	for _, ty := range app.Types {
		if prev, ok := wireSeen[ty.Name]; ok {
			return nil, &BuildError{ty.Line, fmt.Sprintf("type %q redeclared (first at line %d)", ty.Name, prev)}
		}
		wireSeen[ty.Name] = ty.Line
		wireNames[ty.Name] = true
	}
	// A type's contract schema name (`type Name as "schema":`) is unique
	// across every published schema name, its own alias or another's Name.
	schemaSeen := map[string]string{}
	for _, ty := range app.Types {
		sn := ty.Schema
		if sn == "" {
			sn = ty.Name
		}
		if other, ok := schemaSeen[sn]; ok {
			return nil, &BuildError{ty.Line, fmt.Sprintf("type %q publishes schema %q, which type %q already publishes", ty.Name, sn, other)}
		}
		schemaSeen[sn] = ty.Name
	}
	for _, ty := range app.Types {
		if ty.Schema != "" && wireNames[ty.Schema] && ty.Schema != ty.Name {
			return nil, &BuildError{ty.Line, fmt.Sprintf("type %q publishes schema %q, the name of another declared type", ty.Name, ty.Schema)}
		}
	}
	for _, ms := range app.Messages {
		if prev, ok := wireSeen[ms.Name]; ok {
			return nil, &BuildError{ms.Line, fmt.Sprintf("message %q redeclared (first at line %d, possibly as a type)", ms.Name, prev)}
		}
		wireSeen[ms.Name] = ms.Line
		wireNames[ms.Name] = true
	}
	e.wireTypes = wireNames
	e.procNames = map[string]bool{}
	e.entDeriveExprs = map[string]map[string]*Expr{}
	e.procArity = map[string]int{}
	e.procDerives = map[string]bool{}
	for _, p := range app.Procs {
		e.procNames[p.Name] = true
		e.procArity[p.Name] = len(p.Params)
	}
	resolveWireField := func(declName string, f ast.RecordField, seen map[string]bool) (WireField, error) {
		if seen[f.Name] {
			return WireField{}, &BuildError{f.Line, fmt.Sprintf("%q has duplicate field %q", declName, f.Name)}
		}
		seen[f.Name] = true
		wf := WireField{Name: f.Name, Type: f.Type, List: f.List, Optional: f.Optional, Description: f.Description, Aliases: f.Aliases, Enum: f.Enum, Into: f.Into, Nullable: f.Nullable, Map: f.Map, Depth: f.Depth}
		if f.Default != nil {
			wf.Default = *f.Default
		}
		switch {
		case f.Type == "json", f.Type == "number", isPrimitive(f.Type):
			// inline scalar
		case wireNames[f.Type]:
			wf.Ref = true
		default:
			if _, isEnum := e.enums[f.Type]; !isEnum {
				return WireField{}, &BuildError{f.Line, fmt.Sprintf("%q field %q has unknown type %q (not a primitive, `json`, an enum, or a declared type/message)", declName, f.Name, f.Type)}
			}
		}
		if wf.Default != "" {
			// A list field's only legal default is `[]` (an empty list),
			// checked here independent of the scalar switch below — the
			// element type (even Ref/enum) is irrelevant to whether an empty
			// list is a valid default, only List-ness is.
			if wf.List {
				if wf.Default != "[]" {
					return WireField{}, &BuildError{f.Line, fmt.Sprintf("%q field %q: a list field's only supported default is [], not %s", declName, f.Name, wf.Default)}
				}
			} else {
				isBoolLit := wf.Default == "true" || wf.Default == "false"
				isStrLit := len(wf.Default) >= 2 && wf.Default[0] == '"'
				isObjLit := wf.Default == "{}"
				isIntLit := !isBoolLit && !isStrLit && !isObjLit
				switch f.Type {
				case "bool":
					if !isBoolLit {
						return WireField{}, &BuildError{f.Line, fmt.Sprintf("%q field %q: default %s doesn't match its bool type", declName, f.Name, wf.Default)}
					}
				case "int", "money", "number":
					if !isIntLit {
						return WireField{}, &BuildError{f.Line, fmt.Sprintf("%q field %q: default %s doesn't match its numeric type", declName, f.Name, wf.Default)}
					}
				case "text", "date":
					if !isStrLit {
						return WireField{}, &BuildError{f.Line, fmt.Sprintf("%q field %q: default %s doesn't match its text type", declName, f.Name, wf.Default)}
					}
				case "json":
					if !isObjLit {
						return WireField{}, &BuildError{f.Line, fmt.Sprintf("%q field %q: a json field's only supported default is {}, not %s", declName, f.Name, wf.Default)}
					}
				default:
					return WireField{}, &BuildError{f.Line, fmt.Sprintf("%q field %q: a default is only supported on bool/int/money/number/text/date/json, or a list ([]), not %q", declName, f.Name, f.Type)}
				}
			}
		}
		return wf, nil
	}
	e.wireFields = map[string]map[string]bool{}
	e.wireOptional = map[string]map[string]bool{}
	e.wireNullable = map[string][]string{}
	e.wireFieldTypes = map[string]map[string]vtype{}
	for _, ty := range app.Types {
		e.wireOptional[ty.Name] = map[string]bool{}
		e.wireFieldTypes[ty.Name] = map[string]vtype{}
		for _, f := range ty.Fields {
			if f.Optional {
				e.wireOptional[ty.Name][f.Name] = true
			}
			if f.Nullable && !f.Optional {
				// A present-but-maybe-null field: the runtime sends null for
				// an empty value. `T? or null` is left out when empty instead.
				e.wireNullable[ty.Name] = append(e.wireNullable[ty.Name], f.Name)
			}
			if f.Map || f.Depth > 0 {
				// A map or nested list holds structured JSON: an expression
				// filling it is checked as json (e.g. fromJson(…)).
				e.wireFieldTypes[ty.Name][f.Name] = vtype{core: "json"}
			} else {
				e.wireFieldTypes[ty.Name][f.Name] = vtype{core: f.Type, list: f.List}
			}
		}
		seen := map[string]bool{}
		var fields []WireField
		e.wireFields[ty.Name] = map[string]bool{}
		for _, f := range ty.Fields {
			wf, err := resolveWireField(ty.Name, f, seen)
			if err != nil {
				return nil, err
			}
			fields = append(fields, wf)
			e.wireFields[ty.Name][f.Name] = true
		}
		out.Types = append(out.Types, WireType{Name: ty.Name, Schema: ty.Schema, Query: ty.Query, Fields: fields})
	}
	for _, ms := range app.Messages {
		var variants []WireMessageVariant
		for _, v := range ms.Variants {
			seen := map[string]bool{}
			var fields []WireField
			for _, f := range v.Fields {
				wf, err := resolveWireField(fmt.Sprintf("%s.%s", ms.Name, v.Name), f, seen)
				if err != nil {
					return nil, err
				}
				fields = append(fields, wf)
			}
			if v.BodyType != "" && !wireNames[v.BodyType] {
				return nil, &BuildError{v.Line, fmt.Sprintf("%s.%s: body type %q is not a declared wire type", ms.Name, v.Name, v.BodyType)}
			}
			variants = append(variants, WireMessageVariant{Name: v.Name, Fields: fields, Wire: v.Wire, Action: v.Action, Since: v.Since, Summary: v.Summary, BodyParam: v.BodyParam, BodyType: v.BodyType})
		}
		out.Messages = append(out.Messages, WireMessage{Name: ms.Name, Tag: ms.Tag, Variants: variants})
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
		ei := Entity{Name: ent.Name, SoftDelete: ent.SoftDelete, Ephemeral: ent.Ephemeral}
		for _, f := range ent.Fields {
			if f.Secret && f.Name == "id" {
				return nil, &BuildError{f.Line, "the id field cannot be @secret"}
			}
			fld := Field{Name: f.Name, Type: f.Type, Secret: f.Secret, E2E: f.E2E, Password: f.Password, ReadPolicy: f.ReadPolicy, Optional: f.Optional,
				Unique: f.Unique, Required: f.Required, Min: f.Min, Max: f.Max, Matches: f.Matches, OnDelete: f.OnDelete}
			if f.Unique {
				e.markIndex(ent.Name, f.Name) // a uniqueness check reads by value; index it
			}
			if f.Password {
				if e.entPassword[ent.Name] == nil {
					e.entPassword[ent.Name] = map[string]bool{}
				}
				e.entPassword[ent.Name][f.Name] = true
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
		// @ephemeral rows are never durable, so an @softdelete "archive it instead
		// of dropping it" has nothing to persist into — the combination is
		// nonsensical, not merely redundant, so it is refused rather than silently
		// accepted with one annotation quietly doing nothing.
		if ent.Ephemeral && ent.SoftDelete {
			return nil, &BuildError{ent.Line, fmt.Sprintf("entity %q is @ephemeral, so @softdelete has nothing to archive into — drop one", ent.Name)}
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
		// `read:` — validated the same way a policy's predicate is (checkPure: no
		// unknown reference, no effectful builtin — a read rule that consulted
		// now()/rand() would answer a different question on every evaluation of
		// the same row) and lowered the same way (e.low). The parser has already
		// turned every one of the entity's own field names into `Get{Ref{"$row"},
		// name}` (qualifyRowRefs), so the only new name this scope has to admit is
		// "$row" itself — everything else (actor and the identity builtins) comes
		// from withActor exactly as a policy's does.
		if ent.Read != nil {
			locals := withActor(map[string]bool{"$row": true})
			e.rowLocals = map[string]string{"$row": ent.Name}
			err := e.checkPure(ent.Read, locals, ent.Line, fmt.Sprintf("entity %q's read clause", ent.Name))
			e.rowLocals = nil
			if err != nil {
				return nil, err
			}
			ei.Read = e.low(ent.Read)
		}
		// Derives — entity-carried `derive name: Type = expr` fields (ast.Entity.
		// Derives). Validated and lowered exactly like `read:` above (checkPure
		// over the same {"$row"}+withActor locals, e.low), because the parser
		// qualified them the same way (qualifyRowRefs): each one is already
		// rooted at "$row". They are deliberately kept OUT of e.entityFields —
		// that map gates `set`/`add`/order-by/typeahead, every one of which needs
		// a real, stored column — and tracked instead in e.entityDerives, which
		// only the read-side field checks (checkRowFields, an EntityGet's field)
		// consult. Nothing here becomes a database column: ei.Fields never grows
		// from this loop, so the schema/migration never sees it (runtime/region.go
		// applyDerives is what actually computes the value, on every read).
		if len(ent.Derives) > 0 {
			e.entityDerives[ent.Name] = map[string]bool{}
			seenDerive := map[string]int{}
			for _, d := range ent.Derives {
				if e.entityFields[ent.Name][d.Name] {
					return nil, &BuildError{d.Line, fmt.Sprintf("entity %q already has a field %q; derive %q needs a different name", ent.Name, d.Name, d.Name)}
				}
				if prev, ok := seenDerive[d.Name]; ok {
					return nil, &BuildError{d.Line, fmt.Sprintf("entity %q's derive %q redeclared (first at line %d)", ent.Name, d.Name, prev)}
				}
				seenDerive[d.Name] = d.Line
				locals := withActor(map[string]bool{"$row": true})
				e.rowLocals = map[string]string{"$row": ent.Name}
				err := e.checkPure(d.Expr, locals, d.Line, fmt.Sprintf("entity %q's derive %q", ent.Name, d.Name))
				e.rowLocals = nil
				if err != nil {
					return nil, err
				}
				ei.Derives = append(ei.Derives, Derive{Name: d.Name, Type: d.Type, Expr: e.low(d.Expr)})
				if e.entDeriveExprs[ent.Name] == nil {
					e.entDeriveExprs[ent.Name] = map[string]*Expr{}
				}
				e.entDeriveExprs[ent.Name][d.Name] = ei.Derives[len(ei.Derives)-1].Expr
				e.entityDerives[ent.Name][d.Name] = true
			}
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
				// @restrict/@setNull govern what happens to THIS row when the row it
				// points at is deleted — meaningless off a relation, so a primitive or
				// enum field (already lowered to "text" by the time it reaches here)
				// carrying one is refused rather than silently ignored.
				if f.OnDelete != "" {
					return nil, &BuildError{0, fmt.Sprintf(
						"field %q has an on-delete modifier (@restrict/@setNull) but is not a relation to another entity", f.Name)}
				}
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
			if e.entities[pp.Type] {
				if e.rowLocals == nil {
					e.rowLocals = map[string]string{}
				}
				e.rowLocals[pp.Name] = pp.Type
			}
		}
		err := e.checkPure(p.Expr, plocals, p.Line, "a policy")
		e.rowLocals = nil
		if err != nil {
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
	// with every derive it reads already substituted, so the IR a derive
	// carries — and every place it is read — is fully resolved to base cells. A
	// derive is pure and read-only, so it has no placement of its own; its deps
	// drive dependency tracking wherever it is used.
	//
	// They are built in dependency order, not declaration order: an app merged
	// from imported files lists the importer's derives before its imports', so a
	// projection built on another file's projection must not depend on which
	// file happened to be written first.
	derSeen := map[string]int{}
	for _, d := range app.Derives {
		if prev, ok := derSeen[d.Name]; ok {
			return nil, &BuildError{d.Line, fmt.Sprintf("derive %q redeclared (first at line %d)", d.Name, prev)}
		}
		derSeen[d.Name] = d.Line
	}
	derives, err := deriveOrder(app.Derives)
	if err != nil {
		return nil, err
	}
	e.deriveFns = map[string]*deriveFn{}
	for _, d := range derives {
		if err := e.checkName(d.Name, d.Line, "derive"); err != nil {
			return nil, err
		}
		if len(d.Params) > 0 {
			e.inDerive = d.Name
			fn, err := e.deriveFn(d)
			e.inDerive = ""
			if err != nil {
				return nil, err
			}
			e.deriveFns[d.Name] = fn
			out.Derives = append(out.Derives, Derive{Name: d.Name, Params: irParams(d.Params), Type: d.Type, Expr: fn.body, Deps: sortedKeys(e.depsIR(fn.body))})
			continue
		}
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

	// Binary/static files bundled via `asset from "..."` — internal/compile
	// already read them from disk and keyed them by content hash; this is a
	// straight carry-forward into the graph, the same "resolved by the time
	// ir.Build sees it" shape app.CSS has.
	if len(app.Assets) > 0 {
		out.Assets = make(map[string]Asset, len(app.Assets))
		for name, blob := range app.Assets {
			out.Assets[name] = Asset{ContentType: blob.ContentType, Bytes: blob.Bytes}
		}
	}

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
			// A value parameter's type is a primitive, an enum, or an entity (a row
			// of it: `component PostCard(t: Tweet)`). A name that is none of those
			// used to be accepted as "unknown" and checked nowhere — so `t: Tweeet`
			// compiled, and every `t.field` inside rendered as nothing.
			if p.Ref == ast.RefValue && !isPrimitive(p.Type) {
				_, isEnum := e.enums[p.Type]
				if !isEnum && !e.entities[p.Type] {
					return nil, &BuildError{cm.Line, fmt.Sprintf("component %q parameter %q has unknown type %q — a parameter is a primitive (int/text/bool/money/date), an enum, or an entity (a row: `%s: Post`)", cm.Name, p.Name, p.Type, p.Name)}
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
		irsv := Service{Name: sv.Name, URL: sv.URL, URLEnv: sv.URLEnv}
		for _, h := range sv.Headers {
			irsv.Headers = append(irsv.Headers, ServiceHeader{Name: h.Name, Env: h.Env})
		}
		for _, op := range sv.Ops {
			if _, dup := ops[op.Name]; dup {
				return nil, &BuildError{op.Line, fmt.Sprintf("service %q declares operation %q twice", sv.Name, op.Name)}
			}
			// A declared return type must resolve: a primitive, an enum, or a record
			// (the structured-reply case). A bare capitalized name the parser accepted
			// is only valid here if it names a real record/enum.
			// `-> bytes`: the brain answers a file (its raw response body, e.g. a
			// rendered image), which the runtime stores as an upload; the bound
			// value is that file, exactly what a `bytes` action parameter holds.
			if op.Ret == "bytes" && op.RetList {
				return nil, &BuildError{op.Line, fmt.Sprintf("%s.%s returns [bytes] — a service answers one file per call", sv.Name, op.Name)}
			}
			if op.Ret != "" && op.Ret != "bytes" && !isPrimitive(op.Ret) {
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

	// 3d″. Files: declared file resources a proc's `read`/`write` statements
	// operate on (see ast.File) — the same named-resource pattern as Service,
	// for local disk instead of HTTP. Registered before proc bodies are built
	// (procBlock's ast.FileOp case resolves against e.files), and a bad path
	// shape is caught here, at declaration time, rather than only once some
	// proc happens to use it — the same "author's mistake, not something to
	// discover later" stance resolveDataPath's own absolute-path check takes
	// at runtime, just moved as early as it can provably be for a literal path.
	for _, f := range app.Files {
		if _, dup := e.files[f.Name]; dup {
			return nil, &BuildError{f.Line, fmt.Sprintf("file %q redeclared", f.Name)}
		}
		if f.Type != "text" && f.Type != bytesType {
			return nil, &BuildError{f.Line, fmt.Sprintf("file %q type must be text or bytes, got %q", f.Name, f.Type)}
		}
		if strings.HasPrefix(f.Path, "/") {
			return nil, &BuildError{f.Line, fmt.Sprintf("file %q path %q must be relative to the sandboxed data directory, not absolute", f.Name, f.Path)}
		}
		e.files[f.Name] = f
		out.Files = append(out.Files, File{Name: f.Name, Type: f.Type, Path: f.Path})
	}

	// 3d″′. Procs: general-purpose, unconditionally server-executed declarations
	// (self-hosting + product logic — see ROADMAP.md, "Decision superseded: full
	// self-hosting"). Signatures are registered in one pass — so a proc may call
	// another declared later in source order, and an action's `do` resolves
	// against the same table `call` uses for services — then each body is built.
	procSeen := map[string]int{}
	for _, p := range app.Procs {
		if prev, ok := procSeen[p.Name]; ok {
			return nil, &BuildError{p.Line, fmt.Sprintf("proc %q redeclared (first at line %d)", p.Name, prev)}
		}
		procSeen[p.Name] = p.Line
		if e.entities[p.Name] {
			return nil, &BuildError{p.Line, fmt.Sprintf("proc %q collides with an entity name", p.Name)}
		}
		if _, dup := e.services[p.Name]; dup {
			return nil, &BuildError{p.Line, fmt.Sprintf("proc %q collides with a service name", p.Name)}
		}
		if p.Ret != "" && !isPrimitive(p.Ret) {
			_, isEnum := e.enums[p.Ret]
			_, isRec := e.records[p.Ret]
			_, isStruct := e.structs[p.Ret]
			if !isEnum && !isRec && !isStruct && !e.wireTypes[p.Ret] {
				return nil, &BuildError{p.Line, fmt.Sprintf("proc %q returns unknown type %q", p.Name, p.Ret)}
			}
		}
		// A `uses` clause may only name a capability the runtime actually
		// implements (see knownCapabilities) — a typo here (`uses io.fiel`)
		// would otherwise compile clean and simply never satisfy
		// checkProcCapabilities, an unhelpful way to discover it.
		seenUse := map[string]bool{}
		for _, u := range p.Uses {
			if !knownCapabilities[u] {
				return nil, &BuildError{p.Line, fmt.Sprintf("proc %q declares unknown capability %q — known capabilities: io.console, io.env, io.file, io.net, io.net.listen", p.Name, u)}
			}
			if seenUse[u] {
				return nil, &BuildError{p.Line, fmt.Sprintf("proc %q declares capability %q more than once", p.Name, u)}
			}
			seenUse[u] = true
		}
		e.procSigs[p.Name] = procSig{params: p.Params, ret: p.Ret, retList: p.RetList}
	}
	// Shared cells (see ast.Shared) are known before any proc or daemon body
	// is lowered: those are the only bodies that may name one.
	if err := e.buildShareds(app, out); err != nil {
		return nil, err
	}
	entryAt := -1 // the program's `main`, lowered below with the daemons
	// Procs only ever `detach`ed from a daemon-context body are lowered with
	// the daemons too, as daemon-context bodies themselves (detachctx.go).
	daemonCtx, daemonCtxWhy := daemonContextProcs(app)
	e.daemonCtxWhy = daemonCtxWhy
	ctxAt := map[string]int{}
	for _, p := range app.Procs {
		if isProcessEntry(p) {
			entryAt = len(out.Procs)
			out.Procs = append(out.Procs, Proc{})
			continue
		}
		if daemonCtx[p.Name] {
			ctxAt[p.Name] = len(out.Procs)
			out.Procs = append(out.Procs, Proc{})
			continue
		}
		pr, err := e.proc(p)
		if err != nil {
			return nil, err
		}
		out.Procs = append(out.Procs, pr)
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

	// 4b′. Daemons: detached, process-lifetime background tasks (see
	// ast.Daemon's doc). Built here — after actions, unlike procs, which are
	// built earlier in pass 3d′ — because a daemon body's `act ActionName(args)`
	// statements resolve against the actions just built, exactly like a Job's
	// own Action reference above, except a daemon may call several, with
	// arguments, from inside an arbitrary control-flow body instead of naming
	// exactly one zero-arg action.
	actionSigs := make(map[string]actionSig, len(byActionName))
	for name, act := range byActionName {
		actionSigs[name] = actionSig{params: act.Params, placement: act.Placement}
	}
	// The process entry point (`facet exec`, runtime/stdio.go) runs exactly
	// where a daemon body does — on its own goroutine, under no request's
	// lock, for as long as it likes — so its body is lowered with the same
	// allowances: listen/accept, detach and act.
	for _, p := range app.Procs {
		at, ctx := ctxAt[p.Name]
		if isProcessEntry(p) {
			at, ctx = entryAt, true
		}
		if !ctx {
			continue
		}
		pr, err := e.procIn(p, actionSigs)
		if err != nil {
			return nil, err
		}
		out.Procs[at] = pr
	}
	daemonSeen := map[string]int{}
	for _, d := range app.Daemons {
		if prev, ok := daemonSeen[d.Name]; ok {
			return nil, &BuildError{d.Line, fmt.Sprintf("daemon %q redeclared (first at line %d)", d.Name, prev)}
		}
		daemonSeen[d.Name] = d.Line
		dm, err := e.daemon(d, actionSigs)
		if err != nil {
			return nil, err
		}
		out.Daemons = append(out.Daemons, dm)
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
			{Name: "password", Type: "text", Password: true}, {Name: "role", Type: "text"},
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

	// 4d′. Declared HTTP endpoints (`api METHOD "/path" -> action`): the typed
	// contract. Each binds a server-placed action; every `{name}` in the path
	// must be one of its parameters (the rest arrive as query or body fields);
	// method+path is unique; the auth mode is read off the action's gate.
	apiSeen := map[string]int{}
	for _, ap := range app.APIs {
		if msg := findMessage(out.Messages, ap.Action); msg != nil {
			// `api POST "/events" -> Mutation`: a dispatching route.
			if err := e.dispatchRoute(ap, msg, byActionName, apiSeen); err != nil {
				return nil, err
			}
			if ap.Body != "" || len(ap.Errors) > 0 || len(ap.ParamDocs) > 0 {
				return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q -> %s: a dispatching route documents its variants in the message — its block takes only summary, description and operation", ap.Method, ap.Path, msg.Name)}
			}
			out.APIs = append(out.APIs, API{Method: ap.Method, Path: ap.Path, Status: 200, Rate: ap.Rate, Since: ap.Since, Auth: "none", Dispatch: msg.Name,
				Summary: ap.Summary, Description: ap.Description, Operation: ap.Operation})
			continue
		}
		act, ok := byActionName[ap.Action]
		if !ok {
			return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q targets unknown action %q", ap.Method, ap.Path, ap.Action)}
		}
		if act.Placement != Server {
			// An endpoint's action runs on the authority by definition. An
			// action the calculus placed in the browser only because nothing it
			// touches is authoritative simply moves; one that reads or writes
			// @client state cannot, since the authority cannot see that state.
			for _, n := range append(append([]string{}, act.Reads...), act.Writes...) {
				if e.states[n] == Client {
					return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q targets action %q, which reads or writes client-only state %q; an endpoint runs on the authority, which cannot see ephemeral client state", ap.Method, ap.Path, ap.Action, n)}
				}
			}
			act.Placement = Server
			act.Reason = fmt.Sprintf("serves api %s %s — an endpoint runs on the authority", ap.Method, ap.Path)
		}
		key := ap.Method + " " + ap.Path
		if prev, ok := apiSeen[key]; ok {
			return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q redeclared (first at line %d)", ap.Method, ap.Path, prev)}
		}
		apiSeen[key] = ap.Line
		paramSet := map[string]bool{}
		for _, p := range act.Params {
			paramSet[p.Name] = true
		}
		var pathParams []string
		for _, seg := range strings.Split(ap.Path, "/") {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				name := seg[1 : len(seg)-1]
				if !paramSet[name] {
					return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q: path parameter {%s} is not a parameter of action %q", ap.Method, ap.Path, name, ap.Action)}
				}
				for _, p := range act.Params {
					if p.Name == name && p.List {
						return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q: path parameter {%s} is a list — a path segment carries one value; take the list from the query or body instead", ap.Method, ap.Path, name)}
					}
				}
				pathParams = append(pathParams, name)
			} else if strings.ContainsAny(seg, "{}") {
				return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q: a path parameter is a whole segment, {name}", ap.Method, ap.Path)}
			}
		}
		// A `bytes`-typed parameter (see isPrimitive's doc: legal on an action
		// parameter only) is bound from an uploaded file's multipart form
		// field (runtime/apidecl.go's bindMultipartBody) — meaningless on a
		// GET/DELETE route, which carries no request body at all.
		if ap.Method == "GET" || ap.Method == "DELETE" {
			inPathParam := map[string]bool{}
			for _, name := range pathParams {
				inPathParam[name] = true
			}
			for _, p := range act.Params {
				if p.Type == "bytes" && !inPathParam[p.Name] {
					return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q: action %q has a `bytes` parameter %q, which uploads a file — GET and DELETE carry no request body to upload it in", ap.Method, ap.Path, ap.Action, p.Name)}
				}
			}
		}
		status := ap.Status
		if status == 0 {
			status = 200
		}
		if status == 204 && act.Ret != "" {
			return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q answers 204 (no content) but action %q returns a value", ap.Method, ap.Path, ap.Action)}
		}
		auth := "none"
		if len(act.Requires) > 0 {
			auth = "session"
		}
		if ap.Bearer != "" {
			// The bearer credential is the app's own (verified by the action),
			// so it cannot also be a session: the Authorization header carries
			// one or the other.
			if len(act.Requires) > 0 {
				return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q authenticates with %s bearer tokens, so action %q cannot also require a session policy", ap.Method, ap.Path, ap.AuthScheme, ap.Action)}
			}
			var bp *Param
			for i := range act.Params {
				if act.Params[i].Name == ap.Bearer {
					bp = &act.Params[i]
				}
			}
			if bp == nil || bp.Type != "text" || bp.List {
				return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q: bearer parameter %q must be a text parameter of action %q", ap.Method, ap.Path, ap.Bearer, ap.Action)}
			}
			for _, pp := range pathParams {
				if pp == ap.Bearer {
					return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q: {%s} is a path parameter, not the bearer credential", ap.Method, ap.Path, ap.Bearer)}
				}
			}
			if ap.AuthScheme == "none" || ap.AuthScheme == "session" {
				return nil, &BuildError{ap.Line, fmt.Sprintf("api %s %q: auth scheme %q is reserved; name the credential (e.g. dev_token)", ap.Method, ap.Path, ap.AuthScheme)}
			}
			auth = ap.AuthScheme
		}
		docs, err := e.apiDocs(ap, act, pathParams, out.Types)
		if err != nil {
			return nil, err
		}
		out.APIs = append(out.APIs, API{Method: ap.Method, Path: ap.Path, Params: pathParams, Action: ap.Action, Status: status, Rate: ap.Rate, Since: ap.Since, Auth: auth, Ret: act.Ret, RetList: act.RetList, Bearer: ap.Bearer,
			Summary: ap.Summary, Description: ap.Description, Body: ap.Body, Errors: ap.Errors, Operation: ap.Operation, ParamDocs: docs})
	}

	// 4d‴. `contract "/path"`: the runtime answers GET on the path and its
	// /version, /history and /diff children, so none of them may also be a
	// declared route.
	if cd := app.Contract; cd != nil {
		for _, sub := range []string{"", "/version", "/history", "/diff"} {
			if line, ok := apiSeen["GET "+cd.Path+sub]; ok {
				return nil, &BuildError{line, fmt.Sprintf("api GET %q is served by the contract declaration (line %d)", cd.Path+sub, cd.Line)}
			}
		}
		out.Contract = &ContractRoute{Path: cd.Path, Rate: cd.Rate, Since: cd.Since, Title: cd.Title, Description: cd.Description, Bearer: cd.Bearer}
	}

	// 4d″. Streams: `stream "/path" [requires policy]: TypeA, TypeB`, or the
	// block form naming each event. Every payload is a wire type; within one
	// stream an event name is unique; every `emit` names a type (and, when
	// given, an event) some stream carries, or it could never reach anyone;
	// an unnamed `emit Dto{…}` must be unambiguous on every stream carrying Dto.
	streamSeen := map[string]int{}
	carried := map[string]bool{}
	namesOf := map[string]map[string][]string{} // stream path -> payload type -> event names
	streamParams := map[string]int{}            // stream path -> its {param} count
	carriesNamed := map[string]bool{}           // name + " " + type
	for _, st := range app.Streams {
		if prev, ok := streamSeen[st.Path]; ok {
			return nil, &BuildError{st.Line, fmt.Sprintf("stream path %q redeclared (first at line %d)", st.Path, prev)}
		}
		streamSeen[st.Path] = st.Line
		namesOf[st.Path] = map[string][]string{}
		evSeen := map[string]bool{}
		var events []StreamEvent
		var params []string
		for _, seg := range strings.Split(st.Path, "/") {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				params = append(params, seg[1:len(seg)-1])
			} else if strings.ContainsAny(seg, "{}") {
				return nil, &BuildError{st.Line, fmt.Sprintf("stream %q: a path parameter is a whole segment, {name}", st.Path)}
			}
		}
		streamParams[st.Path] = len(params)
		for _, ev := range st.Events {
			if !e.wireTypes[ev.Type] {
				return nil, &BuildError{ev.Line, fmt.Sprintf("stream %q carries %q, which is not a declared wire type", st.Path, ev.Type)}
			}
			if evSeen[ev.Name] {
				return nil, &BuildError{ev.Line, fmt.Sprintf("stream %q declares event %q twice", st.Path, ev.Name)}
			}
			if ev.Name == "hello" {
				return nil, &BuildError{ev.Line, fmt.Sprintf("stream %q: `hello` is the connect frame the runtime sends itself", st.Path)}
			}
			evSeen[ev.Name] = true
			carried[ev.Type] = true
			carriesNamed[ev.Name+" "+ev.Type] = true
			namesOf[st.Path][ev.Type] = append(namesOf[st.Path][ev.Type], ev.Name)
			events = append(events, StreamEvent{Name: ev.Name, Type: ev.Type, Since: ev.Since, Summary: ev.Summary})
		}
		auth := "none"
		if st.Requires != "" {
			params, ok := e.policyParams[st.Requires]
			if !ok {
				return nil, &BuildError{st.Line, fmt.Sprintf("stream %q requires unknown policy %q", st.Path, st.Requires)}
			}
			if len(params) != 0 {
				return nil, &BuildError{st.Line, fmt.Sprintf("stream %q requires policy %q, which takes arguments; a stream gate is a plain permission", st.Path, st.Requires)}
			}
			auth = "session"
		}
		hook := func(kind, name string) error {
			if name == "" {
				return nil
			}
			act, ok := byActionName[name]
			if !ok {
				return &BuildError{st.Line, fmt.Sprintf("stream %q %s -> %s: no such action", st.Path, kind, name)}
			}
			if len(act.Params) != len(params) {
				return &BuildError{st.Line, fmt.Sprintf("stream %q %s -> %s: the action takes the path's parameters (%s), no more and no fewer", st.Path, kind, name, strings.Join(params, ", "))}
			}
			for _, p := range act.Params {
				found := false
				for _, pp := range params {
					found = found || pp == p.Name
				}
				if !found {
					return &BuildError{st.Line, fmt.Sprintf("stream %q %s -> %s: parameter %q is not one of the path's {params}", st.Path, kind, name, p.Name)}
				}
			}
			act.Placement = Server
			act.Reason = fmt.Sprintf("runs on %s of stream %s — the authority holds the connection", kind, st.Path)
			return nil
		}
		var connects []StreamHook
		for _, h := range st.Connects {
			if err := hook("connect", h.Action); err != nil {
				return nil, err
			}
			if h.Event != "" {
				var evType string
				for _, ev := range events {
					if ev.Name == h.Event {
						evType = ev.Type
					}
				}
				if evType == "" {
					return nil, &BuildError{st.Line, fmt.Sprintf("stream %q connect -> %s as %s: the stream carries no event %q", st.Path, h.Action, h.Event, h.Event)}
				}
				if a := byActionName[h.Action]; a.Ret != evType || a.RetList {
					return nil, &BuildError{st.Line, fmt.Sprintf("stream %q connect -> %s as %s: the action must return %s, the event's payload", st.Path, h.Action, h.Event, evType)}
				}
			}
			connects = append(connects, StreamHook{Action: h.Action, Event: h.Event})
		}
		if err := hook("disconnect", st.Disconnect); err != nil {
			return nil, err
		}
		var sdocs []APIParamDoc
		for _, d := range st.ParamDocs {
			isParam := false
			for _, p := range params {
				isParam = isParam || p == d.Name
			}
			if !isParam {
				return nil, &BuildError{d.Line, fmt.Sprintf("stream %q documents %q, which is not one of its {path} parameters", st.Path, d.Name)}
			}
			if d.Type != "text" || d.List || d.Optional || d.Default != nil || len(d.Aliases) > 0 || d.Into != "" {
				return nil, &BuildError{d.Line, fmt.Sprintf("stream %q: a path parameter is text: %s: text \"description\" [one of …]", st.Path, d.Name)}
			}
			sdocs = append(sdocs, APIParamDoc{Name: d.Name, Type: d.Type, Description: d.Description, Enum: d.Enum})
		}
		out.Streams = append(out.Streams, Stream{Path: st.Path, Events: events, Requires: st.Requires, Auth: auth, Rate: st.Rate, Since: st.Since,
			Params: params, Connects: connects, Disconnect: st.Disconnect, Hello: st.Hello, Summary: st.Summary, Description: st.Description, ParamDocs: sdocs, Errors: st.Errors})
	}
	for _, st := range app.Streams {
		if line, ok := apiSeen["GET "+st.Path]; ok {
			return nil, &BuildError{line, fmt.Sprintf("api GET %q is also declared as a stream (line %d) — a route is one or the other", st.Path, st.Line)}
		}
	}
	// An emit into a parameterized stream names its instance (`on id`), and
	// one into a plain stream names none.
	checkOn := func(em emittedEvent) error {
		for _, st := range app.Streams {
			for _, ev := range st.Events {
				if ev.Type != em.typ || (em.name != "" && ev.Name != em.name) {
					continue
				}
				if n := streamParams[st.Path]; n != em.on {
					if n == 0 {
						return &BuildError{em.line, fmt.Sprintf("emit %s{…} on …: stream %q has no path parameters to name", em.typ, st.Path)}
					}
					return &BuildError{em.line, fmt.Sprintf("emit %s{…}: stream %q is one per %d path value(s) — name the instance: emit … %s{…} on <value>", em.typ, st.Path, n, em.typ)}
				}
			}
		}
		return nil
	}
	for _, em := range append(append([]emittedEvent{}, e.emittedEvents...), e.unnamedEmits...) {
		if err := checkOn(em); err != nil {
			return nil, err
		}
	}
	for _, em := range e.emittedEvents {
		if !carriesNamed[em.name+" "+em.typ] {
			return nil, &BuildError{em.line, fmt.Sprintf("emit %s %s{…}: no stream carries an event %q with a %s payload", em.name, em.typ, em.name, em.typ)}
		}
	}
	for typ, line := range e.emittedTypes {
		if !carried[typ] {
			return nil, &BuildError{line, fmt.Sprintf("emit %s{…}: no stream carries %q — declare one: stream \"/path\": %s", typ, typ, typ)}
		}
	}
	// An unnamed emit of a type one stream carries under several names could
	// mean any of them; the author must say which.
	for _, u := range e.unnamedEmits {
		for path, byType := range namesOf {
			if names := byType[u.typ]; len(names) > 1 {
				return nil, &BuildError{u.line, fmt.Sprintf("emit %s{…}: stream %q carries %s as %s — name the event: emit %s %s{…}", u.typ, path, u.typ, strings.Join(names, " and "), names[0], u.typ)}
			}
		}
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
			return nil, &BuildError{0, fmt.Sprintf("%s references unknown action %q", ref.source(), ref.name)}
		}
		if len(act.Params) != ref.argc {
			// `more` (infinite scroll) always calls its action with zero arguments —
			// there is no argument list an author could write for it — so a mismatch
			// there means the action itself takes arguments, explained accordingly.
			// Every other source (button, form, on-change) has a real argument list
			// at the call site, so its mismatch is the ordinary arity message, just
			// attributed to what actually named it.
			if ref.via == "`more`" {
				return nil, &BuildError{0, fmt.Sprintf("%s names action %q, which takes %d argument(s); loading the next page invokes a zero-argument action", ref.source(), ref.name, len(act.Params))}
			}
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
			if queried[f.Name] && f.Password {
				return nil, &BuildError{0, fmt.Sprintf(
					"field %q is @password and cannot be used in a `where`, `by`, or relation; it stores only a salted one-way hash — check a candidate against it with verifyPassword in an action body", f.Name)}
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
	if a.Ret != "" {
		// A reply value's type: a primitive, an entity (the reply is a row),
		// or a wire type (a DTO). A struct is proc-local and has no wire form.
		if e.structs[a.Ret] != nil {
			return Action{}, &BuildError{a.Line, fmt.Sprintf("action %q returns %s, a proc-only type with no wire form — return a primitive, an entity row, or a wire `type`", a.Name, a.Ret)}
		}
		// `-> bytes`: the reply is a stored file (an upload, or a service's
		// `-> bytes` answer), served as the response body itself.
		if a.Ret == "bytes" && a.RetList {
			return Action{}, &BuildError{a.Line, fmt.Sprintf("action %q returns [bytes] — a reply carries one file", a.Name)}
		}
		if a.Ret != "bytes" && !isPrimitive(a.Ret) && !e.entities[a.Ret] && !e.wireTypes[a.Ret] && e.records[a.Ret] == nil {
			if _, isEnum := e.enums[a.Ret]; !isEnum {
				return Action{}, &BuildError{a.Line, fmt.Sprintf("action %q returns unknown type %q", a.Name, a.Ret)}
			}
		}
		act.Ret, act.RetList = a.Ret, a.RetList
	}
	// Record-typed locals (a `let v = call …` whose op returns a record) live for the
	// span of this action build, so a later `v.field` resolves against the record.
	e.locRecords = map[string]recBind{}
	e.rowLocals = map[string]string{}
	e.actLocalTypes = map[string]vtype{}
	e.actParams = map[string]bool{}
	defer func() { e.locRecords, e.rowLocals, e.actLocalTypes, e.actParams = nil, nil, nil, nil }()
	sealParams := map[string]bool{} // params whose value flows into an @e2e field (the client seals them before sending)
	paramSet := map[string]bool{}   // this action's parameter names
	loc := map[string]bool{"actor": true, "role": true, "verified": true, "tenant": true, "tenantRole": true, "session": true, sessionTokenRef: true}
	for _, p := range a.Params {
		if p.List && !isPrimitive(p.Type) && e.enums[p.Type] == nil {
			return Action{}, &BuildError{a.Line, fmt.Sprintf("action %q parameter %q is a list of %s — a list parameter holds scalars (int, text, bool, money, date, float, datetime or an enum), the values a client sends as a JSON array or a comma-separated query", a.Name, p.Name, p.Type)}
		}
		act.Params = append(act.Params, Param{Name: p.Name, Type: p.Type, Optional: p.Optional, List: p.List})
		e.actLocalTypes[p.Name] = vtype{core: p.Type, list: p.List}
		e.actParams[p.Name] = true
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
	usesPrint := false          // calls print(...) — unconditionally server-executed, see printCap
	usesSecret := false         // handles an authentication secret (authorityCap builtins, sessionToken) — unconditionally server-executed
	callsService := false       // calls an external service (an effect)
	callsProc := false          // calls a proc (`do`) — unconditionally server-executed
	establishesID := false      // sets the session identity (`establish`)
	setsHeader := false         // sets a response header (`header "Name" expr`)
	emits := false              // puts an event on a stream (`emit`) — only the authority holds subscribers

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
		if hasPrint(ex) {
			usesPrint = true
		}
		if handlesSecret(ex) {
			usesSecret = true
		}
		for n := range e.depsIR(e.low(ex)) {
			if _, isState := e.states[n]; isState {
				reads[n] = true
			}
		}
		return nil
	}

	// bindLocal admits a new action-local name into a block's scope: a `let`, a
	// `for`'s item variable, or a bound call/proc/add result. It must be fresh
	// (not a parameter, a builtin, or an earlier local of this block or an
	// enclosing one) and must not shadow a state cell or an entity — a local
	// named like a state cell would make a later `name = expr` ambiguous
	// between "reassign the local" (which an action cannot do) and "write the
	// cell" (which it can), and one named like an entity would hide the
	// collection from every expression after it.
	bindLocal := func(name string, locals map[string]bool, what string, line int) error {
		if locals[name] {
			return &BuildError{line, fmt.Sprintf("%q is already in scope — pick another name for %s", name, what)}
		}
		if _, isState := e.states[name]; isState {
			return &BuildError{line, fmt.Sprintf("%s %q would shadow the state cell of the same name — pick another name", what, name)}
		}
		if e.entities[name] {
			return &BuildError{line, fmt.Sprintf("%s %q would shadow the entity of the same name — pick another name", what, name)}
		}
		locals[name] = true
		return nil
	}

	// block lowers one statement list — the action's own body, or the nested
	// body of a `for`/`if`/`else` — against loc, the names in scope there. A
	// nested block gets a copy of its parent's scope widened by what the
	// statement binds (a `for`'s item variable), and the names a block's own
	// `let`s introduce never leak back out of it. Every flag the placement
	// switch below reads (writes/reads/entWrite/impure/…) is a closure over this
	// function's frame, so a write three blocks deep counts exactly as a
	// top-level one does — placement is a property of the whole body, however
	// it is nested.
	//
	// There is no "checks before mutations" ordering rule any more. A `check`
	// may sit anywhere — after a `let`, inside a `for` body, in an `else` —
	// because runtime/server.go rolls the whole action back (runtime/undo.go)
	// when one fails, so a check placed after a write is a guard on a
	// transaction, not a hole in one. That is what lets a per-row guard
	// ("enough stock for THIS line?") live next to the per-row write.
	var block func(stmts []ast.Stmt, loc map[string]bool) ([]Stmt, error)
	block = func(stmts []ast.Stmt, loc map[string]bool) ([]Stmt, error) {
		var body []Stmt
		readExpr := func(ex ast.Expr, line int) error { return readExprIn(ex, loc, line) }
		for _, s := range stmts {
			switch st := s.(type) {
			case ast.Check:
				if err := e.checkPureIn(st.Cond, loc, st.Line, "a check", true); err != nil {
					return nil, err
				}
				if handlesSecret(st.Cond) {
					usesSecret = true
				}
				out := Stmt{Op: "check", Value: e.low(st.Cond), Msg: st.Msg, Status: st.Status, Target: st.Code}
				if st.MsgExpr != nil {
					// `"No league named {league}."`: read in the action's scope
					// when the check fails.
					if err := readExpr(st.MsgExpr, st.Line); err != nil {
						return nil, err
					}
					out.Key = e.low(st.MsgExpr)
				}
				body = append(body, out)
			case ast.Assign:
				p, ok := e.states[st.Target]
				if !ok {
					return nil, &BuildError{st.Line, fmt.Sprintf("assignment to unknown state %q", st.Target)}
				}
				_ = p
				if err := readExpr(st.Value, st.Line); err != nil {
					return nil, err
				}
				writes[st.Target] = true
				body = append(body, Stmt{Op: "assign", Target: st.Target, Value: e.low(st.Value)})
			case ast.Add:
				if !e.entities[st.Entity] {
					return nil, &BuildError{st.Line, fmt.Sprintf("add to unknown entity %q", st.Entity)}
				}
				entWrite = true
				out := Stmt{Op: "add", Entity: st.Entity}
				for _, fi := range st.Fields {
					if e.entityDerives[st.Entity][fi.Name] {
						return nil, &BuildError{st.Line, fmt.Sprintf(
							"entity %q's %q is a derive, not a stored field — it is computed from the row's own other fields on every read and cannot be set in `add`", st.Entity, fi.Name)}
					}
					isE2E, err := e2eWrite(st.Entity, fi.Name, fi.Expr, st.Line)
					if err != nil {
						return nil, err
					}
					// A sealed value is opaque ciphertext to the authority: don't read it (it
					// is a validated parameter), just carry the ref so the row stores it.
					if !isE2E {
						if err := readExpr(fi.Expr, st.Line); err != nil {
							return nil, err
						}
					}
					out.Fields = append(out.Fields, FieldInit{Name: fi.Name, Expr: e.low(fi.Expr)})
				}
				if st.Bind != "" {
					// `let id = add Entity { … }`: the new row's id, for the rest of
					// this block — the way a second write names the row the first
					// one just created.
					if err := bindLocal(st.Bind, loc, "the new row's id", st.Line); err != nil {
						return nil, err
					}
					out.Bind = st.Bind
				}
				body = append(body, out)
			case ast.Set:
				if !e.entities[st.Entity] {
					return nil, &BuildError{st.Line, fmt.Sprintf("set on unknown entity %q", st.Entity)}
				}
				entWrite = true
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
						return nil, err
					}
					lw := e.low(st.Where)
					for n := range e.depsIR(lw) {
						if _, isState := e.states[n]; isState {
							reads[n] = true
						}
					}
					out := Stmt{Op: "set", Entity: st.Entity, Var: st.Var, Where: lw}

					for _, fi := range st.Fields {
						if e.entityDerives[st.Entity][fi.Name] {
							return nil, &BuildError{st.Line, fmt.Sprintf(
								"entity %q's %q is a derive, not a stored field — it is computed from the row's own other fields on every read and cannot be set", st.Entity, fi.Name)}
						}
						if !e.entityFields[st.Entity][fi.Name] {
							return nil, &BuildError{st.Line, fmt.Sprintf(
								"entity %q has no field %q to set", st.Entity, fi.Name)}
						}
						if e.entE2E[st.Entity][fi.Name] {
							// An @e2e field is sealed on the client from one action parameter, so
							// it has exactly one writable shape and a bulk update is not it: the
							// same ciphertext across every matching row is not the same value.
							return nil, &BuildError{st.Line, fmt.Sprintf(
								"@e2e field %s.%s cannot be written by a filtered set — a sealed value is encrypted per row on the client, so it can only be written straight from an action parameter to one row", st.Entity, fi.Name)}
						}
						// The assignment itself is an ordinary action value: it may read the
						// row, the action's parameters and the clock, exactly as the by-id
						// `set` may. Only the *predicate* has to be pure — it is what decides
						// which rows are touched, and a store has to be able to agree.
						prevRow, hadRow := e.rowLocals[st.Var]
						e.rowLocals[st.Var] = st.Entity
						err := readExprIn(fi.Expr, wl, st.Line)
						if hadRow {
							e.rowLocals[st.Var] = prevRow
						} else {
							delete(e.rowLocals, st.Var)
						}
						if err != nil {
							return nil, err
						}
						out.Fields = append(out.Fields, FieldInit{Name: fi.Name, Expr: e.low(fi.Expr)})
					}
					body = append(body, out)
					break
				}
				if e.entityDerives[st.Entity][st.Field] {
					return nil, &BuildError{st.Line, fmt.Sprintf(
						"entity %q's %q is a derive, not a stored field — it is computed from the row's own other fields on every read and cannot be set", st.Entity, st.Field)}
				}
				if err := readExpr(st.Key, st.Line); err != nil {
					return nil, err
				}
				isE2E, err := e2eWrite(st.Entity, st.Field, st.Value, st.Line)
				if err != nil {
					return nil, err
				}
				if !isE2E {
					if err := readExpr(st.Value, st.Line); err != nil {
						return nil, err
					}
				}
				body = append(body, Stmt{Op: "set", Entity: st.Entity, Field: st.Field,
					Key: e.low(st.Key), Value: e.low(st.Value)})
			case ast.Remove:
				if !e.entities[st.Entity] {
					return nil, &BuildError{st.Line, fmt.Sprintf("remove on unknown entity %q", st.Entity)}
				}
				entWrite = true
				if st.Where != nil {
					// Filtered delete: a pure predicate over the item var + action scope.
					wl := map[string]bool{st.Var: true}
					for k := range loc {
						wl[k] = true
					}
					if err := e.checkPure(st.Where, wl, st.Line, "a `remove … where` filter"); err != nil {
						return nil, err
					}
					lw := e.low(st.Where)
					// track state reads for soundness (the authority can't read @client state)
					for n := range e.depsIR(lw) {
						if _, isState := e.states[n]; isState {
							reads[n] = true
						}
					}
					body = append(body, Stmt{Op: "remove", Entity: st.Entity, Var: st.Var, Where: lw})
				} else {
					if err := readExpr(st.Key, st.Line); err != nil {
						return nil, err
					}
					body = append(body, Stmt{Op: "remove", Entity: st.Entity, Key: e.low(st.Key)})
				}
			case ast.Clear:
				if !e.entities[st.Entity] {
					return nil, &BuildError{st.Line, fmt.Sprintf("clear on unknown entity %q", st.Entity)}
				}
				entWrite = true
				body = append(body, Stmt{Op: "clear", Entity: st.Entity})
			case ast.ServiceCall:
				ops, ok := e.services[st.Service]
				if !ok {
					return nil, &BuildError{st.Line, fmt.Sprintf("call to unknown service %q", st.Service)}
				}
				argc, ok := ops[st.Op]
				if !ok {
					return nil, &BuildError{st.Line, fmt.Sprintf("service %q has no operation %q", st.Service, st.Op)}
				}
				if len(st.Args) != argc {
					return nil, &BuildError{st.Line, fmt.Sprintf("%s.%s expects %d argument(s), got %d", st.Service, st.Op, argc, len(st.Args))}
				}
				callsService = true
				cs := Stmt{Op: "call", Service: st.Service, Field: st.Op}
				for _, arg := range st.Args {
					if err := readExpr(arg, st.Line); err != nil {
						return nil, err
					}
					cs.Args = append(cs.Args, e.low(arg))
				}
				// Request→response: `let x = call …` binds the typed result into a local
				// so the rest of the body can use it (e.g. assign it into a state cell).
				if st.Bind != "" {
					ret := e.serviceRets[st.Service][st.Op]
					if ret.ret == "" {
						return nil, &BuildError{st.Line, fmt.Sprintf("%s.%s returns nothing — declare a return type (`%s(…) -> Type`) to bind it", st.Service, st.Op, st.Op)}
					}
					if err := bindLocal(st.Bind, loc, "the bound result", st.Line); err != nil {
						return nil, err
					}
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
				body = append(body, cs)
			case ast.Do:
				// A proc call: same-process, in-binary, synchronous — not egress like a
				// service call, so it is not the reason placement below forces the
				// server (see the placement switch, `case callsProc`). It still can only
				// ever run on the authority, because a proc is unconditionally
				// server-executed (no client mirror exists, or ever will, for it — see
				// ROADMAP.md), so an action that calls one has to be server-placed too:
				// a client-placed action runs in facet.js, which cannot run proc code.
				sig, ok := e.procSigs[st.Proc]
				if !ok {
					return nil, &BuildError{st.Line, fmt.Sprintf("do calls unknown proc %q", st.Proc)}
				}
				if len(st.Args) != len(sig.params) {
					return nil, &BuildError{st.Line, fmt.Sprintf("proc %q expects %d argument(s), got %d", st.Proc, len(sig.params), len(st.Args))}
				}
				callsProc = true
				ds := Stmt{Op: "do", Service: st.Proc}
				for _, arg := range st.Args {
					if err := readExpr(arg, st.Line); err != nil {
						return nil, err
					}
					ds.Args = append(ds.Args, e.low(arg))
				}
				if st.Bind != "" {
					if sig.ret == "" {
						return nil, &BuildError{st.Line, fmt.Sprintf("proc %q returns nothing — declare a return type (`proc %s(...) -> Type`) to bind it", st.Proc, st.Proc)}
					}
					if e.structs[sig.ret] != nil {
						return nil, &BuildError{st.Line, fmt.Sprintf("proc %q returns %s, a struct type usable only inside another proc — an action cannot bind it (structs are proc-local values, with no schema/wire representation yet; see LANGUAGE.md's `proc` section). Read its fields inside the proc and give %q a scalar/list return type instead", st.Proc, sig.ret, st.Proc)}
					}
					if err := bindLocal(st.Bind, loc, "the bound result", st.Line); err != nil {
						return nil, err
					}
					ds.Bind = st.Bind
					ds.Ret = sig.ret
					ds.RetList = sig.retList
					if _, isRec := e.records[sig.ret]; isRec {
						e.locRecords[st.Bind] = recBind{rec: sig.ret, list: sig.retList}
					}
				}
				body = append(body, ds)
			case ast.Restate:
				// Re-roling an account's live sessions is an identity change: the
				// authority's job, applied after the commit (runtime).
				establishesID = true
				if err := readExpr(st.Actor, st.Line); err != nil {
					return nil, err
				}
				if err := readExpr(st.Role, st.Line); err != nil {
					return nil, err
				}
				body = append(body, Stmt{Op: "restate", Value: e.low(st.Actor), Role: e.low(st.Role)})
			case ast.Header:
				if err := checkActionHeaderName(st.Name); err != nil {
					return nil, &BuildError{st.Line, err.Error()}
				}
				if err := readExpr(st.Value, st.Line); err != nil {
					return nil, err
				}
				if err := e.checkNoPrivate(st.Value); err != nil {
					return nil, &BuildError{st.Line, "a response header is sent to the caller, so it cannot carry a @private value"}
				}
				setsHeader = true
				body = append(body, Stmt{Op: "header", Msg: textproto.CanonicalMIMEHeaderKey(st.Name), Value: e.low(st.Value)})
			case ast.Revoke:
				// Ending a session is an identity change like establishing one: the
				// authority's job, and its effect lands after the commit (runtime).
				establishesID = true
				if err := readExpr(st.Session, st.Line); err != nil {
					return nil, err
				}
				body = append(body, Stmt{Op: "revoke", Value: e.low(st.Session)})
			case ast.Establish:
				// Adopt a custom session identity. Setting who you are is the authority's
				// job, so it forces server placement; the actor/role exprs are reads.
				establishesID = true
				if err := readExpr(st.Actor, st.Line); err != nil {
					return nil, err
				}
				// actor/role become the renderable session identity, so they cannot be a
				// @private value — that would copy the secret key into a renderable slot.
				if err := e.checkNoPrivate(st.Actor); err != nil {
					return nil, &BuildError{st.Line, "establish actor sets the renderable identity, so it cannot be a @private value — establish the handle and key policies on the @private UUID instead"}
				}
				es := Stmt{Op: "establish", Value: e.low(st.Actor)}
				if st.Role != nil {
					if err := readExpr(st.Role, st.Line); err != nil {
						return nil, err
					}
					if err := e.checkNoPrivate(st.Role); err != nil {
						return nil, &BuildError{st.Line, "establish role cannot be a @private value"}
					}
					es.Role = e.low(st.Role)
				}
				body = append(body, es)
			case ast.ExprStmt:
				// A bare builtin call for its side effect alone, its result discarded —
				// print(...) on its own line, the action-body counterpart to procBlock's
				// identical ast.ExprStmt case. Not print-specific machinery: readExpr's
				// e.check funnel already refuses any builtin that doesn't belong in an
				// action body (readFile/writeFile/httpGet/httpPost/channel/bitwise all
				// stay proc-only via checkNoIO/checkNoConcurrency/checkNoBitwise; float
				// itself is real everywhere now — see isPrimitive's doc), so print is
				// simply the one builtin this shape is actually useful for today.
				if err := readExpr(st.Call, st.Line); err != nil {
					return nil, err
				}
				body = append(body, Stmt{Op: "exprstmt", Value: e.low(st.Call)})
			case ast.Emit:
				// `emit Dto{…} [to expr]`: the value must be a literal of a wire type
				// some stream carries (checked once streams are built, in the
				// stream pass); here it is an ordinary action expression.
				// The payload is a wire-type literal, or any expression of a
				// wire type (a projection: `emit room roomDTO(f, me) on id`).
				lit, ok := st.Value.(ast.StructLit)
				if !ok {
					if vt := e.exprType(st.Value, e.actionScope(loc)); vt.known() && !vt.list && e.wireTypes[vt.core] {
						lit, ok = ast.StructLit{Type: vt.core}, true
					}
				}
				if !ok || !e.wireTypes[lit.Type] {
					return nil, &BuildError{st.Line, "emit takes a wire type value: emit EventType{field: value, …} or a projection returning one"}
				}
				if err := readExpr(st.Value, st.Line); err != nil {
					return nil, err
				}
				out := Stmt{Op: "emit", Field: lit.Type, Target: st.Event, Value: e.low(st.Value)}
				if st.To != nil {
					if err := readExpr(st.To, st.Line); err != nil {
						return nil, err
					}
					out.Key = e.low(st.To)
				}
				for _, x := range st.On {
					if err := readExpr(x, st.Line); err != nil {
						return nil, err
					}
					out.Args = append(out.Args, e.low(x))
				}
				emits = true
				e.emittedTypes[lit.Type] = st.Line
				if st.Event != "" {
					e.emittedEvents = append(e.emittedEvents, emittedEvent{name: st.Event, typ: lit.Type, line: st.Line, on: len(st.On)})
				} else {
					e.unnamedEmits = append(e.unnamedEmits, emittedEvent{typ: lit.Type, line: st.Line, on: len(st.On)})
				}
				body = append(body, out)
			case ast.Return:
				// `return expr`: the reply value. Its expression is an ordinary
				// action expression (rows, aggregates, parameters, locals, the
				// clock), so it goes through the same read bookkeeping.
				if st.Value == nil {
					if a.Ret != "" {
						return nil, &BuildError{st.Line, fmt.Sprintf("action %q returns %s, so `return` needs a value", a.Name, a.Ret)}
					}
					body = append(body, Stmt{Op: "return"})
					continue
				}
				if a.Ret == "" {
					return nil, &BuildError{st.Line, fmt.Sprintf("action %q declares no return type (`action %s(...) -> Type:`), so `return` cannot carry a value", a.Name, a.Name)}
				}
				if err := readExpr(st.Value, st.Line); err != nil {
					return nil, err
				}
				body = append(body, Stmt{Op: "return", Value: e.low(st.Value), Status: st.Status})
			case ast.Let:
				// `let name = expr`: an action-local bound once, visible for the rest
				// of this block. The value is an ordinary action expression — it may
				// read rows, parameters, state, and the clock — so it goes through the
				// same read bookkeeping every other value does.
				if err := readExpr(st.Value, st.Line); err != nil {
					return nil, err
				}
				if err := bindLocal(st.Name, loc, "the local", st.Line); err != nil {
					return nil, err
				}
				if row, ok := st.Value.(ast.EntityGet); ok && row.Field == "" {
					e.rowLocals[st.Name] = row.Entity
				}
				e.actLocalTypes[st.Name] = e.exprType(st.Value, e.actionScope(loc))
				body = append(body, Stmt{Op: "let", Target: st.Name, Value: e.low(st.Value)})
			case ast.IfStmt:
				if err := readExpr(st.Cond, st.Line); err != nil {
					return nil, err
				}
				then, err := block(st.Then, cloneNameSet(loc))
				if err != nil {
					return nil, err
				}
				var els []Stmt
				if len(st.Else) > 0 {
					if els, err = block(st.Else, cloneNameSet(loc)); err != nil {
						return nil, err
					}
				}
				body = append(body, Stmt{Op: "if", Value: e.low(st.Cond), Body: then, Else: els})
			case ast.ForStmt:
				// `for item in Entity [where cond] [by field] [limit n]:` over a block of
				// action statements. The predicate is pure, exactly as a `set … where`/
				// `remove … where` filter is — it decides which rows the body sees, and
				// a store has to be able to agree — while the body is ordinary action
				// code with the item variable in scope.
				if !e.entities[st.Coll] && loc[st.Coll] {
					// `for x in names:` over a list-valued local or parameter —
					// each element in order, the body run once per element.
					if st.Where != nil || st.Order != "" || st.Limit != nil {
						return nil, &BuildError{st.Line, fmt.Sprintf("`for %s in %s` walks a list value; where/by/limit filter an entity's rows — shape the list before the loop", st.Var, st.Coll)}
					}
					wl := cloneNameSet(loc)
					if err := bindLocal(st.Var, wl, "the loop variable", st.Line); err != nil {
						return nil, err
					}
					if lt := e.actLocalTypes[st.Coll]; lt.list {
						e.actLocalTypes[st.Var] = vtype{core: lt.core}
					}
					kids, err := block(st.Body, wl)
					if err != nil {
						return nil, err
					}
					body = append(body, Stmt{Op: "foreach", Var: st.Var, Value: &Expr{Kind: "ref", Name: st.Coll}, Body: kids})
					continue
				}
				if !e.entities[st.Coll] {
					return nil, &BuildError{st.Line, fmt.Sprintf("`for` in an action walks an entity's rows or a list-valued local; %q is not an entity or a local", st.Coll)}
				}
				wl := cloneNameSet(loc)
				if err := bindLocal(st.Var, wl, "the loop variable", st.Line); err != nil {
					return nil, err
				}
				e.rowLocals[st.Var] = st.Coll
				out := Stmt{Op: "for", Entity: st.Coll, Var: st.Var, Order: st.Order, Desc: st.Desc}
				if st.Where != nil {
					if err := e.checkPure(st.Where, wl, st.Line, "a `for … where` filter"); err != nil {
						return nil, err
					}
					out.Where = e.low(st.Where)
					for n := range e.depsIR(out.Where) {
						if _, isState := e.states[n]; isState {
							reads[n] = true
						}
					}
				}
				if st.Order != "" && !e.entityFields[st.Coll][st.Order] && st.Order != "id" {
					return nil, &BuildError{st.Line, fmt.Sprintf("entity %q has no field %q to order by", st.Coll, st.Order)}
				}
				if st.Limit != nil {
					if err := readExpr(st.Limit, st.Line); err != nil {
						return nil, err
					}
					out.Limit = e.low(st.Limit)
				}
				kids, err := block(st.Body, wl)
				delete(e.rowLocals, st.Var)
				if err != nil {
					return nil, err
				}
				out.Body = kids
				body = append(body, out)
			}
		}
		return body, nil
	}

	lowered, err := block(a.Body, loc)
	if err != nil {
		return Action{}, err
	}
	act.Body = lowered

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
		walkActionStmts(a.Body, func(s ast.Stmt) {
			switch st := s.(type) {
			case ast.Check:
				collect(st.Cond)
			case ast.Let:
				collect(st.Value)
			case ast.Return:
				collect(st.Value)
			case ast.Emit:
				collect(st.Value)
				collect(st.To)
			case ast.IfStmt:
				collect(st.Cond)
			case ast.ForStmt:
				collect(st.Where)
				collect(st.Limit)
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
			case ast.Do:
				for _, arg := range st.Args {
					collect(arg)
				}
			case ast.Establish:
				collect(st.Actor)
				collect(st.Role)
			case ast.Revoke:
				collect(st.Session)
			case ast.Header:
				collect(st.Value)
			case ast.Restate:
				collect(st.Actor)
				collect(st.Role)
			case ast.ExprStmt:
				collect(st.Call)
			}
		})
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
	//
	// …with one exception, and it is the reason the condition is not simply
	// `impure`. "Every client sees one agreed result" is a rule about a SHARED
	// result. An action whose every write lands in `@client` state has no shared
	// result to agree on: that state is per-browser and ephemeral by definition,
	// so two browsers holding different values is not a disagreement, it is what
	// `@client` means.
	//
	// Without the exception a whole category of ordinary UI is unwritable. "Mark
	// what I have already seen" is `seenAt = now()` into a client cell, and it
	// was refused from both directions at once: the authority must run it because
	// it is impure, and the authority cannot write client state — so the action
	// could not be placed anywhere. The clock is available on both sides
	// (assets/facet.js implements `now` and `rand` in evCall), so the client can
	// simply run it.
	//
	// Anything genuinely shared still goes to the authority: an entity write is
	// caught above, a write to a `@server` cell below, and a service call or an
	// identity change in their own arms here.
	if !callsProc && stmtsCall(act.Body, func(name string) bool {
		_, isProc := e.procSigs[name]
		return isProc || e.procDerives[name]
	}) != "" {
		callsProc = true
	}
	// A builtin the browser does not implement (parser.BuiltinSiteOf) pins the
	// action to the authority, whatever state it writes.
	serverBuiltin := stmtsCall(act.Body, func(name string) bool {
		site, ok := parser.BuiltinSiteOf(name)
		return ok && site != parser.SiteEverywhere
	})
	act.Placement = Client
	act.Reason = "only touches @client state, so it runs in the browser with no round-trip"
	switch {
	case entWrite:
		act.Placement = Server
		act.Reason = "writes durable entity data — the authority owns the database"
	case usesPrint:
		// Unlike now/rand (the `impure` case below), print has no client-side
		// implementation and no "only writes @client state" escape hatch would
		// make sense for it even if it did: the entire point of print(...) is a
		// line in the AUTHORITY's own log output, so an action that calls it
		// must always run there, regardless of what state it writes.
		act.Placement = Server
		act.Reason = "calls print(...) — a server-only debugging aid with no client implementation; the authority is the only process with log output to write it to"
	case usesSecret:
		act.Placement = Server
		act.Reason = "handles an authentication secret or a stored file (verifyPassword/totpSecret/totpValid/randomToken/sessionToken/fileDigest) — only the authority holds it"
	case impure && !writesOnlyClientState(writes, e.states):
		act.Placement = Server
		act.Reason = "uses an effectful builtin (now/rand) — the authority owns nondeterminism, so every client sees one agreed result"
	case callsService:
		act.Placement = Server
		act.Reason = "calls an external service — egress routes through the authority, never the client"
	case callsProc:
		act.Placement = Server
		act.Reason = "calls a `proc`, which is unconditionally server-executed — no client mirror of proc code exists"
	case establishesID:
		act.Placement = Server
		act.Reason = "establishes the session identity — only the authority may set who you are"
	case setsHeader:
		act.Placement = Server
		act.Reason = "sets a response header — only the authority answers the HTTP request"
	case a.Ret != "":
		// A reply value is computed once, by the authority, and read back from
		// the reply — the browser's own runner returns nothing, so a returning
		// action has exactly one place it can run.
		act.Placement = Server
		act.Reason = "returns a value — the authority computes the reply"
	case emits:
		act.Placement = Server
		act.Reason = "emits a stream event — only the authority holds the subscribers"
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
	if act.Placement == Client && serverBuiltin != "" {
		// Last, so an action with any other reason to run on the authority is
		// reported by that reason.
		act.Placement = Server
		act.Reason = fmt.Sprintf("calls %s(...), which only the authority implements — the browser has no mirror of it", serverBuiltin)
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

// proc lowers a `proc` declaration. Deliberately NOT built by action() above: a
// proc needs none of what makes that function big — no placement inference (a
// proc is unconditionally server-executed), no @e2e seal-dataflow tracking, no
// mustValidateFirst check-ordering, no policy gate. It is pure computation
// checked against its own small scope (its parameters plus `let`-bound locals),
// not the action/session model — entities and state are out of reach in this
// milestone (see checkProcExpr).
//
// Milestone 2 adds real control flow (`loop`/`if`, each with a nested,
// recursive statement block), so the statement lowering itself is recursive —
// see procBlock, which this delegates its whole body to. Once every path is
// lowered, a return-typed proc is checked for return-completeness (every
// execution path must reach a `return`) by stmtsReturnComplete, below.
func (e *env) proc(p *ast.Proc) (Proc, error) {
	return e.procIn(p, nil)
}

// isProcessEntry: `proc main(args: [text]) -> int`, the program's entry point
// when it is run as a command (`facet exec`, runtime/stdio.go's RunMain).
func isProcessEntry(p *ast.Proc) bool {
	return p.Name == "main" && len(p.Params) == 1 && p.Params[0].List && p.Params[0].Type == "text" && p.Ret == "int" && !p.RetList
}

// procIn lowers p; actionSigs non-nil lowers it as a daemon-context body
// (the process entry point), exactly as e.daemon lowers a daemon's.
func (e *env) procIn(p *ast.Proc, actionSigs map[string]actionSig) (Proc, error) {
	e.inProc = true
	defer func() { e.inProc = false }()
	pr := Proc{Name: p.Name, Ret: p.Ret, RetList: p.RetList}
	locals := map[string]bool{}  // every name in scope: params + `let`s seen so far
	mutable := map[string]bool{} // the subset declared `let mut`, and so reassignable
	types := map[string]string{} // each name's declared/inferred type — see checkBitwiseTypes
	for _, prm := range p.Params {
		if locals[prm.Name] {
			return Proc{}, &BuildError{p.Line, fmt.Sprintf("proc %q has duplicate parameter %q", p.Name, prm.Name)}
		}
		locals[prm.Name] = true
		// A list-typed parameter (`p: [T]`) is tagged arrayType, exactly like a
		// list literal or an append(...) result — see inferProcType/
		// checkIndexTypes — not its element type, so `p[i]`/`len(p)`/passing p to
		// another proc's list parameter all type-check against it the same way
		// they already do for a `let mut xs = [1,2,3]` local. Only the element
		// type (prm.Type) is real to the runtime/database/wire layers, so it is
		// still what's recorded on the lowered Param below.
		if prm.List {
			types[prm.Name] = arrayType
		} else {
			types[prm.Name] = prm.Type
		}
		pr.Params = append(pr.Params, Param{Name: prm.Name, Type: prm.Type, Optional: prm.Optional, List: prm.List})
	}
	for _, prm := range p.Params {
		if err := e.checkNotShared(prm.Name, p.Line); err != nil {
			return Proc{}, err
		}
	}
	e.seedSharedTypes(types)
	body, err := e.procBlock(p, p.Body, locals, mutable, types, 0, actionSigs)
	if err != nil {
		// A detached proc that is not daemon-context: say which reference
		// made it an ordinary proc (detachctx.go).
		if be, ok := err.(*BuildError); ok && actionSigs == nil && e.daemonCtxWhy[p.Name] != "" &&
			(strings.Contains(be.Msg, "only available inside a daemon body") || strings.Contains(be.Msg, "only valid inside a daemon body")) {
			return Proc{}, &BuildError{be.Line, fmt.Sprintf("%s (proc %q is started by `detach`, but it is also reached by %s, so it runs as an ordinary proc, possibly under a caller's lock)", be.Msg, p.Name, e.daemonCtxWhy[p.Name])}
		}
		return Proc{}, err
	}
	e.lowerSharedRefs(body)
	pr.Body = body
	if len(pr.Body) == 0 {
		return Proc{}, &BuildError{p.Line, fmt.Sprintf("proc %q has no body", p.Name)}
	}
	if p.Ret != "" && !stmtsReturnComplete(pr.Body) {
		return Proc{}, &BuildError{p.Line, fmt.Sprintf(
			"proc %q declares a return type %s, so its body must end with `return <expr>` on every path — an `if` used as the last statement needs an `else`, and both branches must themselves end that way", p.Name, p.Ret)}
	}
	return pr, nil
}

// daemon lowers one detached, process-lifetime background task (see
// ast.Daemon's doc). Its body is proc-shaped — the exact control-flow/locals
// model procBlock already implements for a real proc, since a daemon has no
// parameters and no return either — with actionSigs passed through non-nil,
// which is what lets procBlock accept `act ActionName(args)` here (and only
// here: e.proc, above, always passes nil).
//
// The body is lowered against a synthetic *ast.Proc wrapping the daemon's own
// Uses/Line (never registered in e.procSigs, e.entities, or anywhere else a
// real declaration would be — nothing can `do`/`spawn` a daemon by name, and
// nothing needs to: it is started once, directly, by runtime/daemon.go). This
// is deliberate reuse, not a hack: a daemon body genuinely has the same shape
// procBlock already knows how to lower (no placement inference, no entity
// access except through `act`, the same locals/control-flow/capability
// rules), so giving it a second, parallel lowering function would only
// duplicate procBlock's ~400 lines for no behavioral difference.
func (e *env) daemon(d *ast.Daemon, actionSigs map[string]actionSig) (Daemon, error) {
	e.inDaemon = true
	defer func() { e.inDaemon = false }()
	seenUse := map[string]bool{}
	for _, u := range d.Uses {
		if !knownCapabilities[u] {
			return Daemon{}, &BuildError{d.Line, fmt.Sprintf("daemon %q declares unknown capability %q — known capabilities: io.console, io.env, io.file, io.net, io.net.listen", d.Name, u)}
		}
		if seenUse[u] {
			return Daemon{}, &BuildError{d.Line, fmt.Sprintf("daemon %q declares capability %q more than once", d.Name, u)}
		}
		seenUse[u] = true
	}
	fake := &ast.Proc{Name: "daemon " + d.Name, Uses: d.Uses, Line: d.Line}
	types := map[string]string{}
	e.seedSharedTypes(types)
	body, err := e.procBlock(fake, d.Body, map[string]bool{}, map[string]bool{}, types, 0, actionSigs)
	if err != nil {
		return Daemon{}, err
	}
	e.lowerSharedRefs(body)
	if len(body) == 0 {
		return Daemon{}, &BuildError{d.Line, fmt.Sprintf("daemon %q has no body", d.Name)}
	}
	return Daemon{Name: d.Name, Every: d.Every, Body: body}, nil
}

// cloneNameSet copies a name-set map so a nested block can add its own
// declarations (`let`) without those leaking back into the caller's scope once
// the block ends, while still seeing everything the caller had already
// declared (the copy starts as a full snapshot of it).
// walkActionStmts visits every statement of an action body in source order,
// descending into the nested blocks a `for`/`if`/`else` carries, so a pass that
// needs to see every statement (the @e2e seal-dataflow walk) sees one three
// levels down exactly as it sees one at the top.
func walkActionStmts(body []ast.Stmt, visit func(ast.Stmt)) {
	for _, s := range body {
		visit(s)
		switch st := s.(type) {
		case ast.ForStmt:
			walkActionStmts(st.Body, visit)
		case ast.IfStmt:
			walkActionStmts(st.Then, visit)
			walkActionStmts(st.Else, visit)
		}
	}
}

func cloneNameSet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneTypeMap is cloneNameSet's counterpart for a proc's per-local type map
// (see checkBitwiseTypes) — the same reason: a `let` declared inside a nested
// `loop`/`if` block must not leak its type back into the caller's map once the
// block ends.
func cloneTypeMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// procBlock lowers one nested statement list belonging to proc p: either the
// proc's own top-level body, or a `loop`/`if` statement's own Body/Then/Else
// (recursing into itself for those two — the one genuinely recursive structural
// shape Milestone 2 adds). locals/mutable are cloned on entry so a `let`
// declared in this block cannot leak into the caller's scope once the block
// ends, while everything the caller already declared stays visible here.
// loopDepth counts the number of enclosing `loop`s, so break/continue can be
// rejected outside one. Within THIS block, a `return`/`break`/`continue` must
// be the last statement — anything after it is unreachable — but that
// restriction is per-block, not per-proc, which is what lets `return` appear
// from inside a nested loop/if while dead code after it is still refused.
// actionSigs is nil while lowering a real proc's body (e.proc never passes
// one) and non-nil while lowering a daemon's (e.daemon does) — the one
// difference between the two: it is both what makes `act ActionName(args)`
// resolve at all and, by its nilness, what makes `act` a compile error inside
// an ordinary proc. See ast.Act's and ast.Daemon's docs.
func (e *env) procBlock(p *ast.Proc, stmts []ast.Stmt, locals, mutable map[string]bool, types map[string]string, loopDepth int, actionSigs map[string]actionSig) ([]Stmt, error) {
	locals = cloneNameSet(locals)
	mutable = cloneNameSet(mutable)
	types = cloneTypeMap(types)
	// pendingSpawns is checkSpawnsJoined's own state: every `spawn`-bound
	// handle declared IN THIS EXACT BLOCK that hasn't been `join`ed yet,
	// keyed by handle name to the spawned proc's signature (join needs it to
	// type its bound result). Deliberately NOT cloned/threaded like locals/
	// mutable/types above — a handle's whole lifetime (spawn to join) must
	// fit inside one statement list, never spanning into or out of a nested
	// loop/if block, so each procBlock call starts this fresh and checks it
	// empty before returning. See ast.Spawn's doc for why "same block" is
	// this milestone's chosen structured-concurrency enforcement boundary.
	pendingSpawns := map[string]spawnedTask{}
	var out []Stmt
	for i, s := range stmts {
		last := i == len(stmts)-1
		switch st := s.(type) {
		case ast.Let:
			if locals[st.Name] {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is already declared in proc %q", st.Name, p.Name)}
			}
			if err := e.checkNotShared(st.Name, st.Line); err != nil {
				return nil, err
			}
			if err := e.checkProcExpr(p, st.Value, locals, types, st.Line, actionSigs); err != nil {
				return nil, err
			}
			locals[st.Name] = true
			types[st.Name] = e.structExprType(st.Value, types)
			if st.Mut {
				mutable[st.Name] = true
			}
			out = append(out, Stmt{Op: "let", Target: st.Name, Value: e.low(st.Value)})
		case ast.Assign:
			// A reassignment: only legal against a local this proc already declared
			// `let mut` — a bare `let` local is immutable, and an unknown name is not
			// state (a proc has none in this milestone), so both are compile errors
			// rather than the runtime silently creating or overwriting something.
			// The one non-local a proc may assign is a `shared` cell (see
			// ast.Shared): the whole value is replaced atomically.
			if sh := e.shareds[st.Target]; sh != nil && !locals[st.Target] {
				if err := e.checkProcExpr(p, st.Value, locals, types, st.Line, actionSigs); err != nil {
					return nil, err
				}
				if err := e.checkSharedAssign(sh, st.Value, types, st.Line); err != nil {
					return nil, err
				}
				out = append(out, Stmt{Op: "exprstmt", Value: &Expr{Kind: "call", Name: sharedSetIntrinsic, Args: []*Expr{{Kind: "lit", Val: sh.Name, VType: "text"}, e.low(st.Value)}}})
				continue
			}
			if !locals[st.Target] {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is not declared in proc %q — use `let %s = …` first", st.Target, p.Name, st.Target)}
			}
			if !mutable[st.Target] {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is not mutable — declare it `let mut %s = …` to reassign it", st.Target, st.Target)}
			}
			if err := e.checkProcExpr(p, st.Value, locals, types, st.Line, actionSigs); err != nil {
				return nil, err
			}
			out = append(out, Stmt{Op: "assign", Target: st.Target, Value: e.low(st.Value)})
		case ast.IndexAssign:
			// `xs[i] = expr` — an array element mutation, or `m[k] = expr` — a map
			// insert-or-overwrite. Gated exactly like a plain `name = expr`
			// reassignment (ast.Assign, above), since it mutates the value Target is
			// bound to: Target must already be a declared `let mut` local. On top of
			// that, the type map lets this catch the common case of indexing
			// something that plainly isn't an array or map at compile time (see
			// checkIndexTypes) — the index itself is never bounds-checked here; that
			// can only be a runtime error (runtime/proccompile.go,
			// "indexset"), since bounds are data-dependent (and, for a map, so is
			// whether the key is already present).
			if !locals[st.Target] {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is not declared in proc %q — use `let %s = …` first", st.Target, p.Name, st.Target)}
			}
			if !mutable[st.Target] {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is not mutable — declare it `let mut %s = …` to index-assign into it", st.Target, st.Target)}
			}
			if ty := types[st.Target]; ty != "" && ty != arrayType && ty != bytesType && ty != mapType {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is not an array or map (its type is %s) — index assignment (`%s[...] = …`) needs an array, map, or byte-buffer local", st.Target, ty, st.Target)}
			}
			if err := e.checkProcExpr(p, st.Index, locals, types, st.Line, actionSigs); err != nil {
				return nil, err
			}
			if err := e.checkProcExpr(p, st.Value, locals, types, st.Line, actionSigs); err != nil {
				return nil, err
			}
			// The key-type restriction (int/text only) applies to a map SET too —
			// checkProcExpr's own checkMapKeyTypes call above only recognizes a key
			// inside an ast.Index READ expression, so a bare `st.Index` here (the
			// write side, carried on the IndexAssign statement rather than wrapped
			// in its own ast.Index node) needs the same check spelled out
			// explicitly. Silent ("") when the key's type can't be proven — the
			// runtime backstop (runtime/eval.go's mapKey, reached via
			// runtime/proccompile.go's "indexset" case) catches that case instead.
			if types[st.Target] == mapType {
				if kt := inferProcType(st.Index, types); !isMapKeyType(kt) {
					return nil, &BuildError{st.Line, fmt.Sprintf("map key must be int or text, got %s", kt)}
				}
			}
			// Bytes: true tells the runtime (runtime/proccompile.go's "indexset" case) to
			// range-check the written value to 0-255 and reject anything outside it
			// as a clean error, rather than accepting any int the way a plain array
			// index-write does — the domain invariant a byte buffer exists to
			// enforce. types[st.Target] is exact here (not a "can't prove" guess):
			// bytesType can only reach a local via bytes(...), a list literal never
			// produces it, and a proc param/return can't be array/bytes/map-typed
			// (see isPrimitive), so every value ever bound to a bytesType local was
			// created by bytes(...) or copied from another bytesType local.
			out = append(out, Stmt{Op: "indexset", Target: st.Target, Key: e.low(st.Index), Value: e.low(st.Value), Bytes: types[st.Target] == bytesType})
		case ast.FieldAssign:
			// `s.field = expr` — gated exactly like an index write (ast.IndexAssign
			// above): the local must be a declared `let mut`, its type a struct
			// that has the field, and the value's type — when both are provable —
			// the field's. The write itself is in place (runtime "fieldset"),
			// which cloneCompositeValue makes safe for the same reason it makes
			// an index write safe: no two locals ever share a struct's map.
			if !locals[st.Target] {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is not declared in proc %q — use `let %s = …` first", st.Target, p.Name, st.Target)}
			}
			if !mutable[st.Target] {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is not mutable — declare it `let mut %s = …` to assign its fields", st.Target, st.Target)}
			}
			fields, isStruct := e.structs[types[st.Target]]
			if !isStruct {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is not a struct (its type is %s) — a field write (`%s.%s = …`) needs a struct local", st.Target, types[st.Target], st.Target, st.Field)}
			}
			sf, hasField := fields[st.Field]
			if !hasField {
				return nil, &BuildError{st.Line, fmt.Sprintf("struct %q has no field %q", types[st.Target], st.Field)}
			}
			if err := e.checkProcExpr(p, st.Value, locals, types, st.Line, actionSigs); err != nil {
				return nil, err
			}
			if vt := inferProcType(st.Value, types); vt != "" && !sf.list && vt != sf.typ && !(sf.typ == "float" && vt == "int") {
				return nil, &BuildError{st.Line, fmt.Sprintf("cannot assign %s to field %q of struct %q, whose type is %s", vt, st.Field, types[st.Target], sf.typ)}
			}
			out = append(out, Stmt{Op: "fieldset", Target: st.Target, Field: st.Field, Value: e.low(st.Value)})
		case ast.Do:
			sig, ok := e.procSigs[st.Proc]
			if !ok {
				return nil, &BuildError{st.Line, fmt.Sprintf("do calls unknown proc %q", st.Proc)}
			}
			if len(st.Args) != len(sig.params) {
				return nil, &BuildError{st.Line, fmt.Sprintf("proc %q expects %d argument(s), got %d", st.Proc, len(sig.params), len(st.Args))}
			}
			ds := Stmt{Op: "do", Service: st.Proc}
			for _, arg := range st.Args {
				if err := e.checkProcExpr(p, arg, locals, types, st.Line, actionSigs); err != nil {
					return nil, err
				}
				ds.Args = append(ds.Args, e.low(arg))
			}
			if st.Reassign {
				// `name = do …`: the same gate a plain reassignment has, then the
				// result lands in the EXISTING local (Stmt.Target, which the
				// runtime writes through frame.set, so a local declared outside a
				// loop and reassigned inside it is the one updated).
				if !locals[st.Bind] {
					return nil, &BuildError{st.Line, fmt.Sprintf("%q is not declared in proc %q — use `let %s = …` first", st.Bind, p.Name, st.Bind)}
				}
				if !mutable[st.Bind] {
					return nil, &BuildError{st.Line, fmt.Sprintf("%q is not mutable — declare it `let mut %s = …` to reassign it", st.Bind, st.Bind)}
				}
				if sig.ret == "" {
					return nil, &BuildError{st.Line, fmt.Sprintf("proc %q returns nothing — declare a return type to assign its result", st.Proc)}
				}
				if sig.retList {
					types[st.Bind] = arrayType
				} else {
					types[st.Bind] = sig.ret
				}
				ds.Target = st.Bind
				ds.Ret = sig.ret
				ds.RetList = sig.retList
				out = append(out, ds)
				continue
			}
			if st.Bind != "" {
				if locals[st.Bind] {
					return nil, &BuildError{st.Line, fmt.Sprintf("%q is already in scope — pick another name for the bound result", st.Bind)}
				}
				if sig.ret == "" {
					return nil, &BuildError{st.Line, fmt.Sprintf("proc %q returns nothing — declare a return type to bind it", st.Proc)}
				}
				if err := e.checkNotShared(st.Bind, st.Line); err != nil {
					return nil, err
				}
				locals[st.Bind] = true
				if st.Mut {
					mutable[st.Bind] = true
				}
				// Same arrayType tagging as a parameter (see e.proc's doc): a
				// list-returning proc's result must be tagged arrayType, not its
				// element type, or a later `st.Bind[i]`/`len(st.Bind)` inside THIS
				// proc would wrongly be rejected by checkIndexTypes as "not an
				// array" — exactly the composition shape a proc chaining into
				// another proc's list-typed result needs.
				if sig.retList {
					types[st.Bind] = arrayType
				} else {
					types[st.Bind] = sig.ret
				}
				ds.Bind = st.Bind
				ds.Ret = sig.ret
				ds.RetList = sig.retList
			}
			out = append(out, ds)
		case ast.Act:
			// `act ActionName(args)` — the one way a daemon body touches
			// entity/session state (see ast.Act's doc). actionSigs is nil for
			// a real proc's body, which is exactly what makes this a compile
			// error there rather than a silent no-op or a runtime surprise.
			if actionSigs == nil {
				return nil, &BuildError{st.Line, fmt.Sprintf("act is only valid inside a daemon body — %q is a proc, not a daemon", p.Name)}
			}
			sig, ok := actionSigs[st.Action]
			if !ok {
				return nil, &BuildError{st.Line, fmt.Sprintf("act calls unknown action %q", st.Action)}
			}
			if sig.placement != Server {
				return nil, &BuildError{st.Line, fmt.Sprintf("act calls client-placed action %q; a daemon runs on the server authority, so the action must be authoritative", st.Action)}
			}
			if len(st.Args) != len(sig.params) {
				return nil, &BuildError{st.Line, fmt.Sprintf("action %q expects %d argument(s), got %d", st.Action, len(sig.params), len(st.Args))}
			}
			as := Stmt{Op: "actcall", Service: st.Action}
			for _, arg := range st.Args {
				if err := e.checkProcExpr(p, arg, locals, types, st.Line, actionSigs); err != nil {
					return nil, err
				}
				as.Args = append(as.Args, e.low(arg))
			}
			out = append(out, as)
		case ast.Spawn:
			// `let h = spawn ProcName(args)` — parseProcBody guarantees Bind is
			// never "" (spawn has no fire-and-forget form; see ast.Spawn's doc),
			// so this is `do`'s own arity/argument checking plus: the handle is
			// tagged taskType (never a real value — see checkNoTaskUse) instead
			// of the proc's return type, and recorded in pendingSpawns so this
			// exact block is checked, before it ends, to have joined it.
			sig, ok := e.procSigs[st.Proc]
			if !ok {
				return nil, &BuildError{st.Line, fmt.Sprintf("spawn calls unknown proc %q", st.Proc)}
			}
			if len(st.Args) != len(sig.params) {
				return nil, &BuildError{st.Line, fmt.Sprintf("proc %q expects %d argument(s), got %d", st.Proc, len(sig.params), len(st.Args))}
			}
			sp := Stmt{Op: "spawn", Target: st.Bind, Service: st.Proc}
			for _, arg := range st.Args {
				if err := e.checkProcExpr(p, arg, locals, types, st.Line, actionSigs); err != nil {
					return nil, err
				}
				sp.Args = append(sp.Args, e.low(arg))
			}
			if locals[st.Bind] {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is already in scope — pick another name for the spawned task handle", st.Bind)}
			}
			if err := e.checkNotShared(st.Bind, st.Line); err != nil {
				return nil, err
			}
			locals[st.Bind] = true
			types[st.Bind] = taskType
			pendingSpawns[st.Bind] = spawnedTask{proc: st.Proc, sig: sig}
			out = append(out, sp)
		case ast.Detach:
			// `detach ProcName(args)` — a concurrent call nothing joins, legal
			// only where its lifetime is still bounded: a daemon body, whose
			// own lifetime is the process's (see ast.Detach's doc). Checked
			// exactly like spawn's call; lowered to the runtime intrinsic
			// detachIntrinsic, which starts the proc on its own goroutine and
			// logs a failure, since there is no caller to hand one to.
			if actionSigs == nil {
				return nil, &BuildError{st.Line, fmt.Sprintf("detach is only valid inside a daemon body — %q is a proc, and a proc's concurrency is structured: use `let h = spawn %s(...)` and `join h`", p.Name, st.Proc)}
			}
			sig, ok := e.procSigs[st.Proc]
			if !ok {
				return nil, &BuildError{st.Line, fmt.Sprintf("detach calls unknown proc %q", st.Proc)}
			}
			if len(st.Args) != len(sig.params) {
				return nil, &BuildError{st.Line, fmt.Sprintf("proc %q expects %d argument(s), got %d", st.Proc, len(sig.params), len(st.Args))}
			}
			call := &Expr{Kind: "call", Name: detachIntrinsic, Args: []*Expr{{Kind: "lit", Val: st.Proc, VType: "text"}}}
			for _, arg := range st.Args {
				if err := e.checkProcExpr(p, arg, locals, types, st.Line, actionSigs); err != nil {
					return nil, err
				}
				call.Args = append(call.Args, e.low(arg))
			}
			out = append(out, Stmt{Op: "exprstmt", Value: call})
		case ast.Join:
			// `join h` / `let r = join h` — consumes a handle pendingSpawns is
			// tracking for THIS block. Three distinct failure shapes, each with
			// its own message: never declared at all, declared but not a task
			// handle (a plain local reused by mistake), and a real handle that
			// either isn't pending in this block (already joined here, or —
			// impossible to write given checkNoTaskUse plus the scoping rules,
			// but checked anyway — spawned in a different block).
			if !locals[st.Handle] {
				return nil, &BuildError{st.Line, fmt.Sprintf("join of undeclared local %q — expected a handle from `let %s = spawn ProcName(args)`", st.Handle, st.Handle)}
			}
			if types[st.Handle] != taskType {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q is not a spawned task handle — join needs a `let h = spawn ProcName(args)` result", st.Handle)}
			}
			task, ok := pendingSpawns[st.Handle]
			if !ok {
				return nil, &BuildError{st.Line, fmt.Sprintf("%q was already joined — a task handle can only be joined once, and only in the same block it was spawned in", st.Handle)}
			}
			delete(pendingSpawns, st.Handle)
			js := Stmt{Op: "join", Target: st.Handle}
			if st.Bind != "" {
				if locals[st.Bind] {
					return nil, &BuildError{st.Line, fmt.Sprintf("%q is already in scope — pick another name for the joined result", st.Bind)}
				}
				if task.sig.ret == "" {
					return nil, &BuildError{st.Line, fmt.Sprintf("spawned proc %q returns nothing — declare a return type to bind its joined result", task.proc)}
				}
				if err := e.checkNotShared(st.Bind, st.Line); err != nil {
					return nil, err
				}
				locals[st.Bind] = true
				// Same arrayType tagging as a `do` bind above — a spawned proc's
				// list-typed result must be tagged arrayType, not its element type.
				if task.sig.retList {
					types[st.Bind] = arrayType
				} else {
					types[st.Bind] = task.sig.ret
				}
				js.Bind = st.Bind
				js.Ret = task.sig.ret
				js.RetList = task.sig.retList
			}
			out = append(out, js)
		case ast.ExprStmt:
			// A bare builtin call for its side effect alone (writeFile/httpPost/
			// …), result discarded — `do`'s counterpart for a builtin instead of
			// a proc (see ast.ExprStmt's doc). checkProcExpr covers everything a
			// call needs checked: known-builtin/arity (checkBuiltins), and —
			// since this is exactly the position a capability-gated builtin is
			// used for its effect rather than its value — the capability check.
			if err := e.checkProcExpr(p, st.Call, locals, types, st.Line, actionSigs); err != nil {
				return nil, err
			}
			out = append(out, Stmt{Op: "exprstmt", Value: e.low(st.Call)})
		case ast.FileOp:
			// `write Name(content)` / `let x = read Name()` — the verb form of
			// readFile/writeFile over a declared `file` resource (see
			// ast.FileOp's doc). Name must resolve to a real `file` declaration,
			// the capability gate is the exact same one a raw readFile/writeFile
			// call goes through (requireProcCapability, checkProcCapabilities'
			// own per-capability check), and the content/return type is checked
			// against the file's declared Type — the real compile-time
			// enforcement LANGUAGE.md's `write`/`read` promise, not a runtime
			// coercion: a `text` file always round-trips a string, a `bytes`
			// file always round-trips a byte-buffer array (bytesType).
			f, ok := e.files[st.File]
			if !ok {
				return nil, &BuildError{st.Line, fmt.Sprintf("%s %s(...) — %q is not a declared file (add `file %s: text at \"...\"` or `bytes`)", st.Op, st.File, st.File, st.File)}
			}
			isBytes := f.Type == bytesType
			switch st.Op {
			case "read":
				if len(st.Args) != 0 {
					return nil, &BuildError{st.Line, fmt.Sprintf("read %s() takes no arguments", st.File)}
				}
				if err := requireProcCapability(p, "io.file", fmt.Sprintf("read %s(...)", st.File), st.Line); err != nil {
					return nil, err
				}
				rs := Stmt{Op: "fileread", File: f.Name, Path: f.Path, Bytes: isBytes}
				if st.Bind != "" {
					if locals[st.Bind] {
						return nil, &BuildError{st.Line, fmt.Sprintf("%q is already in scope — pick another name for the bound result", st.Bind)}
					}
					if err := e.checkNotShared(st.Bind, st.Line); err != nil {
						return nil, err
					}
					locals[st.Bind] = true
					if isBytes {
						types[st.Bind] = bytesType
					} else {
						types[st.Bind] = "text"
					}
					rs.Bind = st.Bind
				}
				out = append(out, rs)
			case "write":
				if len(st.Args) != 1 {
					return nil, &BuildError{st.Line, fmt.Sprintf("write %s(...) takes exactly one argument (the content to write)", st.File)}
				}
				if err := requireProcCapability(p, "io.file", fmt.Sprintf("write %s(...)", st.File), st.Line); err != nil {
					return nil, err
				}
				if err := e.checkProcExpr(p, st.Args[0], locals, types, st.Line, actionSigs); err != nil {
					return nil, err
				}
				wantType := "text"
				if isBytes {
					wantType = bytesType
				}
				if argType := inferProcType(st.Args[0], types); argType != "" && argType != wantType {
					if isBytes {
						return nil, &BuildError{st.Line, fmt.Sprintf("write %s(...) needs a byte buffer — file %q is declared `bytes`, got %s", st.File, st.File, argType)}
					}
					return nil, &BuildError{st.Line, fmt.Sprintf("write %s(...) needs text — file %q is declared `text`, got %s", st.File, st.File, argType)}
				}
				ws := Stmt{Op: "filewrite", File: f.Name, Path: f.Path, Bytes: isBytes, Value: e.low(st.Args[0])}
				if st.Bind != "" {
					if locals[st.Bind] {
						return nil, &BuildError{st.Line, fmt.Sprintf("%q is already in scope — pick another name for the bound result", st.Bind)}
					}
					if err := e.checkNotShared(st.Bind, st.Line); err != nil {
						return nil, err
					}
					locals[st.Bind] = true
					types[st.Bind] = "bool"
					ws.Bind = st.Bind
				}
				out = append(out, ws)
			default:
				return nil, &BuildError{st.Line, fmt.Sprintf("unknown file operation %q", st.Op)}
			}
		case ast.Loop:
			if err := checkSpawnsJoined(pendingSpawns, p.Name, st.Line, "starting a loop"); err != nil {
				return nil, err
			}
			if err := e.checkProcExpr(p, st.Cond, locals, types, st.Line, actionSigs); err != nil {
				return nil, err
			}
			kids, err := e.procBlock(p, st.Body, locals, mutable, types, loopDepth+1, actionSigs)
			if err != nil {
				return nil, err
			}
			out = append(out, Stmt{Op: "loop", Value: e.low(st.Cond), Body: kids})
		case ast.IfStmt:
			if err := checkSpawnsJoined(pendingSpawns, p.Name, st.Line, "branching (if)"); err != nil {
				return nil, err
			}
			if err := e.checkProcExpr(p, st.Cond, locals, types, st.Line, actionSigs); err != nil {
				return nil, err
			}
			then, err := e.procBlock(p, st.Then, locals, mutable, types, loopDepth, actionSigs)
			if err != nil {
				return nil, err
			}
			var els []Stmt
			if len(st.Else) > 0 {
				els, err = e.procBlock(p, st.Else, locals, mutable, types, loopDepth, actionSigs)
				if err != nil {
					return nil, err
				}
			}
			out = append(out, Stmt{Op: "if", Value: e.low(st.Cond), Body: then, Else: els})
		case ast.Break:
			if err := checkSpawnsJoined(pendingSpawns, p.Name, st.Line, "breaking"); err != nil {
				return nil, err
			}
			if loopDepth == 0 {
				return nil, &BuildError{st.Line, fmt.Sprintf("break outside a loop in proc %q", p.Name)}
			}
			if !last {
				return nil, &BuildError{st.Line, "break must be the last statement of its block"}
			}
			out = append(out, Stmt{Op: "break"})
		case ast.Continue:
			if err := checkSpawnsJoined(pendingSpawns, p.Name, st.Line, "continuing"); err != nil {
				return nil, err
			}
			if loopDepth == 0 {
				return nil, &BuildError{st.Line, fmt.Sprintf("continue outside a loop in proc %q", p.Name)}
			}
			if !last {
				return nil, &BuildError{st.Line, "continue must be the last statement of its block"}
			}
			out = append(out, Stmt{Op: "continue"})
		case ast.Return:
			if err := checkSpawnsJoined(pendingSpawns, p.Name, st.Line, "returning"); err != nil {
				return nil, err
			}
			if !last {
				return nil, &BuildError{st.Line, "return must be the last statement of its block"}
			}
			if p.Ret == "" {
				if st.Value != nil {
					return nil, &BuildError{st.Line, fmt.Sprintf("proc %q declares no return type, so `return` cannot carry a value", p.Name)}
				}
				out = append(out, Stmt{Op: "return"})
				continue
			}
			if st.Value == nil {
				return nil, &BuildError{st.Line, fmt.Sprintf("proc %q returns %s, so `return` needs a value", p.Name, p.Ret)}
			}
			if err := e.checkProcExpr(p, st.Value, locals, types, st.Line, actionSigs); err != nil {
				return nil, err
			}
			out = append(out, Stmt{Op: "return", Value: e.low(st.Value)})
		default:
			return nil, &BuildError{p.Line, fmt.Sprintf("unsupported statement in proc %q", p.Name)}
		}
	}
	// checkSpawnsJoined's backstop: every per-statement call to it above
	// catches a spawn left outstanding across an if/loop/break/continue/
	// return, but a block that spawns and then simply ENDS — no further
	// statement of any kind, so it was never called again — needs its own
	// check here, once, after every statement in it has been processed.
	if err := checkSpawnsJoined(pendingSpawns, p.Name, p.Line, "the block ends"); err != nil {
		return nil, err
	}
	return out, nil
}

// checkSpawnsJoined is Milestone 5's structured-concurrency enforcement: it
// refuses to let procBlock continue past any point where pending (a `spawn`
// declared earlier in the SAME statement list, not yet consumed by a `join`)
// is non-empty and what is about to happen might skip over a `join` written
// later in that same list. procBlock calls this before handling an `if`,
// `loop`, `break`, `continue`, or `return` — every one of those can, on some
// path (however deeply nested), exit this block before reaching a later
// statement — and once more after processing every statement in the block,
// to catch the block simply ending with nothing left to run at all. Two call
// sites, one rule, one message.
//
// This is deliberately more conservative than a precise dataflow analysis
// would need to be (the same standing preference stmtsReturnComplete's own
// doc explains: a simple, provably sound rule over a precise one) — it
// requires spawn everything, then join everything, THEN branch or return,
// rather than trying to prove a specific branch always joins first. That
// trade is what makes "every spawn is joined before its enclosing call
// returns" airtight rather than merely typical.
func checkSpawnsJoined(pending map[string]spawnedTask, procName string, line int, what string) error {
	if len(pending) == 0 {
		return nil
	}
	names := make([]string, 0, len(pending))
	for n := range pending {
		names = append(names, n)
	}
	sort.Strings(names)
	return &BuildError{line, fmt.Sprintf(
		"proc %q has a spawned task handle %q still unjoined when %s — join it with `join %s` first; every spawn must be joined in the same block before it can end", procName, names[0], what, names[0])}
}

// stmtsReturnComplete reports whether every execution path through body
// provably reaches a `return` — the rule a return-typed proc's body must
// satisfy. It looks only at the block's last statement (every earlier
// statement is checked elsewhere to not be a return/break/continue, since
// procBlock already refuses one of those anywhere but last in its own block):
//   - a bare `return` completes the block unconditionally.
//   - an `if` completes the block only when it has BOTH a `then` and an `else`
//     branch, and each of those branches is itself (recursively) complete —
//     mirroring exactly what an author must write for a value to be guaranteed
//     on every path: a one-armed `if` might not run its body at all, so it can
//     never complete a block on its own, no matter what is inside it.
//   - a `loop` never completes a block by itself: a while-style precondition
//     loop may run zero iterations, so nothing inside it can be guaranteed to
//     run — a proc that only "returns" from inside a loop is a proc that can
//     fall off the end whenever the loop's condition starts out false.
//
// This is deliberately simpler than full dataflow (no reachability analysis
// through break/continue, no attempt to prove a loop always iterates at least
// once) — sound but conservative: it can reject a proc a human could prove
// always returns, and it will never accept one that might not.
func stmtsReturnComplete(body []Stmt) bool {
	if len(body) == 0 {
		return false
	}
	last := body[len(body)-1]
	switch last.Op {
	case "return":
		return true
	case "if":
		return len(last.Else) > 0 && stmtsReturnComplete(last.Body) && stmtsReturnComplete(last.Else)
	default:
		return false
	}
}

// checkProcExpr validates an expression inside a proc body: every free name must
// resolve to the proc's own scope (a parameter or an earlier `let`) — never a
// state cell, an entity, or any other action/view name. It otherwise reuses
// checkBuiltins for aggregate/builtin/enum-member validity, exactly as action
// bodies do. A proc is pure computation over its own locals and parameters in
// this milestone; entity/state access from inside a proc is a later milestone.
//
// types is the proc's per-local declared/inferred type map (built by proc/
// procBlock from parameter annotations and each `let`'s initializer) — it is
// checkProcExpr's own addition on top of what action/view checking does,
// because only a proc body has any static type information to check against
// (see checkBitwiseTypes). checkProcExpr deliberately does NOT call check()
// (the action/view funnel that runs checkNoBitwise/checkNoIO): a proc is
// where bitwise operators and the I/O builtins are allowed — subject to
// checkProcCapabilities, below, which is this function's own funnel for the
// I/O capability system (p is threaded through only for that: its name, for
// the diagnostic, and its declared `uses` set).
//
// It ends with checkProcLiteralExpr (braces.go) for the same reason check()
// ends with checkLiteralExpr: a proc body has `{expr}`-shaped string literals
// exactly as any other expression position does, and a proc has no
// interpolation mechanism at all — lowerSegs, the code that actually makes
// `{expr}` render, only runs while lowering a view, and produces a reactive
// binding a one-shot server computation has no use for. Before this, a proc
// body's `"{slot}:{pid}:{score}"` compiled clean and returned the four
// characters `{slot}:{pid}:{score}` unchanged at runtime — check() already
// refused this everywhere else an expression can appear (an argument, a
// `set` value, a `where` operand); checkProcExpr was the one funnel that
// still let it through, because it was written before that refusal existed
// and never got the same call added. checkProcLiteral (braces.go) uses
// locals directly rather than e.resolves, since a proc's actual scope is
// narrower than e.resolves' (no state, no entity, no actor/session builtin).
func (e *env) checkProcExpr(p *ast.Proc, ex ast.Expr, locals map[string]bool, types map[string]string, line int, actionSigs map[string]actionSig) error {
	for n := range freeNames(ex) {
		if !locals[n] && e.shareds[n] == nil {
			return &BuildError{line, fmt.Sprintf(
				"unknown reference %q — a proc sees only its own parameters and `let` locals (no state or entities in this milestone)", n)}
		}
	}
	called := map[string]bool{}
	calledNames(ex, called)
	for n := range called {
		if e.deriveFns[n] != nil {
			return &BuildError{line, fmt.Sprintf(
				"a proc cannot call derive %q — a derive is a projection over the app's rows and state, which a proc does not see (no state or entities in this milestone)", n)}
		}
	}
	if err := e.checkBuiltins(ex, line); err != nil {
		return err
	}
	if err := e.checkPasswordReads(ex, nil, line); err != nil {
		return err
	}
	if err := checkDaemonOnlyBuiltins(ex, actionSigs != nil, line); err != nil {
		return err
	}
	if err := checkEntryOnlyBuiltins(ex, isProcessEntry(p), line); err != nil {
		return err
	}
	if err := checkBitwiseTypes(ex, types, line); err != nil {
		return err
	}
	if err := checkIndexTypes(ex, types, line); err != nil {
		return err
	}
	if err := checkNumericTypes(ex, types, line); err != nil {
		return err
	}
	if err := checkMapKeyTypes(ex, types, line); err != nil {
		return err
	}
	if err := checkNoTaskUse(ex, types, line); err != nil {
		return err
	}
	if err := e.checkStructFieldTypes(ex, types, line); err != nil {
		return err
	}
	if err := checkProcCapabilities(p, ex, line); err != nil {
		return err
	}
	return e.checkProcLiteralExpr(ex, locals, line)
}

// arrayType is inferProcType/checkIndexTypes's type tag for an array-valued
// proc expression — a list literal, an `append(...)` call, or (transitively,
// via ast.Ref's case below) a `let` bound to either. It is a sibling of the
// primitive type names inferProcType already produces ("int"/"text"/"bool"),
// not a real declared type the rest of the language knows about — proc
// locals have no richer type system than this map to hang it off (see
// checkIndexTypes).
const arrayType = "array"

// bytesType is inferProcType/checkIndexTypes's type tag for a byte-buffer
// proc expression — the result of a `bytes(n)` call, or (transitively, via
// ast.Ref's case below) a `let` bound to one. A byte buffer is a deliberate
// specialization of the array machinery above, not a parallel value kind: at
// runtime it is the exact same []any representation an array is (see
// runtime/eval.go's "bytes" case in callBuiltin), so it gets every one of
// arrays' mechanics — index read, `len`, copy-on-assign (cloneArrayValue) —
// for free, purely by being tagged with a different string here. The one
// place it actually diverges is index-WRITE: a byte buffer must reject a
// value outside 0-255, which is exactly what this tag exists to let
// procBlock's ast.IndexAssign case flag on the lowered Stmt (its Bytes
// field), so runtime/proccompile.go can range-check only the
// writes that need it.
const bytesType = "bytes"

// mapType is inferProcType/checkIndexTypes's type tag for a map-valued proc
// expression — a map literal (`{...}`), or (transitively, via ast.Ref's case
// below) a `let` bound to one. A sibling of arrayType/bytesType, not a real
// declared type: proc locals have no richer type system than this map to
// hang it off (see checkIndexTypes). At runtime a map value is a Go
// map[any]any (runtime/eval.go), a distinct representation from an array's
// []any, so — unlike bytesType — it is NOT a specialization of the array
// machinery: index-read/-write dispatch on the actual runtime value's Go
// type (runtime/proccompile.go "index" case, its "indexset" case), and
// this compile-time tag exists only to
// catch the statically-provable cases before that.
const mapType = "map"

// taskType is inferProcType/checkNoTaskUse's type tag for a `spawn`-bound
// task handle (Milestone 5: structured concurrency) — the local a `let h =
// spawn ProcName(args)` statement declares. Unlike arrayType/bytesType/
// mapType, which describe real values a proc may compute with, a task
// handle is deliberately NOT a value at all from the type checker's point of
// view: it exists purely to be consumed by exactly one `join`, so
// checkNoTaskUse refuses it everywhere else (arithmetic, an argument, a
// return, a container element — anything but a bare `join h`). This is what
// makes the enforcement in checkSpawnsJoined (see procBlock) sound: since a
// handle can never be copied, aliased, stashed in an array, or passed to
// another proc, the only way to ever "do something" with one is the single
// `join` procBlock is already watching for.
const taskType = "task"

// inferProcType statically infers the type of an expression inside a proc
// body from literal kinds and the declared/inferred types of the locals it
// names — "" when it cannot be determined (an unknown, not an error). It is
// deliberately shallow — enough to catch a bitwise operator applied to a
// plainly non-int operand (see checkBitwiseTypes below) or an index read on
// something that plainly isn't an array (see checkIndexTypes) — not a general
// type checker for the language, and it errs toward "" (unknown, so
// unchecked) whenever it isn't sure, the same stance every other static check
// in this builder takes: flag what can be proven wrong, stay silent on what
// can't be proven either way.
//
// A float literal's Kind is already "float" (ast.Lit's t.Kind case handles it
// for free, same as "int"/"text"/"bool"), so every one of the numeric cases
// below has to say explicitly which of int/float it accepts and produces —
// there is no automatic int/float promotion in this language (see
// checkNumericTypes), so `int op float` is never one of the cases that
// produces a type here: it is a compile error checkNumericTypes raises before
// inferProcType's answer would ever be used for it.
func inferProcType(ex ast.Expr, types map[string]string) string {
	switch t := ex.(type) {
	case ast.Lit:
		return t.Kind
	case ast.ListLit:
		return arrayType
	case ast.MapLit:
		return mapType
	case ast.StructLit:
		// A struct literal's type is its own declared name (e.g. "Node") — a
		// sibling of arrayType/mapType/bytesType, except it names a REAL
		// declared type (checked against env.structs by
		// checkStructFieldTypes) rather than a generic tag, which is what
		// lets a `.field` read chase back into env.structs for its type
		// (see structExprType).
		return t.Type
	case ast.Ref:
		return types[t.Name]
	case ast.Call:
		switch t.Name {
		case "append":
			return arrayType
		case "split":
			// split(s, sep) -> [text], the one builtin in this milestone that
			// turns a scalar into an array — tagged exactly like append/bytes so
			// a `let lines = split(src, "\n")` local is index-readable
			// (checkIndexTypes) and len()-able, the same as any other array.
			return arrayType
		case "bytes":
			return bytesType
		case "textToBytes":
			// textToBytes(s) -> [int], the real UTF-8 byte sequence of s (one
			// int per byte, not per rune) — tagged exactly like bytes(n)/
			// readBytes (see bytesType's doc) so the result is len()-able,
			// index-readable, and range-checked on index-write the same as
			// any other byte buffer.
			return bytesType
		case "bytesToText":
			// bytesToText(b) -> text, textToBytes' inverse.
			return "text"
		case "aesGcmSeal", "aesGcmOpen":
			// AES-256-GCM (runtime/aesgcm.go): a byte buffer in, one out.
			return bytesType
		case "aesGcmAuthentic":
			return "bool"
		case "byteLen":
			// byteLen(s) -> int, s's real UTF-8 byte length (as opposed to
			// len(s)'s rune count) — always an int, the same as len().
			return "int"
		case "readFile", "httpGet", "httpPost":
			return "text"
		case "listen", "listenOn", "listenTls", "accept":
			// Listener/Conn are, deliberately, just int handles — the exact same
			// "no new type anywhere in this type system" move `channel()` already
			// makes (see its case below and runtime/netconn.go's doc): an int is
			// already a legal proc/daemon local, so a listen()/accept() handle
			// gets every existing mechanic (passing it to another builtin,
			// storing it in a `let`) for free.
			return "int"
		case "readBytes":
			// A raw byte-buffer array, exactly like ioReadFileBytes's return —
			// see bytesType's doc for why this reuses the array machinery.
			return bytesType
		case "pollBytes":
			// pollBytes(c, maxLen, waitMs) -> [int]: a byte buffer, like readBytes.
			return bytesType
		case "connOpen":
			return "bool"
		case "sleepMs", "shutdownConn":
			return "bool"
		case "monoMs", "nowMs", "signals":
			// signals() is a channel handle, the same int channel() mints.
			return "int"
		case "connectTls":
			// connectTls(host, port, serverName, trustFile) -> int: the same
			// connection handle connect() mints (runtime/tlsconnect.go).
			return "int"
		case "awaitAny":
			// awaitAny(chans, ms) -> int: the position in chans of a channel
			// with a value waiting (or closed), -1 when ms pass first
			// (runtime/channel.go).
			return "int"
		case "closeChannel", "exitProcess":
			return "bool"
		case "processStats":
			// processStats() -> text: a JSON object (runtime/process.go).
			return "text"
		case "writeBytes", "closeConn", "setTimeoutMs":
			// true on success. writeBytes answers false on a transport
			// failure (connection reset, deadline passed) — a value, so a
			// client can report an unreachable peer as data; connError says
			// why (see runtime/netconn.go's doc). A bad handle still aborts.
			return "bool"
		case "connect":
			// An outbound connection is the same int handle accept() mints.
			return "int"
		case "connError", "connPeer", "listenError":
			return "text"
		case "closeListener", "grantRead":
			return "bool"
		case "writeStdout", "writeStderr":
			return "bool"
		case "readStdin":
			return "text"
		case "envVar":
			// envVar(name) -> text: the process environment variable, "" when
			// unset (runtime/sysenv.go).
			return "text"
		case "envSet":
			// envSet(name) -> bool: whether it is set at all — what tells an
			// unset variable from one set to "".
			return "bool"
		case "randomBytes":
			// randomBytes(n) -> bytes: n bytes from the OS's cryptographic RNG.
			return bytesType
		case "channel":
			// A channel value is, deliberately, just an int handle — see
			// runtime/channel.go's doc for why that needs no new type anywhere
			// in this type system at all (a channel can be a proc parameter,
			// a `let` local, an array element... simply by already being int).
			return "int"
		case "recv":
			return "text" // channels are text-only in this milestone — see LANGUAGE.md
		case "send":
			return "bool"
		case "appendFile", "truncateFile", "fileExists":
			// appendFile/truncateFile: true on success, a failure is a runtime
			// error exactly as writeFile's below. fileExists answers the one
			// question readFile cannot ask without failing: is it there.
			return "bool"
		case "fileSize":
			// fileSize(path) -> int: the file's length in bytes, -1 when absent.
			return "int"
		case "readFileAt":
			// readFileAt(path, offset, n): exactly n bytes from offset, as a
			// byte buffer (a short read is a runtime error, never a short buffer).
			return bytesType
		case "writeFileAt", "syncFile", "renameFile", "removeFile":
			// true on success; a failure is a runtime error, as writeFile's.
			return "bool"
		case "writeFile":
			// true on success — a failure (missing dir, permission, disk full)
			// never reaches here at all: it is a runtime error that aborts the
			// proc, the same "clean error, not a silent wrong answer" stance an
			// out-of-bounds array read already takes (see runtime/io.go).
			return "bool"
		case "print":
			// print(value) returns value unchanged (the same "log it, keep going"
			// shape as Rust's dbg!()) — so it types identically to its own
			// argument, whatever that argument's type is, exactly like abs
			// below preserves int-vs-float.
			if len(t.Args) == 1 {
				return inferProcType(t.Args[0], types)
			}
		case "toFloat":
			return "float"
		case "floatFromBits":
			// floatFromBits(b) -> float, the IEEE-754 bit-cast inverse of
			// floatBits(f) -> int.
			return "float"
		case "u64Cmp", "u64Min", "u64Max", "u64SatSub", "u64Div", "u64Rem", "u64Parse":
			// the u64 builtins (runtime/u64.go): an int's 64 bits read unsigned.
			return "int"
		case "u64Text", "u64ParseError":
			return "text"
		case "u64ToFloat":
			return "float"
		case "floatBits":
			// floatBits(f) -> int, the raw IEEE-754 bit pattern of f
			// reinterpreted as a signed 64-bit int — always int-typed, the
			// same as toInt/floor/round, regardless of the float's value.
			return "int"
		case "slice":
			// slice(s, start, end) is a substring of a text and a sublist of
			// a list — the same type it was handed.
			if len(t.Args) == 3 {
				if at := inferProcType(t.Args[0], types); at == arrayType || at == bytesType || at == "text" {
					return at
				}
			}
		case "toInt", "floor", "round":
			// floor/round always return int — see runtime/eval.go's callBuiltin
			// doc for why a rounded value is int-typed regardless of whether the
			// input was int (identity, preserving this builtin's pre-float
			// behavior) or float (the fractional part is genuinely gone).
			return "int"
		case "abs":
			// abs preserves whichever numeric flavour it was handed (int stays
			// int, float stays float) — unlike floor/round, it never changes
			// whether the value has a fractional part.
			if len(t.Args) == 1 {
				if at := inferProcType(t.Args[0], types); at == "int" || at == "float" {
					return at
				}
			}
		case "min", "max":
			// Preserve the flavour only when both arguments already agree —
			// same reasoning as abs. A provable int/float mismatch is a
			// compile error raised by checkNumericTypes, not answered here.
			if len(t.Args) == 2 {
				at, bt := inferProcType(t.Args[0], types), inferProcType(t.Args[1], types)
				if at == bt && (at == "int" || at == "float") {
					return at
				}
			}
		}
	case ast.Un:
		switch t.Op {
		case "!":
			return "bool"
		case "-", "~":
			return inferProcType(t.X, types)
		}
	case ast.Bin:
		switch t.Op {
		case "==", "!=", "<", "<=", ">", ">=", "&&", "||", "in":
			return "bool"
		case "&", "|", "^", "<<", ">>":
			return "int"
		case "+":
			// `+` also concatenates text (see applyBin in runtime/eval.go); only
			// call the result "int"/"float" when neither side could be text,
			// otherwise stay unknown rather than guess.
			lt, rt := inferProcType(t.L, types), inferProcType(t.R, types)
			if lt == "text" || rt == "text" {
				return "text"
			}
			if lt == "int" && rt == "int" {
				return "int"
			}
			if lt == "float" && rt == "float" {
				return "float"
			}
		case "-", "*", "/":
			lt, rt := inferProcType(t.L, types), inferProcType(t.R, types)
			if lt == "int" && rt == "int" {
				return "int"
			}
			if lt == "float" && rt == "float" {
				return "float"
			}
		case "%":
			// int-only (see checkNumericTypes) — a float operand here is
			// already a compile error, so this case never needs a float branch.
			if lt, rt := inferProcType(t.L, types), inferProcType(t.R, types); lt == "int" && rt == "int" {
				return "int"
			}
		}
	}
	return ""
}

// structExprType is inferProcType plus the one thing inferProcType cannot do
// on its own: chase a `.field` read (ast.Get) back through env.structs to the
// field's own declared type — inferProcType has no env to consult, so it
// cannot see a struct's field table at all. This is what lets a field-access
// chain like `node.left.op` be checked one struct at a time: structExprType
// resolves `node` (a plain Ref, via inferProcType) to "Node", looks up
// "left" in structs["Node"] to get "Node" back (a self-referential field),
// then the caller resolves ".op" the same way against that.
//
// A list-typed field (`children: [Node]`) answers arrayType here, the same
// tag a list literal or an `append(...)` result carries — element-type
// information a list value never keeps (see arrayType's own doc) — so
// `node.children[i].kind` is checkable up to the index read (as an array
// operation) but not past it, the same "prove what can be proven" limit
// every other array use already has in this builder.
func (e *env) structExprType(ex ast.Expr, types map[string]string) string {
	if g, ok := ex.(ast.Get); ok {
		ot := e.structExprType(g.Obj, types)
		if fields, ok := e.structs[ot]; ok {
			if f, ok := fields[g.Field]; ok {
				if f.list {
					return arrayType
				}
				return f.typ
			}
		}
		return ""
	}
	// An element of a struct's list field has the field's declared element
	// type — the one place an array's element type is not erased.
	if ix, ok := ex.(ast.Index); ok {
		if g, ok := ix.Obj.(ast.Get); ok {
			if fields, ok := e.structs[e.structExprType(g.Obj, types)]; ok {
				if f, ok := fields[g.Field]; ok && f.list {
					return f.typ
				}
			}
		}
	}
	return inferProcType(ex, types)
}

// checkStructFieldTypes walks a proc expression for the two places a struct
// type's shape actually matters: a `Type{...}` literal (every declared field
// must be set exactly once, to a value of the right type, and no unknown
// field) and a `.field` read (ast.Get) whose object's statically known type
// is a declared struct (the field must exist on it). This is deliberately
// stronger than checkIndexTypes' array bounds can ever be — an array's
// element type is erased at compile time by design (arrayType, a generic
// tag), but a struct's OWN type is never erased (its local/param type tag IS
// the struct's real name), so every field name and, wherever the value's own
// type is provable, its type too can be checked here rather than deferred to
// a runtime backstop.
//
// Like checkIndexTypes/checkMapKeyTypes, this stays silent wherever
// structExprType cannot prove an answer (e.g. a value that flows through a
// builtin call) rather than guessing — runtime/eval.go's "get" case is the
// backstop for whatever this cannot see.
func (e *env) checkStructFieldTypes(ex ast.Expr, types map[string]string, line int) error {
	switch t := ex.(type) {
	case ast.StructLit:
		if _, isStruct := e.structs[t.Type]; !isStruct && e.wireTypes[t.Type] {
			// A wire-type literal a proc builds (its reply to an action): the
			// same field rules an action's literal is held to.
			if err := e.checkWireLits(t, line); err != nil {
				return err
			}
			for _, fi := range t.Fields {
				if err := e.checkStructFieldTypes(fi.Expr, types, line); err != nil {
					return err
				}
			}
			return nil
		}
		fields, ok := e.structs[t.Type]
		if !ok {
			return &BuildError{line, fmt.Sprintf("unknown struct type %q", t.Type)}
		}
		set := map[string]bool{}
		for _, fi := range t.Fields {
			fdecl, exists := fields[fi.Name]
			if !exists {
				return &BuildError{line, fmt.Sprintf("struct %q has no field %q", t.Type, fi.Name)}
			}
			if set[fi.Name] {
				return &BuildError{line, fmt.Sprintf("field %q set twice in a %s{...} literal", fi.Name, t.Type)}
			}
			set[fi.Name] = true
			if err := e.checkStructFieldTypes(fi.Expr, types, line); err != nil {
				return err
			}
			got := e.structExprType(fi.Expr, types)
			if got == "" {
				continue // unprovable — left to the runtime backstop, same stance as elsewhere
			}
			if fdecl.list {
				// A byte buffer is an [int] whose elements are range-checked on
				// write (see bytesType), so it fills an [int] field exactly as it
				// already satisfies a `-> [int]` proc return.
				bytesAsInts := got == bytesType && fdecl.typ == "int"
				if got != arrayType && !bytesAsInts {
					return &BuildError{line, fmt.Sprintf("field %q of %s{...} wants [%s], got %s", fi.Name, t.Type, fdecl.typ, got)}
				}
			} else if got != fdecl.typ {
				return &BuildError{line, fmt.Sprintf("field %q of %s{...} wants %s, got %s", fi.Name, t.Type, fdecl.typ, got)}
			}
		}
		for fn := range fields {
			if !set[fn] {
				return &BuildError{line, fmt.Sprintf("%s{...} literal is missing field %q", t.Type, fn)}
			}
		}
	case ast.Get:
		if ot := e.structExprType(t.Obj, types); ot != "" {
			if fields, ok := e.structs[ot]; ok {
				if _, exists := fields[t.Field]; !exists {
					return &BuildError{line, fmt.Sprintf("struct %q has no field %q", ot, t.Field)}
				}
			} else if ot == arrayType || ot == mapType || ot == bytesType || ot == taskType || isPrimitive(ot) {
				return &BuildError{line, fmt.Sprintf(
					"field access (`.%s`) needs a struct value, but this expression is %s", t.Field, ot)}
			}
		}
		return e.checkStructFieldTypes(t.Obj, types, line)
	case ast.Bin:
		if err := e.checkStructFieldTypes(t.L, types, line); err != nil {
			return err
		}
		return e.checkStructFieldTypes(t.R, types, line)
	case ast.Un:
		return e.checkStructFieldTypes(t.X, types, line)
	case ast.Call:
		for _, a := range t.Args {
			if err := e.checkStructFieldTypes(a, types, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := e.checkStructFieldTypes(el, types, line); err != nil {
				return err
			}
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := e.checkStructFieldTypes(k, types, line); err != nil {
				return err
			}
			if err := e.checkStructFieldTypes(t.Vals[i], types, line); err != nil {
				return err
			}
		}
	case ast.Index:
		if err := e.checkStructFieldTypes(t.Obj, types, line); err != nil {
			return err
		}
		return e.checkStructFieldTypes(t.Idx, types, line)
	}
	return nil
}

// bitwiseCheckedTypes are the declared/inferred proc types checkBitwiseTypes
// refuses as a bitwise operand: everything except "int" (and the unknown ""
// type, which is left alone — see inferProcType's doc comment). `money`,
// though it happens to share `int`'s runtime representation (see
// runtime/eval.go's callBuiltin, "money" case — a money value is minor units,
// a plain Go int, until formatMoney renders it to text), is a distinct
// *declared* type in this list: bit-shifting a monetary amount is a silent
// unit error even though the machine would happily compute an answer, and
// this check runs at compile time against the declared type, not the runtime
// value, so it can catch that where a runtime type switch could not. `float`
// is refused for a sharper reason than `money`: it is a genuinely different
// runtime representation (a Go `float64`, not an `int`), so `~`/`<<`/etc.
// could not even be computed on one without first truncating it — there is
// no "silent unit error" reading to have an opinion about, it is simply not
// an integer.
func isIntType(t string) bool { return t == "" || t == "int" }

// isMapKeyType reports whether t is a legal map-key type under this
// milestone's restriction (see ast.MapLit's doc): int or text, the two
// scalar types with obvious, unambiguous equality/hashing — never bool,
// money, date, array, map, or bytes. `float` is refused for the same reason
// bool/money/date are — no good equality/hashing story (a Go float64 is a
// famously bad map/hash key: 0.1+0.2 != 0.3) — and additionally, unlike
// bool/money/date, has a genuinely different runtime representation (a
// float64 alongside int's int) that mapKey (runtime/eval.go) has no case for
// at all, so an unchecked float key would be a runtime error there rather
// than a silently wrong answer. "" (unknown — inferProcType could not
// determine it, e.g. a proc parameter or a `do`-bound result) is treated as
// legal here, the same "stay silent on what can't be proven" stance every
// other static check in this builder takes; runtime/eval.go's mapKey is the
// backstop that catches a genuinely bad key at the one point it can no
// longer be deferred.
func isMapKeyType(t string) bool { return t == "" || t == "int" || t == "text" }

// checkBitwiseTypes walks a proc expression for a bitwise operator (binary
// `& | ^ << >>` or unary `~`) applied to an operand whose statically known
// type is not `int`. Only proc bodies get this check: only there does the
// builder have any per-local type map to check against (a parameter's
// annotation, or a `let`'s inferred type) — action/view expressions have none,
// so a bitwise operator there is barred entirely by checkNoBitwise instead of
// being type-checked. When an operand's type cannot be inferred (e.g. it flows
// through a builtin call, whose return type this shallow inference does not
// track), the check stays silent rather than guessing — see inferProcType.
func checkBitwiseTypes(ex ast.Expr, types map[string]string, line int) error {
	switch t := ex.(type) {
	case ast.Bin:
		if bitwiseOps[t.Op] {
			if lt := inferProcType(t.L, types); !isIntType(lt) {
				return &BuildError{line, fmt.Sprintf("%s needs int operands; left side is %s", t.Op, lt)}
			}
			if rt := inferProcType(t.R, types); !isIntType(rt) {
				return &BuildError{line, fmt.Sprintf("%s needs int operands; right side is %s", t.Op, rt)}
			}
		}
		if err := checkBitwiseTypes(t.L, types, line); err != nil {
			return err
		}
		return checkBitwiseTypes(t.R, types, line)
	case ast.Un:
		if t.Op == "~" {
			if xt := inferProcType(t.X, types); !isIntType(xt) {
				return &BuildError{line, fmt.Sprintf("~ needs an int operand; got %s", xt)}
			}
		}
		return checkBitwiseTypes(t.X, types, line)
	case ast.Call:
		for _, a := range t.Args {
			if err := checkBitwiseTypes(a, types, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkBitwiseTypes(el, types, line); err != nil {
				return err
			}
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := checkBitwiseTypes(k, types, line); err != nil {
				return err
			}
			if err := checkBitwiseTypes(t.Vals[i], types, line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := checkBitwiseTypes(fi.Expr, types, line); err != nil {
				return err
			}
		}
	case ast.Index:
		if err := checkBitwiseTypes(t.Obj, types, line); err != nil {
			return err
		}
		return checkBitwiseTypes(t.Idx, types, line)
	}
	return nil
}

// isNumericFlavor reports whether t is one of the two numeric flavours this
// language's `float` design distinguishes: "int" or "float". Anything
// else — "text", "bool", the composite tags (arrayType/bytesType/mapType), or
// "" (unknown) — is not, and checkNumericTypes leaves those alone entirely
// (an existing, non-float type mismatch, e.g. int + bool, is a pre-existing
// gap this milestone does not newly police — see checkNumericTypes's doc).
func isNumericFlavor(t string) bool { return t == "int" || t == "float" }

// checkNumericTypes is checkBitwiseTypes's counterpart for `+ - * / %` and the
// six comparison operators (`== != < <= > >=`): it rejects an int operand
// against a float one (in either order), the language's one hard rule about
// the two numeric types (see LANGUAGE.md's `proc` section and this file's
// float design note above isPrimitive).
//
// This language's only sanctioned widening is int/money/date's shared
// int-representation identity and the "anything converts to text" rule
// (internal/ir/types.go's doc) — never a genuine cross-representation
// promotion, since int is a Go `int` and float is a Go `float64`, distinct
// machine representations with no single obviously-correct implicit
// direction. So `1 + 2.5` is a compile error here, not a silently-promoted
// 3.5 — the same "flag what can be proven wrong" stance checkBitwiseTypes
// already takes for a bitwise operand, applied to the boundary this
// milestone actually introduces. `toFloat`/`toInt` make the conversion
// explicit wherever a program genuinely needs to cross it.
//
// `+` keeps its existing text-concatenation exception (see inferProcType):
// the check only fires when NEITHER side could be text. `%` is int-only
// outright (see runtime/eval.go's applyBin) — a float on either side of it is
// rejected regardless of what the other side is, matching this milestone's
// design decision that a fractional modulus is a compile-time error, not a
// runtime one (LANGUAGE.md's `proc` section).
//
// Only the shapes that provably disagree are rejected: when either operand's
// type can't be inferred (e.g. it flows through a `do`-bound call whose
// result inferProcType cannot see through, or a proc parameter — no, proc
// parameters ARE typed, but a nested unanalyzable expression still can be —
// "" from inferProcType), this stays silent, the same "stay silent on what
// can't be proven" stance every other static check in this builder takes;
// runtime/eval.go's applyBin is written to still behave sensibly (never
// panic) if a mismatch nonetheless reaches it.
func checkNumericTypes(ex ast.Expr, types map[string]string, line int) error {
	mismatch := func(op, lt, rt string) error {
		return &BuildError{line, fmt.Sprintf(
			"%s needs matching numeric types; left is %s, right is %s — there is no automatic int/float promotion, convert one side explicitly with toFloat()/toInt()", op, lt, rt)}
	}
	switch t := ex.(type) {
	case ast.Bin:
		switch t.Op {
		case "%":
			lt, rt := inferProcType(t.L, types), inferProcType(t.R, types)
			if lt == "float" {
				return &BuildError{line, "% needs int operands; left side is float (% is int-only — there is no fractional modulus in this language)"}
			}
			if rt == "float" {
				return &BuildError{line, "% needs int operands; right side is float (% is int-only — there is no fractional modulus in this language)"}
			}
		case "+":
			lt, rt := inferProcType(t.L, types), inferProcType(t.R, types)
			if lt != "text" && rt != "text" && isNumericFlavor(lt) && isNumericFlavor(rt) && lt != rt {
				return mismatch(t.Op, lt, rt)
			}
		case "-", "*", "/", "==", "!=", "<", "<=", ">", ">=":
			lt, rt := inferProcType(t.L, types), inferProcType(t.R, types)
			if isNumericFlavor(lt) && isNumericFlavor(rt) && lt != rt {
				return mismatch(t.Op, lt, rt)
			}
		}
		if err := checkNumericTypes(t.L, types, line); err != nil {
			return err
		}
		return checkNumericTypes(t.R, types, line)
	case ast.Un:
		return checkNumericTypes(t.X, types, line)
	case ast.Call:
		if (t.Name == "min" || t.Name == "max") && len(t.Args) == 2 {
			at, bt := inferProcType(t.Args[0], types), inferProcType(t.Args[1], types)
			if isNumericFlavor(at) && isNumericFlavor(bt) && at != bt {
				return &BuildError{line, fmt.Sprintf(
					"%s needs matching numeric types; got %s and %s — there is no automatic int/float promotion, convert one side explicitly with toFloat()/toInt()", t.Name, at, bt)}
			}
		}
		for _, a := range t.Args {
			if err := checkNumericTypes(a, types, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkNumericTypes(el, types, line); err != nil {
				return err
			}
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := checkNumericTypes(k, types, line); err != nil {
				return err
			}
			if err := checkNumericTypes(t.Vals[i], types, line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := checkNumericTypes(fi.Expr, types, line); err != nil {
				return err
			}
		}
	case ast.Index:
		if err := checkNumericTypes(t.Obj, types, line); err != nil {
			return err
		}
		return checkNumericTypes(t.Idx, types, line)
	}
	return nil
}

// checkIndexTypes walks a proc expression for an array/map index read
// (`x[i]` / `m[k]`) whose object is a bare name the proc's own type map
// (types) already knows is NOT an array or map — the one piece of static
// typing an index read can be given without a richer type system for proc
// locals. inferProcType tags a list literal, an `append(...)` result, and
// (transitively) any `let` bound to either with arrayType (a map literal,
// likewise, with mapType); anything the map has no opinion on (a parameter —
// proc parameters cannot be list/map-typed in this milestone — or an index
// over any shape other than a bare name) is accepted unchecked, the same
// "stay silent on what can't be proven" stance checkBitwiseTypes takes
// above. The index/key value itself is never type-checked here beyond that —
// an array's bounds are data-dependent and a map's key-type restriction is
// its own separate check (checkMapKeyTypes) — both can only be fully settled
// at runtime (runtime/proccompile.go, "index" case, and
// runtime/proccompile.go, "indexset" case) — see ast.Index's doc.
func checkIndexTypes(ex ast.Expr, types map[string]string, line int) error {
	switch t := ex.(type) {
	case ast.Index:
		if r, ok := t.Obj.(ast.Ref); ok {
			if ty := types[r.Name]; ty != "" && ty != arrayType && ty != bytesType && ty != mapType {
				return &BuildError{line, fmt.Sprintf(
					"%q is not an array or map (its type is %s) — index access (`%s[...]`) needs an array, map, or byte-buffer local", r.Name, ty, r.Name)}
			}
		}
		if err := checkIndexTypes(t.Obj, types, line); err != nil {
			return err
		}
		return checkIndexTypes(t.Idx, types, line)
	case ast.Bin:
		if err := checkIndexTypes(t.L, types, line); err != nil {
			return err
		}
		return checkIndexTypes(t.R, types, line)
	case ast.Un:
		return checkIndexTypes(t.X, types, line)
	case ast.Call:
		for _, a := range t.Args {
			if err := checkIndexTypes(a, types, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkIndexTypes(el, types, line); err != nil {
				return err
			}
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := checkIndexTypes(k, types, line); err != nil {
				return err
			}
			if err := checkIndexTypes(t.Vals[i], types, line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := checkIndexTypes(fi.Expr, types, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkMapKeyTypes walks a proc expression for a map literal entry or a map
// index read (`m[k]`) whose key's statically known type is neither int nor
// text — the milestone's key-type restriction (see ast.MapLit's doc),
// enforced here wherever inferProcType can prove the type. Anything it
// can't prove (a parameter, a `do`-bound result, ...) is left for
// runtime/eval.go's mapKey to catch instead, the same "stay silent on what
// can't be proven" stance checkBitwiseTypes/checkIndexTypes take. The write
// side (`m[k] = v`, ast.IndexAssign) is a statement, not an expression this
// walk ever reaches, so procBlock's own ast.IndexAssign case applies the
// same rule to its Index field directly.
func checkMapKeyTypes(ex ast.Expr, types map[string]string, line int) error {
	switch t := ex.(type) {
	case ast.MapLit:
		for i, k := range t.Keys {
			if kt := inferProcType(k, types); !isMapKeyType(kt) {
				return &BuildError{line, fmt.Sprintf("map key must be int or text, got %s", kt)}
			}
			if err := checkMapKeyTypes(k, types, line); err != nil {
				return err
			}
			if err := checkMapKeyTypes(t.Vals[i], types, line); err != nil {
				return err
			}
		}
	case ast.Index:
		if r, ok := t.Obj.(ast.Ref); ok && types[r.Name] == mapType {
			if kt := inferProcType(t.Idx, types); !isMapKeyType(kt) {
				return &BuildError{line, fmt.Sprintf("map key must be int or text, got %s", kt)}
			}
		}
		if err := checkMapKeyTypes(t.Obj, types, line); err != nil {
			return err
		}
		return checkMapKeyTypes(t.Idx, types, line)
	case ast.Bin:
		if err := checkMapKeyTypes(t.L, types, line); err != nil {
			return err
		}
		return checkMapKeyTypes(t.R, types, line)
	case ast.Un:
		return checkMapKeyTypes(t.X, types, line)
	case ast.Call:
		for _, a := range t.Args {
			if err := checkMapKeyTypes(a, types, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkMapKeyTypes(el, types, line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := checkMapKeyTypes(fi.Expr, types, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkNoTaskUse walks a proc expression for a bare reference to a `spawn`-
// bound task handle (types[name] == taskType) used anywhere but the one place
// a handle is allowed to appear: as `join h`'s Handle field, which is a plain
// string carried on the Join statement, never an ast.Expr — so it never
// reaches this walk at all. Every other appearance (an arithmetic operand, a
// `do`/`spawn` argument, a `return`, an array/map element, a plain `let` copy)
// is refused: this is what makes checkSpawnsJoined's "must join in this same
// block" rule airtight (see procBlock) — a handle can never be smuggled out
// through an alias, an argument, or a container, so the only way to ever
// observe it again is the `join` procBlock is already watching for.
func checkNoTaskUse(ex ast.Expr, types map[string]string, line int) error {
	switch t := ex.(type) {
	case ast.Ref:
		if types[t.Name] == taskType {
			return &BuildError{line, fmt.Sprintf(
				"%q is a spawned task handle — it can only be consumed by `join %s`, not used as a value", t.Name, t.Name)}
		}
	case ast.Bin:
		if err := checkNoTaskUse(t.L, types, line); err != nil {
			return err
		}
		return checkNoTaskUse(t.R, types, line)
	case ast.Un:
		return checkNoTaskUse(t.X, types, line)
	case ast.Call:
		for _, a := range t.Args {
			if err := checkNoTaskUse(a, types, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkNoTaskUse(el, types, line); err != nil {
				return err
			}
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := checkNoTaskUse(k, types, line); err != nil {
				return err
			}
			if err := checkNoTaskUse(t.Vals[i], types, line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := checkNoTaskUse(fi.Expr, types, line); err != nil {
				return err
			}
		}
	case ast.Index:
		if err := checkNoTaskUse(t.Obj, types, line); err != nil {
			return err
		}
		return checkNoTaskUse(t.Idx, types, line)
	}
	return nil
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
	via  string // what named it, for the diagnostic: "button" (the default), "`more`", or "`on change`"
}

func (c call) source() string {
	if c.via == "" {
		return "button"
	}
	return c.via
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
	slot               []Node
	slotOK             bool
	nb, nl, nf, nu, ng int
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
		if err := c.checkView(s.Expr, sc, 0, "a view"); err != nil {
			return nil, err
		}
		// A row has no text. `{p}` or `{Post(id)}` would stringify a record — a
		// different string on each side, and never what anyone meant.
		if isRowExpr(s.Expr, sc, "") {
			return nil, &BuildError{0, fmt.Sprintf("%s is a whole row and cannot be rendered as text — interpolate one of its fields (`%s.body`), or pass it to a component", rowName(s.Expr), rowName(s.Expr))}
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
		if err := c.checkView(rg.Limit, sc, line, "a `limit`"); err != nil {
			return err
		}
		node.Limit = c.e.low(rg.Limit)
	}
	// `more` names the action that loads the next page. The parser has already
	// insisted on a `limit`; the action is validated with every other action a
	// view calls (exists, zero arguments) — see the `calls` pass in Build. A
	// choice list (a select's or radio group's `for`) has no next page: it is
	// the control's options, which render whole.
	if rg.More != "" {
		if node.Kind != "list" {
			return &BuildError{line, "`more` belongs to a `for` list; a choice list renders every option and has no next page"}
		}
		node.More = rg.More
		c.calls = append(c.calls, call{name: rg.More, argc: 0, via: "`more`"})
	}
	// `where` filter: a pure predicate over the item var + outer scope.
	if rg.Where != nil {
		wlocals := viewScope(sc.locals)
		wlocals[rg.Var] = true
		if err := c.e.checkPure(rg.Where, wlocals, line, "a `where` filter"); err != nil {
			return err
		}
		if err := c.checkRowFields(rg.Where, c.bindRange(sc, rg), line); err != nil {
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
			poster, err := c.lowerSegs(t.Poster, sc, false)
			if err != nil {
				return nil, err
			}
			// `autoplay` implies `muted` here, once, so neither renderer decides it:
			// no browser starts a clip with sound on its own, and a player that
			// never starts is worse than one that starts silent (see ast.Video).
			out = append(out, Node{Kind: "video", Segs: segs, Alt: alt, Poster: poster,
				Autoplay: t.Autoplay, Loop: t.Loop, Muted: t.Muted || t.Autoplay})

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
			if err := c.checkView(t.Expr, sc, t.Line, "a `match`"); err != nil {
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
				if err := c.checkView(arg, sc, t.Line, "a view"); err != nil {
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

		case ast.Stage:
			// A stage has no `for`-region children of its own — no partial redraw,
			// the whole canvas repaints when anything it draws from changes. So it
			// gets exactly one region id, and every sprite's collection/where/draw
			// expressions feed that one id instead of minting one each, unlike a
			// `for`'s children (which are node trees the client patches piecemeal).
			node := Node{Kind: "stage", Width: t.Width, Height: t.Height}
			if t.Tiles != nil {
				if err := c.checkView(t.Tiles, sc, t.Line, "a stage's `tiles`"); err != nil {
					return nil, err
				}
				node.Tiles = c.e.low(t.Tiles)
			}
			if !sc.inRegion {
				node.ID = c.id("g", c.ng)
				c.ng++
				if node.Tiles != nil {
					for _, d := range sortedKeys(c.e.depsIR(node.Tiles)) {
						c.addDep(d, node.ID)
					}
				}
			}
			for _, sp := range t.Sprites {
				spNode := Node{Kind: "sprite"}
				if err := c.lowerRange(sp.Range, &spNode, sc, sp.Line); err != nil {
					return nil, err
				}
				child := c.bindRange(sc, sp.Range)
				if err := c.checkView(sp.X, child, sp.Line, "a sprite's `x`"); err != nil {
					return nil, err
				}
				spNode.X = c.e.low(sp.X)
				if err := c.checkView(sp.Y, child, sp.Line, "a sprite's `y`"); err != nil {
					return nil, err
				}
				spNode.Y = c.e.low(sp.Y)
				if sp.Facing != nil {
					if err := c.checkView(sp.Facing, child, sp.Line, "a sprite's `facing`"); err != nil {
						return nil, err
					}
					spNode.Facing = c.e.low(sp.Facing)
				}
				if err := c.checkView(sp.Image, child, sp.Line, "a sprite's `image`"); err != nil {
					return nil, err
				}
				spNode.Image = c.e.low(sp.Image)
				if sp.Label != nil {
					label, err := c.lowerSegs(sp.Label, child, false)
					if err != nil {
						return nil, err
					}
					spNode.Label = label
				}
				if node.ID != "" {
					c.addDep(sp.Coll, node.ID)
					exprs := []*Expr{spNode.Where, spNode.Limit, spNode.X, spNode.Y, spNode.Facing, spNode.Image}
					for _, ex := range exprs {
						if ex == nil {
							continue
						}
						for _, d := range sortedKeys(c.e.depsIR(ex)) {
							c.addDep(d, node.ID)
						}
					}
				}
				node.Children = append(node.Children, spNode)
			}
			out = append(out, node)

		case ast.If:
			if err := c.checkView(t.Cond, sc, 0, "a view"); err != nil {
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

		case ast.After:
			// A client-only, fire-once timer (ast.After). It carries no ID/dependency
			// edge of its own — unlike If/Overlay/Popover it never re-fills on a cell
			// change, it fires once when assets/facet.js's render0 builds it, which
			// happens exactly when its enclosing region (re)renders it — so "on
			// mount" falls out of the existing region-refresh mechanism for free,
			// with no new one to add. Each statement must assign a @client cell,
			// checked exactly like Control/Input/Overlay/Popover's own bind checks
			// above: only a client cell may be written with no round trip to the
			// authority.
			var body []Stmt
			for _, s := range t.Body {
				as, ok := s.(ast.Assign)
				if !ok {
					return nil, &BuildError{t.Line, "after body can only assign a client cell"}
				}
				p, ok := c.e.states[as.Target]
				if !ok {
					return nil, &BuildError{as.Line, fmt.Sprintf("after assigns unknown state %q", as.Target)}
				}
				if p != Client {
					return nil, &BuildError{as.Line, fmt.Sprintf("after assigns %q, which is authoritative; a timer runs client-side with no round trip, so it needs a @client state — declare it `@client` or flip it from an action instead", as.Target)}
				}
				if err := c.checkView(as.Value, sc, as.Line, "an `after` assignment"); err != nil {
					return nil, err
				}
				body = append(body, Stmt{Op: "assign", Target: as.Target, Value: c.e.low(as.Value)})
			}
			out = append(out, Node{Kind: "after", Seconds: t.Seconds, Body: body})

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
			node := Node{Kind: "input", Bind: t.Bind, Placeholder: ph, ID: id}
			// `on change -> Action(args) [debounce …]`: the same action-call lowering
			// Button gets (checkView each arg, then c.e.low it), just fired by the
			// client's own debounce timer instead of a click. Validated against the
			// action table with the rest of allCalls below, with its own `via` so a
			// wrong arity is explained as an on-change dispatch, not "button".
			if t.OnChange != nil {
				for _, arg := range t.OnChange.Args {
					if err := c.checkView(arg, sc, t.OnChange.Line, "a view"); err != nil {
						return nil, err
					}
					node.Args = append(node.Args, c.e.low(arg))
				}
				node.Action = t.OnChange.Action
				node.Debounce = t.OnChange.DebounceMS
				c.calls = append(c.calls, call{name: t.OnChange.Action, argc: len(t.OnChange.Args), via: "`on change`"})
			}
			out = append(out, node)

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

		case ast.Popover:
			// Same state contract as Overlay, checked the same way, for the same
			// reason: a popover toggles client-side, so only a @client bool cell
			// may back it.
			p, ok := c.e.states[t.Bind]
			if !ok {
				return nil, &BuildError{0, fmt.Sprintf("popover binds unknown state %q", t.Bind)}
			}
			if p != Client {
				return nil, &BuildError{0, fmt.Sprintf("popover binds %q, which is authoritative; a popover toggles client-side, so it needs a @client state", t.Bind)}
			}
			if c.e.stateTypes[t.Bind] != "bool" {
				return nil, &BuildError{0, fmt.Sprintf("popover binds %q, which is not a bool; a popover is shown while a bool cell is true", t.Bind)}
			}
			node := Node{Kind: "popover", Bind: t.Bind}
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
				if err := c.checkView(arg, sc, t.Line, "a view"); err != nil {
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
				if err := c.checkView(arg, sc, t.Line, "a view"); err != nil {
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
	if err := e.checkPasswordReads(ex, e.rowLocals, line); err != nil {
		return err
	}
	if err := checkNoBitwise(ex, line); err != nil {
		return err
	}
	if err := e.checkWireLits(ex, line); err != nil {
		return err
	}
	if err := e.checkIndexable(ex, line); err != nil {
		return err
	}
	if err := checkNoIO(ex, line); err != nil {
		return err
	}
	if err := checkNoConcurrency(ex, line); err != nil {
		return err
	}
	if err := checkDaemonOnlyBuiltins(ex, false, line); err != nil {
		return err
	}
	if err := checkArgRowVars(ex, locals, line); err != nil {
		return err
	}
	for n := range freeNames(ex) {
		if !e.resolves(n, locals) {
			if fn, isFn := e.deriveFns[n]; isFn {
				return &BuildError{line, fmt.Sprintf("derive %q takes %d argument(s) — call it: %s(…)", n, len(fn.params), n)}
			}
			return &BuildError{line, fmt.Sprintf("unknown reference %q", n)}
		}
	}
	if err := e.checkDeriveArgs(ex, scope{locals: locals, varTypes: map[string]vtype{}}, line); err != nil {
		return err
	}
	return e.checkLiteralExpr(ex, locals, line)
}

// checkPasswordReads refuses a read of a @password field anywhere except as
// verifyPassword's first argument, and refuses a verifyPassword whose first
// argument is not one. The column holds a salted one-way hash of what was
// written (runtime/server.go hashes on every write), so the one question it
// can answer is "does this candidate match?" — any other read could only carry
// the hash somewhere it must not go: a reply, a render, an export. rows types
// the row-valued locals in scope (their entity); an aggregate's item variable
// is added as its filter and selection are entered, and a view adds its own
// row locals through checkRowFields.
func (e *env) checkPasswordReads(ex ast.Expr, rows map[string]string, line int) error {
	refuse := func(ent, field string) error {
		return &BuildError{line, fmt.Sprintf(
			"%s.%s is @password: it stores only a one-way hash, which nothing may read — check a candidate against it with verifyPassword(%s(…).%s, candidate)", ent, field, ent, field)}
	}
	var walk func(ex ast.Expr, rows map[string]string) error
	walk = func(ex ast.Expr, rows map[string]string) error {
		switch t := ex.(type) {
		case ast.EntityGet:
			if e.entPassword[t.Entity][t.Field] {
				return refuse(t.Entity, t.Field)
			}
			return walk(t.Key, rows)
		case ast.Get:
			if r, ok := t.Obj.(ast.Ref); ok && e.entPassword[rows[r.Name]][t.Field] {
				return refuse(rows[r.Name], t.Field)
			}
			return walk(t.Obj, rows)
		case ast.Call:
			args := t.Args
			if t.Name == "verifyPassword" && len(args) == 2 {
				switch h := args[0].(type) {
				case ast.EntityGet:
					if !e.entPassword[h.Entity][h.Field] {
						return &BuildError{line, "verifyPassword's first argument must be a @password field (e.g. `Account(id).password`) — only such a column holds a hash to check against"}
					}
					if err := walk(h.Key, rows); err != nil {
						return err
					}
				case ast.Get:
					r, ok := h.Obj.(ast.Ref)
					if !ok || !e.entPassword[rows[r.Name]][h.Field] {
						return &BuildError{line, "verifyPassword's first argument must be a @password field (e.g. `Account(id).password`) — only such a column holds a hash to check against"}
					}
				default:
					return &BuildError{line, "verifyPassword's first argument must be a @password field (e.g. `Account(id).password`) — only such a column holds a hash to check against"}
				}
				args = args[1:]
			}
			for _, a := range args {
				if err := walk(a, rows); err != nil {
					return err
				}
			}
		case ast.Agg:
			if t.Sel == nil && e.entPassword[t.Coll][t.Field] {
				return refuse(t.Coll, t.Field)
			}
			inner := rows
			if t.Var != "" {
				inner = make(map[string]string, len(rows)+1)
				for k, v := range rows {
					inner[k] = v
				}
				inner[t.Var] = t.Coll
			}
			for _, sub := range []ast.Expr{t.Where, t.Sel} {
				if sub != nil {
					if err := walk(sub, inner); err != nil {
						return err
					}
				}
			}
			if t.Limit != nil {
				return walk(t.Limit, rows)
			}
		case ast.ListLit:
			for _, el := range t.Elems {
				if err := walk(el, rows); err != nil {
					return err
				}
			}
		case ast.MapLit:
			for i, k := range t.Keys {
				if err := walk(k, rows); err != nil {
					return err
				}
				if err := walk(t.Vals[i], rows); err != nil {
					return err
				}
			}
		case ast.StructLit:
			for _, fi := range t.Fields {
				if err := walk(fi.Expr, rows); err != nil {
					return err
				}
			}
		case ast.Index:
			if err := walk(t.Obj, rows); err != nil {
				return err
			}
			return walk(t.Idx, rows)
		case ast.Bin:
			if err := walk(t.L, rows); err != nil {
				return err
			}
			return walk(t.R, rows)
		case ast.Un:
			return walk(t.X, rows)
		}
		return nil
	}
	return walk(ex, rows)
}

// bitwiseOps is the operator set restricted to `proc` bodies (see
// checkNoBitwise and checkBitwiseTypes): `& | ^ << >>` and unary `~` have
// exactly one interpreter today, runtime/eval.go's applyBin/runtime/proccompile.go/
// evalRest, which only ever runs on the server. A `proc` is unconditionally
// server-executed (LANGUAGE.md's `proc` section), so it can use them safely.
// An action or view expression, by contrast, may be placed on (or
// re-evaluated by) the client — assets/facet.js has no bitwise cases — so
// using one there would silently diverge between server and browser with no
// diagnostic. checkNoBitwise below is what keeps that from happening.
var bitwiseOps = map[string]bool{"&": true, "|": true, "^": true, "<<": true, ">>": true}

// checkNoBitwise rejects a bitwise operator anywhere outside a proc body. It
// is called from check() — the one funnel every action/view/policy/derive
// expression in the language passes through (see check's doc comment) —
// which is what makes this a complete restriction with a single call site:
// checkProcExpr (proc bodies) calls checkBuiltins directly and never calls
// check, so it is unaffected and a proc may use these operators freely.
func checkNoBitwise(ex ast.Expr, line int) error {
	switch t := ex.(type) {
	case ast.Bin:
		if bitwiseOps[t.Op] {
			return &BuildError{line, fmt.Sprintf(
				"%q is only available inside a proc — a proc always runs on the server, but this expression may run on the client too, and the client has no bitwise operators", t.Op)}
		}
		if err := checkNoBitwise(t.L, line); err != nil {
			return err
		}
		return checkNoBitwise(t.R, line)
	case ast.Un:
		if t.Op == "~" {
			return &BuildError{line, "`~` is only available inside a proc — a proc always runs on the server, but this expression may run on the client too, and the client has no bitwise operators"}
		}
		return checkNoBitwise(t.X, line)
	case ast.Get:
		return checkNoBitwise(t.Obj, line)
	case ast.EntityGet:
		return checkNoBitwise(t.Key, line)
	case ast.Call:
		for _, a := range t.Args {
			if err := checkNoBitwise(a, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkNoBitwise(el, line); err != nil {
				return err
			}
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := checkNoBitwise(k, line); err != nil {
				return err
			}
			if err := checkNoBitwise(t.Vals[i], line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := checkNoBitwise(fi.Expr, line); err != nil {
				return err
			}
		}
	case ast.Agg:
		if err := checkNoBitwise(t.Where, line); err != nil {
			return err
		}
		return checkNoBitwise(t.Sel, line)
	case ast.Index:
		if err := checkNoBitwise(t.Obj, line); err != nil {
			return err
		}
		return checkNoBitwise(t.Idx, line)
	}
	return nil
}

// checkNoIndex rejects an array/map index read (`x[i]` / `m[k]`) and a map
// literal (`{...}`) anywhere outside a proc body, the same way checkNoBitwise
// (above) rejects a bitwise operator there and for the same underlying
// reason: an index read's only interpreter is runtime/proccompile.go,
// which resolves it against a proc's own scope-frame. eval() — the flat
// scope evaluator every action/view/policy/derive expression runs through
// instead — has no "index" or "map" case, so without this check either would
// silently evaluate to nil rather than fail to compile.
//
// A map literal is barred outright (unlike ast.ListLit, an array literal,
// which IS allowed outside a proc — it long predates this milestone as a
// general list-typed state/entity field default, and only indexing into one
// is proc-gated): a map has no such existing general field type, and giving
// it one — schema representation, wire serialization for client hydration,
// facet.js support — is well outside this milestone's scope (proc-local
// maps only, per the task ordering that put maps last). checkProcExpr (proc
// bodies) never calls check(), so a proc may use a map literal and index an
// array or map freely — see checkIndexTypes/checkMapKeyTypes for the
// (different) checks that DO apply there.
//
// A struct literal (`Type{...}`, ast.StructLit) is barred the same way, for
// the same reason: its only interpreter is runtime/proccompile.go's "struct" case,
// there is no schema representation, wire encoding, or facet.js mirror for
// one yet, and — like a map — it is a genuinely new proc-local value kind,
// not a general field type any other part of the language already knows.
// checkWireLits validates every wire-type literal in ex — `Dto{f: v, …}` where
// Dto is a declared wire `type` — against the type's declared fields: each
// field named must exist and be set once. A wire type is a plain JSON shape
// with a schema, so its literal is legal in any expression position (an
// action's `return`, a derive, a view), unlike a proc-local `struct` literal,
// which checkNoIndex still refuses outside a proc. Field VALUE types are not
// checked here (the wire schema's own types are for codegen; the runtime
// encodes whatever the expression yields), which is the same stance the
// generic `/api/<entity>` JSON already takes.
func (e *env) checkWireLits(ex ast.Expr, line int) error {
	// The types a field value can be read in: the action's parameters and
	// lets, a derive's parameters, and each aggregate's row.
	vt := map[string]vtype{}
	for n, t := range e.actLocalTypes {
		vt[n] = t
	}
	for n, t := range e.deriveParamTypes {
		vt[n] = t
	}
	var walk func(ex ast.Expr) error
	walk = func(ex ast.Expr) error {
		switch t := ex.(type) {
		case ast.StructLit:
			if fields, ok := e.wireFields[t.Type]; ok {
				set := map[string]bool{}
				for _, fi := range t.Fields {
					if !fields[fi.Name] {
						return &BuildError{line, fmt.Sprintf("type %q has no field %q", t.Type, fi.Name)}
					}
					if set[fi.Name] {
						return &BuildError{line, fmt.Sprintf("field %q set twice in a %s{...} literal", fi.Name, t.Type)}
					}
					set[fi.Name] = true
					if err := e.checkWireFieldValue(t.Type, fi, vt, line); err != nil {
						return err
					}
					if err := walk(fi.Expr); err != nil {
						return err
					}
				}
			}
			return nil
		case ast.Bin:
			if err := walk(t.L); err != nil {
				return err
			}
			return walk(t.R)
		case ast.Un:
			return walk(t.X)
		case ast.Get:
			return walk(t.Obj)
		case ast.EntityGet:
			return walk(t.Key)
		case ast.Call:
			for _, a := range t.Args {
				if err := walk(a); err != nil {
					return err
				}
			}
		case ast.ListLit:
			for _, el := range t.Elems {
				if err := walk(el); err != nil {
					return err
				}
			}
		case ast.Agg:
			prev, had := vt[t.Var]
			if t.Var != "" {
				vt[t.Var] = vtype{core: t.Coll}
			}
			for _, sub := range []ast.Expr{t.Where, t.Sel, t.Limit} {
				if sub != nil {
					if err := walk(sub); err != nil {
						return err
					}
				}
			}
			if t.Var != "" {
				if had {
					vt[t.Var] = prev
				} else {
					delete(vt, t.Var)
				}
			}
		}
		return nil
	}
	return walk(ex)
}

// checkWireFieldValue refuses a wire field value whose type is provably not
// the field's: an instant (`datetime`, format date-time on the wire) filled
// with text or a bare number rather than iso(<unix seconds>), and a DTO field
// filled with a different DTO (a projection of the wrong shape). Values whose
// type cannot be proved here are left to the runtime.
func (e *env) checkWireFieldValue(typ string, fi ast.FieldInit, vt map[string]vtype, line int) error {
	want, ok := e.wireFieldTypes[typ][fi.Name]
	if !ok {
		return nil
	}
	if _, isList := fi.Expr.(ast.ListLit); isList && !want.list && want.core != "json" {
		return &BuildError{line, fmt.Sprintf("%s.%s is one value (a %s), but this value is a list", typ, fi.Name, want.core)}
	}
	locals := map[string]bool{}
	for n := range vt {
		locals[n] = true
	}
	got := e.exprType(fi.Expr, scope{locals: locals, varTypes: vt})
	if !got.known() {
		return nil
	}
	if got.list != want.list {
		if want.core == "json" {
			return nil // an opaque value takes any shape
		}
		what := map[bool]string{true: "a list", false: "one value"}
		return &BuildError{line, fmt.Sprintf("%s.%s is %s, but this value is %s", typ, fi.Name, what[want.list], what[got.list])}
	}
	if want.core == "datetime" {
		switch e.unify(got.core) {
		case "text", "int", "money", "date":
			return &BuildError{line, fmt.Sprintf("%s.%s is an instant (datetime, RFC 3339 on the wire) — fill it with iso(<unix seconds>), not a %s", typ, fi.Name, got.core)}
		}
	}
	// A scalar of the wrong JSON kind: text where the wire carries a number
	// or a boolean, a number or a boolean where it carries text.
	kind := func(c string) string {
		switch e.unify(c) {
		case "int", "money", "number", "float", "date":
			return "number"
		case "text":
			return "string"
		case "bool":
			return "boolean"
		}
		return ""
	}
	if kw, kg := kind(want.core), kind(got.core); want.core != "datetime" && kw != "" && kg != "" && kw != kg {
		return &BuildError{line, fmt.Sprintf("%s.%s is a %s on the wire, but this value is a %s", typ, fi.Name, kw, kg)}
	}
	if e.wireTypes[want.core] && e.wireTypes[got.core] && want.core != got.core {
		return &BuildError{line, fmt.Sprintf("%s.%s is a %s, but this value is a %s", typ, fi.Name, want.core, got.core)}
	}
	return nil
}

// checkIndexable is checkNoIndex for an expression in an action, view,
// policy or derive: `x[i]` is allowed where x is a list or a `json` value
// (an element, or an object's member by key; nothing when absent) — the
// values these scopes do hold — and refused for proc-local arrays and maps.
func (e *env) checkIndexable(ex ast.Expr, line int) error {
	sc := scope{locals: map[string]bool{}, varTypes: map[string]vtype{}}
	for _, m := range []map[string]vtype{e.actLocalTypes, e.deriveParamTypes} {
		for n, t := range m {
			sc.locals[n] = true
			sc.varTypes[n] = t
		}
	}
	return checkNoIndexIf(ex, e.wireTypes, line, func(ix ast.Index) bool {
		t := e.exprType(ix.Obj, sc)
		return t.list || t.core == "json"
	})
}

func checkNoIndex(ex ast.Expr, wire map[string]bool, line int) error {
	return checkNoIndexIf(ex, wire, line, nil)
}

func checkNoIndexIf(ex ast.Expr, wire map[string]bool, line int, allow func(ast.Index) bool) error {
	checkNoIndex := func(ex ast.Expr, wire map[string]bool, line int) error {
		return checkNoIndexIf(ex, wire, line, allow)
	}
	switch t := ex.(type) {
	case ast.Index:
		if allow != nil && allow(t) {
			if err := checkNoIndex(t.Obj, wire, line); err != nil {
				return err
			}
			return checkNoIndex(t.Idx, wire, line)
		}
		return &BuildError{line, "indexing (`x[i]`) outside a proc reads a list or a `json` value; this is neither (arrays and maps are proc-local values)"}
	case ast.MapLit:
		return &BuildError{line, "a map literal (`{...}`) is only available inside a proc — maps are proc-local values, not readable from an action, view, policy, or derive yet"}
	case ast.StructLit:
		if wire[t.Type] {
			// A wire `type` literal — a DTO — is a plain JSON object anywhere
			// (see checkWireLits); only its field values still need this walk.
			for _, fi := range t.Fields {
				if err := checkNoIndex(fi.Expr, wire, line); err != nil {
					return err
				}
			}
			return nil
		}
		return &BuildError{line, fmt.Sprintf("a %s{...} struct literal is only available inside a proc — structs are proc-local values, not readable from an action, view, policy, or derive yet", t.Type)}
	case ast.Bin:
		if err := checkNoIndex(t.L, wire, line); err != nil {
			return err
		}
		return checkNoIndex(t.R, wire, line)
	case ast.Un:
		return checkNoIndex(t.X, wire, line)
	case ast.Get:
		return checkNoIndex(t.Obj, wire, line)
	case ast.EntityGet:
		return checkNoIndex(t.Key, wire, line)
	case ast.Call:
		for _, a := range t.Args {
			if err := checkNoIndex(a, wire, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkNoIndex(el, wire, line); err != nil {
				return err
			}
		}
	case ast.Agg:
		if err := checkNoIndex(t.Where, wire, line); err != nil {
			return err
		}
		return checkNoIndex(t.Sel, wire, line)
	}
	return nil
}

// ioBuiltins is the set of capability-gated I/O builtins (see builtinCapability)
// — proc-only, the same way bitwise operators are, and for the same
// underlying reason (checkNoBitwise's doc): each has exactly one
// interpreter, runtime/io.go, which only ever runs on the server inside a
// proc's own frame (runtime/proccompile.go), so an action/view/policy/
// derive expression — which may be placed on or re-evaluated by the client —
// must never be able to write one into source at all. checkNoIO is the
// syntactic barrier that guarantees that, exactly mirroring checkNoBitwise's
// shape and its single call site inside check().
var ioBuiltins = map[string]bool{"readFile": true, "writeFile": true, "appendFile": true, "fileExists": true, "truncateFile": true, "fileSize": true, "readFileAt": true, "writeFileAt": true, "syncFile": true, "renameFile": true, "removeFile": true, "httpGet": true, "httpPost": true,
	"connect": true, "connectTls": true, "readBytes": true, "writeBytes": true, "closeConn": true, "setTimeoutMs": true, "connError": true, "pollBytes": true, "connOpen": true,
	"closeListener": true, "listenError": true, "grantRead": true,
	"writeStdout": true, "writeStderr": true, "readStdin": true}

// checkNoIO rejects a call to readFile/writeFile/httpGet/httpPost anywhere
// outside a proc body. checkProcExpr (proc bodies) never calls check(), so a
// proc may call these freely, subject only to checkProcCapabilities proving
// the proc declared the capability each one requires.
func checkNoIO(ex ast.Expr, line int) error {
	switch t := ex.(type) {
	case ast.Call:
		if ioBuiltins[t.Name] {
			cap, _ := builtinCapability(t.Name)
			if cap == netConnCap {
				cap = "io.net"
			}
			return &BuildError{line, fmt.Sprintf(
				"%s(...) is only available inside a proc that declares `uses %s` — it is a real I/O effect, and only a proc is unconditionally server-executed with no client mirror to disagree with it", t.Name, cap)}
		}
		for _, a := range t.Args {
			if err := checkNoIO(a, line); err != nil {
				return err
			}
		}
	case ast.Bin:
		if err := checkNoIO(t.L, line); err != nil {
			return err
		}
		return checkNoIO(t.R, line)
	case ast.Un:
		return checkNoIO(t.X, line)
	case ast.Get:
		return checkNoIO(t.Obj, line)
	case ast.EntityGet:
		return checkNoIO(t.Key, line)
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkNoIO(el, line); err != nil {
				return err
			}
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := checkNoIO(k, line); err != nil {
				return err
			}
			if err := checkNoIO(t.Vals[i], line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := checkNoIO(fi.Expr, line); err != nil {
				return err
			}
		}
	case ast.Index:
		if err := checkNoIO(t.Obj, line); err != nil {
			return err
		}
		return checkNoIO(t.Idx, line)
	case ast.Agg:
		if err := checkNoIO(t.Where, line); err != nil {
			return err
		}
		return checkNoIO(t.Sel, line)
	}
	return nil
}

// concurrencyBuiltins is channel/send/recv — Milestone 5's minimal channel
// primitive — proc-only for the same reason ioBuiltins is: it is real,
// blocking, in-process concurrency machinery (runtime/channel.go) with
// exactly one interpreter, reached only from a proc's own frame
// (runtime/proccompile.go), so an action/view/policy/derive
// expression — which the client may itself evaluate, or the server may
// re-evaluate outside any proc call — must never be able to write one into
// source at all. checkNoConcurrency is ioBuiltins/checkNoIO's exact shape,
// applied to this set instead.
var concurrencyBuiltins = map[string]bool{"channel": true, "send": true, "recv": true, "sleepMs": true, "monoMs": true, "nowMs": true, "signals": true, "awaitAny": true, "closeChannel": true}

// checkNoConcurrency rejects a call to channel/send/recv anywhere outside a
// proc body, mirroring checkNoIO exactly (see its doc) — checkProcExpr (proc
// bodies) never calls check(), so a proc may call these freely.
func checkNoConcurrency(ex ast.Expr, line int) error {
	switch t := ex.(type) {
	case ast.Call:
		if concurrencyBuiltins[t.Name] {
			return &BuildError{line, fmt.Sprintf(
				"%s(...) is only available inside a proc body — it is real, blocking concurrency machinery, and only a proc is unconditionally server-executed with no client mirror to disagree with it", t.Name)}
		}
		for _, a := range t.Args {
			if err := checkNoConcurrency(a, line); err != nil {
				return err
			}
		}
	case ast.Bin:
		if err := checkNoConcurrency(t.L, line); err != nil {
			return err
		}
		return checkNoConcurrency(t.R, line)
	case ast.Un:
		return checkNoConcurrency(t.X, line)
	case ast.Get:
		return checkNoConcurrency(t.Obj, line)
	case ast.EntityGet:
		return checkNoConcurrency(t.Key, line)
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkNoConcurrency(el, line); err != nil {
				return err
			}
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := checkNoConcurrency(k, line); err != nil {
				return err
			}
			if err := checkNoConcurrency(t.Vals[i], line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := checkNoConcurrency(fi.Expr, line); err != nil {
				return err
			}
		}
	case ast.Index:
		if err := checkNoConcurrency(t.Obj, line); err != nil {
			return err
		}
		return checkNoConcurrency(t.Idx, line)
	case ast.Agg:
		if err := checkNoConcurrency(t.Where, line); err != nil {
			return err
		}
		return checkNoConcurrency(t.Sel, line)
	}
	return nil
}

// listenBuiltins is listen/accept — the io.net.listen capability's two
// handle-minting builtins (runtime/netconn.go). The connection-handle I/O
// builtins (readBytes/writeBytes/closeConn/setTimeoutMs/connError) are not
// here: they also serve connect()'s outbound connections, which are always
// deadline-bounded and so safe in an ordinary proc. Unlike
// every other capability-gated builtin (readFile/writeFile/httpGet/httpPost,
// restricted only to "inside some proc-shaped body" by checkNoIO),
// accept()/readBytes() can block INDEFINITELY — waiting for a client to
// connect, or to send more bytes — not the bounded 5s timeout httpGet/
// httpPost already have. runtime/server.go's runProcLocked doc is explicit
// that an ordinary proc "runs synchronously, under the caller's lock": one
// reached via `do` from an action runs for that action's whole s.mu hold,
// and one reached via `spawn` is still `join`ed before the spawning block
// (itself under s.mu, if it's an action) ends — so either path would hold
// the durable-store lock hostage to an indefinitely blocking accept(). Only
// a daemon body (runtime/daemon.go's runDaemonOnce/runDaemonBody) runs
// detached, under no request's lock at all, for exactly as long as it likes.
// checkDaemonOnlyBuiltins is that restriction, reusing the exact signal
// procBlock's own `act ActionName(...)` gate already uses (actionSigs == nil
// means "lowering a real proc's body", non-nil means "lowering a daemon's")
// rather than inventing a second one.
var listenBuiltins = map[string]bool{"listen": true, "listenOn": true, "listenTls": true, "accept": true}

// checkEntryOnlyBuiltins rejects grantRead(...) anywhere but the process
// entry point (`proc main`): a read grant must come from the one body that
// runs before any request, connection or client input exists — see
// runtime/grants.go for the whole model.
func checkEntryOnlyBuiltins(ex ast.Expr, isEntry bool, line int) error {
	if isEntry {
		return nil
	}
	var found bool
	ast.WalkExpr(ex, func(x ast.Expr) {
		if c, ok := x.(ast.Call); ok && c.Name == "grantRead" {
			found = true
		}
	})
	if found {
		return &BuildError{line, "grantRead(...) is only available in `proc main(args: [text]) -> int` — a file becomes readable outside the sandbox only when the operator names it (an argument or an environment variable) to the process entry point, before any request or connection exists"}
	}
	return nil
}

// checkDaemonOnlyBuiltins rejects a call to listen/accept anywhere isDaemon
// is false, mirroring checkNoIO's
// exact recursive shape (see its doc) over this narrower set.
func checkDaemonOnlyBuiltins(ex ast.Expr, isDaemon bool, line int) error {
	if isDaemon {
		return nil
	}
	switch t := ex.(type) {
	case ast.Call:
		if listenBuiltins[t.Name] {
			return &BuildError{line, fmt.Sprintf(
				"%s(...) is only available inside a daemon body — it can block indefinitely, and only a daemon runs detached from every request's lock (see `daemon Name uses io.net.listen:`)", t.Name)}
		}
		for _, a := range t.Args {
			if err := checkDaemonOnlyBuiltins(a, isDaemon, line); err != nil {
				return err
			}
		}
	case ast.Bin:
		if err := checkDaemonOnlyBuiltins(t.L, isDaemon, line); err != nil {
			return err
		}
		return checkDaemonOnlyBuiltins(t.R, isDaemon, line)
	case ast.Un:
		return checkDaemonOnlyBuiltins(t.X, isDaemon, line)
	case ast.Get:
		return checkDaemonOnlyBuiltins(t.Obj, isDaemon, line)
	case ast.EntityGet:
		return checkDaemonOnlyBuiltins(t.Key, isDaemon, line)
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := checkDaemonOnlyBuiltins(el, isDaemon, line); err != nil {
				return err
			}
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := checkDaemonOnlyBuiltins(k, isDaemon, line); err != nil {
				return err
			}
			if err := checkDaemonOnlyBuiltins(t.Vals[i], isDaemon, line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := checkDaemonOnlyBuiltins(fi.Expr, isDaemon, line); err != nil {
				return err
			}
		}
	case ast.Index:
		if err := checkDaemonOnlyBuiltins(t.Obj, isDaemon, line); err != nil {
			return err
		}
		return checkDaemonOnlyBuiltins(t.Idx, isDaemon, line)
	case ast.Agg:
		if err := checkDaemonOnlyBuiltins(t.Where, isDaemon, line); err != nil {
			return err
		}
		return checkDaemonOnlyBuiltins(t.Sel, isDaemon, line)
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
	return e.checkPureIn(ex, locals, line, ctx, false)
}

// checkPureIn is checkPure with the one exception an action's `check` makes:
// secrets says whether ex may call an authorityCap builtin (see its doc).
func (e *env) checkPureIn(ex ast.Expr, locals map[string]bool, line int, ctx string, secrets bool) error {
	if caps := procCapabilities(ex); caps[authorityCap] != "" && !secrets {
		return &BuildError{line, fmt.Sprintf(
			"%s cannot call %s(...); it handles an authentication secret and runs only on the authority. Call it from an action body instead", ctx, caps[authorityCap])}
	}
	if err := e.check(ex, locals, line); err != nil {
		return err
	}
	if hasImpure(ex) {
		return &BuildError{line, fmt.Sprintf(
			"%s cannot use an effectful builtin (now/rand); it must be pure so it can run on any client. Compute it in an action instead", ctx)}
	}
	if hasPrint(ex) {
		return &BuildError{line, fmt.Sprintf(
			"%s cannot call print(...); it is a server-only debugging aid with no client implementation. Call it from an action body or a proc instead", ctx)}
	}
	return nil
}

// rowName spells a row expression the way the author wrote it, for a diagnostic.
func rowName(ex ast.Expr) string {
	switch t := ex.(type) {
	case ast.Ref:
		return "`" + t.Name + "`"
	case ast.EntityGet:
		return "`" + t.Entity + "(…)`"
	}
	return "this"
}

// checkView is what every expression written in a view goes through: the
// purity and name checks of checkPure, then checkRowFields, which needs the
// scope's TYPES and not only its names — which is why it is a viewCtx method
// and checkPure is not.
func (c *viewCtx) checkView(ex ast.Expr, sc scope, line int, ctx string) error {
	if err := c.e.checkPure(ex, viewScope(sc.locals), line, ctx); err != nil {
		return err
	}
	return c.checkRowFields(ex, sc, line)
}

// checkRowFields refuses `x.field` when x is a row (a `for` variable or an
// entity-typed component parameter) and the entity has no such field.
//
// It used to be accepted: a row local resolved, the field was looked up at
// render time, missed, and rendered as nothing — the same nothing an empty
// column renders, so a typo in `{p.bdoy}` was a blank post and not an error.
// The types to refuse it were already in scope (rowEntity); nothing asked. An
// aggregate's item variable is typed the way exprType types it, so a filtered
// count's predicate is checked too.
func (c *viewCtx) checkRowFields(ex ast.Expr, sc scope, line int) error {
	switch t := ex.(type) {
	case ast.Get:
		if r, ok := t.Obj.(ast.Ref); ok {
			if ent, isRow := c.rowEntity(sc, r.Name); isRow && t.Field != "id" && !c.e.entityFields[ent][t.Field] && !c.e.entityDerives[ent][t.Field] {
				return &BuildError{line, fmt.Sprintf("entity %q has no field %q (in `%s.%s`)", ent, t.Field, r.Name, t.Field)}
			} else if isRow && c.e.entPassword[ent][t.Field] {
				// A view can never call verifyPassword (it runs only on the
				// authority), so any read of a @password column here is a leak.
				return c.e.checkPasswordReads(t, map[string]string{r.Name: ent}, line)
			}
		}
		return c.checkRowFields(t.Obj, sc, line)
	case ast.EntityGet:
		return c.checkRowFields(t.Key, sc, line)
	case ast.Call:
		for _, a := range t.Args {
			if err := c.checkRowFields(a, sc, line); err != nil {
				return err
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := c.checkRowFields(el, sc, line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := c.checkRowFields(fi.Expr, sc, line); err != nil {
				return err
			}
		}
	case ast.Bin:
		if err := c.checkRowFields(t.L, sc, line); err != nil {
			return err
		}
		return c.checkRowFields(t.R, sc, line)
	case ast.Un:
		return c.checkRowFields(t.X, sc, line)
	case ast.Agg:
		inner := sc
		if t.Var != "" {
			inner = sc.with(t.Var)
			inner.varTypes[t.Var] = vtype{core: t.Coll}
		}
		if err := c.checkRowFields(t.Where, inner, line); err != nil {
			return err
		}
		return c.checkRowFields(t.Sel, inner, line)
	}
	return nil
}

// checkBuiltins validates aggregates (must range over a real entity; `sum` needs
// an existing field) and builtin calls (known name, correct arity).
func (e *env) checkBuiltins(ex ast.Expr, line int) error {
	switch t := ex.(type) {
	case *ast.Asset:
		// internal/compile resolves every `asset from "..."` reachable from a
		// parsed file before this ever runs (see its walkAssets) — an
		// unresolved one here means either the app was compiled with
		// compile.String (no file, nowhere to resolve a relative path
		// against — the same reason it refuses `import`/`css from`), or the
		// reference sits somewhere that pass doesn't reach. Either way it is
		// a compile error, not a value: lower() only knows how to lower a
		// *resolved* Asset (as a literal), so letting this through would
		// silently drop the expression instead of failing loudly.
		if !t.Resolved {
			return &BuildError{line, fmt.Sprintf(
				"asset from %q was never resolved — asset from \"...\" is only supported when compiling from a file (run `facet <command> <file.fct>`)", t.Path)}
		}
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
		if t.Op == "list" {
			if t.Order != "" && t.Order != "id" && !e.entityFields[t.Coll][t.Order] && !e.entityDerives[t.Coll][t.Order] {
				return &BuildError{line, fmt.Sprintf("entity %q has no field %q to order list(...) by (nor a derive of that name)", t.Coll, t.Order)}
			}
			if t.OrderExpr != nil {
				if t.Var == "" {
					return &BuildError{line, fmt.Sprintf("list(... in %s by <expression>) needs the row named — write list(x … in %s where … by <expression over x>)", t.Coll, t.Coll)}
				}
				if err := e.checkBuiltins(t.OrderExpr, line); err != nil {
					return err
				}
			}
			if t.Limit != nil {
				if err := e.checkBuiltins(t.Limit, line); err != nil {
					return err
				}
			}
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
		case "readFile", "httpGet":
			if len(t.Args) != 1 {
				return &BuildError{line, fmt.Sprintf("%s(...) takes exactly one argument", t.Name)}
			}
		case "writeFile", "httpPost", "appendFile", "truncateFile":
			if len(t.Args) != 2 {
				return &BuildError{line, fmt.Sprintf("%s(...) takes exactly two arguments", t.Name)}
			}
		case "fileExists", "fileSize", "syncFile", "removeFile":
			if len(t.Args) != 1 {
				return &BuildError{line, fmt.Sprintf("%s(...) takes exactly one argument", t.Name)}
			}
		case "renameFile":
			if len(t.Args) != 2 {
				return &BuildError{line, fmt.Sprintf("%s(...) takes exactly two arguments", t.Name)}
			}
		case "readFileAt", "writeFileAt", "pollBytes":
			if len(t.Args) != 3 {
				return &BuildError{line, fmt.Sprintf("%s(...) takes exactly three arguments", t.Name)}
			}
		case "writeStdout", "writeStderr", "envVar", "envSet", "randomBytes":
			if len(t.Args) != 1 {
				return &BuildError{line, fmt.Sprintf("%s(...) takes exactly one argument", t.Name)}
			}
		case "readStdin":
			if len(t.Args) != 0 {
				return &BuildError{line, "readStdin() takes no arguments"}
			}
		case "sleepMs":
			if len(t.Args) != 1 {
				return &BuildError{line, "sleepMs(ms) takes exactly one argument (milliseconds, 0-600000)"}
			}
		case "monoMs", "nowMs", "signals", "processStats":
			if len(t.Args) != 0 {
				return &BuildError{line, t.Name + "() takes no arguments"}
			}
		case "closeChannel", "exitProcess":
			if len(t.Args) != 1 {
				return &BuildError{line, fmt.Sprintf("%s(...) takes exactly one argument", t.Name)}
			}
		case "connectTls":
			if len(t.Args) != 4 {
				return &BuildError{line, "connectTls(host, port, serverName, trustFile) takes exactly four arguments: a host, a port, the name the certificate must carry (\"\": the host) and a PEM file of extra trusted roots (\"\": the system's only)"}
			}
		case "awaitAny":
			if len(t.Args) != 2 {
				return &BuildError{line, "awaitAny(chans, ms) takes exactly two arguments: a list of channel handles and a wait in milliseconds (negative: no limit)"}
			}
		case "listenTls":
			if len(t.Args) != 3 {
				return &BuildError{line, "listenTls(port, identity, password) takes exactly three arguments: a port, a PKCS#12 identity file and its password"}
			}
		case "listen", "accept", "closeConn", "connError", "connPeer", "shutdownConn", "connOpen", "closeListener", "listenError", "grantRead":
			if len(t.Args) != 1 {
				return &BuildError{line, fmt.Sprintf("%s(...) takes exactly one argument", t.Name)}
			}
		case "readBytes", "writeBytes", "connect", "setTimeoutMs", "listenOn":
			if len(t.Args) != 2 {
				return &BuildError{line, fmt.Sprintf("%s(...) takes exactly two arguments", t.Name)}
			}
		case "channel":
			if len(t.Args) != 0 {
				return &BuildError{line, "channel() takes no arguments"}
			}
		case "totpSecret":
			if len(t.Args) != 0 {
				return &BuildError{line, "totpSecret() takes no arguments"}
			}
		case "verifyPassword":
			if len(t.Args) != 2 {
				return &BuildError{line, "verifyPassword(stored, candidate) takes exactly two arguments: a @password field and the candidate text"}
			}
		case "randomToken":
			if len(t.Args) != 1 {
				return &BuildError{line, "randomToken(n) takes exactly one argument: the token's length in characters"}
			}
		case "fileDigest":
			if len(t.Args) != 1 {
				return &BuildError{line, "fileDigest(file) takes exactly one argument: an uploaded file (a `bytes` value)"}
			}
		case "ed25519Verify", "ecdsaP256Verify":
			if len(t.Args) != 3 {
				return &BuildError{line, fmt.Sprintf("%s(publicKey, message, signature) takes exactly three arguments", t.Name)}
			}
			if err := e.authorityOnlyCall(t.Name, line); err != nil {
				return err
			}
		case "sha256Hex", "canonicalJson":
			if len(t.Args) != 1 {
				return &BuildError{line, fmt.Sprintf("%s(value) takes exactly one argument", t.Name)}
			}
			if err := e.authorityOnlyCall(t.Name, line); err != nil {
				return err
			}
		case "given":
			// given(p): p must be one of the action's own parameters.
			r, isRef := func() (ast.Ref, bool) {
				if len(t.Args) != 1 {
					return ast.Ref{}, false
				}
				r, ok := t.Args[0].(ast.Ref)
				return r, ok
			}()
			if !isRef || e.actLocalTypes == nil || e.inDerive != "" || e.inProc {
				return &BuildError{line, "given(p) takes one of the action's own parameters by name, in the action's body"}
			}
			if !e.actParams[r.Name] {
				return &BuildError{line, fmt.Sprintf("given(%s): %q is not a parameter of this action", r.Name, r.Name)}
			}
		case "fromLocal", "formatIn", "zoneValid":
			want := map[string]int{"fromLocal": 2, "formatIn": 3, "zoneValid": 1}[t.Name]
			if len(t.Args) != want {
				return &BuildError{line, fmt.Sprintf("%s takes %d argument(s): fromLocal(wallClock, zone), formatIn(unixSeconds, zone, layout), zoneValid(zone)", t.Name, want)}
			}
			if err := e.authorityOnlyCall(t.Name, line); err != nil {
				return err
			}
		case "shuffleOrder":
			if len(t.Args) != 2 {
				return &BuildError{line, "shuffleOrder(seed, n) takes exactly two arguments: the seed text and how many positions to permute"}
			}
			if err := e.authorityOnlyCall(t.Name, line); err != nil {
				return err
			}
		case "totpValid":
			if len(t.Args) != 2 {
				return &BuildError{line, "totpValid(secret, code) takes exactly two arguments: the shared secret and the presented code"}
			}
		case "recv":
			if len(t.Args) != 1 {
				return &BuildError{line, "recv(ch) takes exactly one argument (the channel)"}
			}
		case "send":
			if len(t.Args) != 2 {
				return &BuildError{line, "send(ch, value) takes exactly two arguments"}
			}
		default:
			if fn, isFn := e.deriveFns[t.Name]; isFn {
				if len(t.Args) != len(fn.params) {
					return &BuildError{line, fmt.Sprintf("derive %q takes %d argument(s), got %d", t.Name, len(fn.params), len(t.Args))}
				}
				if e.procDerives[t.Name] {
					// A projection that calls a proc is server-only, like the proc.
					if e.inDerive != "" {
						e.procDerives[e.inDerive] = true
					} else if e.actLocalTypes == nil {
						return &BuildError{line, fmt.Sprintf("derive %q calls a proc, so it runs only on the authority — use it in an action, not a view or policy", t.Name)}
					}
				}
				break
			}
			// A proc is callable in an action's expressions (the authority runs
			// both) and in a parameterized derive, which then is usable only in
			// actions; a view or policy has no proc runner behind it.
			if e.procNames[t.Name] {
				switch {
				case e.inDerive != "":
					e.procDerives[e.inDerive] = true
				case e.inProc:
					// a proc calling a proc: both run on the authority
				case e.actLocalTypes == nil:
					return &BuildError{line, fmt.Sprintf("proc %q runs on the authority, so it can be called only in an action or a derive used by one (a view or policy is evaluated where no proc runs) — bind it in the action and pass the value", t.Name)}
				}
				if len(t.Args) != e.procArity[t.Name] {
					return &BuildError{line, fmt.Sprintf("proc %q takes %d argument(s), got %d", t.Name, e.procArity[t.Name], len(t.Args))}
				}
				break
			}
			// A pure standard-library builtin (string/date/math): fixed arity.
			n, ok := pureBuiltinArity(t.Name)
			if _, isBuiltin := parser.BuiltinSiteOf(t.Name); !ok && !isBuiltin {
				return &BuildError{line, fmt.Sprintf("unknown function %q — neither a builtin (see `facet lang`) nor a derive declared with parameters", t.Name)}
			}
			if ok && len(t.Args) != n {
				return &BuildError{line, fmt.Sprintf("%s takes %d argument(s), got %d", t.Name, n, len(t.Args))}
			}
		}
		// Where the builtin may run (parser.BuiltinSiteOf — the one table): a
		// server-only one is refused where the browser evaluates, a proc-only
		// one anywhere but a proc body.
		if site, ok := parser.BuiltinSiteOf(t.Name); ok && !e.procNames[t.Name] {
			switch site {
			case parser.SiteAuthority:
				// given and print keep their own, narrower rules (above, and
				// checkPure's printCap); the rest follow the proc rule.
				if t.Name != "given" && t.Name != "print" {
					if err := e.authorityOnlyCall(t.Name, line); err != nil {
						return err
					}
				}
			case parser.SiteProc:
				// I/O, concurrency and listen builtins keep their own, more
				// specific barriers (checkNoIO / checkNoConcurrency /
				// checkDaemonOnlyBuiltins name the capability or the context).
				if !e.inProc && !e.inDaemon && !ioBuiltins[t.Name] && !concurrencyBuiltins[t.Name] && !listenBuiltins[t.Name] {
					return &BuildError{line, fmt.Sprintf("%s(...) is only available inside a proc body — only the proc engine implements it, and a proc always runs on the authority", t.Name)}
				}
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
	case ast.MapLit:
		for i, k := range t.Keys {
			if err := e.checkBuiltins(k, line); err != nil {
				return err
			}
			if err := e.checkBuiltins(t.Vals[i], line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := e.checkBuiltins(fi.Expr, line); err != nil {
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
		// `Post(id).field` names a field the entity has (or `id`); `Post(id)` alone
		// is the row and names none. Checked only for an entity whose fields the
		// builder recorded, so a managed entity it did not is not refused.
		if fields := e.entityFields[t.Entity]; t.Field != "" && t.Field != "id" && fields != nil && !fields[t.Field] && !e.entityDerives[t.Entity][t.Field] {
			return &BuildError{line, fmt.Sprintf("entity %q has no field %q (in `%s(…).%s`)", t.Entity, t.Field, t.Entity, t.Field)}
		}
		return e.checkBuiltins(t.Key, line)
	case ast.Bin:
		if err := e.checkBuiltins(t.L, line); err != nil {
			return err
		}
		return e.checkBuiltins(t.R, line)
	case ast.Un:
		return e.checkBuiltins(t.X, line)
	case ast.Index:
		if err := e.checkBuiltins(t.Obj, line); err != nil {
			return err
		}
		return e.checkBuiltins(t.Idx, line)
	}
	return nil
}

// builtinCapability names the declared `uses` capability a builtin requires,
// and whether it requires one at all. now/rand are deliberately NOT here —
// they are effectful (nondeterministic) but need no declaration; they force
// server placement instead (see impureCap, procCapabilities). Only the real
// I/O builtins — file access and outbound HTTP — are gated behind a capability
// a proc must opt into on its own header.
func builtinCapability(name string) (string, bool) {
	switch name {
	case "readFile", "writeFile", "appendFile", "fileExists", "truncateFile",
		"fileSize", "readFileAt", "writeFileAt", "syncFile", "renameFile", "removeFile":
		return "io.file", true
	case "httpGet", "httpPost", "connect", "connectTls":
		return "io.net", true
	case "writeStdout", "writeStderr", "readStdin", "signals", "exitProcess", "processStats":
		// io.console: the process's own stdio, for a program run as a
		// command (`facet exec`, runtime/stdio.go). A server's stdout is its
		// operator log, so writing to it is opted into by name.
		return "io.console", true
	case "envVar", "envSet":
		// io.env: the process environment — how a program run as a service
		// is configured (ports, keys, credentials), so reading it is opted
		// into by name like any other channel to the outside.
		return "io.env", true
	case "readBytes", "writeBytes", "closeConn", "setTimeoutMs", "connError", "connPeer", "shutdownConn", "pollBytes", "connOpen":
		// I/O on a connection handle, whichever way it was minted: an
		// outbound connect() (io.net) or a daemon's accept() (io.net.listen).
		return netConnCap, true
	case "grantRead":
		// io.file: it makes a file readable (runtime/grants.go).
		return "io.file", true
	case "closeListener", "listenError":
		// Operations on a listener handle: the listener's own capability.
		return "io.net.listen", true
	case "listen", "listenOn", "listenTls", "accept":
		// io.net.listen: deliberately a MORE specific capability than io.net
		// (outbound httpGet/httpPost), not a reuse of it — accepting arbitrary
		// inbound connections is a materially bigger trust boundary than this
		// instance choosing to make its own outbound calls, so a proc/daemon
		// must opt into it by name, separately from io.net. See
		// checkDaemonOnlyBuiltins for the other half of this gate: these two
		// are additionally restricted to a daemon body specifically, never an
		// ordinary proc, because accept() blocks indefinitely (see
		// runtime/netconn.go).
		return "io.net.listen", true
	}
	return "", false
}

// knownCapabilities is every capability name a proc's `uses` clause may
// declare — the set builtinCapability's second return value ranges over.
// Checked against at proc-registration time (see e.proc's caller) so a typo
// (`uses io.fiel`) is a clear compile error instead of a capability that can
// never be satisfied.
var knownCapabilities = map[string]bool{"io.file": true, "io.net": true, "io.net.listen": true, "io.console": true, "io.env": true}

// netConnCap is the never-user-declared capability key of the connection-
// handle I/O builtins (readBytes/writeBytes/closeConn/setTimeoutMs/
// connError): either declared network capability satisfies it, since a
// handle comes from connect() under io.net or accept() under io.net.listen
// (see requireProcCapability).
const netConnCap = "io.net.conn"

// impureCap is the internal (never user-declared) capability key
// procCapabilities uses to flag now()/rand() — the nondeterminism that forces
// an action onto the server, tracked in the very same walk as the declared
// I/O capabilities so the two mechanisms share one tree-walker instead of
// drifting apart as two parallel ones. It is never checked against a proc's
// declared `uses` set (checkProcCapabilities skips it): now/rand need no
// declaration, only placement.
const impureCap = "impure"

// printCap is printCap's own never-user-declared capability key, the same
// device impureCap uses for now()/rand(): procCapabilities' one shared walker
// flags a print(...) call under this key so hasPrint (below) can answer "does
// ex call print" without a second tree-walk, and checkProcCapabilities skips
// it exactly like impureCap — print needs no `uses` declaration (see
// IsBuiltinCall's doc in internal/parser/expr.go: it is a language-level
// debugging aid, not I/O to an external resource). It is deliberately its own
// key rather than reusing impureCap: unlike now/rand, print must never fall
// into the "an impure action that only writes @client state runs on the
// client instead" placement exception (env.action's placement switch), since
// there is no client-side implementation of it and its whole point is a line
// in the AUTHORITY's own log output.
const printCap = "print"

// authorityCap is the never-user-declared capability key for the builtins that
// handle an authentication secret: verifyPassword (reads a @password hash),
// totpSecret (mints a TOTP shared secret), totpValid (checks a code against
// one) and randomToken (mints an unguessable token from the OS CSPRNG — rand()
// is a fast, predictable generator, fine for a die roll and wrong for a
// credential). Like print they have no client implementation and must never run
// anywhere but the authority — the secret they touch is exactly what a client
// must not hold — so they force server placement unconditionally and are
// refused in every expression a client may evaluate (checkPure). Unlike print
// they are allowed in an action's `check`: that call pins the action to the
// server, so the guard runs where the secret is. It needs no `uses`
// declaration in a proc (a proc is server-executed already).
const authorityCap = "authority"

// sessionTokenRef is the action-only name for the caller's signed session
// credential (runtime/server.go binds it; see isBuiltinRef for the names every
// context shares). It is a credential, so it is visible to an action body and
// nowhere a view, policy or derive could render or stream it.
const sessionTokenRef = "sessionToken"

// handlesSecret reports whether ex touches an authentication secret — an
// authorityCap builtin or the sessionToken credential — which pins an action
// that evaluates it to the server.
func handlesSecret(ex ast.Expr) bool {
	if _, ok := procCapabilities(ex)[authorityCap]; ok {
		return true
	}
	return freeNames(ex)[sessionTokenRef]
}

// procCapabilities walks ex and collects every capability its builtin calls
// require, keyed by capability name to one builtin call that needed it (for a
// clear diagnostic — see checkProcCapabilities). This is hasImpure's exact
// former walk (same node kinds, same recursive shape), generalized from a
// single bool to a named set: hasImpure(ex) is now simply "does this set
// contain impureCap", so the two effectful-builtin questions this builder
// answers — "does this force server placement" (now/rand) and "does this
// require a proc to declare a capability" (readFile/writeFile/httpGet/
// httpPost) — are one mechanism, not two that could silently disagree about
// which calls in ex they found.
func procCapabilities(ex ast.Expr) map[string]string {
	caps := map[string]string{}
	var walk func(ast.Expr)
	walk = func(ex ast.Expr) {
		switch t := ex.(type) {
		case ast.Call:
			if t.Name == "now" || t.Name == "rand" || t.Name == "randomBytes" {
				caps[impureCap] = t.Name
			} else if t.Name == "print" {
				caps[printCap] = t.Name
			} else if t.Name == "verifyPassword" || t.Name == "totpSecret" || t.Name == "totpValid" || t.Name == "randomToken" || t.Name == "fileDigest" ||
				t.Name == "ed25519Verify" || t.Name == "ecdsaP256Verify" || t.Name == "sha256Hex" || t.Name == "canonicalJson" || t.Name == "shuffleOrder" {
				caps[authorityCap] = t.Name
			} else if cap, ok := builtinCapability(t.Name); ok {
				caps[cap] = t.Name
			}
			for _, a := range t.Args {
				walk(a)
			}
		case ast.Get:
			walk(t.Obj)
		case ast.EntityGet:
			walk(t.Key)
		case ast.ListLit:
			for _, el := range t.Elems {
				walk(el)
			}
		case ast.MapLit:
			for i, k := range t.Keys {
				walk(k)
				walk(t.Vals[i])
			}
		case ast.StructLit:
			for _, fi := range t.Fields {
				walk(fi.Expr)
			}
		case ast.Bin:
			walk(t.L)
			walk(t.R)
		case ast.Un:
			walk(t.X)
		case ast.Agg:
			if t.Where != nil {
				walk(t.Where)
			}
			if t.Sel != nil {
				walk(t.Sel)
			}
		case ast.Index:
			walk(t.Obj)
			walk(t.Idx)
		}
	}
	walk(ex)
	return caps
}

// hasImpure reports whether ex invokes an effectful builtin that forces server
// placement (now/rand). Every other kind of effect (readFile/writeFile/
// httpGet/httpPost) is barred from ever reaching an action/view/policy/derive
// expression at all by checkNoIO, so by the time hasImpure's two call sites
// (action placement, checkPure) run, procCapabilities(ex) can only ever
// contain impureCap or nothing — this is not a narrower check than the old
// hand-written boolean walk, just the same answer computed off the shared set.
func hasImpure(ex ast.Expr) bool {
	_, ok := procCapabilities(ex)[impureCap]
	return ok
}

// hasPrint reports whether ex invokes print(...) anywhere in its tree — the
// print counterpart to hasImpure, above, with the same "computed off the
// shared procCapabilities set" shape. Its two call sites are env.action's
// placement switch (print forces server placement unconditionally — see
// printCap's doc) and checkPure (print has a side effect and no client
// implementation, so it is barred from a view/check/requires expression,
// exactly as an impure now()/rand() call already is).
func hasPrint(ex ast.Expr) bool {
	_, ok := procCapabilities(ex)[printCap]
	return ok
}

// checkProcCapabilities rejects a proc expression that calls a capability-
// gated builtin (readFile/writeFile/httpGet/httpPost) the proc did not declare
// in its own `uses` clause — the static enforcement side of the capability
// system (builtinCapability/procCapabilities do the walking; parseProc/
// ast.Proc.Uses do the declaring). Capability names are sorted before being
// walked so a proc missing more than one reports the same one first on every
// build, not whichever a Go map iteration happened to visit.
func checkProcCapabilities(p *ast.Proc, ex ast.Expr, line int) error {
	caps := procCapabilities(ex)
	names := make([]string, 0, len(caps))
	for cap := range caps {
		if cap != impureCap && cap != printCap && cap != authorityCap {
			names = append(names, cap)
		}
	}
	sort.Strings(names)
	for _, cap := range names {
		if err := requireProcCapability(p, cap, caps[cap]+"(...)", line); err != nil {
			return err
		}
	}
	return nil
}

// requireProcCapability is checkProcCapabilities' one-capability check,
// pulled out so ast.FileOp's `read`/`write` statements (internal/ir/build.go's
// procBlock, ast.FileOp case) can enforce the exact same `uses io.file` gate
// that a raw readFile/writeFile builtin CALL goes through above — an io.file
// effect requires the capability whichever surface syntax spelled it, so both
// paths report the identical diagnostic. what names the effect in the message
// (e.g. "readFile(...)" for a builtin call, "read Config(...)" for a file
// statement) — the two callers differ only in that string.
func requireProcCapability(p *ast.Proc, cap, what string, line int) error {
	for _, u := range p.Uses {
		if u == cap || (cap == netConnCap && (u == "io.net" || u == "io.net.listen")) {
			return nil
		}
	}
	if cap == netConnCap {
		return &BuildError{line, fmt.Sprintf(
			"proc %q calls %s, which requires capability \"io.net\" (or \"io.net.listen\" for an accepted connection) — declare it on the proc header (e.g. `proc %s(...) -> ... uses io.net:`)",
			p.Name, what, p.Name)}
	}
	return &BuildError{line, fmt.Sprintf(
		"proc %q calls %s, which requires capability %q — declare it on the proc header (e.g. `proc %s(...) -> ... uses %s:`)",
		p.Name, what, cap, p.Name, cap)}
}

// pureBuiltinArity gives the fixed argument count of a pure standard-library
// builtin, and whether the name is one.
func pureBuiltinArity(name string) (int, bool) {
	switch name {
	case "abs", "floor", "round", "money", "len", "upper", "lower", "trim", "year", "month", "day",
		"ago", "compact", "commas", "iso", "fromIso", "first", "fromJson", "bytes", "toFloat", "toInt", "toMoney", "slug",
		"textToBytes", "bytesToText", "byteLen", "floatBits", "floatFromBits",
		"u64Text", "u64Parse", "u64ParseError", "u64ToFloat":
		return 1, true
	case "print":
		// print(value): a debugging aid, not real arithmetic/string/date
		// computation like the rest of this group — arity-checked the exact
		// same way (one argument, any type) because it shares this function's
		// only job (arity), not its "pure" name. See printCap's doc for why it
		// is walked and capability-exempted alongside now/rand instead of
		// living with readFile/writeFile/httpGet/httpPost's capability gate.
		return 1, true
	case "append":
		return 2, true
	case "min", "max", "contains", "take", "split", "join", "charAt",
		"u64Cmp", "u64Min", "u64Max", "u64SatSub", "u64Div", "u64Rem":
		return 2, true
	case "slice", "replace", "aesGcmSeal", "aesGcmOpen", "aesGcmAuthentic":
		return 3, true
	}
	return 0, false
}

// isPrimitive is the set of scalar types real everywhere in the language: an
// entity field, a record field, a state cell, a component parameter, a
// service op's return type, and (via the separate isProcScalar-shaped checks
// in e.proc/procBlock) a proc's own parameters/locals/return type.
//
// "float" is deliberately NOT in this set, even though the parser's isType
// (internal/parser/parser.go) accepts it as a syntactically valid type name —
// it is real only inside a proc (a parameter, a `let`/`let mut` local, or a
// return type), which is why every one of isPrimitive's callers below rejects
// it with its own dedicated, clearer error instead of silently accepting a
// type with no database column, no client-side (assets/facet.js)
// representation, and no wire encoding. A proc's own return-type check
// (e.app-building's procSeen loop) explicitly allows "float" alongside this
// function's answer instead of adding it here, precisely so every OTHER
// caller keeps rejecting it unchanged.
func isPrimitive(t string) bool {
	switch t {
	case "int", "text", "bool", "money", "date", "float", "datetime":
		return true
	}
	return false
}

// writesOnlyClientState reports whether an action writes at least one state cell
// and every cell it writes is `@client`.
//
// "At least one" is load-bearing. An impure action that writes nothing shares no
// result either, but it also has nothing to run FOR — and one of them can carry a
// `requires`, which must be authoritative. Keeping those on the server preserves
// every placement this compiler made before the exception existed; the only
// behaviour that changes is the case the exception was written for.
func writesOnlyClientState(writes map[string]bool, states map[string]string) bool {
	if len(writes) == 0 {
		return false
	}
	for name := range writes {
		if states[name] != Client {
			return false
		}
	}
	return true
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
	if _, ok := e.deriveFns[name]; ok {
		return &BuildError{line, fmt.Sprintf("%s %q collides with an existing derive", kind, name)}
	}
	return nil
}

// deriveOrder returns the derives so that each follows every derive it reads
// (by name or by call), keeping declaration order wherever that already holds,
// and refuses a derive defined in terms of itself.
func deriveOrder(ds []*ast.Derive) ([]*ast.Derive, error) {
	byName := make(map[string]*ast.Derive, len(ds))
	for _, d := range ds {
		byName[d.Name] = d
	}
	const visiting, done = 1, 2
	state := map[string]int{}
	var out []*ast.Derive
	var visit func(d *ast.Derive, path []string) error
	visit = func(d *ast.Derive, path []string) error {
		switch state[d.Name] {
		case done:
			return nil
		case visiting:
			return &BuildError{d.Line, fmt.Sprintf("derive %q is defined in terms of itself (%s)", d.Name, strings.Join(append(path, d.Name), " -> "))}
		}
		state[d.Name] = visiting
		reads := freeNames(d.Expr)
		calledNames(d.Expr, reads)
		for _, p := range d.Params {
			delete(reads, p.Name) // a parameter shadows a derive of its name (and is refused for it)
		}
		for _, n := range sortedKeys(reads) {
			if dep, ok := byName[n]; ok {
				if err := visit(dep, append(path, d.Name)); err != nil {
					return err
				}
			}
		}
		state[d.Name] = done
		out = append(out, d)
		return nil
	}
	for _, d := range ds {
		if err := visit(d, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// calledNames adds the name of every call in ex to out.
func calledNames(ex ast.Expr, out map[string]bool) {
	switch t := ex.(type) {
	case ast.Call:
		out[t.Name] = true
		for _, a := range t.Args {
			calledNames(a, out)
		}
	case ast.Agg:
		for _, sub := range []ast.Expr{t.Where, t.Sel, t.Limit} {
			calledNames(sub, out)
		}
	case ast.Get:
		calledNames(t.Obj, out)
	case ast.EntityGet:
		calledNames(t.Key, out)
	case ast.Bin:
		calledNames(t.L, out)
		calledNames(t.R, out)
	case ast.Un:
		calledNames(t.X, out)
	case ast.ListLit:
		for _, el := range t.Elems {
			calledNames(el, out)
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			calledNames(fi.Expr, out)
		}
	case ast.MapLit:
		for i, k := range t.Keys {
			calledNames(k, out)
			calledNames(t.Vals[i], out)
		}
	case ast.Index:
		calledNames(t.Obj, out)
		calledNames(t.Idx, out)
	}
}

// deriveFn checks a parameterized derive and lowers its body. The body is an
// ordinary pure expression over the actor and the parameters, each typed as
// declared — so `w.bdoy` on a row parameter, a wrong-typed argument to another
// projection, and a body whose type is not the declared one are all refused here,
// at the definition, rather than at whichever call site first expands it.
func (e *env) deriveFn(d *ast.Derive) (*deriveFn, error) {
	if parser.IsBuiltinCall(d.Name) {
		return nil, &BuildError{d.Line, fmt.Sprintf("derive %q collides with the builtin %s(...) — a parameterized derive is called the same way", d.Name, d.Name)}
	}
	locals := withActor(nil)
	sc := scope{locals: map[string]bool{}, varTypes: map[string]vtype{}}
	for _, p := range d.Params {
		if isBuiltinRef(p.Name) {
			return nil, &BuildError{d.Line, fmt.Sprintf("derive %q's parameter %q shadows the builtin %q", d.Name, p.Name, p.Name)}
		}
		if sc.locals[p.Name] {
			return nil, &BuildError{d.Line, fmt.Sprintf("derive %q has duplicate parameter %q", d.Name, p.Name)}
		}
		if _, ok := e.inline[p.Name]; ok || e.deriveFns[p.Name] != nil {
			return nil, &BuildError{d.Line, fmt.Sprintf("derive %q's parameter %q shadows the policy or derive of that name", d.Name, p.Name)}
		}
		t := e.declType(p.Type, p.List)
		if !t.known() {
			return nil, &BuildError{d.Line, fmt.Sprintf("derive %q's parameter %q has unknown type %q", d.Name, p.Name, p.Type)}
		}
		locals[p.Name] = true
		sc.locals[p.Name] = true
		sc.varTypes[p.Name] = t
	}
	e.deriveParamTypes = sc.varTypes
	defer func() { e.deriveParamTypes = nil }()
	core, list := strings.TrimSuffix(strings.TrimPrefix(d.Type, "["), "]"), strings.HasPrefix(d.Type, "[")
	ret := e.declType(core, list)
	if e.wireTypes[core] {
		ret = vtype{core: core, list: list}
	}
	if !ret.known() {
		return nil, &BuildError{d.Line, fmt.Sprintf("derive %q returns unknown type %q", d.Name, d.Type)}
	}
	ctx := fmt.Sprintf("derive %q", d.Name)
	if err := e.checkPure(d.Expr, locals, d.Line, ctx); err != nil {
		return nil, err
	}
	if err := (&viewCtx{e: e}).checkRowFields(d.Expr, sc, d.Line); err != nil {
		return nil, err
	}
	if err := e.checkDeriveArgs(d.Expr, sc, d.Line); err != nil {
		return nil, err
	}
	if got := e.exprType(d.Expr, sc); !e.assignable(got, ret) {
		return nil, &BuildError{d.Line, fmt.Sprintf("derive %q is declared %s, but its definition is %s", d.Name, ret.label(), got.label())}
	}
	return &deriveFn{params: d.Params, ret: ret, body: e.low(d.Expr)}, nil
}

// checkDeriveArgs checks every call of a parameterized derive in ex against the
// derive's parameters, in the scope ex is written in (an aggregate's item
// variable typed as a row of the collection it walks).
func (e *env) checkDeriveArgs(ex ast.Expr, sc scope, line int) error {
	switch t := ex.(type) {
	case ast.Call:
		if fn, ok := e.deriveFns[t.Name]; ok && len(t.Args) == len(fn.params) {
			for i, p := range fn.params {
				if err := e.checkDeriveArg(t.Name, i, p, t.Args[i], sc, line); err != nil {
					return err
				}
			}
		}
		for _, a := range t.Args {
			if err := e.checkDeriveArgs(a, sc, line); err != nil {
				return err
			}
		}
	case ast.Agg:
		inner := sc
		if t.Var != "" {
			inner = sc.with(t.Var)
			inner.varTypes[t.Var] = vtype{core: t.Coll}
		}
		for _, sub := range []ast.Expr{t.Where, t.Sel} {
			if err := e.checkDeriveArgs(sub, inner, line); err != nil {
				return err
			}
		}
		return e.checkDeriveArgs(t.Limit, sc, line)
	case ast.Get:
		return e.checkDeriveArgs(t.Obj, sc, line)
	case ast.EntityGet:
		return e.checkDeriveArgs(t.Key, sc, line)
	case ast.Bin:
		if err := e.checkDeriveArgs(t.L, sc, line); err != nil {
			return err
		}
		return e.checkDeriveArgs(t.R, sc, line)
	case ast.Un:
		return e.checkDeriveArgs(t.X, sc, line)
	case ast.ListLit:
		for _, el := range t.Elems {
			if err := e.checkDeriveArgs(el, sc, line); err != nil {
				return err
			}
		}
	case ast.StructLit:
		for _, fi := range t.Fields {
			if err := e.checkDeriveArgs(fi.Expr, sc, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkDeriveArg is checkArgType's rule for one argument of a derive call: the
// value must be assignable to the parameter, and an entity parameter needs a
// row. A bare local the builder holds no type for (an action `let`) may be one
// and is accepted; anything shown not to be one — an id like `w.id` or
// `h.work`, a literal — is refused, since it would read every field as nothing.
func (e *env) checkDeriveArg(name string, i int, p ast.Param, arg ast.Expr, sc scope, line int) error {
	want := e.declType(p.Type, p.List)
	got := e.exprType(arg, sc)
	if !e.assignable(got, want) {
		return &BuildError{line, fmt.Sprintf("derive %q parameter %q is %s, but argument %d is %s", name, p.Name, want.label(), i+1, got.label())}
	}
	if want.known() && !want.list && e.entities[want.core] && !isRowExpr(arg, sc, want.core) {
		if r, isRef := arg.(ast.Ref); isRef {
			if _, typed := sc.varTypes[r.Name]; !typed {
				return nil
			}
		}
		return &BuildError{line, fmt.Sprintf(
			"derive %q parameter %q is a %s row, but argument %d (%s) is not one — pass the row itself (the variable a `list(… in %s …)` binds, or `%s(id)`), not its id",
			name, p.Name, want.core, i+1, describeArg(arg, got), want.core, want.core)}
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
			// So may the value a list shapes per row (a Dto{…} counting another
			// entity's rows) and its limit.
			walk(x.Where)
			walk(x.Sel)
			walk(x.Limit)
		case "call", "list", "struct":
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

// actionScope is a type scope over locals carrying the types known for an
// action's parameters and `let`s (empty outside an action body).
func (e *env) actionScope(locals map[string]bool) scope {
	vt := map[string]vtype{}
	for n, t := range e.actLocalTypes {
		if locals[n] {
			vt[n] = t
		}
	}
	return scope{locals: locals, varTypes: vt}
}

// stmtsCall returns the name of the first call in body (any expression, any
// nesting) that match accepts, or "" when there is none.
func stmtsCall(body []Stmt, match func(name string) bool) string {
	var hit func(x *Expr) string
	hit = func(x *Expr) string {
		if x == nil {
			return ""
		}
		if x.Kind == "call" && match(x.Name) {
			return x.Name
		}
		for _, k := range x.Kids() {
			if n := hit(k); n != "" {
				return n
			}
		}
		return ""
	}
	for _, st := range body {
		for _, x := range []*Expr{st.Value, st.Key, st.Where, st.Limit, st.Role} {
			if n := hit(x); n != "" {
				return n
			}
		}
		for _, a := range st.Args {
			if n := hit(a); n != "" {
				return n
			}
		}
		for _, f := range st.Fields {
			if n := hit(f.Expr); n != "" {
				return n
			}
		}
		if n := stmtsCall(st.Body, match); n != "" {
			return n
		}
		if n := stmtsCall(st.Else, match); n != "" {
			return n
		}
	}
	return ""
}

func findMessage(ms []WireMessage, name string) *WireMessage {
	for i := range ms {
		if ms[i].Name == name {
			return &ms[i]
		}
	}
	return nil
}

// dispatchRoute validates `api METHOD "/path" -> Message`: a body-carrying,
// literal route whose every variant names the action it runs, and whose every
// variant field (and alias) binds one of that action's parameters, the rest
// of which must be optional. Each variant's action runs on the authority.
func (e *env) dispatchRoute(ap *ast.API, msg *WireMessage, acts map[string]*Action, apiSeen map[string]int) error {
	if ap.Method == "GET" || ap.Method == "DELETE" {
		return &BuildError{ap.Line, fmt.Sprintf("api %s %q -> %s: a dispatching route decodes its body, so it is a POST, PUT or PATCH", ap.Method, ap.Path, msg.Name)}
	}
	if strings.ContainsAny(ap.Path, "{}") {
		return &BuildError{ap.Line, fmt.Sprintf("api %s %q -> %s: a dispatching route's path is literal — the variant carries everything", ap.Method, ap.Path, msg.Name)}
	}
	if ap.Bearer != "" {
		return &BuildError{ap.Line, fmt.Sprintf("api %s %q -> %s: each variant's action carries its own auth", ap.Method, ap.Path, msg.Name)}
	}
	key := ap.Method + " " + ap.Path
	if prev, ok := apiSeen[key]; ok {
		return &BuildError{ap.Line, fmt.Sprintf("api %s %q redeclared (first at line %d)", ap.Method, ap.Path, prev)}
	}
	apiSeen[key] = ap.Line
	for _, v := range msg.Variants {
		if v.Action == "" {
			return &BuildError{ap.Line, fmt.Sprintf("api %s %q -> %s: variant %q names no action (| %s -> action)", ap.Method, ap.Path, msg.Name, v.WireName(), v.Name)}
		}
		act, ok := acts[v.Action]
		if !ok {
			return &BuildError{ap.Line, fmt.Sprintf("%s variant %q runs unknown action %q", msg.Name, v.WireName(), v.Action)}
		}
		bound := map[string]bool{}
		if v.BodyParam != "" {
			ok := false
			for _, p := range act.Params {
				ok = ok || (p.Name == v.BodyParam && p.Type == v.BodyType && !p.List)
			}
			if !ok {
				return &BuildError{ap.Line, fmt.Sprintf("%s variant %q: body %s: %s must be a %s parameter of action %q", msg.Name, v.WireName(), v.BodyParam, v.BodyType, v.BodyType, v.Action)}
			}
			bound[v.BodyParam] = true
		}
		for _, f := range v.Fields {
			name := f.Name
			if f.Into != "" {
				name = f.Into
			}
			var param *Param
			for i := range act.Params {
				if act.Params[i].Name == name {
					param = &act.Params[i]
				}
			}
			if f.Into != "" {
				if param == nil || param.Type != "json" || !param.Optional {
					return &BuildError{ap.Line, fmt.Sprintf("%s variant %q: pattern field %q gathers into %q, which must be an optional json parameter of action %q", msg.Name, v.WireName(), f.Name, f.Into, v.Action)}
				}
				bound[name] = true
				continue
			}
			if param == nil {
				return &BuildError{ap.Line, fmt.Sprintf("%s variant %q field %q is not a parameter of action %q", msg.Name, v.WireName(), f.Name, v.Action)}
			}
			if param.List != f.List {
				return &BuildError{ap.Line, fmt.Sprintf("%s variant %q field %q and action %q's parameter disagree on being a list", msg.Name, v.WireName(), f.Name, v.Action)}
			}
			bound[f.Name] = true
		}
		for _, p := range act.Params {
			if !bound[p.Name] && !p.Optional {
				return &BuildError{ap.Line, fmt.Sprintf("%s variant %q does not carry action %q's required parameter %q", msg.Name, v.WireName(), v.Action, p.Name)}
			}
		}
		if act.Placement != Server {
			act.Placement = Server
			act.Reason = fmt.Sprintf("serves %s %s (%s) — an endpoint runs on the authority", ap.Method, ap.Path, v.WireName())
		}
	}
	return nil
}

// authorityOnlyCall applies the proc rule to a builtin only the server
// implements (no browser mirror): callable in an action, a proc, or a derive
// (which then is usable only in actions) — never a view or policy, which are
// evaluated client-side too.
func (e *env) authorityOnlyCall(name string, line int) error {
	switch {
	case e.inDerive != "":
		e.procDerives[e.inDerive] = true
	case e.actLocalTypes == nil && !e.inProc && !e.inDaemon:
		return &BuildError{line, fmt.Sprintf("%s runs only on the authority, so it can be called only in an action, a proc, or a derive used by one — not in a view or policy", name)}
	}
	return nil
}

// checkArgRowVars refuses a `list(f(x, …) in Coll)` whose row name was read
// off a bare call argument (ast.Agg.VarFromArg) when that name is also a
// local: the author may have meant the local, so the row must be named
// explicitly (`where x.id > 0` or a field read).
func checkArgRowVars(ex ast.Expr, locals map[string]bool, line int) error {
	var err error
	ast.WalkExpr(ex, func(n ast.Expr) {
		if a, ok := n.(ast.Agg); ok && a.VarFromArg && locals[a.Var] && err == nil {
			err = &BuildError{line, fmt.Sprintf("list(... in %s): %q is a local, so it cannot also name the row — read a field of the row (list(f(r) in %s where r.id > 0)) or rename the local", a.Coll, a.Var, a.Coll)}
		}
	})
	return err
}

// freeNames returns every root name an expression references.
// detachIntrinsic and the shared-cell intrinsics are the runtime entry points
// procBlock lowers `detach P(args)` and a shared cell's reads/assignments to
// (runtime/daemon.go, runtime/shared.go). The `$` makes each a name no
// source program can spell: they exist only as lowered IR.
const (
	detachIntrinsic    = "$detach"
	sharedGetIntrinsic = "$shared.get"
	sharedSetIntrinsic = "$shared.set"
)

// buildShareds validates every `shared` declaration (see ast.Shared) and
// records it for the proc-shaped bodies built after it. A cell's type is a
// primitive or a declared struct/record/enum (or a list of one); its name may
// not also name a state cell, an entity or a proc, since a proc body resolves
// a bare name to exactly one thing.
func (e *env) buildShareds(app *ast.App, out *IR) error {
	e.shareds = map[string]*ast.Shared{}
	for _, sh := range app.Shareds {
		if prev := e.shareds[sh.Name]; prev != nil {
			return &BuildError{sh.Line, fmt.Sprintf("shared cell %q redeclared (first at line %d)", sh.Name, prev.Line)}
		}
		_, isState := e.states[sh.Name]
		_, isProc := e.procSigs[sh.Name]
		if isState || isProc || e.entities[sh.Name] {
			return &BuildError{sh.Line, fmt.Sprintf("shared cell %q collides with a state cell, entity or proc of the same name", sh.Name)}
		}
		if !isPrimitive(sh.Type) {
			_, isEnum := e.enums[sh.Type]
			_, isRec := e.records[sh.Type]
			_, isStruct := e.structs[sh.Type]
			if !isEnum && !isRec && !isStruct {
				return &BuildError{sh.Line, fmt.Sprintf("shared cell %q has unknown type %q", sh.Name, sh.Type)}
			}
		}
		e.shareds[sh.Name] = sh
		out.Shareds = append(out.Shareds, Shared{Name: sh.Name, Type: sh.Type, List: sh.List})
	}
	return nil
}

// checkNotShared refuses a parameter or local named like a shared cell: a
// proc body's bare name must mean one thing, and lowerSharedRefs rewrites
// every reference to a cell's name.
func (e *env) checkNotShared(name string, line int) error {
	if e.shareds[name] != nil {
		return &BuildError{line, fmt.Sprintf("%q is a shared cell — a parameter or local may not reuse its name", name)}
	}
	return nil
}

// seedSharedTypes gives every shared cell its declared type in a proc
// body's type map, so inferProcType and the struct/index checks see a read
// of one exactly as they see a local of that type.
func (e *env) seedSharedTypes(types map[string]string) {
	for name, sh := range e.shareds {
		if sh.List {
			types[name] = arrayType
		} else {
			types[name] = sh.Type
		}
	}
}

// checkSharedAssign type-checks `cell = value` where the value's type is
// provable, the same leniency a struct field write has.
func (e *env) checkSharedAssign(sh *ast.Shared, value ast.Expr, types map[string]string, line int) error {
	vt := inferProcType(value, types)
	if vt == "" {
		return nil
	}
	want := sh.Type
	if sh.List {
		want = arrayType
	}
	if vt != want && !(want == "float" && vt == "int") {
		return &BuildError{line, fmt.Sprintf("cannot assign %s to shared cell %q, whose type is %s", vt, sh.Name, want)}
	}
	return nil
}

// lowerSharedRefs rewrites every read of a shared cell in a lowered proc or
// daemon body into a call of sharedGetIntrinsic — one atomic snapshot of the
// cell per evaluation. Parameters and locals can never carry a cell's name
// (checkNotShared), so every ref by that name is the cell.
func (e *env) lowerSharedRefs(body []Stmt) {
	if len(e.shareds) == 0 {
		return
	}
	var expr func(x *Expr)
	expr = func(x *Expr) {
		if x == nil {
			return
		}
		if x.Kind == "ref" && e.shareds[x.Name] != nil {
			name := x.Name
			*x = Expr{Kind: "call", Name: sharedGetIntrinsic, Args: []*Expr{{Kind: "lit", Val: name, VType: "text"}}}
			return
		}
		for _, k := range x.Kids() {
			expr(k)
		}
		expr(x.OrderBy)
	}
	var stmts func(ss []Stmt)
	stmts = func(ss []Stmt) {
		for i := range ss {
			st := &ss[i]
			expr(st.Key)
			expr(st.Value)
			expr(st.Where)
			expr(st.Limit)
			expr(st.Role)
			for _, a := range st.Args {
				expr(a)
			}
			for _, f := range st.Fields {
				expr(f.Expr)
			}
			stmts(st.Body)
			stmts(st.Else)
		}
	}
	stmts(body)
}

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
			for _, sub := range [3]ast.Expr{t.Where, t.Sel, t.OrderExpr} {
				if sub == nil {
					continue
				}
				for n := range freeNames(sub) {
					if n != t.Var {
						out[n] = true
					}
				}
			}
			// A list's limit is evaluated once, outside the row, so its names
			// resolve in the enclosing scope like any other.
			if t.Limit != nil {
				for n := range freeNames(t.Limit) {
					out[n] = true
				}
			}
		case ast.Call:
			for _, a := range t.Args {
				walk(a)
			}
		case ast.ListLit:
			// Was missing until array support needed it: a list literal's elements
			// can themselves be references (`[a, b, c]`), and without this case a
			// name used only inside one silently skipped checkProcExpr's/check()'s
			// name-resolution pass instead of being validated like everywhere else
			// that name could appear.
			for _, el := range t.Elems {
				walk(el)
			}
		case ast.MapLit:
			// Same reasoning as ast.ListLit above: a map literal's keys and values
			// can themselves be references (`{a: b}`).
			for i, k := range t.Keys {
				walk(k)
				walk(t.Vals[i])
			}
		case ast.StructLit:
			// Same reasoning again: a struct literal's field values can
			// themselves be references (`Node{left: a, right: b}`).
			for _, fi := range t.Fields {
				walk(fi.Expr)
			}
		case ast.Index:
			walk(t.Obj)
			walk(t.Idx)
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
func (e *env) low(ex ast.Expr) *Expr {
	out := lower(ex, e.inline, e.deriveFns, e.enums)
	e.markTextAggs(out)
	e.deriveOrders(out)
	e.markOmits(out)
	return out
}

// markOmits records, on every wire-type literal, which of its fields are
// optional (`field: T?`), so the evaluators leave an empty one out.
func (e *env) markOmits(x *Expr) {
	if x == nil {
		return
	}
	if x.Kind == "struct" && x.Omit == nil {
		for _, f := range x.Fields {
			if e.wireOptional[x.Name][f] {
				x.Omit = append(x.Omit, f)
			}
		}
		x.Nulls = e.wireNullable[x.Name]
	}
	for _, k := range x.Kids() {
		e.markOmits(k)
	}
	e.markOmits(x.OrderBy)
}

// deriveOrders turns `list(… by d)` where d is an entity derive (a value
// computed from the row, never a column) into the computed key it is: the
// derive's expression over the list's row.
func (e *env) deriveOrders(x *Expr) {
	if x == nil {
		return
	}
	if x.Kind == "agg" && x.Order != "" && e.entDeriveExprs[x.Name][x.Order] != nil {
		v := x.Var
		if v == "" {
			v = "$item"
			x.Var = v
		}
		x.OrderBy = substParams(e.entDeriveExprs[x.Name][x.Order], map[string]*Expr{"$row": {Kind: "ref", Name: v}}, map[string]bool{}, map[string]bool{})
		x.Order = ""
	}
	for _, k := range x.Kids() {
		e.deriveOrders(k)
	}
}

// markTextAggs tags every min/max over a text column VType "text", so both
// evaluators order the values as text and answer "" over an empty range
// (a store's reduction is integer-valued and never answers it).
func (e *env) markTextAggs(x *Expr) {
	if x == nil {
		return
	}
	if x.Kind == "agg" && (x.Op == "min" || x.Op == "max") && x.Field != "" && e.entFieldType[x.Name][x.Field] == "text" {
		x.VType = "text"
	}
	for _, k := range x.Kids() {
		e.markTextAggs(k)
	}
}

// lower converts an ast.Expr to its serializable IR form, inlining any reference
// to a policy or derive name with that name's lowered expression (so the same
// value is computed identically wherever it is read — a server gate, a view
// `if`, or another derivation), expanding any call of a parameterized derive
// (expandDerive), and folding enum member access (`Status.active`) to its
// backing text literal.
func lower(ex ast.Expr, inline map[string]*Expr, fns map[string]*deriveFn, enums map[string][]string) *Expr {
	switch t := ex.(type) {
	case ast.Lit:
		return &Expr{Kind: "lit", Val: t.Val, VType: t.Kind}
	case *ast.Asset:
		// checkBuiltins (called before lower() at every site that reaches it —
		// see check()'s doc) has already refused an unresolved Asset, so by
		// this point Kind/Val are exactly what an ast.Lit's are: the file's own
		// text, or the URL it was copied to.
		return &Expr{Kind: "lit", Val: t.Val, VType: t.Kind}
	case ast.ListLit:
		out := &Expr{Kind: "list"}
		for _, el := range t.Elems {
			out.Args = append(out.Args, lower(el, inline, fns, enums))
		}
		return out
	case ast.MapLit:
		// Keys and Args (its values) stay parallel, exactly as ast.MapLit's own
		// Keys/Vals do — see its doc.
		out := &Expr{Kind: "map"}
		for i := range t.Keys {
			out.Keys = append(out.Keys, lower(t.Keys[i], inline, fns, enums))
			out.Args = append(out.Args, lower(t.Vals[i], inline, fns, enums))
		}
		return out
	case ast.StructLit:
		// Fields (its field names) and Args (its values) stay parallel, the
		// same shape MapLit's Keys/Args already have — see ir.Expr's doc.
		out := &Expr{Kind: "struct", Name: t.Type}
		for _, fi := range t.Fields {
			out.Fields = append(out.Fields, fi.Name)
			out.Args = append(out.Args, lower(fi.Expr, inline, fns, enums))
		}
		return out
	case ast.Index:
		// Reuses Obj (the array) and Key (the index expression) — the same fields
		// "get" and "eget" already carry an addressing sub-expression in — rather
		// than adding new Expr fields just for this one kind.
		return &Expr{Kind: "index", Obj: lower(t.Obj, inline, fns, enums), Key: lower(t.Idx, inline, fns, enums)}
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
		return &Expr{Kind: "get", Obj: lower(t.Obj, inline, fns, enums), Field: t.Field}
	case ast.EntityGet:
		return &Expr{Kind: "eget", Name: t.Entity, Key: lower(t.Key, inline, fns, enums), Field: t.Field}
	case ast.ActState:
		return &Expr{Kind: "astate", Op: t.Op, Name: t.Action}
	case ast.Agg:
		a := &Expr{Kind: "agg", Op: t.Op, Name: t.Coll, Field: t.Field, Var: t.Var, Order: t.Order, Desc: t.Desc}
		if t.Where != nil {
			a.Where = lower(t.Where, inline, fns, enums)
		}
		if t.Limit != nil {
			a.Limit = lower(t.Limit, inline, fns, enums)
		}
		if t.OrderExpr != nil {
			a.OrderBy = lower(t.OrderExpr, inline, fns, enums)
		}
		if t.Sel != nil {
			// The reduced value, when it is more than one of the row's columns.
			// Exclusive with Field (the parser guarantees it), so a bare
			// `sum(x.amount in …)` lowers exactly as it always did and every
			// program written before this is byte-identical through the compiler.
			a.Sel = lower(t.Sel, inline, fns, enums)
		}
		return a
	case ast.Call:
		out := &Expr{Kind: "call", Name: t.Name}
		for _, a := range t.Args {
			out.Args = append(out.Args, lower(a, inline, fns, enums))
		}
		if fn, ok := fns[t.Name]; ok {
			return expandDerive(fn, out.Args)
		}
		return out
	case ast.Bin:
		return &Expr{Kind: "bin", Op: t.Op, L: lower(t.L, inline, fns, enums), R: lower(t.R, inline, fns, enums)}
	case ast.Un:
		return &Expr{Kind: "un", Op: t.Op, X: lower(t.X, inline, fns, enums)}
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
	c.Sel = cloneExpr(e.Sel)
	c.Limit = cloneExpr(e.Limit)
	c.OrderBy = cloneExpr(e.OrderBy)
	if e.Args != nil {
		c.Args = make([]*Expr, len(e.Args))
		for i, a := range e.Args {
			c.Args[i] = cloneExpr(a)
		}
	}
	if e.Keys != nil {
		c.Keys = make([]*Expr, len(e.Keys))
		for i, k := range e.Keys {
			c.Keys[i] = cloneExpr(k)
		}
	}
	return &c
}

// expandDerive is one call of a parameterized derive: its lowered body with each
// parameter replaced by the lowered argument passed for it. The result is
// exactly the expression the author would have written out by hand at the call
// site, so every consumer of the IR — both evaluators, the materializer's
// aggregate addresses, query pushdown of the surrounding `list(… where …)` —
// sees nothing new.
//
// Substitution is hygienic. An argument is caller-scope code, so an aggregate in
// the body whose item variable happens to share a name with anything the
// arguments read is renamed first — otherwise `accountCard(w, me)` handed an
// outer row `w` into a body counting `w in Work` would silently count against
// the wrong row. An item variable that shares a parameter's name shadows it
// inside that aggregate, exactly as it did in the body as written.
func expandDerive(fn *deriveFn, args []*Expr) *Expr {
	bind := make(map[string]*Expr, len(fn.params))
	argNames := map[string]bool{}
	for i, p := range fn.params {
		bind[p.Name] = args[i]
		irNames(args[i], argNames)
	}
	taken := map[string]bool{}
	for n := range argNames {
		taken[n] = true
	}
	irNames(fn.body, taken)
	return substParams(fn.body, bind, argNames, taken)
}

// substParams clones x with every ref bound in bind replaced by a copy of its
// binding — see expandDerive for the renaming argNames and taken drive.
func substParams(x *Expr, bind map[string]*Expr, argNames, taken map[string]bool) *Expr {
	if x == nil {
		return nil
	}
	switch x.Kind {
	case "ref":
		if a, ok := bind[x.Name]; ok {
			return cloneExpr(a)
		}
	case "get":
		obj := substParams(x.Obj, bind, argNames, taken)
		if obj.Kind == "eget" && obj.Field == "" {
			// A row parameter handed a lookup, `card(Work(id))`: `w.body` reads
			// the field straight off it, the same `Work(id).body` a hand-written
			// body would have spelled.
			obj.Field = x.Field
			return obj
		}
		return &Expr{Kind: "get", Obj: obj, Field: x.Field}
	case "agg":
		c := *x
		c.Limit = substParams(x.Limit, bind, argNames, taken)
		inner := bind
		if x.Var != "" {
			inner = make(map[string]*Expr, len(bind)+1)
			for k, v := range bind {
				if k != x.Var {
					inner[k] = v
				}
			}
			if argNames[x.Var] {
				fresh := x.Var
				for i := 2; taken[fresh]; i++ {
					fresh = fmt.Sprintf("%s%d", x.Var, i)
				}
				taken[fresh] = true
				inner[x.Var] = &Expr{Kind: "ref", Name: fresh}
				c.Var = fresh
			}
		}
		c.Where = substParams(x.Where, inner, argNames, taken)
		c.Sel = substParams(x.Sel, inner, argNames, taken)
		c.OrderBy = substParams(x.OrderBy, inner, argNames, taken)
		return &c
	}
	c := *x
	c.L = substParams(x.L, bind, argNames, taken)
	c.R = substParams(x.R, bind, argNames, taken)
	c.X = substParams(x.X, bind, argNames, taken)
	c.Obj = substParams(x.Obj, bind, argNames, taken)
	c.Key = substParams(x.Key, bind, argNames, taken)
	if x.Args != nil {
		c.Args = make([]*Expr, len(x.Args))
		for i, a := range x.Args {
			c.Args[i] = substParams(a, bind, argNames, taken)
		}
	}
	if x.Keys != nil {
		c.Keys = make([]*Expr, len(x.Keys))
		for i, k := range x.Keys {
			c.Keys[i] = substParams(k, bind, argNames, taken)
		}
	}
	return &c
}

// irNames adds every name a lowered expression refs or binds to out.
func irNames(x *Expr, out map[string]bool) {
	if x == nil {
		return
	}
	if x.Kind == "ref" {
		out[x.Name] = true
	}
	if x.Var != "" {
		out[x.Var] = true
	}
	for _, k := range x.Kids() {
		irNames(k, out)
	}
	irNames(x.OrderBy, out)
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
// (`verified`), the active tenant id (`tenant`), the actor's role within it
// (`tenantRole`), or the caller's session id (`session`). The tenant values are
// 0/"" unless multi-tenancy is enabled. `session` is set for every caller,
// signed in or not — it is the one identity a pre-login, anonymous visitor
// already has, minted the moment their browser first arrives (see
// Server.session), so it is what an anonymous-cart-style feature keys state to
// before there is an `actor` to key it to instead.
func isBuiltinRef(n string) bool {
	switch n {
	case "actor", "role", "verified", "tenant", "tenantRole", "session":
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
	m := map[string]bool{"actor": true, "role": true, "verified": true, "tenant": true, "tenantRole": true, "session": true}
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

// runtimeOwnedHeaders are the response headers an action may not set with
// `header "Name" expr`: the ones the runtime itself writes and whose meaning
// it guarantees — the session (cookies, X-Session-Token), HTTP framing and
// content negotiation, caching and conditional GET, rate limiting, auth
// challenges, and the security policy every response carries. An app header
// that overrode one of these would silently break a guarantee made elsewhere.
var runtimeOwnedHeaders = map[string]bool{
	"Set-Cookie": true, "Cookie": true, "X-Session-Token": true,
	"Content-Type": true, "Content-Length": true, "Content-Encoding": true,
	"Content-Disposition": true, "Content-Range": true, "Accept-Ranges": true,
	"Transfer-Encoding": true, "Connection": true, "Keep-Alive": true,
	"Upgrade": true, "Trailer": true, "Te": true, "Host": true, "Date": true,
	"Server": true, "Location": true, "Authorization": true, "Www-Authenticate": true,
	"Cache-Control": true, "Etag": true, "Last-Modified": true, "Vary": true,
	"Expires": true, "Pragma": true, "Age": true, "Retry-After": true,
	"Strict-Transport-Security": true, "Content-Security-Policy": true,
	"X-Content-Type-Options": true, "X-Frame-Options": true, "Referrer-Policy": true,
	"Permissions-Policy": true, "Allow": true, "Alt-Svc": true,
}

// runtimeOwnedHeaderPrefixes are whole families the runtime owns (CORS, the
// rate-limit quota, proxy auth, cross-origin isolation).
var runtimeOwnedHeaderPrefixes = []string{"Access-Control-", "X-Ratelimit-", "Proxy-", "Cross-Origin-", "Sec-"}

// checkActionHeaderName vets `header "Name" expr`'s name: an RFC 9110 token,
// and not a header the runtime owns.
func checkActionHeaderName(name string) error {
	if name == "" {
		return fmt.Errorf("header needs a name: header \"HX-Redirect\" expr")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return fmt.Errorf("header name %q is not an HTTP token (letters, digits, and !#$%%&'*+-.^_`|~ only)", name)
		}
	}
	canon := textproto.CanonicalMIMEHeaderKey(name)
	owned := runtimeOwnedHeaders[canon]
	for _, p := range runtimeOwnedHeaderPrefixes {
		owned = owned || strings.HasPrefix(canon, p)
	}
	if owned {
		return fmt.Errorf("header %q is written by the runtime itself (session, framing, caching, rate limits or security policy) — an action cannot set it", canon)
	}
	return nil
}

// apiDocs checks a declared route's contract documentation against its action
// and returns the parameter docs as the IR carries them: every body parameter
// of the action is a field of the body type, with its type, and each documented parameter is
// one the route reads from its path or query, with a type a client may send
// it as. `errors` is the route's published error set, as its contract states
// it (validated by the parser: distinct 4xx/5xx codes).
func (e *env) apiDocs(ap *ast.API, act *Action, pathParams []string, types []WireType) ([]APIParamDoc, error) {
	where := fmt.Sprintf("api %s %q", ap.Method, ap.Path)
	inPath := map[string]bool{}
	for _, p := range pathParams {
		inPath[p] = true
	}
	params := map[string]Param{}
	for _, p := range act.Params {
		params[p.Name] = p
	}
	if ap.Body != "" {
		if ap.Method == "GET" {
			return nil, &BuildError{ap.Line, where + ": a GET carries no request body — drop `body`"}
		}
		var wt *WireType
		for i := range types {
			if types[i].Name == ap.Body {
				wt = &types[i]
			}
		}
		if wt == nil {
			return nil, &BuildError{ap.Line, fmt.Sprintf("%s: body %q is not a declared wire type", where, ap.Body)}
		}
		fields := map[string]WireField{}
		for _, f := range wt.Fields {
			fields[f.Name] = f
		}
		for _, p := range act.Params {
			if inPath[p.Name] || p.Name == ap.Bearer {
				continue
			}
			f, ok := fields[p.Name]
			if !ok {
				return nil, &BuildError{ap.Line, fmt.Sprintf("%s: action %q's body parameter %q is not a field of body %s", where, act.Name, p.Name, ap.Body)}
			}
			structured := (f.Map || f.Depth > 0) && p.Type == "json" && !p.List       // a map or nested list binds a json parameter
			sameType := f.Type == p.Type || (f.Type == "number" && p.Type == "float") // a wire number is a float
			if !structured && (!sameType || f.List != p.List || f.Map || f.Depth > 0) {
				return nil, &BuildError{ap.Line, fmt.Sprintf("%s: body %s field %q and action %q's parameter disagree on its type", where, ap.Body, p.Name, act.Name)}
			}
		}
		// A field the action does not take is part of the published body
		// (a legacy client may send it) and is ignored.
	}
	var docs []APIParamDoc
	for _, d := range ap.ParamDocs {
		p, ok := params[d.Name]
		if !ok {
			return nil, &BuildError{d.Line, fmt.Sprintf("%s documents %q, which is not a parameter of action %q", where, d.Name, act.Name)}
		}
		if !inPath[d.Name] && ap.Method != "GET" {
			return nil, &BuildError{d.Line, fmt.Sprintf("%s documents %q, which travels in the body — document it on the body type", where, d.Name)}
		}
		// The documented type is the parameter's own, or text: an int a
		// client holds as an opaque string, or a list it sends comma-separated.
		asText := d.Type == "text" && !d.List && (p.Type == "int" || p.List)
		if (d.List != p.List && !asText) || d.Optional || d.Default != nil || len(d.Aliases) > 0 || d.Into != "" {
			return nil, &BuildError{d.Line, fmt.Sprintf("%s: a parameter doc is `name: type \"description\" [one of …]`, its type the action parameter's (or text for an int a client holds as an opaque string, or a list it sends comma-separated)", where)}
		}
		if d.Type != p.Type && !asText && !(d.Type == "number" && p.Type == "float") {
			return nil, &BuildError{d.Line, fmt.Sprintf("%s documents %q as %s, but action %q takes it as %s", where, d.Name, d.Type, act.Name, p.Type)}
		}
		docs = append(docs, APIParamDoc{Name: d.Name, Type: d.Type, Description: d.Description, Enum: d.Enum})
	}
	return docs, nil
}
