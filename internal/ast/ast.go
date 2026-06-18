// Package ast defines the FDL syntax tree for the whole primitive taxonomy —
// facet/feed/stream/lifecycle/pipe (server-rendered) and vault/media/signal
// (client-rendered), distinguished by Facet.Kind.
//
// Block keywords (canonical, see README.md): `who` (authorization), `what`
// (data), `looks` (template; server-rendered kinds), `when <event>` (handler;
// subscription is implied). Client-rendered kinds use `decrypt:`/`source:` bodies
// (Facet.Client) and never compile to a server template.
//
// The looks/render body is represented as a FLAT stream of nodes (Text / Interp
// / Ctrl) rather than a tree. This mirrors the Go html/template output it will
// compile to ({{if}}...{{end}} is itself flat), so codegen is a straight pass
// with no tree-walking. Block `if`/`for` and inline `{ if } ... { end }` both
// lower to the same Ctrl markers; block control gets a synthesized "end" when
// its indented body closes.
package ast

import "strings"

// Pos is a 1-based source position.
type Pos struct {
	Line int
	Col  int
}

// Facet is one primitive definition — `<kind> Name:`. Despite the type name, it
// covers the whole taxonomy: Kind distinguishes facet/feed/stream/lifecycle/pipe
// (server-rendered) from vault/media/signal (client-rendered).
type Facet struct {
	Kind    string // facet (default) | feed | stream | lifecycle | pipe | vault | media | signal
	Name    string
	FacetID string   // explicit `facet-id:` override; "" means derive
	Who     Who      // the `who:` authorization block (zero value = public)
	Fields  []Field  // the `what:` block
	State   []Field  // the `state:` block — local client reactive values (signals); each Field's Expr is its required initial value (see docs/REACTIVITY.md)
	Looks   []Node   // the `looks:` block (server-rendered kinds), as a flat render stream
	Client  []Node   // the `decrypt:`/`source:` body (client-rendered kinds); never lowered to a server template
	Whens   []When   // the `when <event>:` handlers
	Actions []Action // the `actions:` block — named client-side handlers that mutate state signals (see docs/REACTIVITY.md, Brick 3)
	Effects []Effect // the `effects:` block — run an action when its dependency signals change (see docs/REACTIVITY.md, Brick 7)
	Queries []Query  // the `query:` block — async server fetches exposed as reactive {loading,error,data} values (see docs/REACTIVITY.md, Brick 11)
	// Per-kind declarative extras. Recorded in the manifest; runtime semantics are
	// staged for a later round (this is the compiler-surface pass).
	Order    string      // feed: ordering field/expression
	Throttle string      // stream | pipe: min interval between pushes
	Window   string      // stream: max retained items
	TTL      string      // signal: time-to-live of the ephemeral value
	States   []string    // lifecycle: the state-machine states
	Style    []StyleProp // the `style:` block — cross-platform style tokens (ordered)
	Pos      Pos
}

// StyleProp is one `key: value` line in a facet's `style:` block — a token-based,
// cross-platform style declaration (e.g. `gap: 2`, `bg: surface`, `radius: md`).
// The compiler resolves it to concrete inline style on the facet's root element.
type StyleProp struct {
	Key string
	Val string
	Pos Pos
}

// KindName returns the primitive kind, normalising the zero value to "facet" (so
// an AST built without an explicit Kind — e.g. in tests — behaves as a facet).
func (f *Facet) KindName() string {
	if f.Kind == "" {
		return "facet"
	}
	return f.Kind
}

// ServerRendered reports whether this primitive renders on the server (its body
// compiles to an html/template). The client-rendered kinds — vault, media,
// signal — emit ZERO server template: their content is produced in the browser,
// which is the architectural guarantee (a compromised server cannot render vault
// plaintext). See README "primitive taxonomy".
func (f *Facet) ServerRendered() bool {
	switch f.Kind {
	case "vault", "media", "signal":
		return false
	default: // "", facet, feed, stream, lifecycle, pipe
		return true
	}
}

// RenderBody returns the active render node stream for the facet — Looks for
// server-rendered kinds, Client for client-rendered kinds. Compiler passes that
// are render-target-agnostic (field-ref + composition checks) walk this.
func (f *Facet) RenderBody() []Node {
	if f.ServerRendered() {
		return f.Looks
	}
	return f.Client
}

// Who is the `who:` authorization block (audit C2). v0 uses named policies the
// app implements, not inline expressions. It is recorded in the manifest so the
// access-control surface is auditable (`fct audit`).
type Who struct {
	Require    []string    // policy names that must all pass to view the facet
	Redactions []Redaction // fields stripped from the data before render
}

// Redaction strips Field before render — unconditionally if UnlessPolicy is "",
// otherwise unless that policy passes.
type Redaction struct {
	Field        string
	UnlessPolicy string
}

// HasWho reports whether the facet declares any authorization.
func (f *Facet) HasWho() bool { return len(f.Who.Require) > 0 || len(f.Who.Redactions) > 0 }

// Field is one entry in the `what:` block. A plain field is an input prop
// (`name: Type`); a computed field (`name: Type = expr` or `name = expr`)
// carries Expr, a value derived from earlier fields and resolved at render time
// — the caller never supplies it. For computed fields Type may be empty.
type Field struct {
	Name string
	Type string // int|float|str|bool, or a custom Ident (capitalized); "" allowed when computed
	Expr string // computed-field expression; "" = a plain input prop
	Pos  Pos
}

// IsComputed reports whether the field is a derived (`=`) value rather than an
// input prop.
func (f Field) IsComputed() bool { return f.Expr != "" }

