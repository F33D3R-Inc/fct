// Package ir defines the Facet Intermediate Representation: the fully-resolved
// application graph that every target reads. It is the real product — the
// language is an authoring surface and the runtimes are dumb executors over
// this. The IR is plain, target-neutral data.
package ir

// Placement domains, computed by the compiler, never authored.
const (
	Server = "server" // the authority: durable/shared/secured state and its mutations
	Client = "client" // ephemeral, local, latency-sensitive state and its mutations
)

// IR is one compiled application.
type IR struct {
	App        string                       `json:"app"`
	Auth       bool                         `json:"auth,omitempty"` // built-in users/login enabled
	Entities   []Entity                     `json:"entities"`
	Records    []Record                     `json:"records,omitempty"` // value-object types: the typed shape of a service (brain) return
	Structs    []Struct                     `json:"structs,omitempty"` // proc-local named-field composite types (declared with `struct Name:`)
	Enums      []Enum                       `json:"enums,omitempty"`
	Types      []WireType                   `json:"types,omitempty"`    // wire-schema value types (SCHEMA_IDL_SCOPE.md Tier C) — codegen only, never entity/action-bound
	Messages   []WireMessage                `json:"messages,omitempty"` // wire-schema tagged unions
	States     []State                      `json:"states"`
	Derives    []Derive                     `json:"derives"`
	Policies   []Policy                     `json:"policies"`
	Actions    []Action                     `json:"actions"`
	Procs      []Proc                       `json:"procs,omitempty"` // general-purpose, unconditionally server-executed code (self-hosting + product logic)
	Jobs       []Job                        `json:"jobs"`
	Components []Component                  `json:"components,omitempty"` // reusable view fragments
	Services   []Service                    `json:"services,omitempty"`   // external services (brains) actions may call
	Files      []File                       `json:"files,omitempty"`      // declared file resources a proc may read/write (io.file)
	Webhooks   []Webhook                    `json:"webhooks,omitempty"`   // inbound endpoints external systems POST to
	Triggers   []Trigger                    `json:"triggers,omitempty"`   // event reactions: an action's success runs another action
	Theme      map[string]string            `json:"theme,omitempty"`      // CSS custom properties (--fa-<name>)
	ThemeDark  map[string]string            `json:"themeDark,omitempty"`  // dark-mode token overrides (prefers-color-scheme: dark)
	Themes     map[string]map[string]string `json:"themes,omitempty"`     // named alternate palettes (name -> tokens), selected at runtime via the `theme` state
	CSS        string                       `json:"css,omitempty"`        // raw author stylesheet from `css:` blocks, emitted verbatim into the page <head>
	// Assets is every binary/static file bundled via `asset from "..."`
	// (an image, a font, ...), keyed by the content-addressed name it is
	// served under at /assets/<name>. It travels inside the graph exactly
	// like CSS travels as a string field: `facet build --release` has no
	// separate output directory to carry files in, only the one artifact it
	// produces, so the bytes have to ride along in the same JSON the rest of
	// the app does. A text file resolved by the same directive is inlined
	// directly at its use (an ordinary "lit" Expr) and never appears here.
	Assets map[string]Asset `json:"assets,omitempty"`
	Routes []Route          `json:"routes,omitempty"` // every page's path + guard, for client link-hiding and SPA navigation
	Pages  []Page           `json:"pages"`            // one per view; each is a route
	// View/Bindings/DepGraph mirror the *current* page. `facet build` shows the
	// first page; the server swaps them per request to the matched route, so the
	// client runtime can keep reading these three fields unchanged.
	Bindings []Binding           `json:"bindings"`
	View     []Node              `json:"view"`
	DepGraph map[string][]string `json:"depGraph"`
}

// Asset is one binary/static file bundled via `asset from "..."` — an image,
// a font, a PDF, anything not recognized as inlinable text. Bytes round-trips
// through JSON as a base64 string, encoding/json's default for a []byte
// field, so the whole graph (this included) stays the one JSON document every
// target already reads. ContentType is sniffed once at compile time
// (internal/compile) from the extension, falling back to content sniffing,
// so runtime/staticassets.go never has to reopen the file to serve it.
type Asset struct {
	ContentType string `json:"contentType"`
	Bytes       []byte `json:"bytes"`
}

// Page is one routed view: its URL plus its own node tree, tracked bindings, and
// dependency graph (ids are page-local). Path may contain `:param` segments;
// Params lists them in order. Requires names a zero-arg policy guarding the route
// (the authority refuses to render it, the client hides links to it).
type Page struct {
	Name     string              `json:"name"`
	Path     string              `json:"path"`
	Params   []string            `json:"params,omitempty"`
	Requires string              `json:"requires,omitempty"`
	Screen   bool                `json:"screen,omitempty"` // a composed playground screen: a failing guard redirects to the first enterable screen, not a dead end
	View     []Node              `json:"view"`
	Bindings []Binding           `json:"bindings"`
	DepGraph map[string][]string `json:"depGraph"`
	// Page metadata, evaluated server-side at render: <title> and <meta
	// description>/OpenGraph. Interpolated against the route scope (params, entities).
	Title []Seg `json:"title,omitempty"`
	Desc  []Seg `json:"desc,omitempty"`
}

