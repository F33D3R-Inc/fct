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
	// Composed is set on the single flattened graph a layered build produces (and
	// never on an authored file). Downstream passes read it only to phrase a
	// diagnostic in the vocabulary the author actually wrote in: a route is
	// declared by a playground `mount` or a brick `view` there, not by a top-level
	// `view`. It changes no compilation behavior.
	Composed bool
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
	Records    []*Record
	Enums      []*Enum
	States     []*State
	Derives    []*Derive
	Policies   []*Policy
	Actions    []*Action
	Jobs       []*Job
	Components []*Component
	Layouts    []*Layout
	Views      []*View
	Services   []*Service
	Webhooks   []*Webhook
	Triggers   []*Trigger
	Theme      []ThemeVar   // base design tokens (the light palette)
	DarkTheme  []ThemeVar   // `theme dark:` — token overrides applied under prefers-color-scheme: dark
	Themes     []NamedTheme // `theme <name>:` — alternate palettes selectable at runtime
	CSS        string       // raw stylesheet from `css:` blocks, emitted verbatim into the page
	Line       int
}

// Trigger is a programmatic event reaction: when the action named On completes
// successfully, the runtime runs Action. `on post -> notifyFollowers`. It is the
// non-cron sibling of a `job` — a domain event (an action finishing), not a clock,
// fires the work. Reactions are zero-argument server actions run with system
// authority, synchronously after the triggering action commits.
type Trigger struct {
	On     string // the action whose success fires the reaction
	Action string // the reaction to run
	Line   int
}

