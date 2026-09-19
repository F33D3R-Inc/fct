// Package parser turns the source indentation tree into an ast.App. The grammar
// is line-oriented: a header line plus its nested children. Expressions go
// through a precedence-climbing parser (expr.go).
package parser

import (
	"fmt"
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"facet/internal/ast"
	"facet/internal/source"
)

// Error is a parse error with a source line.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// Parse compiles source text to an ast.App. A file is exactly one `app`
// definition, optionally preceded by `import "..."` lines (resolved and merged
// by internal/compile). Imports must come before the `app` header.
func Parse(src string) (*ast.App, error) {
	roots, err := source.Parse(src)
	if err != nil {
		return nil, err
	}
	comments := source.Comments(src)
	var imports []string
	var appNode *source.Node
	for _, r := range roots {
		t := strings.TrimSpace(r.Line.Text)
		if t == "import" || strings.HasPrefix(t, "import ") {
			if appNode != nil {
				return nil, &Error{r.Line.No, "`import` must come before the `app` definition"}
			}
			if len(r.Children) > 0 {
				return nil, &Error{r.Line.No, "`import` takes no indented block"}
			}
			path, err := unquote(strings.TrimSpace(strings.TrimPrefix(t, "import")), r.Line.No)
			if err != nil {
				return nil, &Error{r.Line.No, `import needs a quoted path: import "posts.fct"`}
			}
			if path == "" {
				return nil, &Error{r.Line.No, "import path is empty"}
			}
			// Versions are not written in the source — one source of truth lives in
			// facet.lock, managed by the CLI. Reject an inline @version with a pointer.
			if strings.Contains(path, "@") {
				return nil, &Error{r.Line.No, "remove the @version from the import; pin it with `facet add <ref>@<version>`"}
			}
			imports = append(imports, path)
			continue
		}
		if appNode != nil {
			return nil, &Error{r.Line.No, "only one facet definition per file (app/playground/wireframe/ui/data)"}
		}
		appNode = r
	}
	if appNode == nil {
		return nil, &Error{0, "empty source: expected a facet definition (app/playground/wireframe/ui/data)"}
	}
	app, err := parseFacet(appNode, comments)
	if err != nil {
		return nil, err
	}
	app.Imports = imports
	return app, nil
}

// parseFacet dispatches on the facet kind keyword that opens a file. A plain
// `app` is the original self-contained graph; the typed kinds compose as bricks.
func parseFacet(n *source.Node, comments []source.Line) (*ast.App, error) {
	switch firstWord(n.Line.Text) {
	case "app":
		return parseApp(n, comments)
	case "playground":
		return parsePlayground(n, comments)
	case "wireframe":
		return parseWireframe(n, comments)
	case "ui":
		return parseUIData(n, "ui", comments)
	case "data":
		return parseUIData(n, "data", comments)
	default:
		return nil, &Error{n.Line.No, "file must start with `app`, `playground`, `wireframe`, `ui`, or `data`"}
	}
}

func parseApp(n *source.Node, comments []source.Line) (*ast.App, error) {
	name, ok := keyword(n.Line.Text, "app")
	if !ok {
		return nil, &Error{n.Line.No, "file must start with `app Name:`"}
	}
	name = strings.TrimSuffix(name, ":")
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid app name %q", name)}
	}
	app := &ast.App{Name: name, Kind: "app", Line: n.Line.No}
	for _, c := range n.Children {
		if err := parseDecl(app, c, comments); err != nil {
			return nil, err
		}
	}
	return app, nil
}

// parseDecl parses one body item shared by `app`, `ui`, and `data` facets — the
// full vocabulary of entities, state, logic, and UI. Kind-specific guards (e.g.
// a `ui` facet may not declare an entity) are applied by the caller via the
// returned facet's contents; here we only parse what is structurally a member.
func parseDecl(app *ast.App, c *source.Node, comments []source.Line) error {
	var err error
	// `private` marks a proc/view whose name is file-local: internal/compile
	// mangles it (and every same-file call to it) to a name unique to this
	// file before merging imported modules together, so two files that each
	// declare their own file-local helper under the same name never collide.
	// Stripped here so every case below (and the specific parseProc/parseView
	// that reads c.Line.Text directly) sees the declaration as if `private `
	// were never there; the flag itself is threaded onto the parsed node
	// right after a successful parse.
	private := false
	if strings.HasPrefix(c.Line.Text, "private ") {
		private = true
		c.Line.Text = strings.TrimPrefix(c.Line.Text, "private ")
	}
	if private && !strings.HasPrefix(c.Line.Text, "proc ") && !strings.HasPrefix(c.Line.Text, "view ") {
		return &Error{c.Line.No, fmt.Sprintf("`private` is only supported on proc and view declarations, not %q", firstWord(c.Line.Text))}
	}
	switch {
	case c.Line.Text == "auth" || c.Line.Text == "auth:":
		app.Auth = true
	case strings.HasPrefix(c.Line.Text, "entity "):
		var e *ast.Entity
		if e, err = parseEntity(c); err == nil {
			app.Entities = append(app.Entities, e)
		}
	case strings.HasPrefix(c.Line.Text, "record "):
		var rc *ast.Record
		if rc, err = parseRecord(c); err == nil {
			app.Records = append(app.Records, rc)
		}
	case strings.HasPrefix(c.Line.Text, "struct "):
		var sc *ast.Struct
		if sc, err = parseStruct(c); err == nil {
			app.Structs = append(app.Structs, sc)
		}
	case strings.HasPrefix(c.Line.Text, "enum "):
		var en *ast.Enum
		if en, err = parseEnum(c); err == nil {
			app.Enums = append(app.Enums, en)
		}
	case strings.HasPrefix(c.Line.Text, "type "):
		var ty *ast.Type
		if ty, err = parseType(c); err == nil {
			app.Types = append(app.Types, ty)
		}
	case strings.HasPrefix(c.Line.Text, "message "):
		var ms *ast.Message
		if ms, err = parseMessage(c); err == nil {
			app.Messages = append(app.Messages, ms)
		}
	case strings.HasPrefix(c.Line.Text, "component "):
		var cm *ast.Component
		if cm, err = parseComponent(c); err == nil {
			app.Components = append(app.Components, cm)
		}
	case strings.HasPrefix(c.Line.Text, "layout "):
		var ly *ast.Layout
		if ly, err = parseLayout(c); err == nil {
			app.Layouts = append(app.Layouts, ly)
		}
	case c.Line.Text == "theme dark:" || c.Line.Text == "theme dark":
		var tv []ast.ThemeVar
		if tv, err = parseTheme(c); err == nil {
			app.DarkTheme = append(app.DarkTheme, tv...)
		}
	case c.Line.Text == "theme:" || c.Line.Text == "theme":
		var tv []ast.ThemeVar
		if tv, err = parseTheme(c); err == nil {
			app.Theme = append(app.Theme, tv...)
		}
	case strings.HasPrefix(c.Line.Text, "theme "):
		var nt ast.NamedTheme
		if nt, err = parseNamedTheme(c); err == nil {
			app.Themes = append(app.Themes, nt)
		}
	case c.Line.Text == "css:" || c.Line.Text == "css":
		var css string
		if css, err = parseCSS(c, comments); err == nil {
			app.CSS = joinCSS(app.CSS, css)
		}
	case strings.HasPrefix(c.Line.Text, "css from "):
		var path string
		if path, err = parseCSSFrom(c.Line.Text, c.Line.No); err == nil {
			app.CSSFiles = append(app.CSSFiles, ast.CSSFile{Path: path, Line: c.Line.No})
		}
	case strings.HasPrefix(c.Line.Text, "state "):
		var s *ast.State
		if s, err = parseState(c); err == nil {
			app.States = append(app.States, s)
		}
	case strings.HasPrefix(c.Line.Text, "derive "):
		var d *ast.Derive
		if d, err = parseDerive(c); err == nil {
			app.Derives = append(app.Derives, d)
		}
	case strings.HasPrefix(c.Line.Text, "policy "):
		var p *ast.Policy
		if p, err = parsePolicy(c); err == nil {
			app.Policies = append(app.Policies, p)
		}
	case strings.HasPrefix(c.Line.Text, "action "):
		var a *ast.Action
		if a, err = parseAction(c); err == nil {
			app.Actions = append(app.Actions, a)
		}
	case strings.HasPrefix(c.Line.Text, "proc "):
		var p *ast.Proc
		if p, err = parseProc(c); err == nil {
			p.Private = private
			app.Procs = append(app.Procs, p)
		}
	case strings.HasPrefix(c.Line.Text, "job "):
		var j *ast.Job
		if j, err = parseJob(c); err == nil {
			app.Jobs = append(app.Jobs, j)
		}
	case strings.HasPrefix(c.Line.Text, "daemon "):
		var d *ast.Daemon
		if d, err = parseDaemon(c); err == nil {
			app.Daemons = append(app.Daemons, d)
		}
	case strings.HasPrefix(c.Line.Text, "service "):
		var sv *ast.Service
		if sv, err = parseService(c); err == nil {
			app.Services = append(app.Services, sv)
		}
	case strings.HasPrefix(c.Line.Text, "file "):
		var f *ast.File
		if f, err = parseFile(c); err == nil {
			app.Files = append(app.Files, f)
		}
	case strings.HasPrefix(c.Line.Text, "webhook "):
		var wh *ast.Webhook
		if wh, err = parseWebhook(c.Line.Text, c.Line.No); err == nil {
			app.Webhooks = append(app.Webhooks, wh)
		}
	case strings.HasPrefix(c.Line.Text, "on "):
		var tr *ast.Trigger
		if tr, err = parseTrigger(c.Line.Text, c.Line.No); err == nil {
			app.Triggers = append(app.Triggers, tr)
		}
	case strings.HasPrefix(c.Line.Text, "view "):
		var v *ast.View
		if v, err = parseView(c); err == nil {
			v.Private = private
			app.Views = append(app.Views, v)
		}
	default:
		err = &Error{c.Line.No, fmt.Sprintf("unexpected %q; expected entity/record/struct/enum/type/message/state/derive/policy/action/proc/job/daemon/service/file/webhook/component/layout/theme/view", firstWord(c.Line.Text))}
	}
	return err
}

// parsePlayground parses `playground Name:` — the baseplate. It holds global
// concerns (auth, theme) and mounts exactly one wireframe; it accepts nothing
// else, because a playground only takes a wireframe.
func parsePlayground(n *source.Node, comments []source.Line) (*ast.App, error) {
	name := strings.TrimSuffix(keywordRest(n.Line.Text, "playground"), ":")
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid playground name %q", name)}
	}
	app := &ast.App{Name: name, Kind: "playground", Line: n.Line.No}
	for _, c := range n.Children {
		t := strings.TrimSpace(c.Line.Text)
		switch {
		case t == "auth" || t == "auth:":
			app.Auth = true
		case t == "theme dark:" || t == "theme dark":
			tv, err := parseTheme(c)
			if err != nil {
				return nil, err
			}
			app.DarkTheme = append(app.DarkTheme, tv...)
		case t == "theme:" || t == "theme":
			tv, err := parseTheme(c)
			if err != nil {
				return nil, err
			}
			app.Theme = append(app.Theme, tv...)
		case strings.HasPrefix(t, "theme "):
			nt, err := parseNamedTheme(c)
			if err != nil {
				return nil, err
			}
			app.Themes = append(app.Themes, nt)
		case t == "css:" || t == "css":
			css, err := parseCSS(c, comments)
			if err != nil {
				return nil, err
			}
			app.CSS = joinCSS(app.CSS, css)
		case strings.HasPrefix(t, "css from "):
			path, err := parseCSSFrom(t, c.Line.No)
			if err != nil {
				return nil, err
			}
			app.CSSFiles = append(app.CSSFiles, ast.CSSFile{Path: path, Line: c.Line.No})
		case strings.HasPrefix(t, "mount "):
			m, err := parseMount(t, c.Line.No)
			if err != nil {
				return nil, err
			}
			app.Mounts = append(app.Mounts, m)
		default:
			return nil, &Error{c.Line.No, fmt.Sprintf("unexpected %q in playground; a playground takes `auth`, `theme`, and `mount <Wireframe> [at \"/path\"] [requires <policy>]`", firstWord(t))}
		}
	}
	if len(app.Mounts) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("playground %q must `mount <Wireframe>`", name)}
	}
	return app, nil
}

// parseMount parses one screen the playground mounts:
//
//	mount Shell                         # one screen at "/"
//	mount Auth  at "/login"             # at a route
//	mount Shell at "/" requires member  # at a route, behind a guard
//
// `at` (if present) precedes `requires`. A missing path defaults to "/".
func parseMount(t string, line int) (ast.Mount, error) {
	rest := strings.TrimSpace(t[len("mount "):])
	requires := ""
	if i := strings.Index(rest, " requires "); i >= 0 {
		requires = strings.TrimSuffix(strings.TrimSpace(rest[i+len(" requires "):]), ":")
		rest = strings.TrimSpace(rest[:i])
		if !isIdent(requires) {
			return ast.Mount{}, &Error{line, fmt.Sprintf("invalid guard policy %q after `requires`", requires)}
		}
	}
	path := "/"
	if i := strings.Index(rest, " at "); i >= 0 {
		p, err := unquote(strings.TrimSpace(rest[i+len(" at "):]), line)
		if err != nil || p == "" {
			return ast.Mount{}, &Error{line, "mount route must be a quoted path: `mount W at \"/path\"`"}
		}
		path = p
		rest = strings.TrimSpace(rest[:i])
	}
	w := strings.TrimSuffix(strings.TrimSpace(rest), ":")
	if !isIdent(w) {
		return ast.Mount{}, &Error{line, fmt.Sprintf("invalid wireframe name %q after `mount`", w)}
	}
	return ast.Mount{Wireframe: w, Path: path, Requires: requires, Line: line}, nil
}

// parseWireframe parses `wireframe Name:` — the structural brick. It declares
// typed `socket`s and a `frame:` layout that places each socket with `slot
// <name>`. It is pure structure: no data, no behavior.
func parseWireframe(n *source.Node, comments []source.Line) (*ast.App, error) {
	name := strings.TrimSuffix(keywordRest(n.Line.Text, "wireframe"), ":")
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid wireframe name %q", name)}
	}
	app := &ast.App{Name: name, Kind: "wireframe", Line: n.Line.No}
	for _, c := range n.Children {
		t := strings.TrimSpace(c.Line.Text)
		switch {
		case t == "theme dark:" || t == "theme dark":
			tv, err := parseTheme(c)
			if err != nil {
				return nil, err
			}
			app.DarkTheme = append(app.DarkTheme, tv...)
		case t == "theme:" || t == "theme":
			tv, err := parseTheme(c)
			if err != nil {
				return nil, err
			}
			app.Theme = append(app.Theme, tv...)
		case strings.HasPrefix(t, "theme "):
			nt, err := parseNamedTheme(c)
			if err != nil {
				return nil, err
			}
			app.Themes = append(app.Themes, nt)
		case t == "css:" || t == "css":
			css, err := parseCSS(c, comments)
			if err != nil {
				return nil, err
			}
			app.CSS = joinCSS(app.CSS, css)
		case strings.HasPrefix(t, "css from "):
			path, err := parseCSSFrom(t, c.Line.No)
			if err != nil {
				return nil, err
			}
			app.CSSFiles = append(app.CSSFiles, ast.CSSFile{Path: path, Line: c.Line.No})
		case strings.HasPrefix(t, "socket "):
			sock, err := parseSocket(c)
			if err != nil {
				return nil, err
			}
			app.Sockets = append(app.Sockets, sock)
		case t == "frame:" || t == "frame":
			if app.Frame != nil {
				return nil, &Error{c.Line.No, "a wireframe has one `frame`"}
			}
			nodes, err := parseNodes(c.Children)
			if err != nil {
				return nil, err
			}
			if nodes == nil {
				nodes = []ast.Node{}
			}
			app.Frame = nodes
		default:
			return nil, &Error{c.Line.No, fmt.Sprintf("unexpected %q in wireframe; a wireframe takes `socket <name>: <ui|data>`, `frame:`, and `theme:`", firstWord(t))}
		}
	}
	if app.Frame == nil {
		return nil, &Error{n.Line.No, fmt.Sprintf("wireframe %q needs a `frame:` block", name)}
	}
	return app, nil
}

// parseSocket parses `socket feed: data` — a typed slot. Accept is the facet
// kind (`ui` or `data`) the socket admits.
func parseSocket(n *source.Node) (ast.Socket, error) {
	rest := strings.TrimSpace(n.Line.Text[len("socket "):])
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return ast.Socket{}, &Error{n.Line.No, "socket needs a kind: `socket <name>: <ui|data>`"}
	}
	sname := strings.TrimSpace(rest[:colon])
	accept := strings.TrimSpace(rest[colon+1:])
	if !isIdent(sname) {
		return ast.Socket{}, &Error{n.Line.No, fmt.Sprintf("invalid socket name %q", sname)}
	}
	if accept != "ui" && accept != "data" {
		return ast.Socket{}, &Error{n.Line.No, fmt.Sprintf("socket %q must accept `ui` or `data`, got %q", sname, accept)}
	}
	return ast.Socket{Name: sname, Accept: accept, Line: n.Line.No}, nil
}

// parseUIData parses `ui Name in socket:` / `data Name in socket:` — a content
// brick that snaps into a wireframe socket. A `ui` facet carries skin and
// presentation; a `data` facet carries entities, logic, and its own content.
// Both contribute a `content:` node tree placed at the socket, and may declare
// routed `view`s — the other screens their slice serves, each rendered in the
// same wireframe with this socket holding the view.
func parseUIData(n *source.Node, kind string, comments []source.Line) (*ast.App, error) {
	rest := keywordRest(n.Line.Text, kind)
	rest = strings.TrimSuffix(strings.TrimSpace(rest), ":")
	// `Name in socket`
	parts := strings.Fields(rest)
	if len(parts) != 3 || parts[1] != "in" {
		return nil, &Error{n.Line.No, fmt.Sprintf("%s facet header must read `%s Name in <socket>:`", kind, kind)}
	}
	name, socket := parts[0], parts[2]
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid %s facet name %q", kind, name)}
	}
	if !isIdent(socket) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid socket name %q", socket)}
	}
	app := &ast.App{Name: name, Kind: kind, Into: socket, Line: n.Line.No}
	for _, c := range n.Children {
		t := strings.TrimSpace(c.Line.Text)
		if t == "content:" || t == "content" {
			if app.Content != nil {
				return nil, &Error{c.Line.No, fmt.Sprintf("%s facet %q has one `content` block", kind, name)}
			}
			nodes, err := parseNodes(c.Children)
			if err != nil {
				return nil, err
			}
			if nodes == nil {
				nodes = []ast.Node{}
			}
			app.Content = nodes
			continue
		}
		if err := parseDecl(app, c, comments); err != nil {
			return nil, err
		}
	}
	if app.Content == nil {
		return nil, &Error{n.Line.No, fmt.Sprintf("%s facet %q needs a `content:` block to fill socket %q", kind, name, socket)}
	}
	if kind == "ui" && len(app.Entities) > 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("ui facet %q may not declare an entity — move durable data into a `data` facet", name)}
	}
	// A brick's `view`s are the other routes its slice serves. `content:` is what
	// the socket holds on every screen; a `view` is one more screen of the same
	// wireframe, with this socket holding the view instead. That is the layered
	// spelling of the plain track's `view X in Shell` — the wireframe is the
	// chrome, so a brick view names no layout and needs an explicit route (the
	// playground's mounts already own the defaulted ones).
	for _, v := range app.Views {
		if v.Layout != "" {
			return nil, &Error{v.Line, fmt.Sprintf("%s facet %q: view %q may not name a layout — the wireframe that owns socket %q is this screen's chrome", kind, name, v.Name, socket)}
		}
		if v.Path == "" {
			return nil, &Error{v.Line, fmt.Sprintf("%s facet %q: view %q needs an explicit route — write `view %s at \"/path\":`", kind, name, v.Name, v.Name)}
		}
	}
	return app, nil
}

// keywordRest returns the text after a leading keyword (the keyword is assumed
// present; parseFacet has already matched it).
func keywordRest(s, kw string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), kw))
}