// Route is the routing summary of one page: its path pattern (which may contain
// `:param` segments) and the zero-arg policy guarding it (empty if open). It
// carries no view tree, so the client can know every route — to match links for
// SPA navigation and to hide links to routes the actor may not enter — without
// shipping every page.
type Route struct {
	Path     string `json:"path"`
	Requires string `json:"requires,omitempty"`
}

// Enum is a closed text type: its name and ordered member values.
type Enum struct {
	Name      string   `json:"name"`
	Values    []string `json:"values"`
	WireNames []string `json:"wireNames"`
}

// Record is a value-object type: a flat set of typed fields with no storage or
// identity. It exists so a `let x = call Brain.op()` can bind the structured JSON
// a brain returns and read its fields by name with their declared types. The
// runtime uses Fields to coerce each member of a decoded reply.
type Record struct {
	Name   string        `json:"name"`
	Fields []RecordField `json:"fields"`
}

// RecordField is one typed field of a record: its name, type core (a primitive or
// enum), and whether it is a list and/or optional.
type RecordField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	List     bool   `json:"list,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// Struct is a proc-local named-field composite type declaration (see
// ast.Struct's doc): a name and its typed fields. Reuses RecordField's exact
// shape (name/type/list — a struct field is never optional, see
// ast.StructField's doc) rather than inventing a parallel one, since the two
// are structurally identical; Struct only differs from Record in what a
// field's Type may name (another struct, including itself) and in being
// proc-only rather than a service-reply shape.
type Struct struct {
	Name   string        `json:"name"`
	Fields []RecordField `json:"fields"`
}

// WireType is a `type Name:` declaration (SCHEMA_IDL_SCOPE.md Tier C): a wire
// value object whose fields may reference other WireType/WireMessage
// declarations, including itself — unlike Record, which is deliberately flat.
// Exists only to be handed to codegen (Rust/Go/TS); it has no runtime/entity
// meaning.
type WireType struct {
	Name string `json:"name"`
	// Query marks this type as bound from a URL query string, never a JSON
	// body — see ast.Type.Query's doc comment for what that changes (and
	// deliberately does not change) in each codegen target.
	Query  bool        `json:"query,omitempty"`
	Fields []WireField `json:"fields"`
}

// WireMessage is a `message Name:` tagged union: a closed set of variants,
// each carrying its own fields, discriminated on the wire by the variant's
// own (already snake_case) name.
type WireMessage struct {
	Name     string               `json:"name"`
	Variants []WireMessageVariant `json:"variants"`
}

// WireMessageVariant is one arm of a WireMessage.
type WireMessageVariant struct {
	Name   string      `json:"name"`
	Fields []WireField `json:"fields"`
}

// WireField is one typed field of a WireType or WireMessageVariant. Ref marks
// a field whose Type names another WireType/WireMessage (as opposed to a
// primitive or Enum) — codegen always boxes/points at a Ref field and always
// inlines a non-Ref one, which is what makes self- and mutual reference safe
// to represent in Rust (`Box<T>`) and Go (`*T`) alike; TS needs no such split
// since interfaces reference by name regardless.
type WireField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	List     bool   `json:"list,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	Ref      bool   `json:"ref,omitempty"`
	// Default is the raw literal text of a `= value` clause (`"item"`, `1`,
	// `true`) — never set alongside Optional. Empty string means no default.
	Default string `json:"default,omitempty"`
}

// Component is a reusable view fragment: parameters plus a node tree whose
// interpolations are rendered inline against the call-site argument scope.
type Component struct {
	Name   string  `json:"name"`
	Params []Param `json:"params"`
	View   []Node  `json:"view"`
}

// Entity is a durable, shared, persisted record type — the database. Refs lists
// the entities whose rows reference this one through a relation field (the
// reverse-relation graph), so the runtime can cascade a delete to the children
// the database drops via ON DELETE CASCADE.
type Entity struct {
	Name       string  `json:"name"`
	SoftDelete bool    `json:"softDelete,omitempty"` // remove archives (flags + hides) instead of dropping; the row survives
	Ephemeral  bool    `json:"ephemeral,omitempty"`  // never durable — in-memory working set only (ast.Entity.Ephemeral)
	Fields     []Field `json:"fields"`
	// Read is the compiled `read:` clause (ast.Entity.Read), or nil when the
	// entity declares none. It is rooted at the reserved row variable "$row"
	// (ast.Entity.Read's own doc explains why); a query site folds it into its
	// own predicate by renaming "$row" to whatever variable actually binds the
	// row being tested (runtime/region.go renameRowVar) before evaluating it —
	// this field is never evaluated as-is.
	Read *Expr `json:"read,omitempty"`
	// Derives are the entity's own computed fields (ast.Entity.Derives) — never
	// stored columns, so they never appear in Fields and never reach the
	// database schema/migration. Like Read, each one is compiled once, rooted
	// at the reserved row variable "$row", and evaluated fresh against a row's
	// actual current fields wherever that row is projected to a client
	// (runtime/region.go applyDerives) — the entity-scoped analog of how
	// internal/ir/build.go inlines a top-level Derive wherever it is read.
	Derives []Derive `json:"derives,omitempty"`
}