// Webhook is a typed inbound endpoint: an external system POSTs to Path, the
// runtime verifies an HMAC signature, and runs the named Action with parameters
// decoded from the JSON body. `webhook "/hooks/pay" -> confirmPaid secret PAY_KEY`.
// It is how a payment processor, a transcode worker, or any brain calls *into* the
// app — the inbound counterpart of a `service` call. Secret names an env var
// holding the HMAC key ("" derives one from the master secret).
type Webhook struct {
	Path   string
	Action string
	Secret string
	Line   int
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

// Record is a named value-object type: `record Verdict: score: int, reasons: [text]`.
// Unlike an entity it has no storage, id, or persistence — it is a pure in-flight
// shape. Its reason to exist: a brain (a `service`) returns structured JSON, and a
// record lets a `let v = call Brain.op()` bind the whole object and read its typed
// fields (`v.score`, `v.reasons`) instead of only a scalar. Records are flat — a
// field is a primitive, an enum, or a list of those, never another record — so a
// `v.field` access is always single-level and fully checkable.
type Record struct {
	Name   string
	Fields []RecordField
	Line   int
}

// RecordField is one typed field of a record: a name and a type that may be a
// list (`[T]`) and/or optional (`T?`). The type core is a primitive or an enum.
type RecordField struct {
	Name     string
	Type     string
	List     bool
	Optional bool
	Line     int
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
	Line  int // source line, so a diagnostic about the value can point at it
}

// NamedTheme is a `theme <name>:` block — an alternate palette (e.g. `pride`,
// `high-contrast`) the app can switch to at runtime by setting the built-in
// `theme` state. Its tokens emit under a `[data-theme="<name>"]` selector rather
// than `:root`, so selecting it overrides the base palette with no custom CSS.
// The reserved name `dark` is not a NamedTheme — it stays the auto-OS palette
// (and is additionally forceable via the same `theme` state).
type NamedTheme struct {
	Name string
	Vars []ThemeVar
	Line int
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
	PlaceInfer   = ""
	PlaceClient  = "client"  // @client — ephemeral, local, latency-sensitive
	PlaceServer  = "server"  // @server — explicit authoritative
	PlacePrivate = "private" // @private — authoritative AND server-only: never shipped to a client, never renderable (a compile error to interpolate). For secrets/identity keys (e.g. a PIAL UUID).
)

// Entity is a durable, shared, persisted record type — the database. Its rows
// are always authoritative (server) and survive restarts.
type Entity struct {
	Name string
	// SoftDelete (`entity Post @softdelete:`) makes `remove` archive a row instead
	// of dropping it: the authority flags it and hides it from every read, but the
	// data survives. An audit/restore story without an imperative `archived` flag in
	// every query.
	SoftDelete bool
	Fields     []EntityField
	Line       int
}

// EntityField is one column of an entity. A `@secret` field is encrypted at rest
// (the database stores ciphertext; the working set holds plaintext). A type may
// carry a trailing `?` (Optional), which permits the field to be null.
type EntityField struct {
	Name       string
	Type       string // int | text | bool | money | date | <Enum> | <EntityName>
	Secret     bool   // @secret — encrypted at rest (AES-GCM under FACET_SECRET)
	E2E        bool   // @e2e — end-to-end sealed: the client seals before sending and opens on read; the authority only ever holds ciphertext (never plaintext, never renders it)
	ReadPolicy string // @requires(policy) — field served only to actors the policy admits; never sent over SSE
	Optional   bool   // text? — the column is nullable
	// Declarative constraints — enforced by the authority on every add/set, so
	// invalid data can never reach the store (no imperative `check` needed).
	Unique   bool   // @unique — no two rows may share this value
	Required bool   // @required — must be present and non-empty
	Min      *int   // @min(n) — numeric: value ≥ n; text: length ≥ n (nil = unset)
	Max      *int   // @max(n) — numeric: value ≤ n; text: length ≤ n (nil = unset)
	Matches  string // @matches("regex") — text must match this anchored pattern ("" = unset)
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
	Optimistic bool // @optimistic — the client predicts the result before the round-trip
	// Body holds every statement in source order, including `check` validations —
	// so a check may run after a `let` bind and validate the bound result. Checks
	// (and lets) must precede any mutation, so a failed check rolls back nothing.
	Body []Stmt
	Line int
}

// Service is an external service (a "brain") fct can call over HTTP: a base URL
// and a set of typed operations. Calling one is a server-placed effect, so a
// client can never reach a service directly — it routes through the authority.
type Service struct {
	Name string
	URL  string
	Ops  []ServiceOp
	Line int
}

// ServiceOp is one operation a service exposes — a name, typed parameters, and an
// optional typed return (`rank(posts: [int]) -> [int]`). Together they form the
// contract the compiler checks each `call` against. An op with no `-> T` is
// fire-and-forget; one with a return can be bound back with `let x = call …`.
type ServiceOp struct {
	Name    string
	Params  []Param
	Ret     string // return type core ("" = no return; fire-and-forget only)
	RetList bool   // the return is a list of Ret (`-> [T]`)
	Line    int
}

// ServiceCall invokes a service operation from an action. Two forms, both
// effectful (egress) so both pin the action to the server authority:
//   - fire-and-forget: `call Zodacare.report(id, body)` — a side effect, no result.
//   - request→response: `let verdict = call Verity.check(id)` — Bind names the local
//     the typed result lands in, usable by the rest of the action body (e.g. assign
//     it into a state cell so it reaches the client).
type ServiceCall struct {
	Service string
	Op      string
	Args    []Expr
	Bind    string // request→response: local the result binds to ("" = fire-and-forget)
	Line    int
}

// Establish adopts a custom session identity from app-computed values — the hook
// for PIAL-style auth: a verify-action calls an identity brain (request→response)
// and promotes the verified result into the session with `establish actor handle`
// (optionally `role <expr>`). Setting who you are is the authority's job, so it
// pins the action to the server. `actor` becomes the renderable display identity;
// keep the opaque key (a UUID) in a `@private` cell, which can never be rendered.
type Establish struct {
	Actor Expr
	Role  Expr // optional (nil = leave the session role unchanged)
	Line  int
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

func (Check) stmt() {}

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
	List     bool // a `[T]` parameter (service operations only)
	Optional bool
	// Ref makes the parameter a *reference* rather than a value: it is bound at
	// the call site to the NAME of a declaration, not to the result of an
	// expression. RefValue ("") is an ordinary value parameter.
	//
	// This is what a reusable component is made of. A value parameter can carry
	// what a control *shows*; it cannot carry what a control *writes to* or what a
	// button *calls*, because those are identities the compiler must resolve —
	// `bind` names a state cell, `->` names an action. Without a reference
	// parameter a text field, a select, a submit button and a pending/failed
	// wrapper can only ever be written against one hard-coded cell or action,
	// which is why a component library could not have fields in it at all.
	//
	// Only a component may declare one, and a reference argument must be a bare
	// name (`use Field("Email", email)`), never an expression.
	Ref string
}

// The reference kinds a component parameter may declare.
const (
	RefValue  = ""       // an ordinary value parameter: `label: text`
	RefCell   = "cell"   // a reference to a state cell of type Type: `value: cell text`
	RefAction = "action" // a reference to an action (Type is empty): `submit: action`
)

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

// Set writes stored rows. In its by-id form it updates one field of one row —
// `set Entity(key).field = expr` — and in its filtered form it applies a block of
// field assignments to every row a predicate accepts:
//
//	set p in Product where p.stock > 0:
//	    stock = p.stock - 1
//
// The filtered form is `Remove`'s filtered shape carrying assignments instead of
// a deletion, and it carries them in Fields for the same reason `Add` does: a
// bulk update usually moves more than one column, and one traversal should move
// all of them together. Key/Field/Value describe the by-id form; Var/Where/Fields
// describe the filtered one, and exactly one of the two sets is populated.
type Set struct {
	Entity string
	Key    Expr
	Field  string
	Value  Expr
	Var    string      // filtered form: the item variable each row binds to
	Where  Expr        // filtered form: the predicate (nil = by-id)
	Fields []FieldInit // filtered form: the assignments applied to every match
	Line   int
}

// Remove deletes entity rows: by id (`remove Entity(key)`) or, in its filtered
// form, every row matching a predicate (`remove m in Entity where <cond>`) — the
// delete-by-key needed for things like unfollow.
type Remove struct {
	Entity string
	Key    Expr   // by-id form (nil in the filtered form)
	Var    string // filtered form: item variable
	Where  Expr   // filtered form: predicate (nil = by-id form)
	Line   int
}

// Clear empties an entity: `clear Entity`.
type Clear struct {
	Entity string
	Line   int
}

func (Assign) stmt()      {}
func (ServiceCall) stmt() {}
func (Establish) stmt()   {}
func (Add) stmt()         {}
func (Set) stmt()         {}
func (Remove) stmt()      {}
func (Clear) stmt()       {}

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
	// Page metadata: `meta title "…"` / `meta description "…"`, interpolated and
	// rendered server-side into <title>, <meta description>, and OpenGraph tags.
	TitleSegs []Seg
	DescSegs  []Seg
	Line      int
}