func parseEntity(n *source.Node) (*ast.Entity, error) {
	name := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "entity")), ":")
	// `entity Post @softdelete:` — a `remove` archives instead of dropping.
	softDelete := false
	if strings.HasSuffix(name, "@softdelete") {
		softDelete = true
		name = strings.TrimSpace(strings.TrimSuffix(name, "@softdelete"))
	}
	// `entity Position @ephemeral:` — never durable; see ast.Entity.Ephemeral.
	// Checked independently of @softdelete (not else-if) so either can precede
	// the other in source, though combining them has no real use.
	ephemeral := false
	if strings.HasSuffix(name, "@ephemeral") {
		ephemeral = true
		name = strings.TrimSpace(strings.TrimSuffix(name, "@ephemeral"))
	}
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("entity name %q must be capitalized", name)}
	}
	e := &ast.Entity{Name: name, SoftDelete: softDelete, Ephemeral: ephemeral, Line: n.Line.No}
	// A `read:` line is the entity's row-level read policy, not a field — but it
	// is written exactly like one (`read: <expr>`), so it is told apart the same
	// way a real `read: bool` field (facets/home.fct's Notification.read, a
	// "has this been seen" flag) is kept a field: `ft` is tried as a type first,
	// and only a value that fails as a type name — "published || author ==
	// actor" is never a legal type token — falls through to expression parsing.
	// One `read:` per entity; collected here and resolved after the field loop
	// so it may appear before the fields it references.
	var readRaw ast.Expr
	var readLine int
	// `derive name: Type = expr` lines are collected raw here, exactly as
	// `read:` is above, and qualified (qualifyRowRefs) after the field loop so
	// a derive may reference a field declared later in the same entity — see
	// ast.Entity.Derives. Told apart from a real field the same way `read:` is:
	// parseDerive is reused as-is (the top-level and entity-embedded forms are
	// the same grammar), so this is checked before the generic `name: type`
	// split below ever sees the line (a derive's own name contains no colon,
	// but its declaration line does, right after "derive <name>").
	var rawDerives []*ast.Derive
	for _, c := range n.Children {
		if strings.HasPrefix(strings.TrimSpace(c.Line.Text), "derive ") {
			d, err := parseDerive(c)
			if err != nil {
				return nil, err
			}
			rawDerives = append(rawDerives, d)
			continue
		}
		colon := strings.IndexByte(c.Line.Text, ':')
		if colon < 0 {
			return nil, &Error{c.Line.No, "entity field must be `name: type`"}
		}
		fn := strings.TrimSpace(c.Line.Text[:colon])
		ft := strings.TrimSpace(c.Line.Text[colon+1:])
		if !isIdent(fn) {
			return nil, &Error{c.Line.No, fmt.Sprintf("invalid field name %q", fn)}
		}
		// Field modifiers, in any order. Crypto: `@secret` (at-rest), `@e2e` (sealed).
		// Projection: `@requires(policy)`. Declarative constraints, enforced by the
		// authority on every write: `@unique`, `@required`, `@min(n)`, `@max(n)`,
		// `@matches("regex")`. They are stripped from the type token wherever they sit.
		secret, e2e, unique, required, restrict, setNull := false, false, false, false, false, false
		var fmin, fmax *int
		matches, readPolicy := "", ""
		// `@matches("…")` first — its argument is a quoted string that may hold parens.
		if i := strings.Index(ft, "@matches("); i >= 0 {
			rest := ft[i+len("@matches("):]
			open := strings.IndexByte(rest, '"')
			if open < 0 {
				return nil, &Error{c.Line.No, `@matches needs a quoted pattern: @matches("^[a-z]+$")`}
			}
			closeQ := strings.IndexByte(rest[open+1:], '"')
			if closeQ < 0 {
				return nil, &Error{c.Line.No, `@matches pattern is not closed: @matches("…")`}
			}
			matches = rest[open+1 : open+1+closeQ]
			after := rest[open+1+closeQ+1:]
			if p := strings.IndexByte(after, ')'); p >= 0 {
				after = after[p+1:]
			}
			ft = strings.TrimSpace(ft[:i] + " " + after)
		}
		// `@min(n)` / `@max(n)` — integer bounds.
		for _, m := range []struct {
			name string
			dst  **int
		}{{"@min(", &fmin}, {"@max(", &fmax}} {
			if i := strings.Index(ft, m.name); i >= 0 {
				rest := ft[i+len(m.name):]
				closeP := strings.IndexByte(rest, ')')
				if closeP < 0 {
					return nil, &Error{c.Line.No, fmt.Sprintf("%sn) is not closed", m.name)}
				}
				n, err := strconv.Atoi(strings.TrimSpace(rest[:closeP]))
				if err != nil {
					return nil, &Error{c.Line.No, fmt.Sprintf("%sn) needs an integer", m.name)}
				}
				v := n
				*m.dst = &v
				ft = strings.TrimSpace(ft[:i] + " " + rest[closeP+1:])
			}
		}
		// `@requires(policy)` — the field read-gate.
		if i := strings.Index(ft, "@requires("); i >= 0 {
			rest := ft[i+len("@requires("):]
			closeP := strings.IndexByte(rest, ')')
			if closeP < 0 {
				return nil, &Error{c.Line.No, "field @requires needs a policy: @requires(policyName)"}
			}
			readPolicy = strings.TrimSpace(rest[:closeP])
			if !isIdent(readPolicy) {
				return nil, &Error{c.Line.No, fmt.Sprintf("invalid field policy %q in @requires(...)", readPolicy)}
			}
			ft = strings.TrimSpace(ft[:i] + " " + rest[closeP+1:])
		}
		// Bare flag markers, any order. `@restrict`/`@setNull` govern a relation
		// field's on-delete behavior (ast.EntityField.OnDelete); unset means the
		// historical, sole behavior, cascade.
		for _, m := range []struct {
			name string
			dst  *bool
		}{{"@secret", &secret}, {"@e2e", &e2e}, {"@unique", &unique}, {"@required", &required},
			{"@restrict", &restrict}, {"@setNull", &setNull}} {
			if i := strings.Index(ft, m.name); i >= 0 {
				*m.dst = true
				ft = strings.TrimSpace(ft[:i] + " " + ft[i+len(m.name):])
			}
		}
		// A trailing `?` makes the column nullable; a `[T]` list is not a column type.
		core, list, optional := splitType(ft)
		if list {
			return nil, &Error{c.Line.No, fmt.Sprintf("entity field %q cannot be a list; model a one-to-many with a relation", fn)}
		}
		// A field type is a primitive, an enum, or an entity name (a relation, stored
		// as the referenced row's id). Enum/entity existence is validated in the IR.
		if !isTypeName(core) {
			if fn == "read" {
				if readRaw != nil {
					return nil, &Error{c.Line.No, fmt.Sprintf("entity %q already has a read: clause (first at line %d)", name, readLine)}
				}
				expr, err := parseExpr(ft, c.Line.No)
				if err != nil {
					return nil, err
				}
				readRaw, readLine = expr, c.Line.No
				continue
			}
			return nil, &Error{c.Line.No, fmt.Sprintf("unknown type %q (use int, text, bool, money, date, an enum, or an entity name)", core)}
		}
		if e2e && secret {
			return nil, &Error{c.Line.No, fmt.Sprintf("field %q cannot be both @secret and @e2e — @secret is server-side at-rest encryption (the authority holds plaintext), @e2e is end-to-end (the authority never sees plaintext); pick one", fn)}
		}
		if e2e && core != "text" {
			return nil, &Error{c.Line.No, fmt.Sprintf("@e2e field %q must be text — a sealed value is opaque ciphertext, so it can't be a typed/queryable column", fn)}
		}
		if matches != "" && core != "text" {
			return nil, &Error{c.Line.No, fmt.Sprintf("@matches on field %q applies to text, not %s", fn, core)}
		}
		if restrict && setNull {
			return nil, &Error{c.Line.No, fmt.Sprintf("field %q cannot be both @restrict and @setNull — @restrict refuses the delete while a reference remains, @setNull clears this field instead; pick one", fn)}
		}
		onDelete := ""
		switch {
		case restrict:
			onDelete = "restrict"
		case setNull:
			onDelete = "setNull"
		}
		// A `?` on the type makes the column nullable; @setNull needs that to have
		// anything to set the column to once the row it pointed at is gone. Whether
		// the field is actually a relation (vs. a primitive/enum, where an on-delete
		// modifier makes no sense at all) is checked later, once every entity name is
		// known (internal/ir/build.go).
		if setNull && !optional {
			return nil, &Error{c.Line.No, fmt.Sprintf("field %q is @setNull but not optional — write `%s: %s?` so the column has a null to hold once the referenced row is deleted", fn, fn, core)}
		}
		e.Fields = append(e.Fields, ast.EntityField{Name: fn, Type: core, Secret: secret, E2E: e2e, ReadPolicy: readPolicy, Optional: optional,
			Unique: unique, Required: required, Min: fmin, Max: fmax, Matches: matches, OnDelete: onDelete, Line: c.Line.No})
	}
	if len(e.Fields) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("entity %q has no fields", name)}
	}
	if readRaw != nil || len(rawDerives) > 0 {
		fields := make(map[string]bool, len(e.Fields))
		for _, f := range e.Fields {
			fields[f.Name] = true
		}
		if readRaw != nil {
			e.Read = qualifyRowRefs(readRaw, fields)
		}
		seen := map[string]int{}
		for _, d := range rawDerives {
			if fields[d.Name] {
				return nil, &Error{d.Line, fmt.Sprintf("entity %q already has a field %q; derive %q needs a different name", name, d.Name, d.Name)}
			}
			if prev, ok := seen[d.Name]; ok {
				return nil, &Error{d.Line, fmt.Sprintf("entity %q's derive %q redeclared (first at line %d)", name, d.Name, prev)}
			}
			seen[d.Name] = d.Line
			dd := *d
			dd.Expr = qualifyRowRefs(d.Expr, fields)
			e.Derives = append(e.Derives, &dd)
		}
	}
	return e, nil
}

// qualifyRowRefs rewrites every bare `ast.Ref` in ex that names one of this
// entity's own fields into `Get{Ref{"$row"}, name}` — the shape a `where`
// clause's `p.name` already has, over the reserved row variable a `read:`
// clause implies rather than spells (see ast.Entity.Read). Anything else (most
// importantly `Ref{"actor"}`) is left alone: `actor` is not a field, so it
// never matches, and stays a bare reference exactly as a hand-written `where
// p.author == actor` already treats it.
//
// This runs once, at parse time, on the raw expression `read:` parsed — before
// anything downstream (the IR builder, the query sites that fold this in) ever
// sees it, so every one of them can treat a non-nil Entity.Read as an ordinary
// row predicate and needs no special case for the fact that its author wrote
// it with no row variable at all.
func qualifyRowRefs(ex ast.Expr, fields map[string]bool) ast.Expr {
	switch t := ex.(type) {
	case ast.Ref:
		if fields[t.Name] {
			return ast.Get{Obj: ast.Ref{Name: "$row"}, Field: t.Name}
		}
		return t
	case ast.Get:
		return ast.Get{Obj: qualifyRowRefs(t.Obj, fields), Field: t.Field}
	case ast.EntityGet:
		return ast.EntityGet{Entity: t.Entity, Key: qualifyRowRefs(t.Key, fields), Field: t.Field}
	case ast.Agg:
		t.Where = qualifyRowRefs(t.Where, fields)
		t.Sel = qualifyRowRefs(t.Sel, fields)
		return t
	case ast.Call:
		args := make([]ast.Expr, len(t.Args))
		for i, a := range t.Args {
			args[i] = qualifyRowRefs(a, fields)
		}
		return ast.Call{Name: t.Name, Args: args}
	case ast.ListLit:
		elems := make([]ast.Expr, len(t.Elems))
		for i, el := range t.Elems {
			elems[i] = qualifyRowRefs(el, fields)
		}
		return ast.ListLit{Elems: elems}
	case ast.Bin:
		return ast.Bin{Op: t.Op, L: qualifyRowRefs(t.L, fields), R: qualifyRowRefs(t.R, fields)}
	case ast.Un:
		return ast.Un{Op: t.Op, X: qualifyRowRefs(t.X, fields)}
	default:
		// Lit, ActState, nil: nothing to qualify.
		return ex
	}
}

// parseEnum: `enum Name: a, b, c` (members on the header) or each member on its
// own indented line.
func parseEnum(n *source.Node) (*ast.Enum, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "enum")), ":")
	name := head
	var inline string
	if colon := strings.IndexByte(head, ':'); colon >= 0 {
		name = strings.TrimSpace(head[:colon])
		inline = strings.TrimSpace(head[colon+1:])
	}
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("enum name %q must be capitalized", name)}
	}
	en := &ast.Enum{Name: name, Line: n.Line.No}
	add := func(v string) error {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		// Optional wire-rename escape hatch: `private as "Private"` fixes
		// codegen's serialized form without touching the value's own
		// lowercase identity used everywhere else in the language (see
		// ast.Enum.WireNames's doc comment for why this exists).
		wire := ""
		if idx := strings.Index(v, " as "); idx >= 0 {
			wire = strings.TrimSpace(v[idx+len(" as "):])
			v = strings.TrimSpace(v[:idx])
			if len(wire) < 2 || wire[0] != '"' || wire[len(wire)-1] != '"' {
				return &Error{n.Line.No, fmt.Sprintf("enum value %q's wire name must be a quoted string, e.g. %s as \"Wire\"", v, v)}
			}
			wire = wire[1 : len(wire)-1]
			if wire == "" {
				return &Error{n.Line.No, fmt.Sprintf("enum value %q's wire name must not be empty", v)}
			}
		}
		if !isIdent(v) {
			return &Error{n.Line.No, fmt.Sprintf("enum value %q must be an identifier", v)}
		}
		if wire == "" {
			wire = v
		}
		en.Values = append(en.Values, v)
		en.WireNames = append(en.WireNames, wire)
		return nil
	}
	if inline != "" {
		for _, v := range strings.Split(inline, ",") {
			if err := add(v); err != nil {
				return nil, err
			}
		}
	}
	for _, c := range n.Children {
		for _, v := range strings.Split(c.Line.Text, ",") {
			if err := add(v); err != nil {
				return nil, err
			}
		}
	}
	if len(en.Values) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("enum %q has no values", name)}
	}
	return en, nil
}

// parseRecord: `record Verdict: score: int, reasons: [text], ok: bool` (fields on
// the header) or each `name: type` on its own indented line. A field type may be a
// list (`[T]`) or optional (`T?`) — unlike an entity column, a record field can be
// a list, since a record is in-flight data, not a stored row.
func parseRecord(n *source.Node) (*ast.Record, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "record")), ":")
	name := head
	var inline string
	if colon := strings.IndexByte(head, ':'); colon >= 0 {
		name = strings.TrimSpace(head[:colon])
		inline = strings.TrimSpace(head[colon+1:])
	}
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("record name %q must be capitalized", name)}
	}
	r := &ast.Record{Name: name, Line: n.Line.No}
	add := func(spec string, line int) error {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return nil
		}
		colon := strings.IndexByte(spec, ':')
		if colon < 0 {
			return &Error{line, fmt.Sprintf("record field %q must be `name: type`", spec)}
		}
		fn := strings.TrimSpace(spec[:colon])
		ft := strings.TrimSpace(spec[colon+1:])
		if !isIdent(fn) {
			return &Error{line, fmt.Sprintf("invalid record field name %q", fn)}
		}
		core, list, optional := splitType(ft)
		if !isTypeName(core) {
			return &Error{line, fmt.Sprintf("unknown type %q in record field %q (use a primitive, an enum, or a list of those)", core, fn)}
		}
		r.Fields = append(r.Fields, ast.RecordField{Name: fn, Type: core, List: list, Optional: optional, Line: line})
		return nil
	}
	if inline != "" {
		for _, f := range strings.Split(inline, ",") {
			if err := add(f, n.Line.No); err != nil {
				return nil, err
			}
		}
	}
	for _, c := range n.Children {
		if err := add(c.Line.Text, c.Line.No); err != nil {
			return nil, err
		}
	}
	if len(r.Fields) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("record %q has no fields", name)}
	}
	return r, nil
}

// parseStruct: `struct Node: kind: text, op: text` (fields on the header) or
// each `name: type` on its own indented line — the same grammar parseRecord
// uses. Unlike a record field, a struct field's type may be a list (`[T]`,
// already true for a record too) AND may name another struct (or itself) —
// ast.Struct's doc explains why that composability is the entire point.
// Optional field types (`T?`) are refused: a struct is constructed whole, by
// a literal that must set every field (internal/ir/build.go's
// checkStructFieldTypes), so there is no "field may be absent" story yet —
// the same reason parseRecord accepts one but a struct field does not need
// to, and keeping it out now avoids having to decide its literal/zero-value
// story later.
func parseStruct(n *source.Node) (*ast.Struct, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "struct")), ":")
	name := head
	var inline string
	if colon := strings.IndexByte(head, ':'); colon >= 0 {
		name = strings.TrimSpace(head[:colon])
		inline = strings.TrimSpace(head[colon+1:])
	}
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("struct name %q must be capitalized", name)}
	}
	st := &ast.Struct{Name: name, Line: n.Line.No}
	add := func(spec string, line int) error {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return nil
		}
		colon := strings.IndexByte(spec, ':')
		if colon < 0 {
			return &Error{line, fmt.Sprintf("struct field %q must be `name: type`", spec)}
		}
		fn := strings.TrimSpace(spec[:colon])
		ft := strings.TrimSpace(spec[colon+1:])
		if !isIdent(fn) {
			return &Error{line, fmt.Sprintf("invalid struct field name %q", fn)}
		}
		core, list, optional := splitType(ft)
		if optional {
			return &Error{line, fmt.Sprintf("struct field %q cannot be optional (?) — a struct literal must set every field", fn)}
		}
		if core == "money" || core == "date" {
			return &Error{line, fmt.Sprintf("struct field %q: %s is not usable inside a proc's own struct type — use int/text/bool/float, another struct, or a list of those", fn, core)}
		}
		if !isTypeName(core) {
			return &Error{line, fmt.Sprintf("unknown type %q in struct field %q (use int/text/bool/float, another struct, or a list of those)", core, fn)}
		}
		st.Fields = append(st.Fields, ast.StructField{Name: fn, Type: core, List: list, Line: line})
		return nil
	}
	if inline != "" {
		for _, f := range strings.Split(inline, ",") {
			if err := add(f, n.Line.No); err != nil {
				return nil, err
			}
		}
	}
	for _, c := range n.Children {
		if err := add(c.Line.Text, c.Line.No); err != nil {
			return nil, err
		}
	}
	if len(st.Fields) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("struct %q has no fields", name)}
	}
	return st, nil
}

// isWireTypeName is isTypeName plus two wire-only primitives with no
// equivalent in the entity/record/action type system: `json` (an opaque
// value, for genuinely untyped payloads like a predicate literal or a Node's
// opaque `data`) and `number` (a float, for a real wire field FCT's `int`
// cannot represent without losing precision — e.g. TxOp.set_if's `expect_le`,
// which is `Option<f64>` in the real hand-written type because a lease
// deadline is a Unix-seconds float, not necessarily whole). Scoped to
// `type`/`message` fields only, deliberately not folded into isTypeName: the
// rest of the language has no entity/action use for either.
func isWireTypeName(s string) bool { return s == "json" || s == "number" || isTypeName(s) }

// splitWireDefault splits a wire-schema field's type text on a trailing
// `= literal` clause (`count: int = 1`, `item_var: text = "item"`,
// `edges: [EdgeSpec] = []`, `set: json = {}`) — the default-value escape
// hatch for the rare real field that has an actual non-zero-or-nontrivial
// default (facetql's SequenceRequest.count, several query types' item_var,
// CreateNodeRequest's `#[serde(default)]` empty edges list, TxOp.set_if's
// `#[serde(default)]` empty data map), not just "may be absent" (which `?`
// already covers — a defaulted field always resolves to a real value, an
// optional one may genuinely stay unset). The literal must be a
// double-quoted string, a bare integer, true/false, `[]`, or `{}` — anything
// else is almost certainly a typo, not a real schema, so it is rejected at
// parse time rather than silently accepted as opaque text. Whether `[]`/`{}`
// is actually legal for the field's declared type (list vs. json vs.
// scalar) is checked later, in ir/build.go, once the type is fully resolved.
func splitWireDefault(ft string, line int) (rest string, def *string, err error) {
	idx := strings.Index(ft, " = ")
	if idx < 0 {
		return ft, nil, nil
	}
	rest = strings.TrimSpace(ft[:idx])
	lit := strings.TrimSpace(ft[idx+len(" = "):])
	valid := lit == "true" || lit == "false" || lit == "[]" || lit == "{}"
	if !valid && lit != "" {
		if lit[0] == '"' && len(lit) >= 2 && lit[len(lit)-1] == '"' {
			valid = true
		} else if _, convErr := strconv.ParseInt(lit, 10, 64); convErr == nil {
			valid = true
		}
	}
	if !valid {
		return "", nil, &Error{line, fmt.Sprintf("default %q must be a quoted string, an integer, true/false, [], or {}", lit)}
	}
	return rest, &lit, nil
}

// parseType: `type Name:` then `field: type` lines (inline and/or one per
// child), exactly like parseRecord's field grammar — except a field's type
// may name another `type`/`message` (including itself), which parseRecord's
// fields may not. See ast.Type's doc comment for why this isn't just Record.
func parseType(n *source.Node) (*ast.Type, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "type")), ":")
	name := head
	var inline string
	if colon := strings.IndexByte(head, ':'); colon >= 0 {
		name = strings.TrimSpace(head[:colon])
		inline = strings.TrimSpace(head[colon+1:])
	}
	// `type Name query:` — bound from a URL query string, never a JSON body
	// (see ast.Type.Query's doc comment). The marker sits between the name
	// and the colon, so it must be stripped before the identifier check.
	isQuery := false
	if trimmed := strings.TrimSuffix(name, " query"); trimmed != name {
		isQuery = true
		name = strings.TrimSpace(trimmed)
	}
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("type name %q must be capitalized", name)}
	}
	t := &ast.Type{Name: name, Query: isQuery, Line: n.Line.No}
	add := func(spec string, line int) error {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return nil
		}
		colon := strings.IndexByte(spec, ':')
		if colon < 0 {
			return &Error{line, fmt.Sprintf("type field %q must be `name: type`", spec)}
		}
		fn := strings.TrimSpace(spec[:colon])
		ft := strings.TrimSpace(spec[colon+1:])
		if !isIdent(fn) {
			return &Error{line, fmt.Sprintf("invalid type field name %q", fn)}
		}
		ft, def, err := splitWireDefault(ft, line)
		if err != nil {
			return err
		}
		core, list, optional := splitType(ft)
		if !isWireTypeName(core) {
			return &Error{line, fmt.Sprintf("unknown type %q in field %q (use a primitive, `json`, `number`, an enum, another type/message, or a list of those)", core, fn)}
		}
		if optional && def != nil {
			return &Error{line, fmt.Sprintf("field %q cannot be both optional (?) and have a default (=) — pick one", fn)}
		}
		t.Fields = append(t.Fields, ast.RecordField{Name: fn, Type: core, List: list, Optional: optional, Default: def, Line: line})
		return nil
	}
	if inline != "" {
		for _, f := range strings.Split(inline, ",") {
			if err := add(f, n.Line.No); err != nil {
				return nil, err
			}
		}
	}
	for _, c := range n.Children {
		if err := add(c.Line.Text, c.Line.No); err != nil {
			return nil, err
		}
	}
	if len(t.Fields) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("type %q has no fields", name)}
	}
	return t, nil
}

// parseMessage: `message Name:` then one `| variant_name(field: type, ...)`
// line per child — a tagged union. The variant name is the wire discriminant
// directly (it's already snake_case), so there is no separate rename step.
func parseMessage(n *source.Node) (*ast.Message, error) {
	name := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "message")), ":")
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("message name %q must be capitalized", name)}
	}
	m := &ast.Message{Name: name, Line: n.Line.No}
	seen := map[string]bool{}
	for _, c := range n.Children {
		line := strings.TrimSpace(c.Line.Text)
		if !strings.HasPrefix(line, "|") {
			return nil, &Error{c.Line.No, fmt.Sprintf("message variant %q must start with `|`", line)}
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "|"))
		open := strings.IndexByte(line, '(')
		var vname, inner string
		if open < 0 {
			vname = line
		} else {
			close := strings.LastIndexByte(line, ')')
			if close < open {
				return nil, &Error{c.Line.No, fmt.Sprintf("message variant %q is missing `)`", line)}
			}
			vname = strings.TrimSpace(line[:open])
			inner = strings.TrimSpace(line[open+1 : close])
		}
		if !isIdent(vname) || isUpper(vname) {
			return nil, &Error{c.Line.No, fmt.Sprintf("message variant %q must be a lowercase identifier (it is the wire discriminant, written as-is)", vname)}
		}
		if seen[vname] {
			return nil, &Error{c.Line.No, fmt.Sprintf("message %q has two variants named %q", name, vname)}
		}
		seen[vname] = true
		v := ast.MessageVariant{Name: vname, Line: c.Line.No}
		if inner != "" {
			for _, spec := range strings.Split(inner, ",") {
				spec = strings.TrimSpace(spec)
				if spec == "" {
					continue
				}
				colon := strings.IndexByte(spec, ':')
				if colon < 0 {
					return nil, &Error{c.Line.No, fmt.Sprintf("field %q in variant %q must be `name: type`", spec, vname)}
				}
				fn := strings.TrimSpace(spec[:colon])
				ft := strings.TrimSpace(spec[colon+1:])
				if !isIdent(fn) {
					return nil, &Error{c.Line.No, fmt.Sprintf("invalid field name %q in variant %q", fn, vname)}
				}
				ft, def, err := splitWireDefault(ft, c.Line.No)
				if err != nil {
					return nil, err
				}
				core, list, optional := splitType(ft)
				if !isWireTypeName(core) {
					return nil, &Error{c.Line.No, fmt.Sprintf("unknown type %q in variant %q field %q", core, vname, fn)}
				}
				if optional && def != nil {
					return nil, &Error{c.Line.No, fmt.Sprintf("field %q in variant %q cannot be both optional (?) and have a default (=) — pick one", fn, vname)}
				}
				v.Fields = append(v.Fields, ast.RecordField{Name: fn, Type: core, List: list, Optional: optional, Default: def, Line: c.Line.No})
			}
		}
		m.Variants = append(m.Variants, v)
	}
	if len(m.Variants) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("message %q has no variants", name)}
	}
	return m, nil
}