// Field is one entity column. For a relation field, Ref names the entity it
// points at (the column stores that row's id, a foreign key with ON DELETE
// CASCADE). Index marks a field the compiler saw filtered, ordered, or used as
// a relation — the store builds a real index for it so reads stay sub-linear as
// the table grows past memory.
type Field struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Ref        string `json:"ref,omitempty"`        // relation target entity, else ""
	Enum       string `json:"enum,omitempty"`       // enum type name when Type is text-backed enum
	Index      bool   `json:"index,omitempty"`      // build a database index for this column
	Secret     bool   `json:"secret,omitempty"`     // encrypt this column at rest (AES-GCM)
	E2E        bool   `json:"e2e,omitempty"`        // end-to-end sealed: stored/served as client-sealed ciphertext; the authority never holds plaintext and never renders it
	ReadPolicy string `json:"readPolicy,omitempty"` // @requires(policy): served only to admitted actors, never over SSE
	Optional   bool   `json:"optional,omitempty"`   // column is nullable
	// Declarative constraints — the authority validates them on every add/set.
	Unique   bool   `json:"unique,omitempty"`   // @unique: no two rows share this value
	Required bool   `json:"required,omitempty"` // @required: present and non-empty
	Min      *int   `json:"min,omitempty"`      // @min(n): numeric value ≥ n, or text length ≥ n
	Max      *int   `json:"max,omitempty"`      // @max(n): numeric value ≤ n, or text length ≤ n
	Matches  string `json:"matches,omitempty"`  // @matches("re"): text matches this pattern
	// OnDelete is set only when Ref != "" (a relation field): what happens to this
	// row when the row it points at (Ref) is deleted. "" means the default,
	// cascade — see Reference's doc for the three modes.
	OnDelete string `json:"onDelete,omitempty"` // "" (cascade) | "restrict" | "setNull"
}

// IsRelation reports whether the field is a foreign key to another entity.
func (f Field) IsRelation() bool { return f.Ref != "" }

// Index is one secondary access path the compiler derived: the entity and the
// field it saw ordered, filtered, or used as a relation. It is Field.Index
// addressed as an (entity, field) pair, so a store can reconcile the indexes it
// already has against the ones the app needs without walking every field of
// every entity.
type Index struct {
	Entity string `json:"entity"`
	Field  string `json:"field"`
}

// Indexes is an app's whole derived index set, in entity then field declaration
// order, deduplicated by (entity, field). A row's `id` is never in it: identity
// is already the store's primary order (a Postgres primary key, a FacetQL
// address), which a secondary index cannot improve.
func Indexes(entities []Entity) []Index {
	var out []Index
	seen := map[Index]bool{}
	for _, e := range entities {
		for _, f := range e.Fields {
			ix := Index{Entity: e.Name, Field: f.Name}
			if !f.Index || seen[ix] {
				continue
			}
			seen[ix] = true
			out = append(out, ix)
		}
	}
	return out
}

// Reference is one relation addressed as a rule rather than as a column: rows of
// Entity point at a row of Parent through Field, which holds that row's id.
//
// It is Field.Ref addressed as a triple, for the same reason Index is Field.Index
// addressed as a pair — so a store can reconcile the referential rules it already
// enforces against the ones the app needs, and the runtime can walk the reverse
// graph (parent -> the children that point at it), without every caller
// re-deriving the graph by walking every field of every entity.
//
// The deletion rule defaults to cascade — a foreign key with ON DELETE CASCADE
// (Field's doc) — the sole behavior before a relation field could carry an
// on-delete modifier, and still what every field that declares none gets. A
// field's `@restrict` (OnDelete == "restrict") refuses the parent delete while
// a referencing row remains; `@setNull` (OnDelete == "setNull") clears the
// reference on the child row instead of deleting it. Every backend implements
// all three — pgStore as an FK constraint, fqStore as a declared FacetQL
// reference, memStore in its own map.
type Reference struct {
	Entity   string `json:"entity"`             // the child: the entity whose row holds the reference
	Field    string `json:"field"`              // the relation field on it, holding the parent row's id
	Parent   string `json:"parent"`             // the entity being referenced
	OnDelete string `json:"onDelete,omitempty"` // "" (cascade) | "restrict" | "setNull" — see the type doc
}

// References is an app's whole relation graph, in entity then field declaration
// order. It is the single derivation of that graph: the reverse edges the
// runtime cascades along and the rules a store declares to its engine are the
// same set read in two directions, and deriving them twice is how the two drift.
func References(entities []Entity) []Reference {
	var out []Reference
	for _, e := range entities {
		for _, f := range e.Fields {
			if f.IsRelation() {
				out = append(out, Reference{Entity: e.Name, Field: f.Name, Parent: f.Ref, OnDelete: f.OnDelete})
			}
		}
	}
	return out
}

// State is one state cell with its resolved placement. List/Elem describe a
// `[T]` collection cell; Optional marks a nullable cell.
type State struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Elem      string `json:"elem,omitempty"`
	List      bool   `json:"list,omitempty"`
	Optional  bool   `json:"optional,omitempty"`
	Placement string `json:"placement"`
	Private   bool   `json:"private,omitempty"` // @private: authoritative but server-only — never shipped to a client, never rendered
	Init      *Expr  `json:"init"`
}

// Derive is a named computed value. Its Expr is fully inlined (no derive or
// policy refs remain), so a runtime never has to resolve it — it is reported
// here for introspection (`facet build`) and tooling, while every use site
// already carries the inlined expression. Deps are the base state/entity names
// it transitively reads, for dependency tracking.
type Derive struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Expr *Expr    `json:"expr"`
	Deps []string `json:"deps"`
}

