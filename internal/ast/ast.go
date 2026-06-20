// Package ast is the Facet syntax tree. An application is one declarative graph:
// entities (durable data), state (cells), actions (the only mutators), policies
// (permissions), and views (projections of state). There is no "frontend" or
// "backend" node — placement is computed later (internal/ir), never authored.
package ast

// App is one facet definition — the whole graph contributed by a single `.fct`
// file. A plain `app Name:` is a self-contained facet (its own data + UI). The
// typed kinds compose like Lego bricks: a `playground` is the baseplate, a
// `wireframe` carves the surface into typed sockets, and `ui`/`data` facets snap
// into those sockets. The compiler (internal/compile) flattens the stack into one
// graph before placement, so layering is how an app is *built*, never how it
// *renders* — the result is a single surface.
type App struct {
	Name string
	// Kind is the facet kind: "" / "app" (self-contained), "playground" (the
	// baseplate that mounts a wireframe), "wireframe" (typed sockets + a frame),
	// "ui" or "data" (content that snaps into a socket). Only the typed kinds take
	// part in layered composition; a plain app keeps the original flat-merge model.
	Kind string
	// Mounts (playground only) are the screens the baseplate mounts — each binds a
	// wireframe to a route with an optional guard. A playground accepts wireframes
	// and nothing else. One unguarded mount at "/" is the common single-screen app;
	// several guarded mounts make distinct screens (login vs home) the runtime
	// routes between.
	Mounts []Mount
	// Into (ui/data only) names the wireframe socket this facet snaps into,
	// written `ui Nav in nav:` / `data Feed in feed:`. The socket's declared kind
	// must match this facet's kind, or the bricks don't fit.
	Into string
	// Sockets (wireframe only) are the typed slots that upper-layer facets snap
	// into — each names a region and the facet kind it accepts.
	Sockets []Socket
	// Frame (wireframe only) is the layout tree for the surface; a `slot <name>`
	// (SlotRef) node marks where a socket's composited content lands.
	Frame []Node
	// Content (ui/data only) is the node tree this facet contributes to its socket.
	Content []Node
	// Imports are the paths of `import "..."` modules declared above the facet
	// header. The compiler resolves them relative to the importing file. For plain
	// apps each module's declarations are merged into this graph; for a layered
	// stack the imports pull every brick into the pool the playground composes.
	Imports    []string
	Auth       bool // a bare `auth` line turns on built-in users/login/logout/signup
	Entities   []*Entity
	Enums      []*Enum
	States     []*State
	Derives    []*Derive
	Policies   []*Policy
	Actions    []*Action
	Jobs       []*Job
	Components []*Component
	Layouts    []*Layout
	Views      []*View
	Theme      []ThemeVar
	Line       int
}

// Socket is one typed slot declared by a wireframe: `socket feed: data`. Accept
// is the facet kind ("ui" or "data") allowed to snap in; a mismatch is a compile
// error — the studs don't line up.
type Socket struct {
	Name   string
	Accept string // "ui" | "data"
	Line   int
}

// Mount is one screen a playground mounts: a wireframe bound to a route, with an
// optional zero-arg guard policy. `mount Shell at "/" requires member`. A failing
// guard at runtime redirects to the first screen the actor may enter, so login
// and home are separate surfaces the auth state routes between.
type Mount struct {
	Wireframe string
	Path      string // "" defaults to "/"
	Requires  string // zero-arg guard policy; "" = open
	Line      int
}

// Enum is a closed set of named text values: `enum Status: active, closed`. An
// enum name may be used anywhere a type is expected (entity field, state, param);
// its values are written `Status.active` and stored as their lowercase text. The
// compiler checks every literal is a declared member, so a typo is a compile
// error rather than a bad row.
type Enum struct {
	Name   string
	Values []string
	Line   int
}

// ThemeVar is one `name "value"` line in a `theme:` block. It becomes a CSS
// custom property (`--fa-<name>`) on the document root, so the whole UI restyles
// from one place without touching any view.
type ThemeVar struct {
	Name  string
	Value string
}

// Component is a reusable, parameterized view fragment: `component Card(p: Post):`
// then a node tree. It is invoked from any view with `use Card(expr)`. A
// component is pure projection (no placement of its own) — it renders in whatever
// domain its call site renders in, with its parameters bound for that use.
type Component struct {
	Name   string
	Params []Param
	Root   []Node
	Line   int
}