// parseComponent: `component Name(params):` then a node tree.
func parseComponent(n *source.Node) (*ast.Component, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "component")), ":")
	// allowRef: a component is the only declaration whose parameters may be
	// references (`cell T` / `action`), because it is the only one that renders
	// controls the caller owns.
	name, params, err := parseSignature(head, n.Line.No, false, true)
	if err != nil {
		return nil, err
	}
	if !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("component name %q must be capitalized", name)}
	}
	nodes, err := parseNodes(n.Children)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("component %q has no body", name)}
	}
	return &ast.Component{Name: name, Params: params, Root: nodes, Line: n.Line.No}, nil
}

// parseLayout: `layout Name:` then a node tree containing one `slot`.
func parseLayout(n *source.Node) (*ast.Layout, error) {
	name := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "layout")), ":")
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("layout name %q must be capitalized", name)}
	}
	nodes, err := parseNodes(n.Children)
	if err != nil {
		return nil, err
	}
	// A layout is valid exactly when the splice that inlines a view into it is
	// well-defined, so the check *is* that splice, run here against no view.
	// ast.SpliceLayout is the same call internal/ir makes to perform it — one
	// traversal deciding where a `slot` may appear, rather than a validator and a
	// splicer each holding an opinion and drifting apart (they did: this check
	// used not to look inside `for`, while the splicer did).
	if _, err := ast.SpliceLayout(name, nodes, nil); err != nil {
		return nil, &Error{n.Line.No, err.Error()}
	}
	return &ast.Layout{Name: name, Root: nodes, Line: n.Line.No}, nil
}

// parseTheme: a `theme:` block of `name "value"` lines, each becoming a CSS
// custom property.
func parseTheme(n *source.Node) ([]ast.ThemeVar, error) {
	var out []ast.ThemeVar
	for _, c := range n.Children {
		fields := strings.SplitN(strings.TrimSpace(c.Line.Text), " ", 2)
		if len(fields) != 2 {
			return nil, &Error{c.Line.No, "theme entry must be `name \"value\"`"}
		}
		if !isThemeKey(fields[0]) {
			return nil, &Error{c.Line.No, fmt.Sprintf("invalid theme name %q", fields[0])}
		}
		val, err := unquote(strings.TrimSpace(fields[1]), c.Line.No)
		if err != nil {
			return nil, err
		}
		out = append(out, ast.ThemeVar{Name: fields[0], Value: val, Line: c.Line.No})
	}
	if len(out) == 0 {
		return nil, &Error{n.Line.No, "theme block is empty"}
	}
	return out, nil
}

// parseNamedTheme parses a `theme <name>:` block — an alternate palette (any name
// other than the base `theme:` or the reserved `theme dark:`). Its tokens become
// a `[data-theme="<name>"]` palette the app can switch to at runtime by setting
// the built-in `theme` state.
func parseNamedTheme(n *source.Node) (ast.NamedTheme, error) {
	head := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(n.Line.Text), "theme"))
	name := strings.TrimSpace(strings.TrimSuffix(head, ":"))
	if !isThemeKey(name) {
		return ast.NamedTheme{}, &Error{n.Line.No, fmt.Sprintf("invalid theme name %q", name)}
	}
	tv, err := parseTheme(n)
	if err != nil {
		return ast.NamedTheme{}, err
	}
	return ast.NamedTheme{Name: name, Vars: tv, Line: n.Line.No}, nil
}

// parseCSS reconstructs a raw stylesheet from a `css:` block, joining every nested
// source line in order. The offside tokenizer already dropped blank lines and
// `#`-prefixed lines (its comment marker), so author CSS should target `.classes`
// (the `class "..."` node hook) and attribute selectors rather than `#id`.
func parseCSS(n *source.Node, comments []source.Line) (string, error) {
	var lines []string
	last := n.Line.No

	var walk func(ns []*source.Node)
	walk = func(ns []*source.Node) {
		for _, c := range ns {
			lines = append(lines, c.Line.Text)
			if c.Line.No > last {
				last = c.Line.No
			}
			walk(c.Children)
		}
	}
	walk(n.Children)

	if len(lines) == 0 {
		return "", &Error{n.Line.No, "css block is empty"}
	}

	if err := checkCSSComments(n.Line.No, last, comments); err != nil {
		return "", err
	}

	return strings.Join(lines, "\n"), nil
}

// checkCSSComments refuses a `#`-anchored CSS rule inside a `css:` block
// instead of silently dropping it.
//
// `#` opens a comment in this language and names an id in CSS, so
// `#fa-root .fa-box { border: none }` is discarded by the scanner before any
// parser sees it: the stylesheet compiles, the page renders, and the rule is
// simply not there. That is the worst shape a diagnostic can have — nothing to
// read, and a symptom (a border that will not go away) that points at CSS
// specificity rather than at a missing line.
//
// The heuristic is deliberately narrow. A comment is only reported when it
// contains a `{`, which no prose comment in this codebase does and every CSS
// rule does. A comment that merely starts with `#` inside a `css:` block — the
// section headers this project writes constantly — is left alone.
func checkCSSComments(from, to int, comments []source.Line) error {
	for _, c := range comments {
		if c.No <= from || c.No > to {
			continue
		}

		if !strings.Contains(c.Text, "{") {
			continue
		}

		return &Error{c.No, fmt.Sprintf(
			"this looks like a CSS rule, but `#` starts a comment in this "+
				"language, so the line is discarded and the rule never reaches "+
				"the page: %s\n"+
				"      Anchor the selector on something other than an id — "+
				"`[data-fa-mount]` is the app root and is worth the same as a "+
				"class — or drop the anchor entirely.",
			c.Text)}
	}

	return nil
}

// parseCSSFrom parses `css from "styles.css"` — a reference to a sibling
// stylesheet, the external-file counterpart of an inline `css:` block. It
// mirrors `import "..."`'s path syntax for consistency, but the path is never
// resolved here: this package has no file-system access, and the same grammar
// is used for embedded snippets that have no file to resolve against. The
// compiler (internal/compile), which already resolves `import` paths relative
// to the file on disk, resolves this one the same way and folds the file's raw
// content into the app's CSS.
func parseCSSFrom(t string, line int) (string, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(t, "css from"))
	path, err := unquote(rest, line)
	if err != nil {
		return "", &Error{line, `css from needs a quoted path: css from "styles.css"`}
	}
	if path == "" {
		return "", &Error{line, "css from path is empty"}
	}
	return path, nil
}

// joinCSS concatenates stylesheet fragments (one per `css:` block, across the
// playground and every facet it composes) with a newline between them.
func joinCSS(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}

// parseState: `state name: Type = default [@client|@server|@private]`
func parseState(n *source.Node) (*ast.State, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "state"))
	place := ast.PlaceInfer
	for _, ann := range []string{"@client", "@server", "@private"} {
		if strings.HasSuffix(rest, ann) {
			rest = strings.TrimSpace(strings.TrimSuffix(rest, ann))
			place = strings.TrimPrefix(ann, "@")
		}
	}
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return nil, &Error{n.Line.No, "state needs a type: `state name: int = 0`"}
	}
	name := strings.TrimSpace(rest[:colon])
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid state name %q", name)}
	}
	after := strings.TrimSpace(rest[colon+1:])
	var typ, defSrc string
	if eq := splitTopByte(after, '='); eq >= 0 {
		typ, defSrc = strings.TrimSpace(after[:eq]), strings.TrimSpace(after[eq+1:])
	} else {
		typ = after
	}
	core, list, optional := splitType(typ)
	if !isTypeName(core) {
		return nil, &Error{n.Line.No, fmt.Sprintf("unknown type %q (use int, text, bool, money, date, an enum, or [T])", core)}
	}
	st := &ast.State{Name: name, Placement: place, Optional: optional, List: list, Line: n.Line.No}
	if list {
		st.Type = "[" + core + "]"
		st.Elem = core
	} else {
		st.Type = core
	}
	if defSrc == "" {
		if list {
			st.Default = ast.ListLit{}
		} else {
			st.Default = defaultFor(core)
		}
	} else {
		e, err := parseExpr(defSrc, n.Line.No)
		if err != nil {
			return nil, err
		}
		st.Default = e
	}
	return st, nil
}

// splitTopByte finds the first sep at the top level (outside brackets/parens/
// quotes), so a `[int]` list type or a string default is not mistaken for the
// `=` separator.
func splitTopByte(s string, sep byte) int {
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case c == sep && depth == 0:
			return i
		}
	}
	return -1
}

// parseDerive: `derive name: Type = expr`. No placement annotation — a
// derivation's domain is computed, never authored (that is the whole point).
func parseDerive(n *source.Node) (*ast.Derive, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "derive"))
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return nil, &Error{n.Line.No, "derive needs a type: `derive name: int = expr`"}
	}
	name := strings.TrimSpace(rest[:colon])
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid derive name %q", name)}
	}
	after := strings.TrimSpace(rest[colon+1:])
	eq := strings.IndexByte(after, '=')
	if eq < 0 {
		return nil, &Error{n.Line.No, "derive needs a definition: `derive name: int = expr`"}
	}
	typ := strings.TrimSpace(after[:eq])
	core, _, _ := splitType(typ)
	if !isTypeName(core) {
		return nil, &Error{n.Line.No, fmt.Sprintf("unknown type %q (use int, text, bool, money, date, an enum, or [T])", typ)}
	}
	e, err := parseExpr(strings.TrimSpace(after[eq+1:]), n.Line.No)
	if err != nil {
		return nil, err
	}
	return &ast.Derive{Name: name, Type: typ, Expr: e, Line: n.Line.No}, nil
}

func parsePolicy(n *source.Node) (*ast.Policy, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "policy")), ":")
	// A policy may declare parameters for row-level checks: `policy owns(id: int):`.
	name, params, err := parseSignature(head, n.Line.No, false, false)
	if err != nil {
		return nil, err
	}
	if len(n.Children) != 1 {
		return nil, &Error{n.Line.No, "policy must have exactly one predicate line"}
	}
	e, err := parseExpr(n.Children[0].Line.Text, n.Children[0].Line.No)
	if err != nil {
		return nil, err
	}
	return &ast.Policy{Name: name, Params: params, Expr: e, Line: n.Line.No}, nil
}

// parseAction: `action name(params) [@optimistic]:` then `requires …`,
// `check …`, and statement lines.
func parseAction(n *source.Node) (*ast.Action, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "action")), ":")
	optimistic := false
	if strings.HasSuffix(head, "@optimistic") {
		optimistic = true
		head = strings.TrimSpace(strings.TrimSuffix(head, "@optimistic"))
	}
	name, params, err := parseSignature(head, n.Line.No, false, false)
	if err != nil {
		return nil, err
	}
	a := &ast.Action{Name: name, Params: params, Optimistic: optimistic, Line: n.Line.No}
	for _, c := range n.Children {
		t := c.Line.Text
		switch {
		case strings.HasPrefix(t, "check "):
			// A check is a body statement in source order, so it can validate a value
			// bound earlier by `let` (e.g. a request→response result).
			chk, err := parseCheck(strings.TrimSpace(t[len("check "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			a.Body = append(a.Body, chk)
		case strings.HasPrefix(t, "requires "):
			// `requires admin` or, for row-level checks, `requires owns(id), admin`.
			for _, p := range splitTop(strings.TrimSpace(t[len("requires "):]), ',') {
				req, err := parseRequire(strings.TrimSpace(p), c.Line.No)
				if err != nil {
					return nil, err
				}
				a.Requires = append(a.Requires, req)
			}
		case strings.HasPrefix(t, "add "):
			s, err := parseAdd(c)
			if err != nil {
				return nil, err
			}
			a.Body = append(a.Body, s)
		case strings.HasPrefix(t, "set "):
			s, err := parseSet(c)
			if err != nil {
				return nil, err
			}
			a.Body = append(a.Body, s)
		case strings.HasPrefix(t, "remove "):
			s, err := parseRemove(c)
			if err != nil {
				return nil, err
			}
			a.Body = append(a.Body, s)
		case strings.HasPrefix(t, "clear "):
			ent := strings.TrimSpace(t[len("clear "):])
			if !isIdent(ent) {
				return nil, &Error{c.Line.No, fmt.Sprintf("invalid entity %q", ent)}
			}
			a.Body = append(a.Body, ast.Clear{Entity: ent, Line: c.Line.No})
		case strings.HasPrefix(t, "call "):
			cl, err := parseCall(strings.TrimSpace(t[len("call "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			a.Body = append(a.Body, cl)
		case strings.HasPrefix(t, "do "):
			d, err := parseDo(strings.TrimSpace(t[len("do "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			a.Body = append(a.Body, d)
		case strings.HasPrefix(t, "let "):
			// Request→response bind: `let name = call Service.op(args)` or
			// `let name = do ProcName(args)`.
			rest := strings.TrimSpace(t[len("let "):])
			eq := strings.IndexByte(rest, '=')
			if eq < 0 {
				return nil, &Error{c.Line.No, "let needs `let name = call Service.op(args)` or `let name = do ProcName(args)`"}
			}
			name := strings.TrimSpace(rest[:eq])
			if !isIdent(name) {
				return nil, &Error{c.Line.No, fmt.Sprintf("invalid let binding %q", name)}
			}
			rhs := strings.TrimSpace(rest[eq+1:])
			switch {
			case strings.HasPrefix(rhs, "call "):
				cl, err := parseCall(strings.TrimSpace(rhs[len("call "):]), c.Line.No)
				if err != nil {
					return nil, err
				}
				cl.Bind = name
				a.Body = append(a.Body, cl)
			case strings.HasPrefix(rhs, "do "):
				d, err := parseDo(strings.TrimSpace(rhs[len("do "):]), c.Line.No)
				if err != nil {
					return nil, err
				}
				d.Bind = name
				a.Body = append(a.Body, d)
			default:
				return nil, &Error{c.Line.No, "let binds a service call or a proc call: `let name = call Service.op(args)` or `let name = do ProcName(args)`"}
			}
		case strings.HasPrefix(t, "establish "):
			// `establish actor <expr> [role <expr>]` — adopt a custom session identity.
			rest := strings.TrimSpace(t[len("establish "):])
			if !strings.HasPrefix(rest, "actor ") {
				return nil, &Error{c.Line.No, "establish needs `establish actor <expr> [role <expr>]`"}
			}
			rest = strings.TrimSpace(rest[len("actor "):])
			var roleSrc string
			if i := strings.Index(rest, " role "); i >= 0 {
				roleSrc = strings.TrimSpace(rest[i+len(" role "):])
				rest = strings.TrimSpace(rest[:i])
			}
			actorExpr, err := parseExpr(rest, c.Line.No)
			if err != nil {
				return nil, err
			}
			est := ast.Establish{Actor: actorExpr, Line: c.Line.No}
			if roleSrc != "" {
				roleExpr, err := parseExpr(roleSrc, c.Line.No)
				if err != nil {
					return nil, err
				}
				est.Role = roleExpr
			}
			a.Body = append(a.Body, est)
		case isBareCallStmt(t):
			// A builtin call for its side effect alone, its result discarded —
			// `print(x)` on its own line, the action-body counterpart to
			// parseProcBody's identical case. See ast.ExprStmt's doc.
			ex, err := parseExpr(t, c.Line.No)
			if err != nil {
				return nil, err
			}
			call, ok := ex.(ast.Call)
			if !ok {
				return nil, &Error{c.Line.No, fmt.Sprintf("%q is not a valid statement on its own", t)}
			}
			a.Body = append(a.Body, ast.ExprStmt{Call: call, Line: c.Line.No})
		default:
			eq := strings.IndexByte(t, '=')
			if eq < 0 {
				return nil, unknownActionStatementError(c.Line.No, firstWord(t))
			}
			target := strings.TrimSpace(t[:eq])
			if !isIdent(target) {
				return nil, &Error{c.Line.No, fmt.Sprintf("invalid assignment target %q", target)}
			}
			val, err := parseExpr(strings.TrimSpace(t[eq+1:]), c.Line.No)
			if err != nil {
				return nil, err
			}
			a.Body = append(a.Body, ast.Assign{Target: target, Value: val, Line: c.Line.No})
		}
	}
	if len(a.Body) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("action %q has no body", name)}
	}
	return a, nil
}

// unknownActionStatementError builds the diagnostic for an action-body line
// that is neither one of the recognized statement keywords nor an
// `name = expr` assignment. As with unknownViewNodeError, the "expected one
// of" list is read live off this file's own parseAction switch via
// switchCaseKeywords rather than retyped, so it cannot drift from what the
// switch actually accepts.
func unknownActionStatementError(line int, word string) *Error {
	kws, ok := switchCaseKeywords("parseAction")
	if !ok || len(kws) == 0 {
		return &Error{line, fmt.Sprintf("unknown statement %q", word)}
	}
	return &Error{line, fmt.Sprintf("unknown statement %q — expected one of: %s, or an assignment (`name = expr`)", word, strings.Join(kws, ", "))}
}

// parseService parses `service Name at "url":` plus a block of typed operation
// signatures `op(param: Type, ...)`. It is the contract for an external brain.
func parseService(n *source.Node) (*ast.Service, error) {
	rest := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "service")), ":")
	i := strings.Index(rest, " at ")
	if i < 0 {
		return nil, &Error{n.Line.No, `service needs a base URL: service Name at "http://host:port"`}
	}
	name := strings.TrimSpace(rest[:i])
	url, err := unquote(strings.TrimSpace(rest[i+len(" at "):]), n.Line.No)
	if err != nil || url == "" {
		return nil, &Error{n.Line.No, `service needs a base URL: service Name at "http://host:port"`}
	}
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid service name %q", name)}
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, &Error{n.Line.No, fmt.Sprintf("service %q url must start with http:// or https://", name)}
	}
	sv := &ast.Service{Name: name, URL: url, Line: n.Line.No}
	for _, c := range n.Children {
		// An op may declare a typed return: `op(params) -> Type` or `-> [Type]`.
		head := strings.TrimSpace(c.Line.Text)
		var ret string
		var retList bool
		if arrow := strings.Index(head, "->"); arrow >= 0 {
			rt := strings.TrimSpace(head[arrow+2:])
			head = strings.TrimSpace(head[:arrow])
			core, list, optional := splitType(rt)
			if optional || !isTypeName(core) {
				return nil, &Error{c.Line.No, fmt.Sprintf("invalid return type %q", rt)}
			}
			ret, retList = core, list
		}
		opName, params, err := parseSignature(head, c.Line.No, true, false)
		if err != nil {
			return nil, err
		}
		sv.Ops = append(sv.Ops, ast.ServiceOp{Name: opName, Params: params, Ret: ret, RetList: retList, Line: c.Line.No})
	}
	if len(sv.Ops) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("service %q declares no operations", name)}
	}
	return sv, nil
}

// parseFile parses `file Name: Type at "path"` — a declared file resource
// (mirrors parseService's `service Name at "url"` header shape exactly, for
// local disk instead of HTTP). Type must be `text` or `bytes`; Path is
// resolved through the runtime's existing io.file sandbox exactly as
// readFile/writeFile already are (see ast.File's doc). Unlike a service, a
// file takes no indented block of operations — `read`/`write` are fixed,
// proc-body statements (parseProcBody), not something a file declaration
// itself enumerates.
func parseFile(n *source.Node) (*ast.File, error) {
	if len(n.Children) > 0 {
		return nil, &Error{n.Line.No, "file declares no body — `read`/`write` are proc statements, not part of the declaration"}
	}
	rest := strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "file"))
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return nil, &Error{n.Line.No, `file needs a type: file Name: text at "path.txt"`}
	}
	name := strings.TrimSpace(rest[:colon])
	tail := strings.TrimSpace(rest[colon+1:])
	i := strings.Index(tail, " at ")
	if i < 0 {
		return nil, &Error{n.Line.No, `file needs a path: file Name: text at "path.txt"`}
	}
	typ := strings.TrimSpace(tail[:i])
	path, err := unquote(strings.TrimSpace(tail[i+len(" at "):]), n.Line.No)
	if err != nil || path == "" {
		return nil, &Error{n.Line.No, `file needs a quoted path: file Name: text at "path.txt"`}
	}
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid file name %q", name)}
	}
	if typ != "text" && typ != "bytes" {
		return nil, &Error{n.Line.No, fmt.Sprintf("file %q type must be text or bytes, got %q", name, typ)}
	}
	return &ast.File{Name: name, Type: typ, Path: path, Line: n.Line.No}, nil
}

// parseWebhook parses a one-line inbound endpoint:
//
//	webhook "/hooks/pay" -> confirmPaid secret PAY_KEY
//
// The quoted path is the route an external system POSTs to, the arrow names the
// action the runtime runs with the JSON body decoded into its parameters, and the
// optional `secret <ENV>` names the env var holding the HMAC key (empty derives a
// key from the master secret). It is the inbound counterpart of a `service` call.
func parseWebhook(line string, no int) (*ast.Webhook, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "webhook"))
	arrow := strings.Index(rest, "->")
	if arrow < 0 {
		return nil, &Error{no, `webhook needs a target action: webhook "/path" -> actionName`}
	}
	path, err := unquote(strings.TrimSpace(rest[:arrow]), no)
	if err != nil || path == "" {
		return nil, &Error{no, `webhook needs a quoted path: webhook "/path" -> actionName`}
	}
	if !strings.HasPrefix(path, "/") {
		return nil, &Error{no, fmt.Sprintf("webhook path %q must start with /", path)}
	}
	target := strings.TrimSpace(rest[arrow+2:])
	var secret string
	if i := strings.Index(target, " secret "); i >= 0 {
		secret = strings.TrimSpace(target[i+len(" secret "):])
		target = strings.TrimSpace(target[:i])
		if !isIdent(secret) {
			return nil, &Error{no, fmt.Sprintf("webhook secret %q must be an env-var name", secret)}
		}
	}
	if !isIdent(target) {
		return nil, &Error{no, fmt.Sprintf("webhook target %q must be an action name", target)}
	}
	return &ast.Webhook{Path: path, Action: target, Secret: secret, Line: no}, nil
}

// parseTrigger parses a one-line event reaction:
//
//	on post -> notifyFollowers
//
// "when the `post` action completes, run `notifyFollowers`". Both sides are action
// names; the reaction is the non-cron sibling of a `job`, fired by a domain event
// instead of a clock.
func parseTrigger(line string, no int) (*ast.Trigger, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "on"))
	arrow := strings.Index(rest, "->")
	if arrow < 0 {
		return nil, &Error{no, "trigger needs a reaction: `on <action> -> <reaction>`"}
	}
	on := strings.TrimSpace(rest[:arrow])
	react := strings.TrimSpace(rest[arrow+2:])
	if !isIdent(on) {
		return nil, &Error{no, fmt.Sprintf("trigger source %q must be an action name", on)}
	}
	if !isIdent(react) {
		return nil, &Error{no, fmt.Sprintf("trigger reaction %q must be an action name", react)}
	}
	return &ast.Trigger{On: on, Action: react, Line: no}, nil
}