// Policy is a named predicate; enforced on the server, also shipped so the UI
// can hide what the actor may not do. Params are non-empty for a row-level
// policy: the gate binds them from the `requires` call site's arguments before
// evaluating Expr (a zero-parameter policy is a plain permission).
type Policy struct {
	Name   string  `json:"name"`
	Params []Param `json:"params,omitempty"`
	Expr   *Expr   `json:"expr"`
}

// Action is a mutation: parameters, the policy checks it requires, the
// statements it runs, its resolved placement, and the state it reads/writes.
type Action struct {
	Name       string    `json:"name"`
	Params     []Param   `json:"params"`
	Requires   []Require `json:"requires"`
	Optimistic bool      `json:"optimistic,omitempty"` // client predicts the result pre-round-trip
	Placement  string    `json:"placement"`
	Reason     string    `json:"reason,omitempty"` // why the compiler placed it here (for `facet explain`)
	Writes     []string  `json:"writes"`
	Reads      []string  `json:"reads"`
	Seal       []string  `json:"seal,omitempty"` // @e2e: param names the client must seal (encrypt) before POSTing, so the authority only ever receives ciphertext
	Body       []Stmt    `json:"body"`           // statements in source order, including `check` validations
}

// Proc is a general-purpose, unconditionally server-executed declaration:
// ordinary imperative code — including real control flow (`loop`/`if`, each
// with a nested, recursive statement block; `break`/`continue`; `return` from
// anywhere) as of Milestone 2 — over its own parameters and `let`-bound locals.
// Unlike Action it carries no Placement, Reads/Writes, Requires, or Seal — a
// proc needs none of the placement calculus (it always runs on the authority)
// and does not touch state/entities in this milestone. Called from an action
// (or another proc) via `do ProcName(args)`.
type Proc struct {
	Name    string  `json:"name"`
	Params  []Param `json:"params"`
	Ret     string  `json:"ret,omitempty"`     // return type core ("" = no return)
	RetList bool    `json:"retList,omitempty"` // the return is a list of Ret
	Body    []Stmt  `json:"body"`
}

// Require is one resolved permission check on an action: the policy name plus the
// argument expressions passed to a row-level (parameterized) policy.
type Require struct {
	Name string  `json:"name"`
	Args []*Expr `json:"args,omitempty"`
}

// Param is a typed action/policy/component parameter. List marks a proc's own
// list-typed parameter (`p: [T]` — see internal/parser/parser.go's
// parseSignature); action/policy/component parameters never set it, since only
// a proc parameter may be list-typed.
type Param struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
	List     bool   `json:"list,omitempty"`
}

// Job is a scheduled server action: the runtime invokes Action on a timer
// (Every seconds) and/or once at startup (OnStart), with no user in the loop.
type Job struct {
	Name    string `json:"name"`
	Action  string `json:"action"`
	Every   int    `json:"every,omitempty"`
	OnStart bool   `json:"onStart,omitempty"`
}

// Service is an external service the runtime may call: a base URL and its typed
// operations. A `call` statement posts to URL + "/" + op with the named arguments.
type Service struct {
	Name string      `json:"name"`
	URL  string      `json:"url"`
	Ops  []ServiceOp `json:"ops"`
}

// ServiceOp is one operation: its name, parameter names (the JSON keys a call
// sends), and an optional typed return (decoded back when a `let` binds it).
type ServiceOp struct {
	Name    string   `json:"name"`
	Params  []string `json:"params,omitempty"`
	Ret     string   `json:"ret,omitempty"`     // return type core ("" = no return)
	RetList bool     `json:"retList,omitempty"` // the return is a list of Ret
}

// File is a declared file resource a proc's `read`/`write` statements
// (Stmt.Op "fileread"/"filewrite") operate on — a name, its content type
// ("text" or "bytes"), and the sandboxed path readFile/writeFile already
// resolve through (runtime/io.go). Purely descriptive at this layer: each
// `read`/`write` statement is already lowered with its own resolved Path/
// Bytes fields (see Stmt), so nothing at runtime looks File back up by name;
// it exists on the IR for the same reason Service does — an inspectable
// record of the app's declared external resources (see cmd/facet/inspect.go).
type File struct {
	Name string `json:"name"`
	Type string `json:"type"` // "text" | "bytes"
	Path string `json:"path"`
}

// Webhook is a resolved inbound endpoint: an external system POSTs to Path, the
// runtime verifies an HMAC over the raw body, and runs Action with the JSON body
// decoded into its parameters by name. Secret names the env var holding the HMAC
// key (empty → derived from the master secret). The inbound twin of a Service.
type Webhook struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	Secret string `json:"secret,omitempty"`
}

// Trigger is a resolved event reaction: when the action named On completes
// successfully, the runtime runs Action (a zero-arg server action, system
// authority). The non-cron sibling of a Job; the compiler proves the trigger graph
// is acyclic so reactions always terminate.
type Trigger struct {
	On     string `json:"on"`
	Action string `json:"action"`
}

