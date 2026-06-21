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
	App        string            `json:"app"`
	Auth       bool              `json:"auth,omitempty"` // built-in users/login enabled
	Entities   []Entity          `json:"entities"`
	Enums      []Enum            `json:"enums,omitempty"`
	States     []State           `json:"states"`
	Derives    []Derive          `json:"derives"`
	Policies   []Policy          `json:"policies"`
	Actions    []Action          `json:"actions"`
	Jobs       []Job             `json:"jobs"`
	Components []Component       `json:"components,omitempty"` // reusable view fragments
	Services   []Service         `json:"services,omitempty"`   // external services (brains) actions may call
	Webhooks   []Webhook         `json:"webhooks,omitempty"`   // inbound endpoints external systems POST to
	Triggers   []Trigger         `json:"triggers,omitempty"`   // event reactions: an action's success runs another action
	Theme      map[string]string `json:"theme,omitempty"`      // CSS custom properties (--fa-<name>)
	Routes     []Route           `json:"routes,omitempty"`     // every page's path + guard, for client link-hiding and SPA navigation
	Pages      []Page            `json:"pages"`                // one per view; each is a route
	// View/Bindings/DepGraph mirror the *current* page. `facet build` shows the
	// first page; the server swaps them per request to the matched route, so the
	// client runtime can keep reading these three fields unchanged.
	Bindings []Binding           `json:"bindings"`
	View     []Node              `json:"view"`
	DepGraph map[string][]string `json:"depGraph"`
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
	Name   string   `json:"name"`
	Values []string `json:"values"`
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
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`
}

// Field is one entity column. For a relation field, Ref names the entity it
// points at (the column stores that row's id, a foreign key with ON DELETE
// CASCADE). Index marks a field the compiler saw filtered, ordered, or used as
// a relation — the store builds a real index for it so reads stay sub-linear as
// the table grows past memory.
type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Ref      string `json:"ref,omitempty"`      // relation target entity, else ""
	Enum     string `json:"enum,omitempty"`     // enum type name when Type is text-backed enum
	Index    bool   `json:"index,omitempty"`    // build a database index for this column
	Secret   bool   `json:"secret,omitempty"`   // encrypt this column at rest (AES-GCM)
	Optional bool   `json:"optional,omitempty"` // column is nullable
}

// IsRelation reports whether the field is a foreign key to another entity.
func (f Field) IsRelation() bool { return f.Ref != "" }

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
	Body       []Stmt    `json:"body"` // statements in source order, including `check` validations
}

// Require is one resolved permission check on an action: the policy name plus the
// argument expressions passed to a row-level (parameterized) policy.
type Require struct {
	Name string  `json:"name"`
	Args []*Expr `json:"args,omitempty"`
}

// Param is a typed action/policy/component parameter.
type Param struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
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

// Stmt is one action statement. Op ∈ assign | add | set | remove | clear | call.
type Stmt struct {
	Op      string      `json:"op"`
	Target  string      `json:"target,omitempty"`  // assign
	Entity  string      `json:"entity,omitempty"`  // add/set/remove/clear
	Field   string      `json:"field,omitempty"`   // set; for a call, the operation name
	Key     *Expr       `json:"key,omitempty"`     // set/remove
	Value   *Expr       `json:"value,omitempty"`   // assign/set
	Fields  []FieldInit `json:"fields,omitempty"`  // add
	Service string      `json:"service,omitempty"` // call: the service name
	Args    []*Expr     `json:"args,omitempty"`    // call: the operation arguments
	Var     string      `json:"var,omitempty"`     // remove (filtered): item variable
	Where   *Expr       `json:"where,omitempty"`   // remove (filtered): predicate (nil = by-id)
	Bind    string      `json:"bind,omitempty"`    // call (request→response): local the result binds to
	Ret     string      `json:"ret,omitempty"`     // call (request→response): result type core, for decode/coerce
	RetList bool        `json:"retList,omitempty"` // call (request→response): result is a list of Ret
	Role    *Expr       `json:"role,omitempty"`    // establish: optional new session role (Value holds the new actor)
	Msg     string      `json:"msg,omitempty"`     // check: the message returned when the condition (Value) is false
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
	Kind     string `json:"kind"` // box|row|text|image|icon|video|richtext|badge|button|list|if|match|case|else|input|link|select|form|upload|use|slot|tabs|tab
	Children []Node `json:"children,omitempty"`

	Segs []Seg `json:"segs,omitempty"` // text

	Label  string  `json:"label,omitempty"`  // button/upload/form submit
	Action string  `json:"action,omitempty"` // button/form
	Args   []*Expr `json:"args,omitempty"`   // button/form/use

	ID    string `json:"id,omitempty"`    // list/if: dynamic region id
	Var   string `json:"var,omitempty"`   // list: item variable
	Coll  string `json:"coll,omitempty"`  // list: collection name
	Where *Expr  `json:"where,omitempty"` // list: row filter (nil = all)
	Order string `json:"order,omitempty"` // list: sort field
	Desc  bool   `json:"desc,omitempty"`  // list: descending
	Limit *Expr  `json:"limit,omitempty"` // list: max rows (nil = unlimited); int literal or a dynamic page-size expr
	Cond  *Expr  `json:"cond,omitempty"`  // if: condition

	Bind        string `json:"bind,omitempty"`        // input/select/upload: state cell
	Placeholder string `json:"placeholder,omitempty"` // input

	Path string `json:"path,omitempty"` // link: destination route

	Name    string   `json:"name,omitempty"`    // use: component name / icon: glyph name
	Options []Option `json:"options,omitempty"` // select: choices
	Value   string   `json:"value,omitempty"`   // tab: the value its bound cell takes when active
}

// Option is one select choice (display label → stored value).
type Option struct {
	Label string `json:"label"`
	Value string `json:"value"`
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
	Op    string  `json:"op,omitempty"`    // bin / un / agg (count|sum|exists)
	Args  []*Expr `json:"args,omitempty"`  // call
	L     *Expr   `json:"l,omitempty"`
	R     *Expr   `json:"r,omitempty"`
	X     *Expr   `json:"x,omitempty"`
	Var   string  `json:"var,omitempty"`   // agg: item variable for the filtered form
	Where *Expr   `json:"where,omitempty"` // agg: filter predicate (nil = whole collection)
}
