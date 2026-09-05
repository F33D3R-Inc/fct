// Package parser turns the source indentation tree into an ast.App. The grammar
// is line-oriented: a header line plus its nested children. Expressions go
// through a precedence-climbing parser (expr.go).
package parser

import (
	"fmt"
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
	case strings.HasPrefix(c.Line.Text, "enum "):
		var en *ast.Enum
		if en, err = parseEnum(c); err == nil {
			app.Enums = append(app.Enums, en)
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
	case strings.HasPrefix(c.Line.Text, "job "):
		var j *ast.Job
		if j, err = parseJob(c); err == nil {
			app.Jobs = append(app.Jobs, j)
		}
	case strings.HasPrefix(c.Line.Text, "service "):
		var sv *ast.Service
		if sv, err = parseService(c); err == nil {
			app.Services = append(app.Services, sv)
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
			app.Views = append(app.Views, v)
		}
	default:
		err = &Error{c.Line.No, fmt.Sprintf("unexpected %q; expected entity/record/enum/state/derive/policy/action/job/service/webhook/component/layout/theme/view", firstWord(c.Line.Text))}
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
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("entity name %q must be capitalized", name)}
	}
	e := &ast.Entity{Name: name, SoftDelete: softDelete, Line: n.Line.No}
	for _, c := range n.Children {
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
		secret, e2e, unique, required := false, false, false, false
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
		// Bare flag markers, any order.
		for _, m := range []struct {
			name string
			dst  *bool
		}{{"@secret", &secret}, {"@e2e", &e2e}, {"@unique", &unique}, {"@required", &required}} {
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
		e.Fields = append(e.Fields, ast.EntityField{Name: fn, Type: core, Secret: secret, E2E: e2e, ReadPolicy: readPolicy, Optional: optional,
			Unique: unique, Required: required, Min: fmin, Max: fmax, Matches: matches, Line: c.Line.No})
	}
	if len(e.Fields) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("entity %q has no fields", name)}
	}
	return e, nil
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
		if !isIdent(v) {
			return &Error{n.Line.No, fmt.Sprintf("enum value %q must be an identifier", v)}
		}
		en.Values = append(en.Values, v)
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
		case strings.HasPrefix(t, "let "):
			// Request→response bind: `let name = call Service.op(args)`.
			rest := strings.TrimSpace(t[len("let "):])
			eq := strings.IndexByte(rest, '=')
			if eq < 0 {
				return nil, &Error{c.Line.No, "let needs `let name = call Service.op(args)`"}
			}
			name := strings.TrimSpace(rest[:eq])
			if !isIdent(name) {
				return nil, &Error{c.Line.No, fmt.Sprintf("invalid let binding %q", name)}
			}
			rhs := strings.TrimSpace(rest[eq+1:])
			if !strings.HasPrefix(rhs, "call ") {
				return nil, &Error{c.Line.No, "let binds a service call: `let name = call Service.op(args)`"}
			}
			cl, err := parseCall(strings.TrimSpace(rhs[len("call "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			cl.Bind = name
			a.Body = append(a.Body, cl)
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
		default:
			eq := strings.IndexByte(t, '=')
			if eq < 0 {
				return nil, &Error{c.Line.No, fmt.Sprintf("unknown statement %q", firstWord(t))}
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

// parseSignature parses `name(p: T, ...)`. allowList permits list-typed params
// (`p: [T]`) — used for service operations, whose ops genuinely take collections
// (`rank(posts: [int])`); action/component/policy params stay scalar.
// parseSignature parses `name(p: T, ...)`. allowList admits `[T]` parameters
// (service operations only). allowRef admits the by-reference parameter forms a
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
			segs, alt, altSet, err := parseMedia(strings.TrimSpace(t[len("video "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Video{Segs: segs, Alt: alt, AltSet: altSet, Line: c.Line.No})
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
		case strings.HasPrefix(t, "overlay "):
			ov, err := parseOverlay(c)
			if err != nil {
				return nil, err
			}
			out = append(out, ov)
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
			return nil, &Error{c.Line.No, fmt.Sprintf("unknown view node %q", firstWord(t))}
		}
		// A node carrying a modifier is decorated in place. The cases above each emit
		// exactly one node, so wrapping the just-appended one is unambiguous.
		if (len(class) > 0 || style != "" || anchor != "") && len(out) == before+1 {
			out[before] = ast.Modified{Class: class, Style: style, Anchor: anchor, Inner: out[before], Line: c.Line.No}
		}
	}
	return out, nil
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

// parseFor: `for item in Collection [where cond] [by field desc|asc] [limit n]:`
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

	whereS, byS, limitS, err := splitForClauses(rest, line)
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
	return rg, nil
}

// splitForClauses splits a `for` header tail into its where/by/limit clause
// bodies. It scans at the top level (skipping string literals and parens) so a
// quoted value like `"stand by"` inside a `where` is never mistaken for a clause
// keyword. The clauses, if present, must appear in where→by→limit order.
func splitForClauses(rest string, line int) (whereS, byS, limitS string, err error) {
	if strings.TrimSpace(rest) == "" {
		return "", "", "", nil
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
			if w := rest[i:j]; w == "where" || w == "by" || w == "limit" {
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
	}{{"where", 5}, {"by", 2}, {"limit", 5}}
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

	// reject stray text before the first clause keyword.
	first := len(rest)
	for _, k := range known {
		if p, ok := pos[k.name]; ok && p < first {
			first = p
		}
	}
	if leftover := strings.TrimSpace(rest[:first]); leftover != "" {
		return "", "", "", &Error{line, fmt.Sprintf("unexpected %q in for header (clauses are where/by/limit, in that order)", leftover)}
	}
	return whereS, byS, limitS, nil
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
	// `input bind name [placeholder "text"]`
	if !strings.HasPrefix(s, "bind ") {
		return nil, &Error{line, `input needs a binding: input bind stateName`}
	}
	rest := strings.TrimSpace(s[len("bind "):])
	in := ast.Input{}
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
	return in, nil
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
	if i := indexTop(rest, "alt "); i >= 0 {
		alt, err = parseText(strings.TrimSpace(rest[i+len("alt "):]), line)
		if err != nil {
			return nil, nil, false, err
		}
		altSet, rest = true, strings.TrimSpace(rest[:i])
	}
	segs, err = parseText(rest, line)
	if err != nil {
		return nil, nil, false, err
	}
	return segs, alt, altSet, nil
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
func isType(s string) bool {
	switch s {
	case "int", "text", "bool", "money", "date":
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

// splitTop splits on sep at the top level (ignoring separators inside parens or
// quotes), so `add Post { body: f(a, b) }` and string args survive.
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
		case c == '(' || c == '{':
			depth++
		case c == ')' || c == '}':
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