// Stmt is one action or proc statement.
// Op ∈ assign | add | set | remove | clear | call | check | establish | let |
// return | do | loop | if | break | continue | indexset | spawn | join.
//
// let/return/loop/if/break/continue/indexset are proc-only (a proc's own
// local-variable model, control flow, and result); do calls a proc — from
// either an action or another proc — over the same fields a service `call`
// uses (Service holds the proc's name; there is no operation to route
// through, unlike a service).
// assign is shared: in an action it writes a state cell (into session state +
// wire deltas); in a proc it reassigns a `let mut` local in the proc's own
// frame. The two never alias, because runActionLocked and runProcLocked/
// execProcBlock interpret the two bodies separately.
//
// indexset is a proc-only array element mutation, `xs[i] = expr`: Target names
// the array local (gated the same way a plain reassignment is — see Assign,
// above — since it mutates the value Target is bound to), Key is the index
// expression (reusing the same field a `set`/`remove` entity key already
// carries), and Value is the new element. It is bounds-checked only at
// runtime (runtime/server.go's execProcBlock) — see ast.Index's doc for why.
//
// loop/if are the one place a Stmt nests (Milestone 2): Value holds the
// condition both share, Body is loop's repeated body / if's `then` branch, and
// Else is if's optional `else` branch (nil = none — loop never sets it). A
// runtime interpreter (runtime/server.go execProcBlock) walks Body/Else
// recursively; break/continue carry no payload beyond Op itself.
//
// spawn/join (Milestone 5: structured concurrency) are proc-only, like
// let/return/loop/if/break/continue/indexset above. spawn runs Service (the
// proc name) with Args as a real goroutine and stores its handle into Target
// (the same field "let" uses for a local's name — a handle IS a local, just
// one internal/ir/build.go's checkNoTaskUse refuses every other use of).
// join blocks on the handle named by Target, and — like do/call — binds its
// result into Bind (coerced per Ret/RetList) when Bind is non-empty.
// internal/ir/build.go's procBlock (checkSpawnsJoined) proves every spawn is
// joined before its own enclosing statement block ends; runtime/server.go's
// execProcBlock is the interpreter for both.
type Stmt struct {
	Op      string      `json:"op"`
	Target  string      `json:"target,omitempty"`  // assign (action: a state cell; proc: a `let mut` local); let: the local's name
	Entity  string      `json:"entity,omitempty"`  // add/set/remove/clear
	Field   string      `json:"field,omitempty"`   // set; for a call, the operation name
	Key     *Expr       `json:"key,omitempty"`     // set/remove
	Value   *Expr       `json:"value,omitempty"`   // assign/set/let/return/loop (cond)/if (cond)
	Fields  []FieldInit `json:"fields,omitempty"`  // add
	Service string      `json:"service,omitempty"` // call: the service name; do: the proc name
	Args    []*Expr     `json:"args,omitempty"`    // call/do: the arguments
	Var     string      `json:"var,omitempty"`     // remove (filtered): item variable
	Where   *Expr       `json:"where,omitempty"`   // remove (filtered): predicate (nil = by-id)
	Bind    string      `json:"bind,omitempty"`    // call/do (request→response): local the result binds to
	Ret     string      `json:"ret,omitempty"`     // call/do (request→response): result type core, for decode/coerce
	RetList bool        `json:"retList,omitempty"` // call/do (request→response): result is a list of Ret
	Role    *Expr       `json:"role,omitempty"`    // establish: optional new session role (Value holds the new actor)
	Msg     string      `json:"msg,omitempty"`     // check: the message returned when the condition (Value) is false
	Body    []Stmt      `json:"body,omitempty"`    // loop: the repeated body; if: the `then` branch
	Else    []Stmt      `json:"else,omitempty"`    // if: the `else` branch (nil = none)
	Bytes   bool        `json:"bytes,omitempty"`   // indexset: Target is a byte-buffer local (internal/ir/build.go's bytesType) — range-check Value to 0-255 rather than accepting any int; fileread/filewrite: the file resource is `bytes`-typed rather than `text`
	File    string      `json:"file,omitempty"`    // fileread/filewrite: the declared `file` resource's name (for a clear runtime error)
	Path    string      `json:"path,omitempty"`    // fileread/filewrite: the file's author-facing path, resolved from its `file` declaration at compile time — still passed through the sandbox (resolveDataPath) at runtime
}

// FieldInit is a `name: expr` in an `add`.
type FieldInit struct {
	Name string `json:"name"`
	Expr *Expr  `json:"expr"`
}

// Binding is one top-level interpolation addressed by a stable id; Deps are the
// state/collection names it reads.
type Binding struct {
	ID   string   `json:"id"`
	Expr *Expr    `json:"expr"`
	Deps []string `json:"deps"`
}