// Layout is a page chrome wrapper: `layout Main:` then a node tree containing one
// `slot` node where the routed view's content is injected. A view opts into a
// layout with `view X at "/" in Main:`, so shared chrome (nav, footer) lives once.
type Layout struct {
	Name string
	Root []Node
	Line int
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
// (the database stores ciphertext; the working set holds plaintext). A type may
// carry a trailing `?` (Optional), which permits the field to be null.
type EntityField struct {
	Name     string
	Type     string // int | text | bool | money | date | <Enum> | <EntityName>
	Secret   bool   // @secret — encrypted at rest (AES-GCM under FACET_SECRET)
	Optional bool   // text? — the column is nullable
	Line     int
}

// State is one `state name: Type = default [@client|@server]` cell. Scalar
// server state is per-session; client state is per-browser-instance.
type State struct {
	Name      string
	Type      string // scalar, an enum name, or `[Elem]` for a list
	Elem      string // element type when Type is a list ("" otherwise)
	List      bool   // true for a `[T]` list cell
	Optional  bool   // true for a `T?` nullable cell
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
	Name       string
	Params     []Param
	Requires   []Require
	Checks     []Check // declarative input validation, run before the body
	Optimistic bool    // @optimistic — the client predicts the result before the round-trip
	Body       []Stmt
	Line       int
}

// Check is one `check <expr> "message"` clause: a precondition over the action's
// parameters (and actor) the authority evaluates before running the body. A
// failing check aborts the action and returns its friendly message, so invalid
// input never reaches the store.
type Check struct {
	Cond Expr
	Msg  string
	Line int
}

// Require is one `requires` clause: a policy name and the arguments passed to it
// (empty for a zero-parameter policy). The arguments are expressions over the
// action's parameters and the actor, evaluated when the gate runs.
type Require struct {
	Name string
	Args []Expr
	Line int
}

// Param is one typed action/policy/component parameter.
type Param struct {
	Name     string
	Type     string
	Optional bool
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
	Name     string
	Path     string   // "" until resolved; URL the page is served at, may hold :params
	Params   []string // dynamic path segments (`/post/:id` → ["id"]), bound in scope
	Layout   string   // optional layout name to wrap this page (`in Main`)
	Requires string   // optional zero-arg policy guarding the route ("" = open)
	Screen   bool     // true for a composed playground screen — a failing guard
	// redirects to the first screen the actor may enter, instead of a dead end.
	Root []Node
	Line int
}

// ── view nodes ──────────────────────────────────────────────────────────────

// Node is a UI node. The set is small and target-neutral.
type Node interface{ node() }

// Box is a layout container — its children stack vertically.
type Box struct{ Children []Node }

// Row is a horizontal layout container — its children sit side by side and wrap,
// collapsing to a vertical stack on narrow viewports. It is the seam for
// responsive multi-column layouts (e.g. a nav rail beside a feed beside an
// aside) that reflow with the window.
type Row struct{ Children []Node }

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

// Select is a dropdown two-way bound to a client state cell, choosing among a
// fixed set of options (label → value). Bound to an enum cell, its options
// default to the enum's members.
type Select struct {
	Bind    string
	Options []Option
}

// Option is one `option "Label" -> "value"` entry of a Select.
type Option struct {
	Label string
	Value string
}

// Form groups inputs and submits an action when its submit control fires —
// rendered as a real <form> so the browser's Enter-to-submit and accessibility
// come for free. Children are ordinary nodes (inputs, text); Submit is the
// label of the submit button and Action the server/client action it calls.
type Form struct {
	Action string
	Args   []Expr
	Submit string
	Body   []Node
}

// Upload posts a file to the authority and binds the resulting URL to a client
// state cell, so a view can show or store the uploaded media. The transfer and
// storage are runtime services; the language only names the target cell.
type Upload struct {
	Bind  string
	Label string
}

// Use renders a component with arguments bound to its parameters:
// `use Card(post)`. It is the call site of a reusable view fragment.
type Use struct {
	Name string
	Args []Expr
}

// Slot is the injection point inside a layout where the routed view's content is
// rendered. A layout has exactly one.
type Slot struct{}

// SlotRef is a named injection point inside a wireframe frame: `slot feed`. The
// compiler composites the content of the facet(s) snapped into that socket here.
// Unlike a layout's single anonymous Slot, a frame has one SlotRef per socket.
type SlotRef struct{ Name string }

func (Box) node()     {}
func (Row) node()     {}
func (Text) node()    {}
func (Button) node()  {}
func (For) node()     {}
func (If) node()      {}
func (Input) node()   {}
func (Link) node()    {}
func (Select) node()  {}
func (Form) node()    {}
func (Upload) node()  {}
func (Use) node()     {}
func (Slot) node()    {}
func (SlotRef) node() {}

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

// ListLit is a list literal: `[a, b, c]`. Its elements are pure expressions.
type ListLit struct {
	Elems []Expr
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
func (ListLit) expr()   {}
func (Ref) expr()       {}
func (Get) expr()       {}
func (EntityGet) expr() {}
func (Agg) expr()       {}
func (Call) expr()      {}
func (Bin) expr()       {}
func (Un) expr()        {}
