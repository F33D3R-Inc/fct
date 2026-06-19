// Package ast is the Facet syntax tree. An application is one declarative graph:
// entities (durable data), state (cells), actions (the only mutators), policies
// (permissions), and views (projections of state). There is no "frontend" or
// "backend" node — placement is computed later (internal/ir), never authored.
package ast

// App is one `app Name:` definition — the whole application graph.
type App struct {
	Name     string
	Auth     bool // a bare `auth` line turns on built-in users/login/logout/signup
	Entities []*Entity
	States   []*State
	Derives  []*Derive
	Policies []*Policy
	Actions  []*Action
	Jobs     []*Job
	Views    []*View
	Line     int
}

// Placement annotations the author may put on state. Empty means "infer", which
// resolves to the authoritative default (server). The compiler decides where
// everything runs; these are only hints/overrides.
const (
	PlaceInfer  = ""
	PlaceClient = "client" // @client — ephemeral, local, latency-sensitive
	PlaceServer = "server" // @server — explicit authoritative
)

// Entity is a durable, shared, persisted record type — the database. Its rows
// are always authoritative (server) and survive restarts.
type Entity struct {
	Name   string
	Fields []EntityField
	Line   int
}

// EntityField is one column of an entity. A `@secret` field is encrypted at rest
// (the database stores ciphertext; the working set holds plaintext).
type EntityField struct {
	Name   string
	Type   string // int | text | bool | <EntityName> (a relation, stored as the row id)
	Secret bool   // @secret — encrypted at rest (AES-GCM under FACET_SECRET)
	Line   int
}

// State is one `state name: Type = default [@client|@server]` cell. Scalar
// server state is per-session; client state is per-browser-instance.
type State struct {
	Name      string
	Type      string
	Default   Expr
	Placement string
	Line      int
}

// Derive is a named, read-only computed value: `derive name: Type = expr` over
// states, entities, aggregates, and earlier derives. Placement is never authored
// — a derivation is pure, so the compiler inlines it wherever it is read and the
// value is recomputed in whatever domain renders it (the client mirrors every
// server cell it can see, so derivations cost zero round-trips). It is a
// compile-time abstraction: DRY at the source, free at runtime.
type Derive struct {
	Name string
	Type string
	Expr Expr
	Line int
}

// Policy is a named predicate over the actor, its own parameters, and state. A
// zero-parameter policy is a plain permission (`requires admin`) and can also
// hide UI by being used as a view condition. A parameterized policy is
// row-level authorization — `policy owns(id): actor == Post(id).author` — gated
// by passing arguments at the call site (`requires owns(id)`). It is always
// enforced on the server.
type Policy struct {
	Name   string
	Params []Param
	Expr   Expr
	Line   int
}

// Action is the only thing that may mutate state. Placement is derived from its
// write set; `Requires` lists the policy checks that must pass before it runs.
type Action struct {
	Name     string
	Params   []Param
	Requires []Require
	Body     []Stmt
	Line     int
}

// Require is one `requires` clause: a policy name and the arguments passed to it
// (empty for a zero-parameter policy). The arguments are expressions over the
// action's parameters and the actor, evaluated when the gate runs.
type Require struct {
	Name string
	Args []Expr
	Line int
}

// Param is one typed action parameter.
type Param struct {
	Name string
	Type string
}

// Job is a scheduled, server-authoritative invocation of a zero-argument action
// — the language's background/workflow primitive. It runs on a fixed interval
// (`every Ns`) and/or once at startup (`on start`), driven by the runtime rather
// than a user gesture, under the synthetic `system` actor.
type Job struct {
	Name    string
	Action  string
	Every   int  // seconds between runs; 0 = no interval
	OnStart bool // also run once when the server starts
	Line    int
}

// ── action statements ────────────────────────────────────────────────────────

// Stmt is one line in an action body.
type Stmt interface{ stmt() }

// Assign sets a state cell: `target = expr`.
type Assign struct {
	Target string
	Value  Expr
	Line   int
}

// Add inserts a row into an entity: `add Entity { f: expr, ... }`. The server
// assigns the row's id.
type Add struct {
	Entity string
	Fields []FieldInit
	Line   int
}

// FieldInit is one `name: expr` in an `add`.
type FieldInit struct {
	Name string
	Expr Expr
}