// Node is one view node in the neutral tree.
type Node struct {
	Kind     string `json:"kind"` // box|row|text|heading|image|icon|video|richtext|badge|button|list|if|match|case|else|input|textarea|checkbox|radio|link|select|form|upload|use|slot|tabs|tab|overlay|typeahead|option|options|stage|sprite
	Children []Node `json:"children,omitempty"`

	// Segs is the node's own interpolated value: a text/badge leaf's words, an
	// image or video URL, richtext's markdown source, an icon's glyph name.
	Segs []Seg `json:"segs,omitempty"`

	// Label is an interpolated value a node shows *beside* its own content: a
	// form's submit button, an upload control, a tab, a link.
	//
	// Every one of these is segments rather than a string, for the reason node
	// text is: a component that cannot parameterize the words it renders is not
	// reusable. They were plain strings, so `placeholder "{hint}"` rendered the
	// literal `{hint}` and a form's `"Send {hint}"` silently lost the
	// interpolation entirely. One representation, one flattening, one escaper —
	// see Server.attrText / facet.js segsToStr.
	Label  []Seg   `json:"label,omitempty"`  // upload/form submit/tab/link/checkbox
	Action string  `json:"action,omitempty"` // button/form
	Args   []*Expr `json:"args,omitempty"`   // button/form/use

	ID    string `json:"id,omitempty"`    // list/if: dynamic region id
	Var   string `json:"var,omitempty"`   // list: item variable
	Coll  string `json:"coll,omitempty"`  // list: collection name
	Where *Expr  `json:"where,omitempty"` // list: row filter (nil = all)
	Order string `json:"order,omitempty"` // list: sort field
	Desc  bool   `json:"desc,omitempty"`  // list: descending
	Limit *Expr  `json:"limit,omitempty"` // list: max rows (nil = unlimited); int literal or a dynamic page-size expr
	// More is the zero-argument action a list fires to load its next page — the
	// infinite-scroll clause (`for … limit shown more loadMore:`). While the
	// render cut rows off at `limit`, a "More" control follows the last row; the
	// client fires the action as that control scrolls into view, and a click on
	// it does the same. Whether rows WERE cut off is a fact only the render
	// knows (the store answers `limit` rows and no more), so the renderer asks
	// for one row past the limit and records the answer per region path —
	// runtime/region.go listRowsMore, carried to the client as `@more`/`more`.
	More string `json:"more,omitempty"`
	Cond *Expr  `json:"cond,omitempty"` // if: condition

	// Level is a heading's document level, 1..6 — an expression, because the
	// depth a header renders at belongs to the page that uses it and not to the
	// component that draws it (see ast.Heading). It is evaluated per render and
	// turned into the element name by headingLevel, which clamps; the compiler
	// refuses the levels it can prove wrong before it ever gets there.
	//
	// It is deliberately absent from every dependency walk (nodeDeps,
	// region.go's aggIndex / walkNodeExprs / rowAggregates, and their mirrors in
	// assets/facet.js), and the lowering is what makes that safe: a level whose
	// deps are non-empty is a compile error, so this expression provably contains
	// no aggregate, no entity lookup and no state read. Relax that check and
	// every one of those walks has to learn about this field on the same day.
	Level *Expr `json:"level,omitempty"`

	Bind        string `json:"bind,omitempty"`        // input/textarea/checkbox/radio/select/upload: the state cell this control reads and writes
	Placeholder []Seg  `json:"placeholder,omitempty"` // input/textarea/typeahead

	// Alt is the words that stand in for a picture or a player when it cannot be
	// seen: an `<img alt="…">` and, for a video, the `aria-label` a <video> reads
	// instead (HTML gives <video> no `alt`). One authored value, two attributes,
	// for the reason `toggle` and `checkbox` are one node kind — they differ in
	// what a renderer writes and in nothing else, so neither side holds a mapping
	// the other could disagree with.
	//
	// It is segments, like every other author-visible value, because a picture in
	// a `for` is a picture PER ROW and a description that cannot mention the row
	// describes the wrong thing. It is therefore in SegLists, which is what makes
	// it visible to the dependency walks; an interpolated attribute that is not
	// there is one that never refreshes (see the note on SegLists).
	Alt []Seg `json:"alt,omitempty"`

	// Poster is a video's still before playback — interpolated, and in SegLists,
	// for exactly the reasons Alt is. Autoplay/Loop/Muted are its playback
	// flags; autoplay implies muted in both renderers (see ast.Video).
	Poster   []Seg `json:"poster,omitempty"`
	Autoplay bool  `json:"autoplay,omitempty"`
	Loop     bool  `json:"loop,omitempty"`
	Muted    bool  `json:"muted,omitempty"`

	Path string `json:"path,omitempty"` // link: destination route, when it is a literal

	// PathSegs carries a link whose destination interpolates the surrounding
	// scope. `Path` stays for a purely literal destination so that the common
	// case reads as one string on the wire and in `facet ir`, and so a consumer
	// that only understands literals still resolves every static link correctly.
	//
	// A link's *label* has no such second field: it is `Label`, like every other
	// node's, because a label is only ever displayed — there is nothing for a
	// literals-only consumer to resolve, and one field is one rule.
	PathSegs []Seg `json:"pathSegs,omitempty"`

	// Route marks a link whose destination IS an interpolated value rather than a
	// path the author wrote around interpolated values — `link "{label}" -> "{to}"`,
	// the shape a reusable nav/breadcrumb/pagination component needs.
	//
	// The two are rendered differently and checked differently, and both follow
	// from the same fact: in a path template the interpolations are *data* landing
	// in slots of a known path, and here the interpolation is the *path*. So a
	// template's values are percent-escaped (a handle containing `/` must not
	// become two segments) and its shape is route-checked at compile time; a route
	// expression is written through unescaped (escaping its `/`s would destroy it)
	// and cannot be route-checked at compile time, because its value does not
	// exist yet. The renderers resolve it against Routes instead and refuse to
	// make an anchor of anything that is not a route of this app.
	Route bool `json:"route,omitempty"`

	// External marks a link whose destination leaves this app: an author-written
	// absolute URL (`https://…`, `http://…`, `mailto:…`). It is set only from a
	// destination whose scheme and authority the AUTHOR wrote as literal text —
	// never from a value that arrives at render — which is the whole safety
	// argument. A `Route` destination is a runtime value and stays confined to
	// this app's routes; anything else it names renders as inert text, so a
	// `javascript:` payload sitting in a database row can no more become an
	// anchor now than it could before external links existed.
	//
	// Interpolation is still allowed *after* the authority (`https://api.x/{id}`)
	// and is percent-escaped exactly as a path template's is, so a value can fill
	// a segment but can never move the origin.
	//
	// The renderers re-check the scheme at render time anyway (safeExternalHref,
	// mirrored in assets/facet.js). That check is redundant against the compiler
	// and is meant to be: it is what makes "an anchor to somewhere off-site" a
	// property of the rendered href rather than of a boolean nobody re-reads.
	External bool `json:"external,omitempty"`

	Name string `json:"name,omitempty"` // use: component name
	// Options is a select's or a radio group's choices when every one of them is
	// fixed — the whole world before a choice list could be drawn from data, and
	// the shape it keeps. A control whose list contains anything dynamic (a
	// computed value, or a `for` over a collection) carries its whole list as
	// `option`/`options` Children instead, in source order.
	//
	// The two are exclusive, which is what makes rendering them one rule with no
	// second ordering question: render Options, then render Children, and only one
	// of the two is ever non-empty. It is the same split `Path`/`PathSegs` and
	// `Class`/`ClassSegs` make, for the same reason — the purely literal case
	// stays one flat list on the wire, and a consumer that only understands fixed
	// choices still reads every fixed-choice control exactly as it always did.
	Options []Option `json:"options,omitempty"`
	// Value is an identity the runtime resolves, never a displayed string, so it
	// is not interpolated: a tab's is the value its bound cell takes when active
	// and the key that picks the rendered branch; a typeahead's is the entity
	// field its suggestions are drawn from, a name the compiler checked; a
	// checkbox's is its presentation variant ("switch" — the control the author
	// spelled `toggle`), which is why `toggle` needs no node kind of its own; an
	// `option` node's is the literal it stores.
	Value string `json:"value,omitempty"`

	// Val is a Value the render computes rather than one the author wrote — an
	// `option`/`options` node whose stored identity is `c.id` rather than "draft".
	// It stands beside Value exactly as PathSegs stands beside Path: the literal
	// field survives untouched so everything that could already resolve a fixed
	// identity still can, and the computed one exists only where there was no
	// identity to write down.
	//
	// This is the one thing a data-driven choice necessarily gives up. A literal
	// option's value is checked against the enum it belongs to at compile time;
	// this one cannot be, because the rows it comes from do not exist yet. What
	// the compiler still proves about it is written at lowerOptions.
	Val *Expr `json:"val,omitempty"`

	// CSS escape hatch: author-supplied class/style from a trailing `class "..."` /
	// `style "..."` modifier, appended to the element's built-in `fa-*` class so a
	// `css:` stylesheet can target it. Applied identically on the server and client.
	Class string `json:"class,omitempty"`
	Style string `json:"style,omitempty"`

	// ClassSegs carries a class value that interpolates the surrounding scope
	// (`class "x-rung-c-{tone}"`). `Class` stays for a purely literal one, exactly
	// as `Path` stays beside `PathSegs`: the common case reads as one string on the
	// wire and in `facet ir`, and a consumer that only understands literals still
	// resolves every static class correctly.
	//
	// An interpolated run is filtered to the characters a class token may contain
	// before it is joined — see escapeClassToken, mirrored in assets/facet.js. A
	// class attribute is a whitespace-separated token LIST, so an unfiltered value
	// could add classes it was never given a slot for; filtering makes the value
	// fill its token and nothing more, the same way linkHref's escaping makes a
	// value fill its path segment and nothing more.
	//
	// There is deliberately no `StyleSegs`. See ast.Modified.
	ClassSegs []Seg `json:"classSegs,omitempty"`

	// Anchor is an author-chosen name for this node as a position in the page —
	// what `anchor "install"` writes and what `link "…" -> "#install"` scrolls to.
	// It is rendered as the element's `id`.
	//
	// It is a separate field from ID, and must stay one. ID is the runtime's own
	// region address: the compiler allocates it, the client re-renders a region by
	// it, and nothing about it is the author's to choose or to read. Anchor is the
	// author's name for a place in the document. They answer different questions
	// and a node can want both — a `for` region the reader can also link to — so
	// merging them would mean either the addressing or the anchor losing its name.
	Anchor string `json:"anchor,omitempty"`

	// Width/Height are a stage's canvas pixel size (kind "stage"). Tiles is its
	// optional background tile-grid source; nil means no tile layer. X/Y/Facing/
	// Image are a sprite's (kind "sprite") per-row draw parameters — Image is
	// mandatory, Facing is not. A sprite reuses Var/Coll/Where/Order/Desc/Limit
	// above (the same Range fields a "list" node carries) for the rows it draws
	// one mark per, and reuses Label for its optional on-canvas caption — it is a
	// `for` that draws a mark instead of rendering a node body, so it shares the
	// query half completely and adds only what drawing a mark needs.
	Width  int   `json:"width,omitempty"`
	Height int   `json:"height,omitempty"`
	Tiles  *Expr `json:"tiles,omitempty"`
	X      *Expr `json:"x,omitempty"`
	Y      *Expr `json:"y,omitempty"`
	Facing *Expr `json:"facing,omitempty"`
	Image  *Expr `json:"image,omitempty"`
}