// parseDo parses `ProcName(arg, ...)` — a proc call statement (`do ProcName(args)`,
// or bound via `let x = do ProcName(args)`). Mirrors parseCall, minus the
// `Service.op` dot: a proc has no service namespace to route through.
func parseDo(s string, line int) (ast.Do, error) {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return ast.Do{}, &Error{line, "do needs arguments: do ProcName(args)"}
	}
	name := strings.TrimSpace(s[:open])
	if !isIdent(name) {
		return ast.Do{}, &Error{line, fmt.Sprintf("invalid proc name %q", name)}
	}
	closeP := strings.LastIndexByte(s, ')')
	if closeP < open {
		return ast.Do{}, &Error{line, "missing `)` in do"}
	}
	d := ast.Do{Proc: name, Line: line}
	if inner := strings.TrimSpace(s[open+1 : closeP]); inner != "" {
		for _, a := range splitTop(inner, ',') {
			e, err := parseExpr(strings.TrimSpace(a), line)
			if err != nil {
				return ast.Do{}, err
			}
			d.Args = append(d.Args, e)
		}
	}
	return d, nil
}

// parseSpawn parses `ProcName(arg, ...)` — the argument-list half of
// `let h = spawn ProcName(args)` — identical shape to parseDo (a proc call is
// a proc call; spawn only changes how the runtime executes it, not how it's
// written), kept as its own function rather than shared so the two grammars
// can diverge freely (e.g. if spawn ever grows spawn-specific syntax) the way
// parseDo/parseCall already are two separate functions for the same reason.
func parseSpawn(s string, line int) (ast.Spawn, error) {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return ast.Spawn{}, &Error{line, "spawn needs arguments: spawn ProcName(args)"}
	}
	name := strings.TrimSpace(s[:open])
	if !isIdent(name) {
		return ast.Spawn{}, &Error{line, fmt.Sprintf("invalid proc name %q", name)}
	}
	closeP := strings.LastIndexByte(s, ')')
	if closeP < open {
		return ast.Spawn{}, &Error{line, "missing `)` in spawn"}
	}
	sp := ast.Spawn{Proc: name, Line: line}
	if inner := strings.TrimSpace(s[open+1 : closeP]); inner != "" {
		for _, a := range splitTop(inner, ',') {
			e, err := parseExpr(strings.TrimSpace(a), line)
			if err != nil {
				return ast.Spawn{}, err
			}
			sp.Args = append(sp.Args, e)
		}
	}
	return sp, nil
}

// parseJoin parses the handle name half of `join h` / `let r = join h` — a
// bare local name, never a call: a join consumes an already-spawned handle,
// it doesn't invoke anything new.
func parseJoin(s string, line int) (ast.Join, error) {
	h := strings.TrimSpace(s)
	if !isIdent(h) {
		return ast.Join{}, &Error{line, fmt.Sprintf("join needs a spawned task handle: join <handle>, got %q", h)}
	}
	return ast.Join{Handle: h, Line: line}, nil
}

// parseAct parses `ActionName(arg, ...)` — the argument-list half of a daemon
// body's `act ActionName(args)` (see ast.Act's doc) — identical call shape to
// parseDo/parseSpawn (a named declaration invoked with arguments), kept as its
// own function for the same reason those two are: "act" and "do" name
// different things (an action vs. a proc) and may want to diverge.
func parseAct(s string, line int) (ast.Act, error) {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return ast.Act{}, &Error{line, "act needs arguments: act ActionName(args)"}
	}
	name := strings.TrimSpace(s[:open])
	if !isIdent(name) {
		return ast.Act{}, &Error{line, fmt.Sprintf("invalid action name %q", name)}
	}
	closeP := strings.LastIndexByte(s, ')')
	if closeP < open {
		return ast.Act{}, &Error{line, "missing `)` in act"}
	}
	a := ast.Act{Action: name, Line: line}
	if inner := strings.TrimSpace(s[open+1 : closeP]); inner != "" {
		for _, arg := range splitTop(inner, ',') {
			e, err := parseExpr(strings.TrimSpace(arg), line)
			if err != nil {
				return ast.Act{}, err
			}
			a.Args = append(a.Args, e)
		}
	}
	return a, nil
}

// parseFileOp parses the `Name(args)` half of a file-resource statement —
// `write Name(content)` or the `read Name()` half of `let x = read Name()` —
// identical shape to parseDo/parseSpawn (a named-resource call), since these
// are the human-friendly verb form of readFile/writeFile over a declared
// `file` resource rather than a raw builtin call (see ast.FileOp's doc). op
// is "read" or "write", used only to phrase this function's own errors.
func parseFileOp(op, s string, line int) (ast.FileOp, error) {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return ast.FileOp{}, &Error{line, fmt.Sprintf("%s needs a file resource: %s Name(...)", op, op)}
	}
	name := strings.TrimSpace(s[:open])
	if !isIdent(name) {
		return ast.FileOp{}, &Error{line, fmt.Sprintf("invalid file name %q", name)}
	}
	closeP := strings.LastIndexByte(s, ')')
	if closeP < open {
		return ast.FileOp{}, &Error{line, fmt.Sprintf("missing `)` in %s", op)}
	}
	fo := ast.FileOp{Op: op, File: name, Line: line}
	if inner := strings.TrimSpace(s[open+1 : closeP]); inner != "" {
		for _, a := range splitTop(inner, ',') {
			e, err := parseExpr(strings.TrimSpace(a), line)
			if err != nil {
				return ast.FileOp{}, err
			}
			fo.Args = append(fo.Args, e)
		}
	}
	return fo, nil
}

// parseProc parses `proc Name(params) -> RetType:` (the arrow and its type are
// optional — a proc may return nothing) plus a body, delegating the body itself
// to parseProcBody. Unlike parseAction, a proc isn't policy-gated or optimistic
// and doesn't touch entities directly (out of scope for this milestone), so its
// statement vocabulary is deliberately smaller — see parseProcBody.
func parseProc(n *source.Node) (*ast.Proc, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "proc")), ":")
	// `uses io.file, io.net` — the capability clause — trails the return type
	// when there is one, or the parameter list when there isn't; either way it
	// is the last thing before the header's `:`, so it is pulled off first
	// (mirroring how parseMount/parseView pull " requires " off before parsing
	// the rest of their own headers).
	var uses []string
	if i := strings.Index(head, " uses "); i >= 0 {
		usesPart := strings.TrimSpace(head[i+len(" uses "):])
		head = strings.TrimSpace(head[:i])
		if usesPart == "" {
			return nil, &Error{n.Line.No, "uses needs at least one capability (e.g. `uses io.file`)"}
		}
		for _, c := range splitTop(usesPart, ',') {
			c = strings.TrimSpace(c)
			if !isCapabilityName(c) {
				return nil, &Error{n.Line.No, fmt.Sprintf("invalid capability %q in uses clause (expected a dotted name like io.file)", c)}
			}
			uses = append(uses, c)
		}
	}
	var ret string
	var retList bool
	if arrow := strings.Index(head, "->"); arrow >= 0 {
		rt := strings.TrimSpace(head[arrow+2:])
		head = strings.TrimSpace(head[:arrow])
		core, list, optional := splitType(rt)
		if optional || !isTypeName(core) {
			return nil, &Error{n.Line.No, fmt.Sprintf("invalid return type %q", rt)}
		}
		ret, retList = core, list
	}
	// allowList=true: unlike an action/component/policy, a proc parameter may be
	// list-typed (`buildTree(lines: [text])`) — the shape composing two procs
	// needs (one proc's `-> [T]` return flowing into a second proc's `[T]`
	// parameter), which was previously blocked here even though a proc's own
	// RETURN type could already be a list. See LANGUAGE.md's `proc` section.
	name, params, err := parseSignature(head, n.Line.No, true, false)
	if err != nil {
		return nil, err
	}
	p := &ast.Proc{Name: name, Params: params, Ret: ret, RetList: retList, Uses: uses, Line: n.Line.No}
	body, err := parseProcBody(n.Children, fmt.Sprintf("proc %q", name))
	if err != nil {
		return nil, err
	}
	p.Body = body
	if len(p.Body) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("proc %q has no body", name)}
	}
	return p, nil
}