// ── view nodes ──────────────────────────────────────────────────────────────

// Node is a UI node. The set is small and target-neutral.
type Node interface{ node() }

// Modified wraps any node with the trailing `keyword "value"` modifiers written
// at the end of its line: `class "..."`, `style "..."` and `anchor "..."`. It is
// a pure decorator — lowering unwraps it and stamps the values onto the inner
// node's IR — so the modifier set is one concept with one parse and one lowering
// rather than a special case per keyword.
//
// `class`/`style` are the CSS escape hatch: they ride alongside the element's
// built-in `fa-*` class so a `css:` stylesheet can hook onto specific nodes
// (`box class "rail"`).
//
// `anchor` is an author-chosen name for a position in the page, the thing a
// `#install` link scrolls to. It is deliberately NOT the same concept as
// ir.Node.ID, which is the runtime's own region address — allocated by the
// compiler, meaningless to the author, and what the client uses to re-render a
// region in place. Two names for two jobs: reusing the region id would put an
// author's spelling on the wire where the addressing expects the compiler's, and
// a node can perfectly well need both.
type Modified struct {
	// Class is interpolated segments, not a string, for the reason every other
	// author-visible value is: a component that cannot vary the class it applies
	// cannot vary its appearance, so `class "x-rung-c-{tone}"` emitted the literal
	// braces and the only way to get a tone-dependent style was to write the
	// variants out by hand and pick between them with an `if`.
	//
	// `Style` stays a plain string. A class value is a token list — the safe set is
	// stateable in one line and a value outside it means nothing — whereas a style
	// value is a stylesheet fragment, where what an interpolated value may safely
	// be is a real question with no one-line answer. One of these has a validating
	// rule available and the other does not, so only one of them interpolates.
	Class  []Seg
	Style  string
	Anchor string
	Inner  Node
	// Line is the modifier's own source line. A decorator had no position of its
	// own until `style` gained a diagnostic, and an error about a `style` value
	// that cannot say which line it is on is most of the way back to saying
	// nothing at all.
	Line int
}