// SegLists is every interpolated value hanging off this node: its own segments
// and each attribute's. It exists because a node no longer has one list — the
// walks that ask "what does this node read?" (dependency edges, which aggregates
// to materialize, which collections the client still needs) would each have to
// remember every attribute, and the three that only remembered `Segs` are why a
// link's interpolated label and path were invisible to all of them. One method,
// so adding an interpolated attribute reaches every walk at once.
func (n Node) SegLists() [][]Seg {
	out := [][]Seg{n.Segs, n.Label, n.Placeholder, n.PathSegs, n.ClassSegs, n.Alt, n.Poster}
	for _, o := range n.Options {
		out = append(out, o.Label)
	}
	return out
}

// Option is one fixed select/radio choice (display label → stored value). The
// label is interpolated segments like every other label; the value is not — it
// is a compile-time identity: what the bound cell stores, what the current
// selection is compared against, and what an enum's exhaustiveness is proven
// over. A choice whose value is not known until the render is not one of these;
// it is an `option`/`options` Node (see Node.Options).
type Option struct {
	Label []Seg  `json:"label"`
	Value string `json:"value"`
}

// Seg is a text segment: a literal (Lit), a tracked top-level binding (Bind id),
// or an item-scope expression evaluated inline at render time (Expr).
type Seg struct {
	Lit  string `json:"lit,omitempty"`
	Bind string `json:"bind,omitempty"`
	Expr *Expr  `json:"expr,omitempty"`
	E2E  bool   `json:"e2e,omitempty"` // this interpolation reads an @e2e field: the value is ciphertext, rendered as a placeholder and opened on the client
}

