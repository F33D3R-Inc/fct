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
	App      string   `json:"app"`
	Auth     bool     `json:"auth,omitempty"` // built-in users/login enabled
	Entities []Entity `json:"entities"`
	States   []State  `json:"states"`
	Derives  []Derive `json:"derives"`
	Policies []Policy `json:"policies"`
	Actions  []Action `json:"actions"`
	Jobs     []Job    `json:"jobs"`
	Pages    []Page   `json:"pages"` // one per view; each is a route
	// View/Bindings/DepGraph mirror the *current* page. `facet build` shows the
	// first page; the server swaps them per request to the matched route, so the
	// client runtime can keep reading these three fields unchanged.
	Bindings []Binding           `json:"bindings"`
	View     []Node              `json:"view"`
	DepGraph map[string][]string `json:"depGraph"`
}

// Page is one routed view: its URL plus its own node tree, tracked bindings, and
// dependency graph (ids are page-local).
type Page struct {
	Name     string              `json:"name"`
	Path     string              `json:"path"`
	View     []Node              `json:"view"`
	Bindings []Binding           `json:"bindings"`
	DepGraph map[string][]string `json:"depGraph"`
}

// Entity is a durable, shared, persisted record type — the database.
type Entity struct {
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`
}

// Field is one entity column.
type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// State is one state cell with its resolved placement.
type State struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Placement string `json:"placement"`
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
// can hide what the actor may not do.
type Policy struct {
	Name string `json:"name"`
	Expr *Expr  `json:"expr"`
}

// Action is a mutation: parameters, the policies it requires, the statements it
// runs, its resolved placement, and the state it reads/writes.
type Action struct {
	Name      string   `json:"name"`
	Params    []Param  `json:"params"`
	Requires  []string `json:"requires"`
	Placement string   `json:"placement"`
	Writes    []string `json:"writes"`
	Reads     []string `json:"reads"`
	Body      []Stmt   `json:"body"`
}

// Param is a typed action parameter.
type Param struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Job is a scheduled server action: the runtime invokes Action on a timer
// (Every seconds) and/or once at startup (OnStart), with no user in the loop.
type Job struct {
	Name    string `json:"name"`
	Action  string `json:"action"`
	Every   int    `json:"every,omitempty"`
	OnStart bool   `json:"onStart,omitempty"`
}

// Stmt is one action statement. Op ∈ assign | add | set | remove | clear.
type Stmt struct {
	Op     string      `json:"op"`
	Target string      `json:"target,omitempty"` // assign
	Entity string      `json:"entity,omitempty"` // add/set/remove/clear
	Field  string      `json:"field,omitempty"`  // set
	Key    *Expr       `json:"key,omitempty"`    // set/remove
	Value  *Expr       `json:"value,omitempty"`  // assign/set
	Fields []FieldInit `json:"fields,omitempty"` // add
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
	Kind     string `json:"kind"` // box | text | button | list | if | input
	Children []Node `json:"children,omitempty"`

	Segs []Seg `json:"segs,omitempty"` // text

	Label  string  `json:"label,omitempty"`  // button
	Action string  `json:"action,omitempty"` // button
	Args   []*Expr `json:"args,omitempty"`   // button

	ID    string `json:"id,omitempty"`    // list/if: dynamic region id
	Var   string `json:"var,omitempty"`   // list: item variable
	Coll  string `json:"coll,omitempty"`  // list: collection name
	Where *Expr  `json:"where,omitempty"` // list: row filter (nil = all)
	Order string `json:"order,omitempty"` // list: sort field
	Desc  bool   `json:"desc,omitempty"`  // list: descending
	Limit int    `json:"limit,omitempty"` // list: max rows (0 = unlimited)
	Cond  *Expr  `json:"cond,omitempty"`  // if: condition

	Bind        string `json:"bind,omitempty"`        // input: state cell
	Placeholder string `json:"placeholder,omitempty"` // input

	Path string `json:"path,omitempty"` // link: destination route
}

// Seg is a text segment: a literal (Lit), a tracked top-level binding (Bind id),
// or an item-scope expression evaluated inline at render time (Expr).
type Seg struct {
	Lit  string `json:"lit,omitempty"`
	Bind string `json:"bind,omitempty"`
	Expr *Expr  `json:"expr,omitempty"`
}

// Expr is the serialized expression form interpreted identically by Go and JS.
type Expr struct {
	Kind  string  `json:"kind"` // lit | ref | get | eget | agg | call | bin | un
	Val   any     `json:"val,omitempty"`
	VType string  `json:"vtype,omitempty"`
	Name  string  `json:"name,omitempty"`  // ref / eget entity / agg collection / call builtin
	Field string  `json:"field,omitempty"` // get / eget / agg (sum)
	Obj   *Expr   `json:"obj,omitempty"`   // get
	Key   *Expr   `json:"key,omitempty"`   // eget
	Op    string  `json:"op,omitempty"`    // bin / un / agg (count|sum)
	Args  []*Expr `json:"args,omitempty"`  // call
	L     *Expr   `json:"l,omitempty"`
	R     *Expr   `json:"r,omitempty"`
	X     *Expr   `json:"x,omitempty"`
}