// Box is a layout container — its children stack vertically.
type Box struct{ Children []Node }

// Row is a horizontal layout container — its children sit side by side and wrap,
// collapsing to a vertical stack on narrow viewports. It is the seam for
// responsive multi-column layouts (e.g. a nav rail beside a feed beside an
// aside) that reflow with the window.
type Row struct{ Children []Node }

// Text is a text leaf of literal and interpolated segments.
type Text struct{ Segs []Seg }

// Heading is a `heading <level> "Title"` node: a text leaf that is a heading of
// the document rather than a span that has been styled to look like one. It is
// the node that gives a page an outline — the thing a screen reader navigates by
// and a search engine reads a structure out of, and which nothing in this
// language could express before it, so every page this stack has rendered was a
// flat run of spans.
//
// # Why the level is an EXPRESSION and not part of the keyword
//
// The obvious spelling is six node keywords (`h1` … `h6`), and it is wrong for
// one reason that is fatal in a component library: the same header component
// appears at different depths on different pages, so the level belongs to the
// CALL SITE, not to the component. With six keywords a `SectionHeader` that
// wanted to be an h2 on one page and an h3 on another would have to branch on a
// level it was handed — and `match` cases are compile-time literals, so it could
// not even do that. The level has to be something a parameter can carry, and the
// only thing a parameter carries is a value. So the level is an expression:
//
//	component SectionHeader(title: text, level: int):
//	    heading level "{title}"
//
//	use SectionHeader("Replies", 3)
//
// # Why it is not a modifier on `text`
//
// `text "…" heading 2` would make the level a Modified field, and Modified is
// deliberately a *pure decorator*: it applies to every node (so `box … heading 2`
// would parse and mean nothing), its values are literal strings rather than
// expressions, and it stamps attributes onto whatever it wraps rather than
// choosing what element that is. A heading level chooses the element. It is the
// node, not a decoration on one.
//
// # What the compiler proves about Level, and what it cannot
//
// See the lowering in internal/ir (case ast.Heading). In short: an int literal
// outside 1..6 is an error, a level whose type is known and is not a number is
// an error, and a level that reads state or a collection directly is an error
// (a leaf has no region of its own to re-render, so such a level could go stale
// in place). Whether a document has exactly one h1 and skips no levels is NOT
// checkable here and is not claimed: the level may be a value, a component is
// composed into pages it cannot see, and only one arm of an `if`/`match` renders.
type Heading struct {
	Level Expr
	Segs  []Seg
	Line  int
}

// Image is an `image "url" [alt "…"]` node — its URL is interpolated segments
// like text, so `image "…/avatar?seed={t.author}"` yields a per-row avatar.
//
// # `alt`, and why absence is not the same as empty
//
// `alt` is the words that stand in for the picture. It is interpolated segments
// for the same reason the URL is: a component is handed a picture per row, so it
// must be handed the description per row too — `image "{p.cover}" alt "{p.alt}"`.
//
// The renderers write `alt=""` when there is nothing to say, and that is correct
// markup: an empty alt tells a screen reader the image is decorative and may be
// skipped, which is a real and useful statement. It is not, however, a statement
// the AUTHOR made when the attribute is simply absent — those are two different
// facts wearing the same output, so the syntax tree keeps them apart:
//
//	image "/logo.svg"              nobody decided        → advised (see Advise)
//	image "/rule.svg" alt ""       decorative, on purpose → silent
//	image "/chart.png" alt "…"     described             → silent
//
// AltSet is what makes the middle line sayable. Without it there is no spelling
// of "this picture is decorative and I know it", so the advice could only ever
// be noise an author has no way to answer.
type Image struct {
	Segs   []Seg
	Alt    []Seg
	AltSet bool // the author wrote `alt`, even if what they wrote was empty
	Line   int
}

// Icon is an `icon "name"` node — a named glyph the page's CSS/icon font renders
// (`icon "home"`, `icon "heart"`). The glyph name is interpolated segments like
// image's URL, so a reusable control can be handed the glyph it shows.
type Icon struct{ Segs []Seg }