// Set updates one field of an entity row by id: `set Entity(key).field = expr`.
type Set struct {
	Entity string
	Key    Expr
	Field  string
	Value  Expr
	Line   int
}

// Remove deletes an entity row by id: `remove Entity(key)`.
type Remove struct {
	Entity string
	Key    Expr
	Line   int
}

// Clear empties an entity: `clear Entity`.
type Clear struct {
	Entity string
	Line   int
}

func (Assign) stmt() {}
func (Add) stmt()    {}
func (Set) stmt()    {}
func (Remove) stmt() {}
func (Clear) stmt()  {}

// View is one `view Name [at "/path"]:` projection of state into a UI node tree.
// A view is a page, served at its route; Path defaults are filled in by the
// compiler (the first view answers "/").
type View struct {
	Name string
	Path string // "" until resolved; URL the page is served at
	Root []Node
	Line int
}

// ── view nodes ──────────────────────────────────────────────────────────────

// Node is a UI node. The set is small and target-neutral.
type Node interface{ node() }

// Box is a layout container.
type Box struct{ Children []Node }

// Text is a text leaf of literal and interpolated segments.
type Text struct{ Segs []Seg }

// Seg is one piece of a Text: literal (Expr == nil) or interpolation.
type Seg struct {
	Lit  string
	Expr Expr
}

// Button emits an action (with evaluated argument expressions) when pressed.
type Button struct {
	Label  string
	Action string
	Args   []Expr
}

// For iterates an entity/list, rendering Body once per row with Var bound. It is
// the query/feed primitive: an optional `where <cond>` keeps only matching rows,
// `by <field> [desc|asc]` orders them, and `limit <n>` caps them — applied in
// that order (filter, then sort, then cap).
type For struct {
	Var   string
	Coll  string
	Body  []Node
	Where Expr   // optional row filter; nil = all rows
	Order string // sort field; "" = insertion order
	Desc  bool   // true = descending (newest/highest first)
	Limit int    // optional max rows; 0 = unlimited
}

// If renders Body only when Cond is truthy.
type If struct {
	Cond Expr
	Body []Node
}

// Input is a text control two-way bound to a client state cell.
type Input struct {
	Bind        string
	Placeholder string
}

// Link is navigation to another page: `link "label" -> "/path"`. It renders an
// anchor; following it loads that page (server-rendered).
type Link struct {
	Label string
	Path  string
}

func (Box) node()    {}
func (Text) node()   {}
func (Button) node() {}
func (For) node()    {}
func (If) node()     {}
func (Input) node()  {}
func (Link) node()   {}

// ── expressions ─────────────────────────────────────────────────────────────

// Expr is a pure expression, evaluated identically on every executor from its
// serialized IR form.
type Expr interface{ expr() }

// Lit is an int, text, or bool literal.
type Lit struct {
	Kind string
	Val  any
}

// Ref is a reference to a state cell, param, item var, or the builtin `actor`.
type Ref struct{ Name string }

// Get is member access: `obj.field` (e.g. a list item's column).
type Get struct {
	Obj   Expr
	Field string
}

// EntityGet looks up one field of an entity row by id: `Entity(key).field`.
type EntityGet struct {
	Entity string
	Key    Expr
	Field  string
}

// Agg is an aggregate over a whole entity collection: `count(Entity)` (row
// count) or `sum(Entity.field)` (numeric total of a field). It reads the
// collection, so it tracks a dependency on that entity.
type Agg struct {
	Op    string // count | sum
	Coll  string
	Field string // sum only
}

// Call is an effectful builtin invocation — `now()` (server clock, unix seconds)
// or `rand(n)` (server RNG, 0..n). These are the language's "service" surface:
// impure, so the placement calculus pins any action that uses one to the server
// (the authority owns nondeterminism, so every client agrees). They are rejected
// in pure contexts (derives, policies, views).
type Call struct {
	Name string
	Args []Expr
}

// Bin is a binary operation.
type Bin struct {
	Op   string
	L, R Expr
}

// Un is a unary operation (! or -).
type Un struct {
	Op string
	X  Expr
}

func (Lit) expr()       {}
func (Ref) expr()       {}
func (Get) expr()       {}
func (EntityGet) expr() {}
func (Agg) expr()       {}
func (Call) expr()      {}
func (Bin) expr()       {}
func (Un) expr()        {}