// parseProcBody recursively parses one nested statement block inside a proc: the
// proc's own top-level body, or a `loop`/`if` statement's own Body/Then/Else.
// ctx names the enclosing construct for error messages ("proc %q", "a loop
// body", "an if body", "an else body").
//
// Milestone 2 statement vocabulary: `let`/`let mut` (a proc-local variable), a
// plain reassignment (`name = expr`, legal only for a `let mut` local —
// enforced in internal/ir/build.go, which tracks the proc's own declared-locals
// scope), `return` (from anywhere, not just this block's end), `do` (calling
// another proc), `loop <cond>:` (a while-style precondition loop whose body is
// parsed by recursing into this same function — the one genuinely recursive
// shape a proc statement can carry), `if <cond>:` with an optional sibling
// `else:` (each branch likewise parsed by recursing), and `break`/`continue`
// (loop-nesting validity is a build.go concern, not the parser's).
// check/requires/establish/add/set/remove/clear are explicitly rejected: a proc
// is pure computation over its own locals and parameters.
func parseProcBody(children []*source.Node, ctx string) ([]ast.Stmt, error) {
	var body []ast.Stmt
	for i := 0; i < len(children); i++ {
		c := children[i]
		t := strings.TrimSpace(c.Line.Text)
		switch {
		case strings.HasPrefix(t, "check "), t == "check", strings.HasPrefix(t, "requires "), t == "requires",
			strings.HasPrefix(t, "establish "), t == "establish", strings.HasPrefix(t, "add "), t == "add",
			strings.HasPrefix(t, "set "), t == "set", strings.HasPrefix(t, "remove "), t == "remove",
			strings.HasPrefix(t, "clear "), t == "clear":
			return nil, &Error{c.Line.No, fmt.Sprintf(
				"%s can't use %q — a proc is pure computation over its own locals and parameters; entity/policy access from a proc is a later milestone", ctx, firstWord(t))}
		case t == "break":
			body = append(body, ast.Break{Line: c.Line.No})
		case t == "continue":
			body = append(body, ast.Continue{Line: c.Line.No})
		case t == "else:" || t == "else":
			return nil, &Error{c.Line.No, "`else` with no matching `if`"}
		case strings.HasPrefix(t, "loop "):
			condS := strings.TrimSuffix(strings.TrimSpace(t[len("loop "):]), ":")
			if condS == "" {
				return nil, &Error{c.Line.No, "loop needs a condition: loop <cond>:"}
			}
			cond, err := parseExpr(condS, c.Line.No)
			if err != nil {
				return nil, err
			}
			kids, err := parseProcBody(c.Children, "a loop body")
			if err != nil {
				return nil, err
			}
			if len(kids) == 0 {
				return nil, &Error{c.Line.No, "loop has no body"}
			}
			body = append(body, ast.Loop{Cond: cond, Body: kids, Line: c.Line.No})
		case strings.HasPrefix(t, "if "):
			condS := strings.TrimSuffix(strings.TrimSpace(t[len("if "):]), ":")
			if condS == "" {
				return nil, &Error{c.Line.No, "if needs a condition: if <cond>:"}
			}
			cond, err := parseExpr(condS, c.Line.No)
			if err != nil {
				return nil, err
			}
			then, err := parseProcBody(c.Children, "an if body")
			if err != nil {
				return nil, err
			}
			if len(then) == 0 {
				return nil, &Error{c.Line.No, "if has no body"}
			}
			var els []ast.Stmt
			if i+1 < len(children) {
				nt := strings.TrimSpace(children[i+1].Line.Text)
				if nt == "else:" || nt == "else" {
					els, err = parseProcBody(children[i+1].Children, "an else body")
					if err != nil {
						return nil, err
					}
					if len(els) == 0 {
						return nil, &Error{children[i+1].Line.No, "else has no body"}
					}
					i++ // consume the sibling `else:` node
				}
			}
			body = append(body, ast.IfStmt{Cond: cond, Then: then, Else: els, Line: c.Line.No})
		case strings.HasPrefix(t, "return"):
			rest := strings.TrimSpace(strings.TrimPrefix(t, "return"))
			var val ast.Expr
			var err error
			if rest != "" {
				val, err = parseExpr(rest, c.Line.No)
				if err != nil {
					return nil, err
				}
			}
			body = append(body, ast.Return{Value: val, Line: c.Line.No})
		case strings.HasPrefix(t, "do "):
			d, err := parseDo(strings.TrimSpace(t[len("do "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			body = append(body, d)
		case strings.HasPrefix(t, "act "):
			// `act ActionName(args)` — fire-and-forget only (see ast.Act's doc:
			// an action has no scalar return to bind). Valid to PARSE in any
			// proc-shaped body; internal/ir/build.go's procBlock is what
			// actually restricts it to a daemon body, the same way it lets
			// `check`/`add`/`set`/etc. parse here only to reject them uniformly
			// above — keeping that one semantic gate in one place.
			a, err := parseAct(strings.TrimSpace(t[len("act "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			body = append(body, a)
		case strings.HasPrefix(t, "write "):
			// `write Name(content)` — fire-and-forget (result discarded); a
			// bound `let ok = write Name(content)` is handled below, in the
			// `let ` case, exactly like `do`'s two forms.
			fo, err := parseFileOp("write", strings.TrimSpace(t[len("write "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			body = append(body, fo)
		case strings.HasPrefix(t, "read "):
			// A bare `read Name()`, its content discarded — the read
			// counterpart of a fire-and-forget `write`/`do`. The useful form,
			// `let x = read Name()`, is handled below in the `let ` case.
			fo, err := parseFileOp("read", strings.TrimSpace(t[len("read "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			body = append(body, fo)
		case strings.HasPrefix(t, "spawn "):
			// A bare `spawn ProcName(args)`, its handle discarded, has no legal
			// reading: structured concurrency requires every spawned task be
			// joined (see ast.Spawn's doc), and a discarded handle can never be
			// joined by anything — so, unlike `do`, spawn has no fire-and-forget
			// form at all. Caught here, at parse time, with a message that says
			// so rather than falling through to the generic "unknown statement".
			return nil, &Error{c.Line.No, "spawn has no fire-and-forget form — bind its handle with `let h = spawn " + strings.TrimSpace(t[len("spawn "):]) + "` and `join h` before this proc returns, or the goroutine could outlive it"}
		case strings.HasPrefix(t, "join "):
			j, err := parseJoin(strings.TrimSpace(t[len("join "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			body = append(body, j)
		case strings.HasPrefix(t, "let "):
			rest := strings.TrimSpace(t[len("let "):])
			mut := false
			if strings.HasPrefix(rest, "mut ") {
				mut = true
				rest = strings.TrimSpace(rest[len("mut "):])
			}
			eq := strings.IndexByte(rest, '=')
			if eq < 0 {
				return nil, &Error{c.Line.No, "let needs `let [mut] name = expr`"}
			}
			lname := strings.TrimSpace(rest[:eq])
			if !isIdent(lname) {
				return nil, &Error{c.Line.No, fmt.Sprintf("invalid let binding %q", lname)}
			}
			rhs := strings.TrimSpace(rest[eq+1:])
			if strings.HasPrefix(rhs, "do ") {
				if mut {
					return nil, &Error{c.Line.No, "`let mut` can't bind a `do` call result yet — bind it plainly, then use it to compute a `let mut` local if you need to mutate it"}
				}
				d, err := parseDo(strings.TrimSpace(rhs[len("do "):]), c.Line.No)
				if err != nil {
					return nil, err
				}
				d.Bind = lname
				body = append(body, d)
				continue
			}
			if strings.HasPrefix(rhs, "spawn ") {
				if mut {
					return nil, &Error{c.Line.No, "`let mut` can't bind a `spawn` handle — a task handle is joined once and never reassigned; bind it plainly with `let`"}
				}
				sp, err := parseSpawn(strings.TrimSpace(rhs[len("spawn "):]), c.Line.No)
				if err != nil {
					return nil, err
				}
				sp.Bind = lname
				body = append(body, sp)
				continue
			}
			if strings.HasPrefix(rhs, "join ") {
				if mut {
					return nil, &Error{c.Line.No, "`let mut` can't bind a `join` result yet — bind it plainly, then use it to compute a `let mut` local if you need to mutate it"}
				}
				j, err := parseJoin(strings.TrimSpace(rhs[len("join "):]), c.Line.No)
				if err != nil {
					return nil, err
				}
				j.Bind = lname
				body = append(body, j)
				continue
			}
			if strings.HasPrefix(rhs, "read ") {
				if mut {
					return nil, &Error{c.Line.No, "`let mut` can't bind a `read` result yet — bind it plainly, then use it to compute a `let mut` local if you need to mutate it"}
				}
				fo, err := parseFileOp("read", strings.TrimSpace(rhs[len("read "):]), c.Line.No)
				if err != nil {
					return nil, err
				}
				fo.Bind = lname
				body = append(body, fo)
				continue
			}
			if strings.HasPrefix(rhs, "act ") {
				return nil, &Error{c.Line.No, "act has no bound form — an action has no scalar return, only the deltas its body applies; use a bare `act ActionName(args)`"}
			}
			if strings.HasPrefix(rhs, "write ") {
				if mut {
					return nil, &Error{c.Line.No, "`let mut` can't bind a `write` result yet — bind it plainly, then use it to compute a `let mut` local if you need to mutate it"}
				}
				fo, err := parseFileOp("write", strings.TrimSpace(rhs[len("write "):]), c.Line.No)
				if err != nil {
					return nil, err
				}
				fo.Bind = lname
				body = append(body, fo)
				continue
			}
			val, err := parseExpr(rhs, c.Line.No)
			if err != nil {
				return nil, err
			}
			body = append(body, ast.Let{Name: lname, Mut: mut, Value: val, Line: c.Line.No})
		case isBareCallStmt(t):
			// A builtin call for its side effect alone, its result discarded —
			// `writeFile(path, content)` rather than `let ok = writeFile(...)`.
			// See ast.ExprStmt's doc for why the grammar needs this shape
			// distinct from `do` (which is proc-to-proc, not builtin-to-proc).
			ex, err := parseExpr(t, c.Line.No)
			if err != nil {
				return nil, err
			}
			call, ok := ex.(ast.Call)
			if !ok {
				return nil, &Error{c.Line.No, fmt.Sprintf("%q is not a valid statement on its own", t)}
			}
			body = append(body, ast.ExprStmt{Call: call, Line: c.Line.No})
		default:
			eq := strings.IndexByte(t, '=')
			if eq < 0 {
				return nil, &Error{c.Line.No, fmt.Sprintf("unknown statement %q in %s — expected let/return/do/act/read/write/spawn/join/loop/if/break/continue, or a reassignment (`name = expr`)", firstWord(t), ctx)}
			}
			target := strings.TrimSpace(t[:eq])
			val, err := parseExpr(strings.TrimSpace(t[eq+1:]), c.Line.No)
			if err != nil {
				return nil, err
			}
			// `xs[i] = expr` — an indexed array mutation, told apart from a plain
			// reassignment by the target ending in `]`. Parsed as its own statement
			// shape (ast.IndexAssign) rather than folded into ast.Assign, since it
			// carries an extra sub-expression (the index) that a bare name target
			// never has.
			if strings.HasSuffix(target, "]") {
				lb := strings.IndexByte(target, '[')
				arrName := strings.TrimSpace(target[:lb])
				if lb < 0 || !isIdent(arrName) {
					return nil, &Error{c.Line.No, fmt.Sprintf("invalid assignment target %q", target)}
				}
				idx, err := parseExpr(target[lb+1:len(target)-1], c.Line.No)
				if err != nil {
					return nil, err
				}
				body = append(body, ast.IndexAssign{Target: arrName, Index: idx, Value: val, Line: c.Line.No})
				continue
			}
			if !isIdent(target) {
				return nil, &Error{c.Line.No, fmt.Sprintf("invalid assignment target %q", target)}
			}
			body = append(body, ast.Assign{Target: target, Value: val, Line: c.Line.No})
		}
	}
	return body, nil
}

// parseCall parses `Service.op(arg, ...)` — a service call statement in an action.
func parseCall(s string, line int) (ast.ServiceCall, error) {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return ast.ServiceCall{}, &Error{line, "call needs arguments: call Service.op(args)"}
	}
	target := strings.TrimSpace(s[:open])
	dot := strings.IndexByte(target, '.')
	if dot < 0 {
		return ast.ServiceCall{}, &Error{line, "call must name a service operation: call Service.op(args)"}
	}
	svc, op := strings.TrimSpace(target[:dot]), strings.TrimSpace(target[dot+1:])
	if !isIdent(svc) || !isIdent(op) {
		return ast.ServiceCall{}, &Error{line, fmt.Sprintf("invalid service call %q", target)}
	}
	closeP := strings.LastIndexByte(s, ')')
	if closeP < open {
		return ast.ServiceCall{}, &Error{line, "missing `)` in call"}
	}
	cl := ast.ServiceCall{Service: svc, Op: op, Line: line}
	if inner := strings.TrimSpace(s[open+1 : closeP]); inner != "" {
		for _, a := range splitTop(inner, ',') {
			e, err := parseExpr(strings.TrimSpace(a), line)
			if err != nil {
				return ast.ServiceCall{}, err
			}
			cl.Args = append(cl.Args, e)
		}
	}
	return cl, nil
}

// parseCheck parses a `check <expr> "message"` clause: a boolean precondition
// followed by the friendly message shown when it fails. The message is the
// trailing quoted string; everything before it is the condition.
func parseCheck(s string, line int) (ast.Check, error) {
	s = strings.TrimSpace(s)
	if !strings.HasSuffix(s, `"`) {
		return ast.Check{}, &Error{line, `check needs a message: check <cond> "why it failed"`}
	}
	// scan back from the closing quote to its (unescaped) opener.
	open := -1
	for i := len(s) - 2; i >= 0; i-- {
		if s[i] == '"' && (i == 0 || s[i-1] != '\\') {
			open = i
			break
		}
	}
	if open < 0 {
		return ast.Check{}, &Error{line, "unterminated message string in check"}
	}
	msg, err := unquote(s[open:], line)
	if err != nil {
		return ast.Check{}, err
	}
	condSrc := strings.TrimSpace(s[:open])
	if condSrc == "" {
		return ast.Check{}, &Error{line, "check needs a condition before its message"}
	}
	cond, err := parseExpr(condSrc, line)
	if err != nil {
		return ast.Check{}, err
	}
	return ast.Check{Cond: cond, Msg: msg, Line: line}, nil
}

// parseRequire parses one `requires` clause: a bare policy name (`admin`) or a
// policy call with argument expressions (`owns(id)`).
func parseRequire(s string, line int) (ast.Require, error) {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		if !isIdent(s) {
			return ast.Require{}, &Error{line, fmt.Sprintf("invalid policy name %q in requires", s)}
		}
		return ast.Require{Name: s, Line: line}, nil
	}
	name := strings.TrimSpace(s[:open])
	if !isIdent(name) {
		return ast.Require{}, &Error{line, fmt.Sprintf("invalid policy name %q in requires", name)}
	}
	close := strings.LastIndexByte(s, ')')
	if close < open {
		return ast.Require{}, &Error{line, "missing `)` in requires clause"}
	}
	req := ast.Require{Name: name, Line: line}
	inner := strings.TrimSpace(s[open+1 : close])
	if inner != "" {
		for _, a := range splitTop(inner, ',') {
			e, err := parseExpr(strings.TrimSpace(a), line)
			if err != nil {
				return ast.Require{}, err
			}
			req.Args = append(req.Args, e)
		}
	}
	return req, nil
}

// parseJob: `job Name every 30s -> action` and/or `job Name on start -> action`.
// The schedule clause is one of `every <N>s`, `on start`, or both
// (`every 30s on start`). The arrow names the zero-arg action to run.
func parseJob(n *source.Node) (*ast.Job, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "job"))
	arrow := strings.Index(rest, "->")
	if arrow < 0 {
		return nil, &Error{n.Line.No, "job needs an action: `job Name every 30s -> action`"}
	}
	action := strings.TrimSpace(rest[arrow+2:])
	if !isIdent(action) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid job action %q", action)}
	}
	head := strings.Fields(strings.TrimSpace(rest[:arrow]))
	if len(head) == 0 {
		return nil, &Error{n.Line.No, "job needs a name: `job Name every 30s -> action`"}
	}
	job := &ast.Job{Name: head[0], Action: action, Line: n.Line.No}
	if !isIdent(job.Name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid job name %q", job.Name)}
	}
	for i := 1; i < len(head); i++ {
		switch head[i] {
		case "every":
			if i+1 >= len(head) {
				return nil, &Error{n.Line.No, "`every` needs a duration like `every 30s`"}
			}
			i++
			secs, err := parseDuration(head[i], n.Line.No)
			if err != nil {
				return nil, err
			}
			job.Every = secs
		case "on":
			if i+1 >= len(head) || head[i+1] != "start" {
				return nil, &Error{n.Line.No, "`on` must be `on start`"}
			}
			i++
			job.OnStart = true
		default:
			return nil, &Error{n.Line.No, fmt.Sprintf("unexpected %q in job schedule (use `every Ns` or `on start`)", head[i])}
		}
	}
	if job.Every == 0 && !job.OnStart {
		return nil, &Error{n.Line.No, "job needs a schedule: `every Ns` and/or `on start`"}
	}
	return job, nil
}

// parseDuration reads `30s` or `5m` or `2h` into seconds.
func parseDuration(s string, line int) (int, error) {
	if len(s) < 2 {
		return 0, &Error{line, fmt.Sprintf("invalid duration %q (use 30s, 5m, 2h)", s)}
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, &Error{line, fmt.Sprintf("invalid duration %q (use 30s, 5m, 2h)", s)}
	}
	switch unit {
	case 's':
		return n, nil
	case 'm':
		return n * 60, nil
	case 'h':
		return n * 3600, nil
	default:
		return 0, &Error{line, fmt.Sprintf("unknown duration unit in %q (use s, m, h)", s)}
	}
}

// parseAfter parses `after 5s: cell = false` (ast.After): a client-only,
// fire-once timer nested in a view/component body. The duration reuses
// parseDuration — job/daemon's exact `every 30s`/`5m`/`2h` grammar — rather
// than inventing a second one. The body is one or more plain cell
// assignments, parsed the same way an action body's trailing `target = expr`
// arm already is (parseAction's default case, below); every other action-body
// statement shape (check/requires/add/set/remove/clear/call/do/let/establish)
// is refused here with a diagnostic naming why, since a client timer body can
// only ever run the one thing runClient (assets/facet.js) already knows how
// to execute with no round trip to the authority.
func parseAfter(n *source.Node) (ast.After, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "after")), ":")
	if head == "" {
		return ast.After{}, &Error{n.Line.No, "after needs a duration: `after 5s: cell = false`"}
	}
	secs, err := parseDuration(head, n.Line.No)
	if err != nil {
		return ast.After{}, err
	}
	if len(n.Children) == 0 {
		return ast.After{}, &Error{n.Line.No, "after has no body: `after 5s: cell = false`"}
	}
	var body []ast.Stmt
	for _, c := range n.Children {
		t := c.Line.Text
		switch {
		case strings.HasPrefix(t, "check "), strings.HasPrefix(t, "requires "),
			strings.HasPrefix(t, "add "), strings.HasPrefix(t, "set "),
			strings.HasPrefix(t, "remove "), strings.HasPrefix(t, "clear "),
			strings.HasPrefix(t, "call "), strings.HasPrefix(t, "do "),
			strings.HasPrefix(t, "let "), strings.HasPrefix(t, "establish "):
			return ast.After{}, &Error{c.Line.No, fmt.Sprintf(
				"after body can only assign a @client cell (`name = expr`) — %q needs the authority (entity/service access, validation, identity) and cannot run in a client-side timer; put it in an action instead", firstWord(t))}
		default:
			eq := strings.IndexByte(t, '=')
			if eq < 0 {
				return ast.After{}, &Error{c.Line.No, fmt.Sprintf("after body can only assign a @client cell (`name = expr`), not %q", firstWord(t))}
			}
			target := strings.TrimSpace(t[:eq])
			if !isIdent(target) {
				return ast.After{}, &Error{c.Line.No, fmt.Sprintf("invalid assignment target %q", target)}
			}
			val, err := parseExpr(strings.TrimSpace(t[eq+1:]), c.Line.No)
			if err != nil {
				return ast.After{}, err
			}
			body = append(body, ast.Assign{Target: target, Value: val, Line: c.Line.No})
		}
	}
	return ast.After{Seconds: secs, Body: body, Line: n.Line.No}, nil
}

// parseDaemon parses a detached, process-lifetime background task:
//
//	daemon Heartbeat every 2s:
//	    act Tick()
//
//	daemon Listener uses io.net:
//	    loop true:
//	        ...
//
// The optional `every <duration>` clause and the optional `uses <capabilities>`
// clause may appear in either order before the header's `:`, mirroring how
// parseProc pulls `uses` off a proc header and parseJob reads `every`. Omitting
// `every` means Body runs exactly once, in its own goroutine, for the life of
// the process (see ast.Daemon's doc) — the shape a self-looping body wants.
func parseDaemon(n *source.Node) (*ast.Daemon, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "daemon")), ":")
	var uses []string
	if i := strings.Index(head, " uses "); i >= 0 {
		usesPart := strings.TrimSpace(head[i+len(" uses "):])
		head = strings.TrimSpace(head[:i])
		if usesPart == "" {
			return nil, &Error{n.Line.No, "uses needs at least one capability (e.g. `uses io.file`)"}
		}
		for _, c := range splitTop(usesPart, ',') {
			c = strings.TrimSpace(c)
			if !isCapabilityName(c) {
				return nil, &Error{n.Line.No, fmt.Sprintf("invalid capability %q in uses clause (expected a dotted name like io.file)", c)}
			}
			uses = append(uses, c)
		}
	}
	every := 0
	if i := strings.Index(head, " every "); i >= 0 {
		durS := strings.TrimSpace(head[i+len(" every "):])
		head = strings.TrimSpace(head[:i])
		secs, err := parseDuration(durS, n.Line.No)
		if err != nil {
			return nil, err
		}
		every = secs
	}
	name := strings.TrimSpace(head)
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid daemon name %q", name)}
	}
	d := &ast.Daemon{Name: name, Every: every, Uses: uses, Line: n.Line.No}
	body, err := parseProcBody(n.Children, fmt.Sprintf("daemon %q", name))
	if err != nil {
		return nil, err
	}
	d.Body = body
	if len(d.Body) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("daemon %q has no body", name)}
	}
	return d, nil
}

// parseSignature parses `name(p: T, ...)`. allowList permits list-typed params
// (`p: [T]`) — used for service operations, whose ops genuinely take collections
// (`rank(posts: [int])`), and for a proc, which may compose with another proc's
// list-typed return the same way (`buildTree(lines: [text])`); action/component/
// policy params stay scalar. allowRef admits the by-reference parameter forms a
// component may declare — `p: cell T` (a state cell) and `p: action` (an action)
// — which bind to a NAME at the call site rather than to a value.
func parseSignature(head string, line int, allowList, allowRef bool) (string, []ast.Param, error) {
	open := strings.IndexByte(head, '(')
	if open < 0 {
		if !isIdent(head) {
			return "", nil, &Error{line, fmt.Sprintf("invalid action name %q", head)}
		}
		return head, nil, nil
	}
	name := strings.TrimSpace(head[:open])
	if !isIdent(name) {
		return "", nil, &Error{line, fmt.Sprintf("invalid action name %q", name)}
	}
	close := strings.LastIndexByte(head, ')')
	if close < open {
		return "", nil, &Error{line, "missing `)` in action signature"}
	}
	var params []ast.Param
	inner := strings.TrimSpace(head[open+1 : close])
	if inner != "" {
		for _, p := range strings.Split(inner, ",") {
			colon := strings.IndexByte(p, ':')
			if colon < 0 {
				return "", nil, &Error{line, fmt.Sprintf("parameter %q needs a type", strings.TrimSpace(p))}
			}
			pn := strings.TrimSpace(p[:colon])
			pt := strings.TrimSpace(p[colon+1:])
			if !isIdent(pn) {
				return "", nil, &Error{line, fmt.Sprintf("invalid parameter %q", strings.TrimSpace(p))}
			}
			ref := ast.RefValue
			if allowRef {
				switch {
				case pt == "action":
					ref = ast.RefAction
				case strings.HasPrefix(pt, "cell "):
					ref, pt = ast.RefCell, strings.TrimSpace(pt[len("cell "):])
				}
			}
			if ref == ast.RefAction {
				params = append(params, ast.Param{Name: pn, Ref: ref})
				continue
			}
			core, list, optional := splitType(pt)
			// A reference parameter names a declaration, so the two modifiers a value
			// parameter carries do not apply: a cell is or is not a list because the
			// cell it names is, and there is no such thing as an absent name.
			if ref == ast.RefCell && optional {
				return "", nil, &Error{line, fmt.Sprintf("cell parameter %q cannot be optional — it names a state cell, which either exists or does not", pn)}
			}
			if list && !allowList && ref != ast.RefCell {
				return "", nil, &Error{line, fmt.Sprintf("parameter %q cannot be a list", pn)}
			}
			if !isTypeName(core) {
				return "", nil, &Error{line, fmt.Sprintf("invalid parameter %q", strings.TrimSpace(p))}
			}
			params = append(params, ast.Param{Name: pn, Type: core, List: list, Optional: optional, Ref: ref})
		}
	}
	return name, params, nil
}

func parseAdd(n *source.Node) (ast.Stmt, error) {
	rest := strings.TrimSpace(n.Line.Text[len("add "):])
	rest, err := closeAddRecord(rest, n)
	if err != nil {
		return nil, err
	}
	open := strings.IndexByte(rest, '{')
	close := strings.LastIndexByte(rest, '}')
	if open < 0 || close < open {
		return nil, &Error{n.Line.No, "add needs a record: `add Entity { f: expr, ... }`"}
	}
	ent := strings.TrimSpace(rest[:open])
	if !isIdent(ent) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid entity %q", ent)}
	}
	add := ast.Add{Entity: ent, Line: n.Line.No}
	body := strings.TrimSpace(rest[open+1 : close])
	if body != "" {
		for _, part := range splitTop(body, ',') {
			colon := strings.IndexByte(part, ':')
			if colon < 0 {
				return nil, &Error{n.Line.No, fmt.Sprintf("field init %q needs `name: expr`", part)}
			}
			fn := strings.TrimSpace(part[:colon])
			e, err := parseExpr(strings.TrimSpace(part[colon+1:]), n.Line.No)
			if err != nil {
				return nil, err
			}
			add.Fields = append(add.Fields, ast.FieldInit{Name: fn, Expr: e})
		}
	}
	return add, nil
}

// closeAddRecord reassembles the raw text of an `add Entity { ... }` record
// literal that continues past its header line. FDL's offside rule already
// nests any line indented under `add`'s header as a child of it — the same
// tree shape `if`/`for`/`policy` use for their bodies — so when the header's
// own text doesn't yet balance its opening `{`, the record's remaining fields
// (and its closing `}`) are sitting right there as descendants of n, in
// source order. This walks them depth-first, space-joining each line's text
// onto rest until the running brace/paren/bracket depth returns to zero, and
// hands back the reassembled one-line text for the existing single-line
// record parsing in parseAdd to work on unchanged.
//
// A line whose header already balances (the common single-line case) is
// returned untouched — no children are consumed, so an accidental over-indent
// after a complete `add` can't get silently absorbed into it. A record that
// never closes, even after every descendant line is consumed, is reported
// against the `add` line rather than swallowed: n.Children is a fixed,
// finite list, so this always terminates and never reaches past add's own
// nested lines into whatever statement follows it.
func closeAddRecord(rest string, n *source.Node) (string, error) {
	depth := braceDepth(rest, 0)
	if depth <= 0 {
		return rest, nil
	}
	for _, ln := range flattenLines(n) {
		rest = rest + " " + ln.Text
		depth = braceDepth(ln.Text, depth)
		if depth <= 0 {
			return rest, nil
		}
	}
	return "", &Error{n.Line.No, "add's record literal is missing its closing `}` (indent the fields, and the closing `}`, under `add` — the same offside rule as a block's body)"}
}

// flattenLines returns n's descendant lines, depth-first, in source order —
// the order they appeared in the file, regardless of how deep the offside
// rule nested each one.
func flattenLines(n *source.Node) []source.Line {
	var out []source.Line
	for _, c := range n.Children {
		out = append(out, c.Line)
		out = append(out, flattenLines(c)...)
	}
	return out
}

// braceDepth folds s's `(`/`{`/`[` and `)`/`}`/`]` into depth (starting from
// start), skipping the inside of "..." string literals exactly as splitTop
// does, so a brace quoted in a field's text value never perturbs where the
// record literal actually closes.
func braceDepth(s string, start int) int {
	depth := start
	inStr := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		}
	}
	return depth
}

// parseSet parses the two forms of a write to stored rows:
//
//	set Entity(key).field = expr          one row, addressed by id
//	set item in Entity where cond:        every row the predicate accepts
//	    field = expr
//	    ...
//
// The filtered form is `remove item in Entity where cond`'s shape carrying
// assignments instead of a deletion, and it is detected the same way — an ` in `
// ahead of a ` where ` — so the two bulk statements are told apart by their
// keyword alone. Its body is a block because a bulk update usually touches more
// than one column and one traversal should do all of them.
func parseSet(n *source.Node) (ast.Stmt, error) {
	rest := strings.TrimSpace(n.Line.Text[len("set "):])
	if wi := indexOutside(rest, " where "); wi >= 0 && indexOutside(rest[:wi], " in ") >= 0 {
		return parseSetWhere(n, rest, wi)
	}
	eq := indexAssign(rest)
	if eq < 0 {
		return nil, &Error{n.Line.No, "set needs `set Entity(key).field = expr` or `set item in Entity where cond:`"}
	}
	lhs := strings.TrimSpace(rest[:eq])
	val, err := parseExpr(strings.TrimSpace(rest[eq+1:]), n.Line.No)
	if err != nil {
		return nil, err
	}
	ent, key, field, err := parseEntityPath(lhs, n.Line.No)
	if err != nil {
		return nil, err
	}
	return ast.Set{Entity: ent, Key: key, Field: field, Value: val, Line: n.Line.No}, nil
}

// parseSetWhere parses `set item in Entity where cond:` plus its block of
// `field = expr` assignments. wi is the index of the ` where ` that separates the
// header from the predicate.
func parseSetWhere(n *source.Node, rest string, wi int) (ast.Stmt, error) {
	head := strings.TrimSpace(rest[:wi])
	cond := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest[wi+len(" where "):]), ":"))
	hf := strings.Fields(head)
	if len(hf) != 3 || hf[1] != "in" || !isIdent(hf[0]) || !isIdent(hf[2]) {
		return nil, &Error{n.Line.No, "set filter is `set item in Entity where cond:`"}
	}
	if !strings.HasSuffix(strings.TrimSpace(n.Line.Text), ":") {
		return nil, &Error{n.Line.No, "a filtered set takes a block of assignments: end the line with `:` and indent `field = expr` under it"}
	}
	where, err := parseExpr(cond, n.Line.No)
	if err != nil {
		return nil, err
	}
	st := ast.Set{Entity: hf[2], Var: hf[0], Where: where, Line: n.Line.No}
	for _, c := range n.Children {
		t := strings.TrimSpace(c.Line.Text)
		eq := indexAssign(t)
		if eq < 0 {
			return nil, &Error{c.Line.No, fmt.Sprintf("a filtered set's body is `field = expr`, got %q", t)}
		}
		name := strings.TrimSpace(t[:eq])
		if !isIdent(name) {
			return nil, &Error{c.Line.No, fmt.Sprintf("invalid field %q in a filtered set", name)}
		}
		val, err := parseExpr(strings.TrimSpace(t[eq+1:]), c.Line.No)
		if err != nil {
			return nil, err
		}
		st.Fields = append(st.Fields, ast.FieldInit{Name: name, Expr: val})
	}
	if len(st.Fields) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("`set %s in %s where …` has no assignments — indent `field = expr` under it", hf[0], hf[2])}
	}
	return st, nil
}

func parseRemove(n *source.Node) (ast.Stmt, error) {
	rest := strings.TrimSpace(n.Line.Text[len("remove "):])
	// Filtered form: `remove <item> in <Entity> where <cond>` — delete all matches.
	if wi := indexOutside(rest, " where "); wi >= 0 && indexOutside(rest[:wi], " in ") >= 0 {
		hf := strings.Fields(strings.TrimSpace(rest[:wi]))
		if len(hf) != 3 || hf[1] != "in" || !isIdent(hf[0]) || !isIdent(hf[2]) {
			return nil, &Error{n.Line.No, "remove filter is `remove item in Entity where cond`"}
		}
		cond, err := parseExpr(strings.TrimSpace(rest[wi+len(" where "):]), n.Line.No)
		if err != nil {
			return nil, err
		}
		return ast.Remove{Entity: hf[2], Var: hf[0], Where: cond, Line: n.Line.No}, nil
	}
	// By-id form: `remove Entity(key)`.
	open := strings.IndexByte(rest, '(')
	close := strings.LastIndexByte(rest, ')')
	if open < 0 || close < open {
		return nil, &Error{n.Line.No, "remove needs `remove Entity(key)` or `remove item in Entity where cond`"}
	}
	ent := strings.TrimSpace(rest[:open])
	key, err := parseExpr(strings.TrimSpace(rest[open+1:close]), n.Line.No)
	if err != nil {
		return nil, err
	}
	return ast.Remove{Entity: ent, Key: key, Line: n.Line.No}, nil
}

// parseEntityPath parses `Entity(key).field`.
//
// The key is a whole expression, so its parentheses are matched rather than
// counted to the first `)` — `Product(CartLine(lid).product).stock` is one
// lookup inside another, exactly as the expression parser reads it in a `check`.
func parseEntityPath(s string, line int) (string, ast.Expr, string, error) {
	open := strings.IndexByte(s, '(')
	close := -1
	if open >= 0 {
		close = matchParen(s, open)
	}
	if open < 0 || close < open {
		return "", nil, "", &Error{line, fmt.Sprintf("expected Entity(key).field, got %q", s)}
	}
	ent := strings.TrimSpace(s[:open])
	key, err := parseExpr(strings.TrimSpace(s[open+1:close]), line)
	if err != nil {
		return "", nil, "", err
	}
	tail := strings.TrimSpace(s[close+1:])
	if !strings.HasPrefix(tail, ".") {
		return "", nil, "", &Error{line, fmt.Sprintf("expected .field after Entity(key), got %q", tail)}
	}
	field := strings.TrimSpace(tail[1:])
	if !isIdent(field) {
		return "", nil, "", &Error{line, fmt.Sprintf("invalid field %q", field)}
	}
	return ent, key, field, nil
}