// IsCustomType reports whether Type names a backend domain type (capitalized),
// as opposed to a builtin. Used for facet-id derivation.
func (f Field) IsCustomType() bool {
	if f.Type == "" {
		return false
	}
	c := f.Type[0]
	return c >= 'A' && c <= 'Z'
}

// DerivedFacetID returns the facet-id pattern. If FacetID is set it wins;
// otherwise it derives from the first custom-typed field
// (`Name:field:{field.id}`), falling back to a singleton id of just Name.
func (f *Facet) DerivedFacetID() string {
	if f.FacetID != "" {
		return f.FacetID
	}
	for _, fld := range f.Fields {
		if fld.IsCustomType() && !fld.IsComputed() {
			return f.Name + ":" + fld.Name + ":{" + fld.Name + ".id}"
		}
	}
	return f.Name
}

// When is one `when <events>:` handler block. The events it names are both the
// subscription and the trigger; there is no separate subscribe block.
type When struct {
	Events    []string
	Mutations []Mutation
	Pos       Pos
}

// Mutation is one line inside a `when` block.
type Mutation struct {
	Op     string // replace|append|prepend|remove|replace_all
	Target string // child facet name; "" for replace_all
	With   string // expr after `with`; "" if absent
	Pos    Pos
}

// Action is one named handler in the `actions:` block — a reactive "controller
// method": a name plus the signal mutations it performs when an element's bound
// DOM event fires. Actions are event-agnostic (the element's `on:<event>="name"`
// attribute chooses the trigger), so one action is reusable across elements and
// events. It is the client analogue of When: When maps a *server* event to data
// mutations; Action maps a *client* event to *signal* mutations, applied locally
// with zero round-trip (see docs/REACTIVITY.md, Brick 3).
type Action struct {
	Name      string
	Mutations []Assign
	Pos       Pos
}

// Assign is one `signal = expr` line inside an action: set state signal Target to
// the value of Expr. Target must be a declared `state:` signal — `what:` fields
// are server-authoritative and cannot be mutated on the client.
type Assign struct {
	Target string
	Expr   string
	Pos    Pos
}

// Effect is one `on <deps>: <action>` line in the `effects:` block — a reactive
// side-effect: when any dependency signal changes, the named action runs. It is
// the imperative complement to a derived value (which is a pure function of
// signals): an effect can accumulate history, mirror one signal into another, or
// (later, Tier-2) drive WASM compute. To stay loop-free, effects run once per
// event cycle and do not re-trigger one another (see docs/REACTIVITY.md, Brick 7).
type Effect struct {
	Deps   []string
	Action string
	Pos    Pos
}

// Query is one line in the `query:` block — `name from "url"`: an async fetch of
// a server resource exposed to the client reactive layer as a structured value
// `name` with fields `loading` (bool), `error` (bool) and `data` (the decoded
// JSON body). The runtime starts the fetch on mount and flushes the bound DOM
// when it resolves, so `{name.data.title}` / `hidden="{name.loading}"` light up
// without a round-trip per render. It is the client analogue of a `what:` field
// the server would otherwise have to supply (see docs/REACTIVITY.md, Brick 11).
type Query struct {
	Name string
	URL  string
	Pos  Pos
}

// ── Looks nodes (flat) ──────────────────────────────────────────────────────

// Node is a looks-body element.
type Node interface{ node() }

// Text is literal template text (raw HTML, whitespace, indentation).
type Text struct{ S string }

// Interp is a `{expr}` interpolation hole.
type Interp struct {
	Expr string
	Pos  Pos
}

// Ctrl is a control marker. Op is one of: if, for, else, end.
//   - if:  Expr holds the condition
//   - for: Var + Iter hold `for Var in Iter`
//   - else/end: no operands
type Ctrl struct {
	Op   string
	Expr string
	Var  string
	Iter string
	Pos  Pos
}

// Child is a child-facet call inside a looks body. Self-closing
// (<Avatar user="{user}"/>) has Children == nil. Block form
// (<Card title="x"> …content… </Card>) puts the content in Children, which the
// child renders at its default `slot:`. Content under a `fill name:` line goes
// to Fills[name] instead, targeting the child's matching `slot name:`. The
// compiler lowers each to a nested template call, so the child's data-facet-id
// nests inside the parent's.
type Child struct {
	Name     string // the child facet's name (capitalized)
	Props    []Prop
	Children []Node            // default-slot content (parent scope); nil if none
	Fills    map[string][]Node // named-slot content: slot name → nodes (parent scope)
	Pos      Pos
}

// Slot is a `slot:` (default) or `slot name:` (named) insertion point in a
// facet's looks where a parent's block-form content is injected, with optional
// default content shown when the facet is used self-closing / empty or the
// matching slot is unfilled. Name is "" for the default slot.
type Slot struct {
	Name    string
	Default []Node
	Pos     Pos
}

// Prop is one attribute on a child-facet call. If IsExpr, Expr holds the FDL
// expression from value="{expr}"; otherwise Literal holds the plain string.
type Prop struct {
	Name    string // the child's field name being set
	Expr    string
	Literal string
	IsExpr  bool
}

func (Text) node()   {}
func (Interp) node() {}
func (Ctrl) node()   {}
func (Child) node()  {}
func (Slot) node()   {}

// LooksText concatenates all literal Text of a looks body — handy in tests.
func LooksText(nodes []Node) string {
	var b strings.Builder
	for _, n := range nodes {
		if t, ok := n.(Text); ok {
			b.WriteString(t.S)
		}
	}
	return b.String()
}