// Video is a `video "url" [alt "…"]` node — a media player with controls; the
// URL is interpolated segments like image (`video "{post.media}"`).
//
// It takes `alt` from the same keyword and the same lowering as `image`, and the
// renderers write it as `aria-label`, because `<video>` has no `alt` attribute
// and an accessible name is what it reads instead. That is the `toggle`/
// `checkbox` argument exactly: one thing the author states, two attributes the
// renderers write, and no mapping either side could hold a different copy of.
//
// The one place it differs from an image is the empty case. `alt=""` on an image
// MEANS decorative; `aria-label=""` on a video means nothing at all, so an empty
// or absent alt writes no attribute rather than an empty one.
type Video struct {
	Segs   []Seg
	Alt    []Seg
	AltSet bool
	// Poster is the still shown before playback (`poster "{p.thumb}"`) —
	// interpolated like the source, because a thumbnail in a `for` is one PER
	// ROW. Absent, the browser shows the first frame once it has it.
	Poster []Seg
	// The playback flags a feed needs: `autoplay` starts the clip as it scrolls
	// into view — and implies `muted`, because every browser refuses to autoplay
	// with sound, so writing one without the other produces a player that never
	// starts; `loop` restarts it at the end; `muted` silences it.
	Autoplay bool
	Loop     bool
	Muted    bool
	Line     int
}

// Richtext is a `richtext "{expr}"` node — its interpolated text is rendered as a
// safe subset of Markdown (headings, lists, code, bold/italic), the same algorithm
// on the server and client. For long-form posts and articles.
type Richtext struct{ Segs []Seg }

// Badge is a `badge "label"` node — a small pill of interpolated text for counts
// and status markers (`badge "{unread}"`, `badge "verified"`).
type Badge struct{ Segs []Seg }

// Tabs is a `tabs bind cell:` node — a segmented control whose selected tab is a
// `@client` state cell; each `tab "Label" -> "value":` holds the content shown
// when the cell equals that value. Switching tabs is local (no round-trip), so the
// binding must be client state — the classic Following/Trending/New feed switch.
type Tabs struct {
	Bind string
	Tabs []Tab
	Line int
}

// Tab is one `tab "Label" -> "value":` within a Tabs node. The label is
// interpolated segments; the value is not — it is the identity the bound cell
// takes and the key the active branch is chosen by, not something displayed.
type Tab struct {
	Label []Seg
	Value string
	Body  []Node
}

// Match is a `match <expr>:` node — pattern matching over a value, with one
// `case "value":` branch per match and an optional `else:`. When the matched
// expression is enum-typed (a state cell or an entity field), the compiler
// enforces exhaustiveness: every member must have a case, or there must be an
// `else`. It is the post-kind / notification-kind render switch.
type Match struct {
	Expr  Expr
	Cases []MatchCase
	Else  []Node // nil = no else branch
	Line  int
}

// MatchCase is one `case "value":` arm of a Match.
type MatchCase struct {
	Value string
	Body  []Node
}

// Seg is one piece of a Text: literal (Expr == nil) or interpolation.
type Seg struct {
	Lit  string
	Expr Expr
}

// Button emits an action (with evaluated argument expressions) when pressed. Its
// label is interpolated segments like text, so a count can sit in the label —
// `button "♥ {t.likes}" -> like(t.id)`.
type Button struct {
	Label  []Seg
	Action string
	Args   []Expr
	Line   int // source line, so a diagnostic about an argument can point at it
}

// Range is the header every repeating construct shares: the collection walked,
// the item variable each row binds to, and the clauses that narrow it — an
// optional `where <cond>` keeps only matching rows, `by <field> [desc|asc]`
// orders them, and `limit <n>` caps them, applied in that order (filter, then
// sort, then cap).
//
// It is one type rather than one copy of these six fields per repeating node,
// because the second repeating node is exactly where the two drift: a `for` that
// grew `limit` beside an option list that did not would be the same question
// answered two ways. One type, parsed by one function (parseRange) and lowered
// by one method (lowerRange), so every construct that repeats repeats the same.
type Range struct {
	Var   string
	Coll  string
	Where Expr   // optional row filter; nil = all rows
	Order string // sort field; "" = insertion order
	Desc  bool   // true = descending (newest/highest first)
	Limit Expr   // optional max rows: an int literal or an expr (e.g. a @client page size for load-more); nil = unlimited
	// More names the zero-argument action that loads the next page — `for … limit
	// shown more loadMore:`. It makes the list an infinite scroll: while rows were
	// cut off by `limit`, a "More" control follows the last row, and the client
	// fires the action as that control scrolls into view (a click does the same,
	// so the control works with no observer and from the keyboard). It requires
	// `limit`: without one nothing was held back, so there is nothing to load.
	More string
}

