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
	FacetID string  // explicit `facet-id:` override; "" means derive
	Who     Who     // the `who:` authorization block (zero value = public)
	Fields  []Field // the `what:` block
	Looks   []Node  // the `looks:` block (server-rendered kinds), as a flat render stream
	Client  []Node  // the `decrypt:`/`source:` body (client-rendered kinds); never lowered to a server template
	Whens   []When  // the `when <event>:` handlers
	// Per-kind declarative extras. Recorded in the manifest; runtime semantics are
	// staged for a later round (this is the compiler-surface pass).
	Order    string   // feed: ordering field/expression
	Throttle string   // stream | pipe: min interval between pushes
	Window   string   // stream: max retained items
	TTL      string   // signal: time-to-live of the ephemeral value
	States   []string // lifecycle: the state-machine states
	Pos      Pos
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

// Field is one entry in the `what:` block. v0 supports props only (no computed
// `=` fields).
type Field struct {
	Name string
	Type string // int|float|str|bool, or a custom Ident (capitalized)
	Pos  Pos
}

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
		if fld.IsCustomType() {
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
// child renders at its `slot:`. The compiler lowers it to a nested template
// call, so the child's data-facet-id nests inside the parent's.
type Child struct {
	Name     string // the child facet's name (capitalized)
	Props    []Prop
	Children []Node // block-form slot content (parent scope); nil if self-closing
	Pos      Pos
}

// Slot is a `slot:` insertion point in a facet's looks where a parent's
// block-form content is injected, with optional default content shown when the
// facet is used self-closing / empty.
type Slot struct {
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