// Expr is the serialized expression form interpreted identically by Go and
// JS — its JSON tags are load-bearing for that wire (server.go's per-page
// `reqIR` ships whole Expr trees to runtime/assets/facet.js, which walks the
// same "kind"/"field"/"obj"/"l"/"r"/... shape client-side), which is a
// completely different, single-implementation protocol from FacetQL's HTTP
// wire.
//
// It is NOT the FacetQL wire type, even though its tags happen to look like
// the generated wire.Expr's (schema/facetql_wire.fct) — that resemblance
// used to be exploited directly (Expr was marshaled straight into FacetQL
// requests, relying on the two shapes matching by convention, a duplicate
// definition and a real drift surface). Anything crossing the FacetQL wire
// now goes through wireExprFromIR/wireExprToIR (runtime/wireseam.go)
// instead; do not marshal an *Expr directly into a FacetQL request again.
type Expr struct {
	Kind   string   `json:"kind"` // lit | ref | get | eget | agg | call | bin | un | list | index | map | struct
	Val    any      `json:"val,omitempty"`
	VType  string   `json:"vtype,omitempty"`
	Name   string   `json:"name,omitempty"`   // ref / eget entity / agg collection / call builtin / struct (its type name)
	Field  string   `json:"field,omitempty"`  // get / eget / agg (sum)
	Obj    *Expr    `json:"obj,omitempty"`    // get / index (the array or map)
	Key    *Expr    `json:"key,omitempty"`    // eget / index (the index/key expression)
	Op     string   `json:"op,omitempty"`     // bin / un / agg (count|sum|exists)
	Args   []*Expr  `json:"args,omitempty"`   // call / list (the elements) / map (the values, parallel to Keys) / struct (the field values, parallel to Fields)
	Keys   []*Expr  `json:"keys,omitempty"`   // map literal only: its keys, parallel to Args (its values)
	Fields []string `json:"fields,omitempty"` // struct literal only: its field names, parallel to Args (its values)
	L      *Expr    `json:"l,omitempty"`
	R      *Expr    `json:"r,omitempty"`
	X      *Expr    `json:"x,omitempty"`
	Var    string   `json:"var,omitempty"`   // agg: item variable for the filtered form
	Where  *Expr    `json:"where,omitempty"` // agg: filter predicate (nil = whole collection)
	Sel    *Expr    `json:"sel,omitempty"`   // agg: the value reduced over each row (nil = the bare Field)
}

// Kids is every sub-expression hanging off this one, in the order both renderers
// walk them.
//
// It exists for the reason Node.SegLists does, and the same thing went wrong
// first: the walk `e.L, e.R, e.X, e.Obj, e.Key, e.Where` was written out by hand
// in six places (four in runtime/region.go alone) plus assets/facet.js, so a new
// child field reached whichever of them somebody remembered. That order is also
// load-bearing — the server and the client address a page's aggregates by their
// position in this very walk, so a field one side descends into and the other
// skips renumbers every aggregate after it and each one starts resolving to
// another's value. One method, so a child added here is a child everywhere.
//
// Entries may be nil; every walker over it is nil-tolerant, which keeps callers
// from having to filter what they are about to nil-check anyway.
func (e *Expr) Kids() []*Expr {
	if e == nil {
		return nil
	}
	kids := []*Expr{e.L, e.R, e.X, e.Obj, e.Key, e.Where, e.Sel}
	kids = append(kids, e.Keys...)
	return append(kids, e.Args...)
}