// For iterates an entity/list, rendering Body once per row with Var bound. It is
// the query/feed primitive; its header is the shared Range.
type For struct {
	Range
	Body []Node
}

// If renders Body only when Cond is truthy.
type If struct {
	Cond Expr
	Body []Node
}

// Input is a text control two-way bound to a client state cell. The placeholder
// is interpolated segments, so a reusable field component can be handed its hint.
type Input struct {
	Bind        string
	Placeholder []Seg
}

// Overlay is a modal layer shown while its bound boolean client cell is truthy:
// `overlay bind menuOpen:`. A backdrop dims the page; clicking it (or Escape) sets
// the cell false. Open it by setting the cell true from a client action.
type Overlay struct {
	Bind string
	Body []Node
}

// Typeahead is a text input that suggests existing values of an entity field as
// the actor types: `typeahead bind q from Tag.name`. It binds the chosen text to a
// client cell and offers a native completion list drawn from the collection.
type Typeahead struct {
	Bind        string
	Entity      string
	Field       string
	Placeholder []Seg
}

// Link is navigation to another page: `link "label" -> "/path"`. It renders an
// anchor; following it loads that page (server-rendered).
// Link is `link "Label" -> "/path"`. Both halves are interpolated segments, for
// the same reason `image` is: a link inside a `for` is a link *per row*, and a
// destination that cannot mention the row is a destination that can only ever be
// a fixed page. `link "{t.author}" -> "/profile/{t.handle}"` is what makes a feed
// navigable at all.
type Link struct {
	LabelSegs []Seg
	PathSegs  []Seg
}

// ── controls ─────────────────────────────────────────────────────────────────
//
// A control is the only thing in the language that may *write* a `@client` cell.
// That rule is deliberate: state has exactly one writer, and every other way to
// change a cell is an action the compiler placed. So the way to make a menu, a
// disclosure, a modal or a settings row expressible is not a new statement that
// assigns state — it is more controls, because a control bound to a bool cell is
// by definition a thing that flips it.
//
// Control is that one node. Every control travels the same path — parse, bind
// resolution, the `@client` placement rule, the cell-type check, the dependency
// edge that refreshes it, both renderers — and differs only in the row it has in
// the Controls table below. Adding the fifth control is adding a row and one
// rendering arm on each side; it is not adding a node type.
type Control struct {
	Kind        string   // the keyword the author wrote: textarea|checkbox|toggle|radio|password|newpassword
	Bind        string   // the @client cell this control reads and writes
	Label       []Seg    // checkbox/toggle: the words beside the box
	Placeholder []Seg    // textarea: the hint shown while it is empty
	Options     []Option // radio: the choices, exactly one of which the cell holds
	Line        int
}

func (Control) node() {}

// ControlSpec is one control's row: what it lowers to, what it may be bound to,
// and which parts of the control syntax it accepts.
type ControlSpec struct {
	IRKind  string // the IR node kind the renderers switch on
	Variant string // Node.Value: the variant within that kind ("switch"; a password's autocomplete token)
	Cell    string // the state type it must be bound to; "" = decided by its options
	Rule    string // how a wrong cell type is explained, in the author's words
	Options bool   // takes `option "Label" -> "value"` children
	Hint    bool   // takes a `placeholder "..."` modifier
	Labeled bool   // takes a `label "..."` modifier
}