// parseView: `view Name [at "/path"] [in Layout] [requires policy]:` then a node
// tree. A path segment of the form `:name` is a dynamic parameter bound in scope.
func parseView(n *source.Node) (*ast.View, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "view")), ":")
	v := &ast.View{Line: n.Line.No}

	// `requires policy` and `in Layout` are trailing clauses; pull them off first.
	if i := strings.Index(head, " requires "); i >= 0 {
		v.Requires = strings.TrimSpace(head[i+len(" requires "):])
		if !isIdent(v.Requires) {
			return nil, &Error{n.Line.No, fmt.Sprintf("invalid route guard policy %q", v.Requires)}
		}
		head = strings.TrimSpace(head[:i])
	}
	if i := strings.Index(head, " in "); i >= 0 {
		v.Layout = strings.TrimSpace(head[i+len(" in "):])
		if !isIdent(v.Layout) || !isUpper(v.Layout) {
			return nil, &Error{n.Line.No, fmt.Sprintf("invalid layout name %q", v.Layout)}
		}
		head = strings.TrimSpace(head[:i])
	}

	name := head
	if i := strings.Index(head, " at "); i >= 0 {
		name = strings.TrimSpace(head[:i])
		p, err := unquote(strings.TrimSpace(head[i+len(" at "):]), n.Line.No)
		if err != nil {
			return nil, err
		}
		v.Path = p
		params, err := pathParams(p, n.Line.No)
		if err != nil {
			return nil, err
		}
		v.Params = params
	}
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid view name %q", name)}
	}
	v.Name = name
	// `meta title "…"` / `meta description "…"` are page-metadata directives, not
	// view nodes — pull them off before parsing the node tree.
	var body []*source.Node
	for _, c := range n.Children {
		t := strings.TrimSpace(c.Line.Text)
		switch {
		case strings.HasPrefix(t, "meta title "):
			segs, err := parseText(strings.TrimSpace(t[len("meta title "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			v.TitleSegs = segs
		case strings.HasPrefix(t, "meta description "):
			segs, err := parseText(strings.TrimSpace(t[len("meta description "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			v.DescSegs = segs
		case t == "meta" || strings.HasPrefix(t, "meta "):
			return nil, &Error{c.Line.No, `meta takes title or description: meta title "…"`}
		default:
			body = append(body, c)
		}
	}
	nodes, err := parseNodes(body)
	if err != nil {
		return nil, err
	}
	v.Root = nodes
	return v, nil
}

// pathParams extracts the `:name` dynamic segments of a route, validating each is
// an identifier and unique.
func pathParams(path string, line int) ([]string, error) {
	var params []string
	seen := map[string]bool{}
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if !strings.HasPrefix(seg, ":") {
			continue
		}
		name := seg[1:]
		if !isIdent(name) {
			return nil, &Error{line, fmt.Sprintf("invalid route parameter %q", seg)}
		}
		if seen[name] {
			return nil, &Error{line, fmt.Sprintf("duplicate route parameter %q", name)}
		}
		seen[name] = true
		params = append(params, name)
	}
	return params, nil
}

func parseNodes(children []*source.Node) ([]ast.Node, error) {
	var out []ast.Node
	for _, c := range children {
		// Pull any trailing `class "..."` / `style "..."` / `anchor "..."` modifiers
		// off the line first (mutating c so block parsers that re-read it see the clean
		// line), then wrap the parsed node once it lands.
		class, style, anchor, err := stripNodeMods(c)
		if err != nil {
			return nil, err
		}
		t := c.Line.Text
		before := len(out)
		switch {
		case t == "box:" || t == "box":
			kids, err := parseNodes(c.Children)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Box{Children: kids})
		case t == "row:" || t == "row":
			kids, err := parseNodes(c.Children)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Row{Children: kids})
		case strings.HasPrefix(t, "text "):
			segs, err := parseText(strings.TrimSpace(t[len("text "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Text{Segs: segs})
		case strings.HasPrefix(t, "heading "):
			h, err := parseHeading(strings.TrimSpace(t[len("heading "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, h)
		case strings.HasPrefix(t, "image "):
			segs, alt, altSet, err := parseMedia(strings.TrimSpace(t[len("image "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Image{Segs: segs, Alt: alt, AltSet: altSet, Line: c.Line.No})
		case strings.HasPrefix(t, "button "):
			b, err := parseButton(strings.TrimSpace(t[len("button "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		case strings.HasPrefix(t, "icon "):
			segs, err := parseText(strings.TrimSpace(t[len("icon "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Icon{Segs: segs})
		case strings.HasPrefix(t, "video "):
			v, err := parseVideo(strings.TrimSpace(t[len("video "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		case strings.HasPrefix(t, "richtext "):
			segs, err := parseText(strings.TrimSpace(t[len("richtext "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Richtext{Segs: segs})
		case strings.HasPrefix(t, "badge "):
			segs, err := parseText(strings.TrimSpace(t[len("badge "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Badge{Segs: segs})
		case strings.HasPrefix(t, "tabs "):
			tb, err := parseTabs(c)
			if err != nil {
				return nil, err
			}
			out = append(out, tb)
		case strings.HasPrefix(t, "match "):
			m, err := parseMatch(c)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		case strings.HasPrefix(t, "for "):
			f, err := parseFor(c)
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		case strings.HasPrefix(t, "stage "):
			st, err := parseStage(c)
			if err != nil {
				return nil, err
			}
			out = append(out, st)
		case strings.HasPrefix(t, "if "):
			cond, err := parseExpr(strings.TrimSuffix(strings.TrimSpace(t[len("if "):]), ":"), c.Line.No)
			if err != nil {
				return nil, err
			}
			kids, err := parseNodes(c.Children)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.If{Cond: cond, Body: kids})
		case strings.HasPrefix(t, "after "):
			af, err := parseAfter(c)
			if err != nil {
				return nil, err
			}
			out = append(out, af)
		case strings.HasPrefix(t, "overlay "):
			ov, err := parseOverlay(c)
			if err != nil {
				return nil, err
			}
			out = append(out, ov)
		case strings.HasPrefix(t, "popover "):
			// `before` is how many nodes this same block has already emitted, in
			// document order, independent of any `class`/`style`/`anchor` a
			// sibling carries — which is exactly "does a previous sibling exist
			// to anchor beside." Checked here, not in internal/ir/build.go: a
			// `class`-modified popover is lowered through a nested, single-node
			// call there (see ast.Modified), so that call's own node list is
			// always empty and cannot answer this question.
			if before == 0 {
				return nil, &Error{c.Line.No, "popover has no previous sibling to anchor beside — write it directly after the button (or other element) it opens beside"}
			}
			pv, err := parsePopover(c)
			if err != nil {
				return nil, err
			}
			out = append(out, pv)
		case strings.HasPrefix(t, "typeahead "):
			ta, err := parseTypeahead(strings.TrimSpace(t[len("typeahead "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, ta)
		case ast.Controls[firstWord(t)].IRKind != "":
			ctl, err := parseControl(firstWord(t), c)
			if err != nil {
				return nil, err
			}
			out = append(out, ctl)
		case strings.HasPrefix(t, "input "):
			in, err := parseInput(strings.TrimSpace(t[len("input "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, in)
		case strings.HasPrefix(t, "link "):
			l, err := parseLink(strings.TrimSpace(t[len("link "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, l)
		case strings.HasPrefix(t, "select "):
			sel, err := parseSelect(c)
			if err != nil {
				return nil, err
			}
			out = append(out, sel)
		case strings.HasPrefix(t, "form "):
			f, err := parseForm(c)
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		case strings.HasPrefix(t, "upload "):
			u, err := parseUpload(strings.TrimSpace(t[len("upload "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, u)
		case strings.HasPrefix(t, "use "):
			u, err := parseUse(strings.TrimSpace(t[len("use "):]), c.Line.No, c.Children)
			if err != nil {
				return nil, err
			}
			out = append(out, u)
		case t == "slot" || t == "slot:":
			out = append(out, ast.Slot{})
		case strings.HasPrefix(t, "slot "):
			name := strings.TrimSuffix(strings.TrimSpace(t[len("slot "):]), ":")
			if !isIdent(name) {
				return nil, &Error{c.Line.No, fmt.Sprintf("invalid socket name %q after `slot`", name)}
			}
			out = append(out, ast.SlotRef{Name: name})
		default:
			return nil, unknownViewNodeError(c.Line.No, firstWord(t))
		}
		// A node carrying a modifier is decorated in place. The cases above each emit
		// exactly one node, so wrapping the just-appended one is unambiguous.
		if (len(class) > 0 || style != "" || anchor != "") && len(out) == before+1 {
			out[before] = ast.Modified{Class: class, Style: style, Anchor: anchor, Inner: out[before], Line: c.Line.No}
		}
	}
	return out, nil
}

// unknownViewNodeError builds the diagnostic for a view-tree line whose
// leading word matched none of parseNodes' cases. `unknown view node "box"`
// on its own says only what was rejected, not what would have been valid —
// close to useless when the real cause is e.g. a stale toolchain missing a
// node the source file assumes exists. The "expected one of" list appended
// here is never hand-typed (a hand-typed copy is exactly how the bare message
// above went stale in the first place): it is read live off
// viewNodeKeywords, the same technique `facet lang` (cmd/facet/lang.go) uses
// to keep its own reference tables from drifting out of sync with the
// compiler.
func unknownViewNodeError(line int, word string) *Error {
	kws := viewNodeKeywords()
	if len(kws) == 0 {
		return &Error{line, fmt.Sprintf("unknown view node %q — not a recognized node, control, or block keyword (run `facet lang` to list them)", word)}
	}
	return &Error{line, fmt.Sprintf("unknown view node %q — expected one of: %s, or a component name (see `use`)", word, strings.Join(kws, ", "))}
}

// viewNodeKeywords returns every leading word parseNodes' switch above
// recognizes as a view node, control, or block keyword: the control keywords
// come straight from ast.Controls, the same live map the switch dispatches on
// via `ast.Controls[firstWord(t)].IRKind != ""`; the rest (box, row, text, ...)
// are read out of this file's own parseNodes switch by
// switchCaseKeywords("parseNodes"). Deriving both sets from the code that
// actually decides, instead of retyping them, is what keeps this list from
// silently falling behind as node kinds are added or renamed.
func viewNodeKeywords() []string {
	nodeKws, ok := switchCaseKeywords("parseNodes")
	if !ok {
		// Half a list is worse than none: ast.Controls alone would look like
		// a complete "expected one of" answer while silently missing every
		// plain node keyword (box, row, text, link, ...). See
		// switchCaseKeywords' doc comment — this is the exact case it warns
		// about, and it is the normal case for every binary this project
		// ships (scripts/build.sh always passes -trimpath).
		return nil
	}
	seen := make(map[string]bool)
	for k := range ast.Controls {
		seen[k] = true
	}
	for _, k := range nodeKws {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// switchCaseKeywords derives the leading keywords a line-dispatch switch of
// the shape used throughout this file (`switch { case strings.HasPrefix(t,
// "word "): ... case t == "word" || t == "word:": ... }`) branches on, by
// parsing this very source file and reading the string literals compared
// against the switch's dispatch variable inside the named function.
//
// This mirrors `facet lang` (cmd/facet/lang.go's nodeKinds/builtinNames):
// rather than hand-listing the keywords a second time — a copy that could
// silently drift from the switch as cases are added, renamed or removed, the
// way `unknown view node %q` already had — it reads the literals the code
// actually branches on. Like `facet lang`, it degrades gracefully: if the
// source is not available (e.g. a `facet` binary run somewhere its checkout's
// internal/parser is not, per runtime.Caller(0)'s documented caveats), it
// returns nil rather than a possibly-stale guess, and callers fall back to a
// shorter message.
//
// The second return is false whenever the source could not be read at all —
// distinct from "this function's switch genuinely dispatches on nothing,"
// which cannot happen for parseNodes/parseAction. Callers with a second,
// always-available source of keywords (viewNodeKeywords' ast.Controls) MUST
// check it before merging: `-trimpath` (scripts/build.sh's own flag, used for
// every binary this project ships) records this file's path as
// "facet/internal/parser/parser.go" rather than a real filesystem path, so
// ParseFile fails on every shipped release binary — and merging ast.Controls'
// keys in unconditionally there would silently print a partial "expected one
// of" list that OMITS every plain node keyword (box, row, text, link, ...)
// while looking complete, which is worse than the bare message this replaced.
// Confirmed by building with `-trimpath -ldflags="-s -w"` (scripts/build.sh's
// exact invocation) and observing exactly that silent truncation before this
// two-value return existed.
func switchCaseKeywords(funcName string) (kws []string, ok bool) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return nil, false
	}
	fset := gotoken.NewFileSet()
	f, err := goparser.ParseFile(fset, self, nil, 0)
	if err != nil {
		return nil, false
	}
	seen := make(map[string]bool)
	var out []string
	record := func(lit string) {
		s := strings.TrimSuffix(strings.TrimSpace(lit), ":")
		word, _, _ := strings.Cut(s, " ")
		if word != "" && !seen[word] {
			seen[word] = true
			out = append(out, word)
		}
	}
	goast.Inspect(f, func(n goast.Node) bool {
		fd, ok := n.(*goast.FuncDecl)
		if !ok || fd.Name.Name != funcName {
			return true
		}
		goast.Inspect(fd.Body, func(n goast.Node) bool {
			cc, ok := n.(*goast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range cc.List {
				goast.Inspect(expr, func(n goast.Node) bool {
					bl, ok := n.(*goast.BasicLit)
					if !ok || bl.Kind != gotoken.STRING {
						return true
					}
					if s, err := strconv.Unquote(bl.Value); err == nil && s != "" {
						record(s)
					}
					return true
				})
			}
			return true
		})
		return false
	})
	sort.Strings(out)
	return out, true
}

// stripNodeMods removes the trailing `class "..."`, `style "..."` and
// `anchor "..."` modifiers from a view node's line (in any order, e.g.
// `box class "rail" anchor "install"`), rewriting the node's line text to the
// clean base and returning the captured values. A bare `class`/`style`/`anchor`
// word that is part of a quoted literal (e.g. `text "my style"`) is left alone —
// only a `<keyword> "<value>"` pair at the very end of the line is taken.
func stripNodeMods(c *source.Node) (class []ast.Seg, style, anchor string, err error) {
	line := c.Line.Text

	// A block node's line ends in a colon — `box:`, `row:`, `for t in Tweet:` —
	// so on those the modifier is not at the end of the text at all, it is at the
	// end of the *head*. Without this split the escape hatch reached only leaf
	// nodes, which is close to useless: the nodes that need a class are the ones
	// that lay something out, and every one of those opens a block. Writing
	// `box class "rail":` did not style a box, it failed to parse as one.
	//
	// The colon is put back on the rewritten line so the block parsers that
	// re-read it are unaffected.
	head, colon := line, ""
	if trimmed := strings.TrimRight(line, " "); strings.HasSuffix(trimmed, ":") {
		head, colon = trimmed[:len(trimmed)-1], ":"
	}

	line = head

	for {
		kw, val, rest, ok := trailingNodeMod(line)
		if !ok {
			break
		}
		switch kw {
		case "class":
			// A class value interpolates like any other author-visible text; only
			// `style` stays literal. See ast.Modified.
			if len(class) == 0 {
				segs, perr := parseTextBody(val, c.Line.No)
				if perr != nil {
					return nil, "", "", perr
				}
				class = segs
			}
		case "style":
			if style == "" {
				style = val
			}
		case "anchor":
			// An anchor is a name a link spells back as `#name`, so it is checked
			// here rather than left to render as a fragment nothing can reach. The
			// character set is the one that survives a URL fragment, an HTML `id` and
			// a CSS selector unescaped, so there is exactly one spelling of it.
			if !isAnchorName(val) {
				return nil, "", "", &Error{c.Line.No, fmt.Sprintf(
					"invalid anchor %q: an anchor name is letters, digits, `-` and `_` — it is what a link spells back as `#name`", val)}
			}
			if anchor == "" {
				anchor = val
			}
		}
		line = rest
	}

	c.Line.Text = line + colon

	return class, style, anchor, nil
}

// isAnchorName reports whether s is usable as an author-chosen anchor id.
func isAnchorName(s string) bool {
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

// trailingNodeMod matches a `class "..."`, `style "..."` or `anchor "..."` pair at
// the end of line.
// On success it returns the keyword, the raw value, and the line with that pair
// removed.
//
// The opening quote is found by scanning the line forward — the first string
// literal whose *close* is the line's last byte — rather than by taking the last
// quote before it. Backwards, the last quote in
// `box class "x-avi-c{contains(name, "e")}"` is the one before `e`: the prefix
// then read `box class "x-avi-c{contains(name, ` , matched no keyword, no
// modifier was stripped, and the whole line fell through to
// `unknown view node "box"`. Forward, endOfQuoted knows the inner quote belongs
// to the interpolation's expression.
func trailingNodeMod(line string) (kw, val, rest string, ok bool) {
	s := strings.TrimRight(line, " ")
	if !strings.HasSuffix(s, `"`) {
		return "", "", "", false
	}
	open := -1
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		e := endOfQuoted(s, i)
		if e < 0 {
			return "", "", "", false // an unterminated string is not a modifier
		}
		if e == len(s)-1 {
			open = i
			break
		}
		i = e
	}
	if open < 0 {
		return "", "", "", false
	}
	prefix := strings.TrimRight(s[:open], " ")
	for _, k := range []string{"class", "style", "anchor"} {
		if strings.HasSuffix(prefix, k) {
			base := prefix[:len(prefix)-len(k)]
			// the keyword must stand as its own word (start of line or space before it)
			if base == "" {
				continue // a node line is never just `class "..."` with no node before it
			}
			if !strings.HasSuffix(base, " ") {
				continue
			}
			return k, s[open+1 : len(s)-1], strings.TrimRight(base, " "), true
		}
	}
	return "", "", "", false
}

// parseFor: `for item in Collection [where cond] [by field desc|asc] [limit n] [more action]:`
func parseFor(n *source.Node) (ast.Node, error) {
	rg, err := parseRange(strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len("for "):]), ":"), n.Line.No)
	if err != nil {
		return nil, err
	}
	kids, err := parseNodes(n.Children)
	if err != nil {
		return nil, err
	}
	return ast.For{Range: rg, Body: kids}, nil
}

// parseStage: `stage width <int> height <int>:` with an optional `tiles from
// <expr>` line and one or more `sprite for …` layers as children — the
// canvas-rendered scene node (see ast.Stage).
func parseStage(n *source.Node) (ast.Node, error) {
	head := strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len("stage "):]), ":")
	w, h, err := parseStageSize(head, n.Line.No)
	if err != nil {
		return nil, err
	}
	st := ast.Stage{Width: w, Height: h, Line: n.Line.No}
	for _, c := range n.Children {
		t := strings.TrimSpace(c.Line.Text)
		if len(c.Children) > 0 {
			return nil, &Error{c.Line.No, "a stage's `tiles`/`sprite` lines take no children — they are draw parameters, not a body"}
		}
		switch {
		case strings.HasPrefix(t, "tiles from "):
			if st.Tiles != nil {
				return nil, &Error{c.Line.No, "stage takes at most one `tiles from …`"}
			}
			e, err := parseExpr(strings.TrimSpace(t[len("tiles from "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			st.Tiles = e
		case strings.HasPrefix(t, "sprite "):
			sp, err := parseSprite(c)
			if err != nil {
				return nil, err
			}
			st.Sprites = append(st.Sprites, sp)
		default:
			return nil, &Error{c.Line.No, "stage children must be `tiles from <expr>` or `sprite for …`"}
		}
	}
	if st.Tiles == nil && len(st.Sprites) == 0 {
		return nil, &Error{n.Line.No, "stage needs at least a `tiles from …` or one `sprite for …`"}
	}
	return st, nil
}

// parseStageSize parses a stage's `width <int> height <int>` header, with the
// leading `stage ` keyword and trailing colon already stripped.
func parseStageSize(head string, line int) (int, int, error) {
	fields := strings.Fields(head)
	if len(fields) != 4 || fields[0] != "width" || fields[2] != "height" {
		return 0, 0, &Error{line, "stage needs `stage width <int> height <int>:`"}
	}
	w, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, &Error{line, fmt.Sprintf("stage width must be an integer, got %q", fields[1])}
	}
	h, err := strconv.Atoi(fields[3])
	if err != nil {
		return 0, 0, &Error{line, fmt.Sprintf("stage height must be an integer, got %q", fields[3])}
	}
	if w <= 0 || h <= 0 {
		return 0, 0, &Error{line, "stage width/height must be positive"}
	}
	return w, h, nil
}

// parseSprite: `sprite for <var> in <Coll> [where cond] [by field [desc|asc]]
// [limit n] at (x, y) [facing <expr>] image <expr> [label "…"]`.
//
// The range half (`for <var> in <Coll> …`) parses exactly as a `for`'s does —
// parseRange — because it is the same query over the same collections; only
// the tail differs, since a sprite draws a mark per row instead of rendering
// a node body.
func parseSprite(n *source.Node) (ast.Sprite, error) {
	line := n.Line.No
	rest := strings.TrimSpace(n.Line.Text[len("sprite "):])
	at := indexTop(rest, " at (")
	if at < 0 {
		return ast.Sprite{}, &Error{line, "sprite needs a position: sprite for x in Coll at (x, y) image <expr>"}
	}
	rangeHead := strings.TrimSpace(rest[:at])
	tail := strings.TrimSpace(rest[at+1:]) // "at (...) [facing ...] image ... [label ...]"
	if !strings.HasPrefix(rangeHead, "for ") {
		return ast.Sprite{}, &Error{line, "sprite needs `sprite for <var> in <Coll> …`"}
	}
	rg, err := parseRange(strings.TrimSpace(rangeHead[len("for "):]), line)
	if err != nil {
		return ast.Sprite{}, err
	}

	coordPart := tail[len("at "):] // "(x, y) [facing ...] image ... [label ...]"
	if len(coordPart) == 0 || coordPart[0] != '(' {
		return ast.Sprite{}, &Error{line, "sprite position must be `at (x, y)`"}
	}
	closeParen := matchParen(coordPart, 0)
	if closeParen < 0 {
		return ast.Sprite{}, &Error{line, "unclosed `(` in sprite position"}
	}
	coords := coordPart[1:closeParen]
	comma := indexTop(coords, ",")
	if comma < 0 {
		return ast.Sprite{}, &Error{line, "sprite position needs both coordinates: at (x, y)"}
	}
	xExpr, err := parseExpr(strings.TrimSpace(coords[:comma]), line)
	if err != nil {
		return ast.Sprite{}, err
	}
	yExpr, err := parseExpr(strings.TrimSpace(coords[comma+1:]), line)
	if err != nil {
		return ast.Sprite{}, err
	}

	after := strings.TrimSpace(coordPart[closeParen+1:]) // "[facing ...] image ... [label ...]"
	imgIdx := indexTop(after, "image ")
	if imgIdx < 0 {
		return ast.Sprite{}, &Error{line, "sprite needs an image: image <expr>"}
	}
	var facingExpr ast.Expr
	if facingPart := strings.TrimSpace(after[:imgIdx]); facingPart != "" {
		if !strings.HasPrefix(facingPart, "facing ") {
			return ast.Sprite{}, &Error{line, "sprite's only modifier before `image` is `facing <expr>`"}
		}
		facingExpr, err = parseExpr(strings.TrimSpace(facingPart[len("facing "):]), line)
		if err != nil {
			return ast.Sprite{}, err
		}
	}

	imageTail := strings.TrimSpace(after[imgIdx+len("image "):]) // "<expr> [label \"...\"]"
	var imageExpr ast.Expr
	var label []ast.Seg
	if li := indexTop(imageTail, "label "); li >= 0 {
		imageExpr, err = parseExpr(strings.TrimSpace(imageTail[:li]), line)
		if err != nil {
			return ast.Sprite{}, err
		}
		label, err = parseText(strings.TrimSpace(imageTail[li+len("label "):]), line)
		if err != nil {
			return ast.Sprite{}, err
		}
	} else {
		imageExpr, err = parseExpr(imageTail, line)
		if err != nil {
			return ast.Sprite{}, err
		}
	}
	return ast.Sprite{Range: rg, X: xExpr, Y: yExpr, Facing: facingExpr, Image: imageExpr, Label: label, Line: line}, nil
}

// parseRange parses the header every repeating construct shares —
// `item in Collection [where cond] [by field desc|asc] [limit n]` — with the
// leading keyword and the trailing colon already stripped.
//
// It is one function because there is now more than one thing that repeats: a
// `for` node and the `for` inside a select's or a radio group's option list are
// the same query over the same collections, and a second hand-written header
// parser is how one of them ends up without `limit`.
func parseRange(head string, line int) (ast.Range, error) {
	var rg ast.Range
	fields := strings.Fields(head)
	if len(fields) < 3 || fields[1] != "in" {
		return rg, &Error{line, "for needs `for item in Collection [where cond] [by field desc|asc] [limit n]:`"}
	}
	if !isIdent(fields[0]) || !isIdent(fields[2]) {
		return rg, &Error{line, fmt.Sprintf("invalid for clause %q", head)}
	}
	rg.Var, rg.Coll = fields[0], fields[2]

	// remainder after `<var> in <Coll>`
	rest := strings.TrimSpace(head[len(fields[0]):])
	rest = strings.TrimSpace(rest[len("in"):])
	rest = strings.TrimSpace(rest[len(fields[2]):])

	whereS, byS, limitS, moreS, err := splitForClauses(rest, line)
	if err != nil {
		return rg, err
	}
	if whereS != "" {
		e, err := parseExpr(whereS, line)
		if err != nil {
			return rg, err
		}
		rg.Where = e
	}
	if byS != "" {
		bp := strings.Fields(byS)
		if len(bp) < 1 || len(bp) > 2 || !isIdent(bp[0]) {
			return rg, &Error{line, "ordering is `by field [desc|asc]`"}
		}
		rg.Order = bp[0]
		if len(bp) == 2 {
			switch bp[1] {
			case "desc":
				rg.Desc = true
			case "asc":
				rg.Desc = false
			default:
				return rg, &Error{line, fmt.Sprintf("order direction must be `desc` or `asc`, got %q", bp[1])}
			}
		}
	}
	if limitS != "" {
		e, err := parseExpr(limitS, line)
		if err != nil {
			return rg, &Error{line, fmt.Sprintf("limit needs an integer or expression, got %q", limitS)}
		}
		rg.Limit = e
	}
	if moreS != "" {
		if !isIdent(moreS) {
			return rg, &Error{line, fmt.Sprintf("`more` names the action that loads the next page — `more loadMore` — got %q", moreS)}
		}
		if rg.Limit == nil {
			return rg, &Error{line, "`more` needs a `limit`: nothing is held back from an unbounded list, so there is no next page to load"}
		}
		rg.More = moreS
	}
	return rg, nil
}

// splitForClauses splits a `for` header tail into its where/by/limit/more
// clause bodies. It scans at the top level (skipping string literals and parens)
// so a quoted value like `"stand by"` inside a `where` is never mistaken for a
// clause keyword. The clauses, if present, must appear in where→by→limit→more
// order.
func splitForClauses(rest string, line int) (whereS, byS, limitS, moreS string, err error) {
	if strings.TrimSpace(rest) == "" {
		return "", "", "", "", nil
	}
	pos := map[string]int{} // keyword -> first top-level start index
	inStr := false
	depth := 0
	for i := 0; i < len(rest); {
		c := rest[i]
		switch {
		case c == '"':
			inStr = !inStr
			i++
		case inStr:
			i++
		case c == '(' || c == '{':
			depth++
			i++
		case c == ')' || c == '}':
			depth--
			i++
		case depth == 0 && isIdentStart(c):
			j := i
			for j < len(rest) && isIdentChar(rest[j]) {
				j++
			}
			if w := rest[i:j]; w == "where" || w == "by" || w == "limit" || w == "more" {
				if _, seen := pos[w]; !seen {
					pos[w] = i
				}
			}
			i = j
		default:
			i++
		}
	}

	known := []struct {
		name string
		klen int
	}{{"where", 5}, {"by", 2}, {"limit", 5}, {"more", 4}}
	// the content of a clause runs from just after its keyword to the next clause
	// keyword that starts later in the string.
	end := func(start int) int {
		e := len(rest)
		for _, k := range known {
			if p, ok := pos[k.name]; ok && p > start && p < e {
				e = p
			}
		}
		return e
	}
	clause := func(name string, klen int) (string, error) {
		p, ok := pos[name]
		if !ok {
			return "", nil
		}
		body := strings.TrimSpace(rest[p+klen : end(p)])
		if body == "" {
			return "", &Error{line, fmt.Sprintf("`%s` needs a value", name)}
		}
		return body, nil
	}
	if whereS, err = clause("where", 5); err != nil {
		return
	}
	if byS, err = clause("by", 2); err != nil {
		return
	}
	if limitS, err = clause("limit", 5); err != nil {
		return
	}
	if moreS, err = clause("more", 4); err != nil {
		return
	}

	// reject stray text before the first clause keyword.
	first := len(rest)
	for _, k := range known {
		if p, ok := pos[k.name]; ok && p < first {
			first = p
		}
	}
	if leftover := strings.TrimSpace(rest[:first]); leftover != "" {
		return "", "", "", "", &Error{line, fmt.Sprintf("unexpected %q in for header (clauses are where/by/limit/more, in that order)", leftover)}
	}
	return whereS, byS, limitS, moreS, nil
}

// parseLink: `link "label" -> "/path"`
func parseLink(s string, line int) (ast.Node, error) {
	arrow := indexTop(s, "->")
	if arrow < 0 {
		return nil, &Error{line, `link needs a destination: link "Home" -> "/"`}
	}
	label, err := parseText(strings.TrimSpace(s[:arrow]), line)
	if err != nil {
		return nil, err
	}
	path, err := parseText(strings.TrimSpace(s[arrow+2:]), line)
	if err != nil {
		return nil, err
	}
	return ast.Link{LabelSegs: label, PathSegs: path}, nil
}

func parseInput(s string, line int) (ast.Node, error) {
	// `input bind name [placeholder "text"] [on change -> Action(args) [debounce <duration>]]`
	if !strings.HasPrefix(s, "bind ") {
		return nil, &Error{line, `input needs a binding: input bind stateName`}
	}
	rest := strings.TrimSpace(s[len("bind "):])
	in := ast.Input{}
	// `on change` is pulled off the tail first, via indexTop so a quoted
	// placeholder that happens to contain the literal words is left alone —
	// the same reason placeholder's own scan below is indexTop, not Index.
	var onChange string
	if oc := indexTop(rest, " on change"); oc >= 0 {
		onChange = strings.TrimSpace(rest[oc+len(" on change"):])
		rest = strings.TrimSpace(rest[:oc])
	}
	if ph := indexTop(rest, "placeholder "); ph >= 0 {
		p, err := parseText(strings.TrimSpace(rest[ph+len("placeholder "):]), line)
		if err != nil {
			return nil, err
		}
		in.Placeholder = p
		rest = strings.TrimSpace(rest[:ph])
	}
	if !isIdent(rest) {
		return nil, &Error{line, fmt.Sprintf("invalid input binding %q", rest)}
	}
	in.Bind = rest
	if onChange != "" {
		ca, err := parseControlAction(onChange, line)
		if err != nil {
			return nil, err
		}
		in.OnChange = ca
	}
	return in, nil
}

// defaultDebounceMS is the on-change dispatch delay a control gets when the
// author writes no `debounce` clause: long enough that a fast typist doesn't
// fire one request per keystroke, short enough that a typing indicator or
// live search still reads as "live." It is a plain constant, not a value
// parsed from source, because parseDuration's finest unit is a whole second —
// far coarser than what a responsive debounce needs — so there is no duration
// literal that could spell this default without inventing a sub-second unit
// the rest of the grammar (after/job/daemon) doesn't have and doesn't need.
const defaultDebounceMS = 400

// parseControlAction parses the clause after `on change`: `-> Action(args)
// [debounce <duration>]`. The action-call half is exactly parseButton's arrow
// grammar (name, optional parenthesized args) reused rather than duplicated;
// the optional `debounce` half reuses parseDuration — the same `30s`/`5m`/`2h`
// grammar `after`/`job`/`daemon` already parse — converting its seconds to
// milliseconds, rather than inventing a second duration parser for it.
func parseControlAction(s string, line int) (*ast.ControlAction, error) {
	if !strings.HasPrefix(s, "->") {
		return nil, &Error{line, "`on change` needs a reaction: `on change -> actionName(args)`"}
	}
	call := strings.TrimSpace(s[len("->"):])

	debounceMS := defaultDebounceMS
	if di := indexTop(call, " debounce "); di >= 0 {
		durS := strings.TrimSpace(call[di+len(" debounce "):])
		call = strings.TrimSpace(call[:di])
		secs, err := parseDuration(durS, line)
		if err != nil {
			return nil, err
		}
		debounceMS = secs * 1000
	}

	name := call
	var args []ast.Expr
	if open := strings.IndexByte(call, '('); open >= 0 {
		name = strings.TrimSpace(call[:open])
		closeP := strings.LastIndexByte(call, ')')
		if closeP < open {
			return nil, &Error{line, "missing `)` in action call"}
		}
		inner := strings.TrimSpace(call[open+1 : closeP])
		if inner != "" {
			for _, a := range splitTop(inner, ',') {
				e, err := parseExpr(strings.TrimSpace(a), line)
				if err != nil {
					return nil, err
				}
				args = append(args, e)
			}
		}
	}
	if !isIdent(name) {
		return nil, &Error{line, fmt.Sprintf("invalid action reference %q", name)}
	}
	return &ast.ControlAction{Action: name, Args: args, DebounceMS: debounceMS, Line: line}, nil
}

// parseOverlay: `overlay bind <cell>:` with a child node tree shown while the cell
// is truthy.
func parseOverlay(n *source.Node) (ast.Node, error) {
	head := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len("overlay"):]), ":"))
	if !strings.HasPrefix(head, "bind ") {
		return nil, &Error{n.Line.No, "overlay needs a bound cell: overlay bind <cell>:"}
	}
	bind := strings.TrimSpace(head[len("bind "):])
	if !isIdent(bind) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid overlay binding %q", bind)}
	}
	kids, err := parseNodes(n.Children)
	if err != nil {
		return nil, err
	}
	return ast.Overlay{Bind: bind, Body: kids}, nil
}

// parsePopover: `popover bind <cell>:`, same shape as `overlay bind <cell>:`
// (see parseOverlay) but for a layer anchored beside its previous sibling
// instead of centered over the page. The "has a previous sibling" rule itself
// is enforced by the caller (parseNodes), which is the one place that still
// knows this node's position among its true siblings.
func parsePopover(n *source.Node) (ast.Node, error) {
	head := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len("popover"):]), ":"))
	if !strings.HasPrefix(head, "bind ") {
		return nil, &Error{n.Line.No, "popover needs a bound cell: popover bind <cell>:"}
	}
	bind := strings.TrimSpace(head[len("bind "):])
	if !isIdent(bind) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid popover binding %q", bind)}
	}
	kids, err := parseNodes(n.Children)
	if err != nil {
		return nil, err
	}
	return ast.Popover{Bind: bind, Body: kids}, nil
}

// parseTypeahead: `typeahead bind <cell> from <Entity>.<field> [placeholder "…"]`.
func parseTypeahead(s string, line int) (ast.Node, error) {
	var ph []ast.Seg
	if i := indexTop(s, "placeholder "); i >= 0 {
		p, err := parseText(strings.TrimSpace(s[i+len("placeholder "):]), line)
		if err != nil {
			return nil, err
		}
		ph = p
		s = strings.TrimSpace(s[:i])
	}
	if !strings.HasPrefix(s, "bind ") {
		return nil, &Error{line, "typeahead needs: typeahead bind <cell> from <Entity>.<field>"}
	}
	rest := strings.TrimSpace(s[len("bind "):])
	fi := strings.Index(rest, " from ")
	if fi < 0 {
		return nil, &Error{line, "typeahead needs a source: typeahead bind <cell> from <Entity>.<field>"}
	}
	bind := strings.TrimSpace(rest[:fi])
	src := strings.TrimSpace(rest[fi+len(" from "):])
	dot := strings.IndexByte(src, '.')
	if dot < 0 {
		return nil, &Error{line, "typeahead source must be <Entity>.<field>"}
	}
	ent := strings.TrimSpace(src[:dot])
	field := strings.TrimSpace(src[dot+1:])
	if !isIdent(bind) || !isIdent(ent) || !isIdent(field) {
		return nil, &Error{line, "typeahead needs identifiers: bind <cell> from <Entity>.<field>"}
	}
	return ast.Typeahead{Bind: bind, Entity: ent, Field: field, Placeholder: ph}, nil
}

// parseSelect: `select bind state:` with `option "Label" -> "value"` children,
// or `select bind state` for an enum cell (options default to the enum members).
func parseSelect(n *source.Node) (ast.Node, error) {
	head := strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len("select "):]), ":")
	if !strings.HasPrefix(head, "bind ") {
		return nil, &Error{n.Line.No, "select needs a binding: select bind choice"}
	}
	bind := strings.TrimSpace(head[len("bind "):])
	if !isIdent(bind) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid select binding %q", bind)}
	}
	opts, err := parseOptions("select", n.Children)
	if err != nil {
		return nil, err
	}
	return ast.Select{Bind: bind, Options: opts, Line: n.Line.No}, nil
}

// parseOptions parses the choice list shared by every node that offers one — a
// `select` and a `radio` group are the same choice written two ways, so they
// read their options with one parser and cannot drift on what an option means.
//
// A choice list holds two kinds of entry, in any order:
//
//	option "Draft" -> "draft"        one fixed choice
//	for c in Category by name:       one choice per row of a collection
//	    option "{c.name}" -> c.id
//
// The repeating entry is written with the language's existing repeating header,
// not a keyword of its own, because it *is* that header: a data-driven option
// list wants the same `where`/`by`/`limit` a feed does, and inventing a second
// spelling of them would be a second thing to keep in step.
func parseOptions(kw string, kids []*source.Node) ([]ast.Option, error) {
	var out []ast.Option
	for _, c := range kids {
		switch {
		case strings.HasPrefix(c.Line.Text, "option "):
			o, err := parseOption(strings.TrimSpace(c.Line.Text[len("option "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, o)

		case strings.HasPrefix(c.Line.Text, "for "):
			rg, err := parseRange(strings.TrimSuffix(strings.TrimSpace(c.Line.Text[len("for "):]), ":"), c.Line.No)
			if err != nil {
				return nil, err
			}
			// One option per row, so the loop body is one option. Several would mean
			// rows × options, which is not a thing a choice list can mean.
			if len(c.Children) != 1 || !strings.HasPrefix(c.Children[0].Line.Text, "option ") {
				return nil, &Error{c.Line.No, fmt.Sprintf(
					"a `for` in a %s renders one option per row, so it holds exactly one `option \"Label\" -> value` line", kw)}
			}
			o, err := parseOption(strings.TrimSpace(c.Children[0].Line.Text[len("option "):]), c.Children[0].Line.No)
			if err != nil {
				return nil, err
			}
			o.From = &rg
			out = append(out, o)

		case firstWord(c.Line.Text) == "slot":
			// A `slot` here is refused with its own reason, because it is the one
			// thing an author reaches for next and the generic message does not
			// explain why it cannot work.
			//
			// An option is not a view node. It is an entry in this list — a label
			// plus the identity it stores — and the value half is a compile-time
			// constant, which is what enum defaulting and the "no such member" check
			// rest on. A `use` block, by contrast, is parsed by parseNodes as view
			// nodes: `option "A" -> "a"` written under a `use` is not a mis-typed
			// option, it is `unknown view node "option"`, because at that point in
			// the parse nothing knows which component is being called, let alone that
			// its slot wants options. Knowing would mean resolving component
			// signatures — across imports, registry fetches and layered composition —
			// during parsing, which is a whole-program dependency the parser does not
			// have and should not grow for one node's children.
			//
			// So the two ways to let a caller decide the choices are the two the
			// language already has, and both keep the value half provable.
			return nil, &Error{c.Line.No, "a `slot` cannot stand in a " + kw + "'s choice list: an option is not a view node (a `use` block holds view nodes, so the caller cannot write `option` lines into one), and its value half is a compile-time constant. " +
				"Let the caller supply the choices as data — `for item in Collection:` with one `option` line, over a collection the caller fills — or let the caller own the " + kw + " itself and give the component the chrome around it, with the `slot` outside"}

		default:
			return nil, &Error{c.Line.No, kw + ` children must be options: option "Label" -> "value", or a ` +
				"`for item in Collection:` holding one such line"}
		}
	}
	return out, nil
}

// parseOption parses one `option "Label" -> value` line, with the keyword already
// stripped.
//
// The value half is a quoted literal or a bare expression, and which one it is
// decides what the compiler can still prove about it: a literal is a compile-time
// identity (what enum defaulting and the typo check rest on), an expression is a
// value that does not exist until the render. Every option written before this
// distinction existed is quoted, so every one of them keeps the literal reading.
func parseOption(rest string, line int) (ast.Option, error) {
	o := ast.Option{Line: line}
	arrow := indexTop(rest, "->")
	if arrow < 0 {
		// `option "Draft"` — one literal standing for both halves. The shorthand is
		// only available to a label that is a literal too; otherwise the author must
		// say what is stored.
		v, err := unquote(rest, line)
		if err != nil {
			return o, err
		}
		if strings.Contains(v, "{") {
			return o, &Error{line, fmt.Sprintf(
				"option %q interpolates, so it needs the value it stores: option %s -> \"value\"", v, rest)}
		}
		o.Label, o.Value = []ast.Seg{{Lit: v}}, v
		return o, nil
	}
	label, err := parseText(strings.TrimSpace(rest[:arrow]), line)
	if err != nil {
		return o, err
	}
	o.Label = label
	val := strings.TrimSpace(rest[arrow+2:])
	if strings.HasPrefix(val, `"`) {
		o.Value, err = unquote(val, line)
		return o, err
	}
	if val == "" {
		return o, &Error{line, `option needs the value it stores: option "Label" -> "value"`}
	}
	e, err := parseExpr(val, line)
	if err != nil {
		return o, err
	}
	o.Val = e
	return o, nil
}

// parseControl parses every control in ast.Controls from one grammar:
//
//	<keyword> bind <cell> [placeholder "..."] [label "..."]
//	<keyword> bind <cell>:            with `option` children (radio)
//
// One parser, because a control is one idea — a cell with a way to write it —
// and which modifiers a given control accepts is a fact stated once, in its
// ast.Controls row, rather than in a fourth hand-written parse function that
// spells `bind` slightly differently.
// parseMedia splits a media line into its source and its `alt` — the one
// grammar `image` and `video` share, parsed once so the two cannot drift.
//
// `alt` is pulled off with indexTop, which skips over quoted strings, so the
// separator is found after the URL rather than inside it: `image "/a?x=alt "` is
// one URL and not a URL with an empty description. That is the same mechanism a
// control's `placeholder` and `label` are found by.
//
// The bool is the point of the function. `alt ""` and no `alt` at all produce the
// same empty segment list and render the same markup, but they are different
// statements by the author — decorative on purpose, versus undecided — and only
// the second is worth telling anyone about. See ast.Image and ast.Advise.
func parseMedia(rest string, line int) (segs, alt []ast.Seg, altSet bool, err error) {
	rest, alt, altSet, err = mediaClause(rest, "alt", line)
	if err != nil {
		return nil, nil, false, err
	}
	segs, err = parseText(rest, line)
	if err != nil {
		return nil, nil, false, err
	}
	return segs, alt, altSet, nil
}

// mediaClause pulls one trailing `<kw> "…"` clause off a media line: the words
// after the keyword, parsed as interpolated text, and the line with the clause
// removed. The keyword is found with indexTop so a URL containing it is left
// alone (see parseMedia). Absent, the line comes back untouched and set is false.
func mediaClause(rest, kw string, line int) (remain string, segs []ast.Seg, set bool, err error) {
	i := indexTop(rest, kw+" ")
	if i < 0 {
		return rest, nil, false, nil
	}
	segs, err = parseText(strings.TrimSpace(rest[i+len(kw)+1:]), line)
	if err != nil {
		return "", nil, false, err
	}
	return strings.TrimSpace(rest[:i]), segs, true, nil
}

// parseVideo: `video "src" [poster "url"] [alt "…"] [autoplay] [loop] [muted]`.
//
// The source and `alt` are the grammar `image` has (parseMedia), so the two
// cannot drift on what they share. A video has two things a picture does not:
// a `poster` — the still shown before playback, interpolated like the source —
// and playback flags, which are bare words at the end of the line. The flags
// are peeled off the tail first, then `alt`, then `poster`, so each clause runs
// from its keyword to what was the end of the line when it was parsed and no
// clause has to know which others exist. `alt` must therefore follow `poster`;
// that is the order the words are read in aloud ("this clip, its poster, its
// description, how it plays"), and the diagnostic says so if they are swapped.
func parseVideo(rest string, line int) (ast.Video, error) {
	v := ast.Video{Line: line}
	for {
		i := strings.LastIndexByte(rest, ' ')
		if i < 0 {
			break
		}
		word := strings.TrimSpace(rest[i+1:])
		// A bare word after the closing quote is a flag; the same word inside a
		// string literal is a URL or a description and is left where it is.
		if strings.Count(rest[:i], `"`)%2 != 0 {
			break
		}
		switch word {
		case "autoplay":
			v.Autoplay = true
		case "loop":
			v.Loop = true
		case "muted":
			v.Muted = true
		default:
			if word != "" && word[0] != '"' && isIdent(word) && strings.HasSuffix(strings.TrimSpace(rest[:i]), `"`) {
				return v, &Error{line, fmt.Sprintf("video: unknown flag %q (the playback flags are autoplay, loop, muted)", word)}
			}
			goto flagsDone
		}
		rest = strings.TrimSpace(rest[:i])
	}
flagsDone:
	// Each clause runs to the end of what is left, so they are peeled in reverse
	// order of the grammar; `alt` written before `poster` would swallow it.
	if a, p := indexTop(rest, "alt "), indexTop(rest, "poster "); a >= 0 && p > a {
		return v, &Error{line, `video clauses are ordered: video "src" [poster "…"] [alt "…"] [autoplay] [loop] [muted]`}
	}
	var err error
	rest, v.Alt, v.AltSet, err = mediaClause(rest, "alt", line)
	if err != nil {
		return v, err
	}
	rest, v.Poster, _, err = mediaClause(rest, "poster", line)
	if err != nil {
		return v, err
	}
	if strings.TrimSpace(rest) == "" {
		return v, &Error{line, `video needs a source: video "{p.media}" [poster "…"] [alt "…"] [autoplay] [loop] [muted]`}
	}
	if v.Segs, err = parseText(rest, line); err != nil {
		return v, err
	}
	return v, nil
}

// parseHeading: `heading <level> "Title"` — a level expression, then the words.
//
// THE SPLIT IS THE FIRST TOP-LEVEL QUOTE, and that is a rule rather than a
// guess: the level is a number and the text is a string, so the first `"` on the
// line is where one ends and the other begins. Trailing `class`/`style`/`anchor`
// modifiers are already off the line by the time this runs (parseNodes strips
// them first), so what is left is exactly the two halves.
//
// Both halves are required, and each missing half has its own message, because
// `heading "Title"` (no level) and `heading 2` (no text) are different mistakes:
// the first is a `text` node the author reached for the wrong keyword for, and
// the second is an unfinished line.
func parseHeading(rest string, line int) (ast.Node, error) {
	q := strings.IndexByte(rest, '"')
	switch {
	case q < 0:
		return nil, &Error{line, `heading needs text: heading 2 "Title"`}
	case strings.TrimSpace(rest[:q]) == "":
		return nil, &Error{line, `heading needs a level: heading 2 "Title"`}
	}
	lvl, err := parseExpr(strings.TrimSpace(rest[:q]), line)
	if err != nil {
		return nil, err
	}
	segs, err := parseText(strings.TrimSpace(rest[q:]), line)
	if err != nil {
		return nil, err
	}
	return ast.Heading{Level: lvl, Segs: segs, Line: line}, nil
}

func parseControl(kw string, n *source.Node) (ast.Node, error) {
	spec := ast.Controls[kw]
	line := n.Line.No
	head := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len(kw):]), ":"))
	ctl := ast.Control{Kind: kw, Line: line}
	if spec.Hint {
		if i := indexTop(head, "placeholder "); i >= 0 {
			p, err := parseText(strings.TrimSpace(head[i+len("placeholder "):]), line)
			if err != nil {
				return nil, err
			}
			ctl.Placeholder = p
			head = strings.TrimSpace(head[:i])
		}
	}
	if spec.Labeled {
		if i := indexTop(head, "label "); i >= 0 {
			l, err := parseText(strings.TrimSpace(head[i+len("label "):]), line)
			if err != nil {
				return nil, err
			}
			ctl.Label = l
			head = strings.TrimSpace(head[:i])
		}
	}
	if !strings.HasPrefix(head, "bind ") {
		return nil, &Error{line, fmt.Sprintf("%s needs a binding: %s bind <cell>", kw, kw)}
	}
	bind := strings.TrimSpace(head[len("bind "):])
	if !isIdent(bind) {
		return nil, &Error{line, fmt.Sprintf("invalid %s binding %q", kw, bind)}
	}
	ctl.Bind = bind
	if spec.Options {
		opts, err := parseOptions(kw, n.Children)
		if err != nil {
			return nil, err
		}
		ctl.Options = opts
	} else if len(n.Children) > 0 {
		return nil, &Error{line, fmt.Sprintf("%s takes no children — it is one control, not a container", kw)}
	}
	return ctl, nil
}

// parseTabs: `tabs bind cell:` with `tab "Label" -> "value":` children, each
// holding the nodes shown when the bound cell equals that value.
func parseTabs(n *source.Node) (ast.Node, error) {
	head := strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len("tabs "):]), ":")
	if !strings.HasPrefix(head, "bind ") {
		return nil, &Error{n.Line.No, "tabs needs a binding: tabs bind cell"}
	}
	bind := strings.TrimSpace(head[len("bind "):])
	if !isIdent(bind) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid tabs binding %q", bind)}
	}
	tabs := ast.Tabs{Bind: bind, Line: n.Line.No}
	for _, c := range n.Children {
		if !strings.HasPrefix(c.Line.Text, "tab ") {
			return nil, &Error{c.Line.No, `tabs children must be tabs: tab "Label" -> "value":`}
		}
		rest := strings.TrimSuffix(strings.TrimSpace(c.Line.Text[len("tab "):]), ":")
		arrow := indexTop(rest, "->")
		if arrow < 0 {
			return nil, &Error{c.Line.No, `tab needs a value: tab "Label" -> "value":`}
		}
		label, err := parseText(strings.TrimSpace(rest[:arrow]), c.Line.No)
		if err != nil {
			return nil, err
		}
		value, err := unquote(strings.TrimSpace(rest[arrow+2:]), c.Line.No)
		if err != nil {
			return nil, err
		}
		body, err := parseNodes(c.Children)
		if err != nil {
			return nil, err
		}
		tabs.Tabs = append(tabs.Tabs, ast.Tab{Label: label, Value: value, Body: body})
	}
	if len(tabs.Tabs) == 0 {
		return nil, &Error{n.Line.No, "tabs needs at least one `tab`"}
	}
	return tabs, nil
}

// parseMatch: `match <expr>:` with `case "value":` arms and an optional `else:`,
// each holding a node body.
func parseMatch(n *source.Node) (ast.Node, error) {
	exprS := strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len("match "):]), ":")
	if exprS == "" {
		return nil, &Error{n.Line.No, "match needs a value: match <expr>:"}
	}
	e, err := parseExpr(exprS, n.Line.No)
	if err != nil {
		return nil, err
	}
	m := ast.Match{Expr: e, Line: n.Line.No}
	for _, c := range n.Children {
		ct := strings.TrimSpace(c.Line.Text)
		switch {
		case strings.HasPrefix(ct, "case "):
			val, err := caseValue(strings.TrimSuffix(strings.TrimSpace(ct[len("case "):]), ":"), c.Line.No)
			if err != nil {
				return nil, err
			}
			body, err := parseNodes(c.Children)
			if err != nil {
				return nil, err
			}
			m.Cases = append(m.Cases, ast.MatchCase{Value: val, Body: body})
		case ct == "else:" || ct == "else":
			body, err := parseNodes(c.Children)
			if err != nil {
				return nil, err
			}
			m.Else = body
		default:
			return nil, &Error{c.Line.No, `match children must be cases: case "value": (or else:)`}
		}
	}
	if len(m.Cases) == 0 {
		return nil, &Error{n.Line.No, "match needs at least one `case`"}
	}
	return m, nil
}

// caseValue reads the constant one `case` compares against: a quoted string, or
// an integer literal.
//
// Both spellings produce the same thing — the decimal text of the value — because
// that is what the comparison is. Every renderer stringifies the match subject and
// compares it to the case's text, so `case 1:` and `case "1":` are the same case
// (writing both is a duplicate), and a number needs no coercion in the head:
// `match "" + month(at):` was a workaround for a message, not for a mechanism.
//
// A number does not weaken enum exhaustiveness, which is the property `match`
// exists to prove. Exhaustiveness has exactly two sources and this adds neither:
// an enum subject, whose members are all known, or an `else`. An int has no finite
// member list — there is no set of `case`s that covers it — so an int-subject
// match falls under the rule an open type already fell under, that it must carry
// an `else`, and a numeric case on an *enum* subject is still rejected as a member
// that enum does not have. `case 1:` is a spelling, not a new kind of proof.
func caseValue(s string, line int) (string, error) {
	if strings.HasPrefix(s, `"`) {
		return unquote(s, line)
	}
	if n, err := strconv.Atoi(s); err == nil {
		// Canonical decimal, because that is what the subject stringifies to:
		// `case 007:` has to match the number 7 or it is a case that never runs.
		return strconv.Itoa(n), nil
	}
	return "", &Error{line, fmt.Sprintf(
		`a case value is a compile-time constant: a quoted string (case "draft":) or a whole number (case 1:) — got %s`, s)}
}

// parseForm: `form "Submit" -> action(args):` then child nodes (inputs, text).
func parseForm(n *source.Node) (ast.Node, error) {
	head := strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len("form "):]), ":")
	btn, err := parseButton(head, n.Line.No)
	if err != nil {
		return nil, err
	}
	kids, err := parseNodes(n.Children)
	if err != nil {
		return nil, err
	}
	return ast.Form{Action: btn.Action, Args: btn.Args, Submit: btn.Label, Body: kids, Line: n.Line.No}, nil
}

// parseUpload: `upload bind url [label "text"]`.
func parseUpload(s string, line int) (ast.Node, error) {
	if !strings.HasPrefix(s, "bind ") {
		return nil, &Error{line, "upload needs a binding: upload bind avatarUrl"}
	}
	rest := strings.TrimSpace(s[len("bind "):])
	up := ast.Upload{Label: []ast.Seg{{Lit: "Upload"}}}
	if lp := indexTop(rest, "label "); lp >= 0 {
		l, err := parseText(strings.TrimSpace(rest[lp+len("label "):]), line)
		if err != nil {
			return nil, err
		}
		up.Label = l
		rest = strings.TrimSpace(rest[:lp])
	}
	if !isIdent(rest) {
		return nil, &Error{line, fmt.Sprintf("invalid upload binding %q", rest)}
	}
	up.Bind = rest
	return up, nil
}

// parseUse: `use Component(arg, ...)` — invoke a reusable view fragment,
// optionally with an indented block of children that fill the component's `slot`.
//
// kids used to be dropped on the floor here: a block written under a `use` parsed
// fine and then rendered nothing at all. It is carried now, and a block handed to
// a component with no `slot` is rejected in the IR builder rather than silently
// discarded.
func parseUse(s string, line int, kids []*source.Node) (ast.Node, error) {
	name := s
	var args []ast.Expr
	if open := strings.IndexByte(s, '('); open >= 0 {
		name = strings.TrimSpace(s[:open])
		close := strings.LastIndexByte(s, ')')
		if close < open {
			return nil, &Error{line, "missing `)` in use(...)"}
		}
		inner := strings.TrimSpace(s[open+1 : close])
		if inner != "" {
			for _, a := range splitTop(inner, ',') {
				e, err := parseExpr(strings.TrimSpace(a), line)
				if err != nil {
					return nil, err
				}
				args = append(args, e)
			}
		}
	}
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{line, fmt.Sprintf("invalid component name %q", name)}
	}
	body, err := parseNodes(kids)
	if err != nil {
		return nil, err
	}
	return ast.Use{Name: name, Args: args, Body: body, Line: line}, nil
}

func parseText(s string, line int) ([]ast.Seg, error) {
	str, err := unquoteText(s, line)
	if err != nil {
		return nil, err
	}
	return parseTextBody(str, line)
}

// parseTextBody splits already-unquoted text into literal and `{expr}` segments.
//
// Split out from parseText because a trailing `class "..."` modifier arrives at
// stripNodeMods already unquoted — it had to be, to find where the line's base
// text ends. Re-quoting it to hand back to parseText would put the value through
// unquote a second time, so `\n` in a class would become a newline on that path
// and two characters on every other. One splitter, one meaning of a brace.
func parseTextBody(str string, line int) ([]ast.Seg, error) {
	var segs []ast.Seg
	var lit strings.Builder
	for i := 0; i < len(str); i++ {
		if str[i] == '{' {
			// Balanced, string-aware: the `}` that closes this interpolation is not
			// necessarily the next one in the value, because the expression between
			// them may hold a string literal containing a brace.
			end := endOfInterp(str, i)
			if end < 0 {
				return nil, &Error{line, "unterminated `{` in text"}
			}
			if lit.Len() > 0 {
				segs = append(segs, ast.Seg{Lit: lit.String()})
				lit.Reset()
			}
			inner := str[i+1 : end]
			e, err := parseExpr(inner, line)
			if err != nil {
				return nil, interpErr(err, inner, line)
			}
			segs = append(segs, ast.Seg{Expr: e})
			i = end
			continue
		}
		lit.WriteByte(str[i])
	}
	if lit.Len() > 0 {
		segs = append(segs, ast.Seg{Lit: lit.String()})
	}
	return segs, nil
}

// interpErr names the one way an interpolation can now fail that the author has
// no reason to guess at: a nested string written `\"e\"` in a value that is not
// escape-decoded.
//
// The two interpolating paths differ in exactly this, deliberately. A `text`,
// label or link destination is decoded as a Go string literal first, so both `"e"`
// and `\"e\"` arrive here as `"e"`. A `class` value is never decoded — that is why
// a CSS class may hold a backslash (`w-1\/2`) at all — so in a class a `\` is a
// literal backslash and the expression parser is right to refuse it. Plain quotes
// are the spelling that works in every interpolating position, so the message says
// to write those rather than explaining the asymmetry at the point of failure.
func interpErr(err error, inner string, line int) error {
	if strings.Contains(inner, `\"`) {
		return &Error{line, fmt.Sprintf(
			"cannot parse `{%s}`: inside `{…}` the text is an expression, so a nested string is written with plain quotes and no backslashes — `{contains(name, \"e\")}`", inner)}
	}
	return err
}

// parseButton: `"label" -> action` or `"label" -> action(arg, ...)`
func parseButton(s string, line int) (ast.Button, error) {
	arrow := indexTop(s, "->")
	if arrow < 0 {
		return ast.Button{}, &Error{line, `button needs an action: button "Label" -> actionName`}
	}
	label, err := parseText(strings.TrimSpace(s[:arrow]), line)
	if err != nil {
		return ast.Button{}, err
	}
	call := strings.TrimSpace(s[arrow+2:])
	name := call
	var args []ast.Expr
	if open := strings.IndexByte(call, '('); open >= 0 {
		name = strings.TrimSpace(call[:open])
		close := strings.LastIndexByte(call, ')')
		if close < open {
			return ast.Button{}, &Error{line, "missing `)` in action call"}
		}
		inner := strings.TrimSpace(call[open+1 : close])
		if inner != "" {
			for _, a := range splitTop(inner, ',') {
				e, err := parseExpr(strings.TrimSpace(a), line)
				if err != nil {
					return ast.Button{}, err
				}
				args = append(args, e)
			}
		}
	}
	if !isIdent(name) {
		return ast.Button{}, &Error{line, fmt.Sprintf("invalid action reference %q", name)}
	}
	return ast.Button{Label: label, Action: name, Args: args, Line: line}, nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

func keyword(s, kw string) (string, bool) {
	if s == kw || strings.HasPrefix(s, kw+" ") {
		return strings.TrimSpace(strings.TrimPrefix(s, kw)), true
	}
	return "", false
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

// isType reports whether s names a built-in scalar type. Enum names and entity
// relations are also valid type positions but are validated later, in the IR,
// once every declaration is known.
//
// `float` is accepted here — at the syntax level, a type name — the same as
// every other primitive, but it is real only inside a proc: a proc parameter,
// `let`/`let mut` local, or return type. internal/ir/build.go rejects it
// everywhere else (an entity field, a record field, a state cell, a component
// parameter, a service op's return type) with a dedicated error, since none
// of those has a database column, a client-side (assets/facet.js) runtime
// representation, or a wire encoding for it yet — see isPrimitive, which
// deliberately does NOT include "float", and each of those sites' explicit
// float check. See LANGUAGE.md's `proc` section for the full design.
func isType(s string) bool {
	switch s {
	case "int", "text", "bool", "money", "date", "float":
		return true
	}
	return false
}

// splitType pulls a trailing `?` (optional/nullable) and a `[...]` list wrapper
// off a type expression, returning the core type, whether it is a list, and
// whether it is optional.
func splitType(s string) (core string, list, optional bool) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "?") {
		optional = true
		s = strings.TrimSpace(strings.TrimSuffix(s, "?"))
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		list = true
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s, list, optional
}

// isTypeName reports whether s is a syntactically valid type token: a built-in
// scalar or a capitalized identifier (an enum or entity name, resolved later).
func isTypeName(s string) bool {
	return isType(s) || (isIdent(s) && isUpper(s))
}

func isUpper(s string) bool { return s != "" && s[0] >= 'A' && s[0] <= 'Z' }

// isCapabilityName reports whether s is syntactically a valid capability name
// for a proc's `uses` clause: one or more dot-separated identifiers (`io.file`,
// `io.net`). This only checks shape — whether the name is actually one of the
// language's known capabilities is a build.go concern (validCapability),
// exactly the same division of labor parseRequire/policyPasses already have
// for a `requires` policy name (syntax here, existence/meaning at build time).
func isCapabilityName(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !isIdent(part) {
			return false
		}
	}
	return true
}

// isBareCallStmt reports whether t is a bare builtin call written as its own
// statement — `writeFile(path, content)` rather than `let ok = writeFile(...)`
// or `name = writeFile(...)` — the shape ast.ExprStmt exists for (see its
// doc). Told apart from a reassignment by having no top-level `=` at all
// (splitTopByte, so `x = a == b` is never mistaken for one) and from every
// other statement keyword by starting with an identifier immediately followed
// by `(` and ending the line at the matching `)`.
func isBareCallStmt(t string) bool {
	if splitTopByte(t, '=') >= 0 {
		return false
	}
	open := strings.IndexByte(t, '(')
	if open <= 0 || !strings.HasSuffix(t, ")") {
		return false
	}
	return isIdent(strings.TrimSpace(t[:open]))
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// isThemeKey allows the interior hyphens that CSS custom-property names use
// (e.g. `card-border` → `--fa-card-border`), which a plain identifier forbids.
func isThemeKey(s string) bool {
	if s == "" || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	return isIdent(strings.ReplaceAll(s, "-", "_"))
}

// splitTop splits on sep at the top level (ignoring separators inside parens,
// braces, brackets, or quotes), so `add Post { body: f(a, b) }`, a list
// literal argument (`do sumList([1, 2, 3])`), and string args all survive.
// `[`/`]` joined `(`/`{` and `)`/`}` here once a list literal could appear as a
// top-level, comma-separated argument to `do`/`spawn`/`call` — before that, a
// bracketed list only ever showed up already nested one level inside some
// other bracket pair this function already tracked (e.g. `add`'s own `{...}`),
// so the gap was invisible; splitTopByte (this file's own sibling function)
// already tracked `[`/`]` for exactly this reason.
func splitTop(s string, sep byte) []string {
	var out []string
	depth := 0
	inStr := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case c == sep && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

func defaultFor(typ string) ast.Expr {
	switch typ {
	case "int":
		return ast.Lit{Kind: "int", Val: 0}
	case "bool":
		return ast.Lit{Kind: "bool", Val: false}
	case "float":
		// Unreachable for a state cell today (internal/ir/build.go's state
		// check rejects a `float`-typed one before this default would ever be
		// read), but defaultFor is typ-total the same way every other
		// conversion in this codebase is, so a future proc-only default-value
		// position finds a real zero value here rather than the wrong text one.
		return ast.Lit{Kind: "float", Val: 0.0}
	default:
		return ast.Lit{Kind: "text", Val: ""}
	}
}

// endOfQuoted returns the index of the `"` that closes the string literal opening
// at s[i] (which must be a `"`), or -1 if the string never closes.
//
// It is the one place the language decides where a quoted value stops, and it
// stops later than "at the next quote" for exactly one reason: a `{…}`
// interpolation holds an *expression*, and an expression may contain a string
// literal of its own. `class "x-avi-c{contains(name, "e")}"` is one attribute
// with one interpolation in it, not an attribute that ended at `x-avi-c{contains(name, `.
// Before this scan the inner quote ended the attribute, the rest of the line
// became an unparsable tail, and the node reported as `unknown view node "box"`
// — which is how a library component ended up hashing a name with `len()` and a
// ladder of `if` probes instead of one expression.
//
// A `\` escapes the byte after it, so `"he said \"hi\""` still closes where it
// always did. An unterminated `{` is not an interpolation: the scan falls back to
// treating it as an ordinary character so the string closes at the next quote and
// parseTextBody gets to report the unterminated brace against the real value.
func endOfQuoted(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return j
		case '{':
			if end := endOfInterp(s, j); end >= 0 {
				j = end
			}
		}
	}
	return -1
}

// endOfInterp returns the index of the `}` closing the interpolation opening at
// s[i] (which must be a `{`), or -1 if it never closes. Braces nest and a string
// literal inside is opaque, so `{f("}")}` ends where the author meant it to.
func endOfInterp(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '\\':
			// A backslash escapes the byte after it here too, so `\"` inside an
			// interpolation does not open a nested string. The interpolation then
			// ends where the author meant, and the expression parser gets to say
			// what is actually wrong with `contains(name, \"e\")` — see interpErr.
			j++
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j
			}
		case '"':
			e := endOfQuoted(s, j)
			if e < 0 {
				return -1
			}
			j = e
		}
	}
	return -1
}

// indexTop returns the index of the first occurrence of sep in s that is outside
// every quoted string, or -1.
//
// Every `"label" -> value` line splits on the first arrow the author wrote
// outside a string — which is not the first arrow in the line once a label may
// hold a string of its own: `link "A -> B" -> "/ab"` split at the wrong one, and
// so would a `->` inside an interpolation's nested literal. The same helper finds
// the `placeholder`/`label` keywords, for the same reason.
func indexTop(s, sep string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			e := endOfQuoted(s, i)
			if e < 0 {
				return -1
			}
			i = e
			continue
		}
		if strings.HasPrefix(s[i:], sep) {
			return i
		}
	}
	return -1
}

// matchParen returns the index of the `)` that closes the `(` at s[i] (which
// must be a `(`), or -1 if it never closes.
//
// It is endOfQuoted's rule applied to parentheses: nesting counts, and a quoted
// string inside is opaque. It exists because `parseEntityPath` split its target
// on the FIRST `)` in the line, which made `set Product(CartLine(lid).product)
// .stock = 0` a parse error against the same text `check` accepts one line up —
// the expression parser has always balanced its parentheses, and the statement
// scanner in front of it had not.
func matchParen(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '"':
			e := endOfQuoted(s, j)
			if e < 0 {
				return -1
			}
			j = e
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// indexOutside returns the index of the first occurrence of sep in s that is
// outside every quoted string AND every bracket, or -1.
//
// It is indexTop's question asked of a *statement* rather than a view node: the
// clause keywords that shape a bulk write (` in `, ` where `) are the statement's
// own only at the top level, and a nested aggregate carries the identical words
// inside its parentheses — `set Product(id).stock = count(l in Line where …)` is
// a by-id write, not a filtered one, and the words that would say otherwise
// belong to the count.
func indexOutside(s, sep string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			e := endOfQuoted(s, i)
			if e < 0 {
				return -1
			}
			i = e
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
			continue
		case c == ')' || c == ']' || c == '}':
			depth--
			continue
		}
		if depth == 0 && strings.HasPrefix(s[i:], sep) {
			return i
		}
	}
	return -1
}

// indexAssign returns the index of the `=` that separates an assignment's target
// from its value: the first `=` outside every quoted string and every bracket
// that is not part of a comparison operator (`==`, `!=`, `<=`, `>=`).
//
// `set` used to take the first `=` in the line, which is the assignment only
// until a key expression contains one — `set Product(p == q).x = 1`, or a
// string key holding an `=`. The same scan serves the filtered form's field
// assignments, so a bulk update and a by-id one agree about where the value
// starts.
func indexAssign(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			e := endOfQuoted(s, i)
			if e < 0 {
				return -1
			}
			i = e
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == '=' && depth == 0:
			if i+1 < len(s) && s[i+1] == '=' {
				i++ // `==` is a comparison, not the assignment
				continue
			}
			if i > 0 && (s[i-1] == '!' || s[i-1] == '<' || s[i-1] == '>') {
				continue // the tail of `!=` / `<=` / `>=`
			}
			return i
		}
	}
	return -1
}

// unquoteText decodes a quoted value written in an interpolating position — node
// text, a label, a placeholder, a link destination, `meta`.
//
// It is unquote plus one allowance: the value may hold a `{…}` whose expression
// contains a string literal, which Go's own unquoter reads as the end of the
// value. Those inner quotes are re-escaped before the decode, so the decode is
// still exactly Go's — one pass of `\n`, `\t`, `\"` over the whole value, nothing
// invented — and the two spellings `{contains(n, "e")}` and `{contains(n, \"e\")}`
// reach the expression parser as the same characters.
func unquoteText(s string, line int) (string, error) {
	if len(s) < 2 || s[0] != '"' || endOfQuoted(s, 0) != len(s)-1 {
		return "", &Error{line, fmt.Sprintf("expected a quoted string, got %q", s)}
	}
	v, err := strconv.Unquote(`"` + escapeInterpQuotes(s[1:len(s)-1]) + `"`)
	if err != nil {
		return "", &Error{line, fmt.Sprintf("invalid string %q", s)}
	}
	return v, nil
}

// escapeInterpQuotes rewrites the bare `"` characters inside a `{…}` as `\"`, so
// that a value the scanner accepted is a string literal Go can decode. Outside
// the braces nothing is touched, and an already-escaped byte is copied as the
// pair it is — that is what makes the two spellings converge instead of one of
// them losing a backslash.
func escapeInterpQuotes(body string) string {
	if !strings.ContainsRune(body, '{') {
		return body
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '\\' && i+1 < len(body):
			b.WriteByte(body[i])
			i++
			b.WriteByte(body[i])
		case body[i] == '{':
			end := endOfInterp(body, i)
			if end < 0 {
				b.WriteByte(body[i])
				continue
			}
			for j := i; j <= end; j++ {
				switch {
				case body[j] == '\\' && j+1 <= end:
					b.WriteByte(body[j])
					j++
					b.WriteByte(body[j])
				case body[j] == '"':
					b.WriteString(`\"`)
				default:
					b.WriteByte(body[j])
				}
			}
			i = end
		default:
			b.WriteByte(body[i])
		}
	}
	return b.String()
}

func unquote(s string, line int) (string, error) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", &Error{line, fmt.Sprintf("expected a quoted string, got %q", s)}
	}
	v, err := strconv.Unquote(s)
	if err != nil {
		return "", &Error{line, fmt.Sprintf("invalid string %q", s)}
	}
	return v, nil
}