// Controls is the whole set of two-way controls the language has beyond the two
// that predate this table (`input`, `select`, `typeahead`, `upload`). It is the
// single place a control is declared: the parser dispatches on it, the lowering
// checks against it, and both renderers key off the IRKind it names.
//
// `toggle` is deliberately NOT its own IR kind. A toggle and a checkbox have the
// same cell contract (a bool), the same event, the same hydration and the same
// refresh; they differ in a CSS class and an ARIA role, which is presentation.
// Shipping it as a second node kind would duplicate every one of those paths so
// that a stylesheet could tell them apart — so it is a variant of `checkbox`,
// carried as data, and the two renderers read that data in one place each.
var Controls = map[string]ControlSpec{
	"textarea": {
		IRKind: "textarea", Cell: "text", Hint: true,
		Rule: "a textarea edits a text cell",
	},
	"checkbox": {
		IRKind: "checkbox", Cell: "bool", Labeled: true,
		Rule: "a checkbox toggles a bool cell",
	},
	"toggle": {
		IRKind: "checkbox", Variant: "switch", Cell: "bool", Labeled: true,
		Rule: "a toggle flips a bool cell",
	},
	"radio": {
		IRKind: "radio", Options: true,
		Rule: "a radio group stores one of its option values, so it needs a text or enum cell",
	},
	// A password box is an `input` and nothing else: the same `text` cell, the
	// same `input` event, the same hydration and the same refresh. What it adds
	// is the two attributes the browser reads — `type="password"`, which is what
	// masks the characters and what makes the browser offer its own reveal, and
	// `autocomplete`, which is what a password manager reads to decide whether to
	// fill an existing secret or offer to generate a new one.
	//
	// Until this row there was no way to write either. `input` renders `<input>`
	// with no type, and a node has no attribute escape hatch, so the library's
	// PasswordField masked with `-webkit-text-security` — paint, not a password
	// input: no manager fill or save, no `autocomplete`, no browser reveal. Both
	// real apps rendered sign-in passwords through it.
	//
	// TWO KEYWORDS, ONE KIND, for the reason `toggle` and `checkbox` are two
	// keywords over one kind: they differ in one attribute the renderers write
	// and in nothing else. The attribute is the autocomplete token, and it cannot
	// be inferred — `current-password` on a sign-up box makes a manager fill the
	// account's existing password into a field for a new one and never offer to
	// generate, and `new-password` on a sign-in box stops it filling the saved
	// one. Which of the two a field is is the author's fact, so the author says
	// it by choosing the word, and the token itself is what travels to both
	// renderers, so neither holds a mapping the other could disagree with.
	"password": {
		IRKind: "input", Variant: "current-password", Cell: "text", Hint: true,
		Rule: "a password box edits a text cell",
	},
	"newpassword": {
		IRKind: "input", Variant: "new-password", Cell: "text", Hint: true,
		Rule: "a password box edits a text cell",
	},
}

// Select is a dropdown two-way bound to a client state cell, choosing among a
// fixed set of options (label → value). Bound to an enum cell, its options
// default to the enum's members.
type Select struct {
	Bind    string
	Options []Option
	Line    int
}

// Option is one entry of a select's or a radio group's choice list.
//
// Its two halves are checked differently, on purpose. The label is interpolated
// segments, like every other label in the language. The value is the identity the
// bound cell stores, and it is written one of two ways:
//
//	option "Draft" -> "draft"        Value: a literal, a compile-time identity
//	option "{c.name}" -> c.id        Val:   an expression, evaluated per render
//
// The literal form is what enum defaulting and the typo check rest on, and it
// keeps every guarantee it has always had. The expression form is what a choice
// list drawn from data needs, and its identity cannot exist until the render.
//
// From is what makes an entry repeat. With a range, the entry is not one option
// but one option per row of that collection, with Label and Val evaluated against
// the row — `for c in Category by name: option "{c.name}" -> c.id`. It is the
// same Range a `for` node carries, parsed by the same function, so a data-driven
// option list filters, orders and caps exactly the way a feed does.
type Option struct {
	Label []Seg
	Value string
	Val   Expr   // computed value; nil = the Value literal
	From  *Range // nil = one option; non-nil = one option per row of this range
	Line  int
}

// Form groups inputs and submits an action when its submit control fires —
// rendered as a real <form> so the browser's Enter-to-submit and accessibility
// come for free. Children are ordinary nodes (inputs, text); Submit is the
// label of the submit button and Action the server/client action it calls.
type Form struct {
	Action string
	Args   []Expr
	Submit []Seg
	Body   []Node
	Line   int // source line, so a diagnostic about an argument can point at it
}

// Upload posts a file to the authority and binds the resulting URL to a client
// state cell, so a view can show or store the uploaded media. The transfer and
// storage are runtime services; the language only names the target cell.
type Upload struct {
	Bind  string
	Label []Seg
}

// Use renders a component with arguments bound to its parameters:
// `use Card(post)`. It is the call site of a reusable view fragment.
//
// Body is the indented block written under the call — the children handed to the
// component, rendered where its `slot` sits. That is what makes a wrapper
// (a card, a panel, a stack) expressible as a component instead of as loose CSS
// a call site has to remember to apply. Passing a block to a component that has
// no `slot` is a compile error; it used to be silently discarded.
type Use struct {
	Name string
	Args []Expr
	Body []Node
	Line int
}

// Slot is the injection point where content from outside is rendered: the routed
// view's content inside a layout, or the caller's children inside a component. A
// layout has exactly one; a component has at most one.
type Slot struct{}

// SlotRef is a named injection point inside a wireframe frame: `slot feed`. The
// compiler composites the content of the facet(s) snapped into that socket here.
// Unlike a layout's single anonymous Slot, a frame has one SlotRef per socket.
type SlotRef struct{ Name string }

func (Modified) node()  {}
func (Box) node()       {}
func (Row) node()       {}
func (Text) node()      {}
func (Heading) node()   {}
func (Image) node()     {}
func (Icon) node()      {}
func (Video) node()     {}
func (Richtext) node()  {}
func (Badge) node()     {}
func (Tabs) node()      {}
func (Match) node()     {}
func (Button) node()    {}
func (For) node()       {}
func (If) node()        {}
func (Input) node()     {}
func (Overlay) node()   {}
func (Typeahead) node() {}
func (Link) node()      {}
func (Select) node()    {}
func (Form) node()      {}
func (Upload) node()    {}
func (Use) node()       {}
func (Slot) node()      {}
func (SlotRef) node()   {}

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

// ActState reads client-side reactive status. For an action: `pending(post)` (is a
// `post` call in flight?) → bool, `failed(post)` (the last error message, "" if
// none) → text. For a form (state) cell: `dirty(cell)` (does the cell differ from
// its value at page load?) → bool, `touched(cell)` (has the user edited its input
// yet?) → bool. All are reactive client values — a form shows a spinner, an inline
// error, or a "save" button only once something changed, with no wiring. On the
// server (first paint) they are false / "". The Action field holds the action name
// (pending/failed) or the cell name (dirty/touched).
type ActState struct {
	Op     string // pending | failed | dirty | touched
	Action string
}

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

// Agg is an aggregate over an entity collection. In its whole-collection form it
// is `count(Entity)` (row count) or `sum(Entity.field)` (numeric total). In its
// filtered form it ranges with an item variable and a predicate —
// `count(x in Entity where <cond>)` and `exists(x in Entity where <cond>)` (does
// any row match) — so a count/exists can be scoped to one row's relations (e.g.
// likes of a tweet, or whether the actor has liked it). It reads the collection,
// so it tracks a dependency on that entity (plus any state the filter reads).
type Agg struct {
	Op    string // count | sum | exists | avg | min | max
	Coll  string
	Field string // sum/avg/min/max, when the reduced value is a bare field of the row
	Var   string // item variable for the filtered form ("" = whole collection)
	Where Expr   // filter predicate over the item var + outer scope (nil = whole collection)

	// Sel is the value reduced over each row when it is more than one of its
	// columns: `sum(l.qty * l.unitPrice in CartLine where l.owner == actor)`.
	//
	// A cart subtotal is Σ qty × price, and until this field existed the language
	// could only sum a stored column — so the only subtotal a program could write
	// was one over a `lineTotal` column somebody had to remember to keep in step
	// with `qty` and `unitPrice`. That is the denormalisation this field exists to
	// delete: the total is computed from the two values it is a function of, every
	// time it is read.
	//
	// Field and Sel are exclusive: a bare field keeps Field so the reduce stays a
	// column read (and every program written before this stays byte-identical
	// through the compiler), and anything else lands here and is evaluated once
	// per row with Var bound to it.
	Sel Expr
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
func (ActState) expr()  {}
func (Get) expr()       {}
func (EntityGet) expr() {}
func (Agg) expr()       {}
func (Call) expr()      {}
func (Bin) expr()       {}
func (Un) expr()        {}
