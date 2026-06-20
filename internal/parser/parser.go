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
	app, err := parseFacet(appNode)
	if err != nil {
		return nil, err
	}
	app.Imports = imports
	return app, nil
}

// parseFacet dispatches on the facet kind keyword that opens a file. A plain
// `app` is the original self-contained graph; the typed kinds compose as bricks.
func parseFacet(n *source.Node) (*ast.App, error) {
	switch firstWord(n.Line.Text) {
	case "app":
		return parseApp(n)
	case "playground":
		return parsePlayground(n)
	case "wireframe":
		return parseWireframe(n)
	case "ui":
		return parseUIData(n, "ui")
	case "data":
		return parseUIData(n, "data")
	default:
		return nil, &Error{n.Line.No, "file must start with `app`, `playground`, `wireframe`, `ui`, or `data`"}
	}
}

func parseApp(n *source.Node) (*ast.App, error) {
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
		if err := parseDecl(app, c); err != nil {
			return nil, err
		}
	}
	return app, nil
}

// parseDecl parses one body item shared by `app`, `ui`, and `data` facets — the
// full vocabulary of entities, state, logic, and UI. Kind-specific guards (e.g.
// a `ui` facet may not declare an entity) are applied by the caller via the
// returned facet's contents; here we only parse what is structurally a member.
func parseDecl(app *ast.App, c *source.Node) error {
	var err error
	switch {
	case c.Line.Text == "auth" || c.Line.Text == "auth:":
		app.Auth = true
	case strings.HasPrefix(c.Line.Text, "entity "):
		var e *ast.Entity
		if e, err = parseEntity(c); err == nil {
			app.Entities = append(app.Entities, e)
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
	case c.Line.Text == "theme:" || c.Line.Text == "theme":
		var tv []ast.ThemeVar
		if tv, err = parseTheme(c); err == nil {
			app.Theme = append(app.Theme, tv...)
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
	case strings.HasPrefix(c.Line.Text, "view "):
		var v *ast.View
		if v, err = parseView(c); err == nil {
			app.Views = append(app.Views, v)
		}
	default:
		err = &Error{c.Line.No, fmt.Sprintf("unexpected %q; expected entity/enum/state/derive/policy/action/job/component/layout/theme/view", firstWord(c.Line.Text))}
	}
	return err
}

// parsePlayground parses `playground Name:` — the baseplate. It holds global
// concerns (auth, theme) and mounts exactly one wireframe; it accepts nothing
// else, because a playground only takes a wireframe.
func parsePlayground(n *source.Node) (*ast.App, error) {
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
		case t == "theme:" || t == "theme":
			tv, err := parseTheme(c)
			if err != nil {
				return nil, err
			}
			app.Theme = append(app.Theme, tv...)
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
func parseWireframe(n *source.Node) (*ast.App, error) {
	name := strings.TrimSuffix(keywordRest(n.Line.Text, "wireframe"), ":")
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid wireframe name %q", name)}
	}
	app := &ast.App{Name: name, Kind: "wireframe", Line: n.Line.No}
	for _, c := range n.Children {
		t := strings.TrimSpace(c.Line.Text)
		switch {
		case t == "theme:" || t == "theme":
			tv, err := parseTheme(c)
			if err != nil {
				return nil, err
			}
			app.Theme = append(app.Theme, tv...)
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
// Both contribute a `content:` node tree placed at the socket.
func parseUIData(n *source.Node, kind string) (*ast.App, error) {
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
		if err := parseDecl(app, c); err != nil {
			return nil, err
		}
	}
	if app.Content == nil {
		return nil, &Error{n.Line.No, fmt.Sprintf("%s facet %q needs a `content:` block to fill socket %q", kind, name, socket)}
	}
	if kind == "ui" && len(app.Entities) > 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("ui facet %q may not declare an entity — move durable data into a `data` facet", name)}
	}
	if len(app.Views) > 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("%s facet %q provides `content` for a socket, not routed `view`s", kind, name)}
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
	if !isIdent(name) || !isUpper(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("entity name %q must be capitalized", name)}
	}
	e := &ast.Entity{Name: name, Line: n.Line.No}
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
		// A trailing `@secret` marks the field encrypted at rest.
		secret := false
		if strings.HasSuffix(ft, "@secret") {
			secret = true
			ft = strings.TrimSpace(strings.TrimSuffix(ft, "@secret"))
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
		e.Fields = append(e.Fields, ast.EntityField{Name: fn, Type: core, Secret: secret, Optional: optional, Line: c.Line.No})
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

// parseComponent: `component Name(params):` then a node tree.
func parseComponent(n *source.Node) (*ast.Component, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "component")), ":")
	name, params, err := parseSignature(head, n.Line.No)
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
	if !hasSlot(nodes) {
		return nil, &Error{n.Line.No, fmt.Sprintf("layout %q must contain a `slot`", name)}
	}
	return &ast.Layout{Name: name, Root: nodes, Line: n.Line.No}, nil
}

func hasSlot(nodes []ast.Node) bool {
	for _, n := range nodes {
		switch t := n.(type) {
		case ast.Slot:
			return true
		case ast.Box:
			if hasSlot(t.Children) {
				return true
			}
		case ast.Row:
			if hasSlot(t.Children) {
				return true
			}
		case ast.If:
			if hasSlot(t.Body) {
				return true
			}
		}
	}
	return false
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
		out = append(out, ast.ThemeVar{Name: fields[0], Value: val})
	}
	if len(out) == 0 {
		return nil, &Error{n.Line.No, "theme block is empty"}
	}
	return out, nil
}

// parseState: `state name: Type = default [@client|@server]`
func parseState(n *source.Node) (*ast.State, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "state"))
	place := ast.PlaceInfer
	for _, ann := range []string{"@client", "@server"} {
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
	name, params, err := parseSignature(head, n.Line.No)
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
	name, params, err := parseSignature(head, n.Line.No)
	if err != nil {
		return nil, err
	}
	a := &ast.Action{Name: name, Params: params, Optimistic: optimistic, Line: n.Line.No}
	for _, c := range n.Children {
		t := c.Line.Text
		switch {
		case strings.HasPrefix(t, "check "):
			chk, err := parseCheck(strings.TrimSpace(t[len("check "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			a.Checks = append(a.Checks, chk)
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

func parseSignature(head string, line int) (string, []ast.Param, error) {
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
			core, list, optional := splitType(pt)
			if list {
				return "", nil, &Error{line, fmt.Sprintf("parameter %q cannot be a list", pn)}
			}
			if !isIdent(pn) || !isTypeName(core) {
				return "", nil, &Error{line, fmt.Sprintf("invalid parameter %q", strings.TrimSpace(p))}
			}
			params = append(params, ast.Param{Name: pn, Type: core, Optional: optional})
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

func parseSet(n *source.Node) (ast.Stmt, error) {
	rest := strings.TrimSpace(n.Line.Text[len("set "):])
	eq := strings.IndexByte(rest, '=')
	if eq < 0 {
		return nil, &Error{n.Line.No, "set needs `set Entity(key).field = expr`"}
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

func parseRemove(n *source.Node) (ast.Stmt, error) {
	rest := strings.TrimSpace(n.Line.Text[len("remove "):])
	open := strings.IndexByte(rest, '(')
	close := strings.LastIndexByte(rest, ')')
	if open < 0 || close < open {
		return nil, &Error{n.Line.No, "remove needs `remove Entity(key)`"}
	}
	ent := strings.TrimSpace(rest[:open])
	key, err := parseExpr(strings.TrimSpace(rest[open+1:close]), n.Line.No)
	if err != nil {
		return nil, err
	}
	return ast.Remove{Entity: ent, Key: key, Line: n.Line.No}, nil
}

// parseEntityPath parses `Entity(key).field`.
func parseEntityPath(s string, line int) (string, ast.Expr, string, error) {
	open := strings.IndexByte(s, '(')
	close := strings.IndexByte(s, ')')
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
	nodes, err := parseNodes(n.Children)
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
		t := c.Line.Text
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
		case strings.HasPrefix(t, "image "):
			segs, err := parseText(strings.TrimSpace(t[len("image "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Image{Segs: segs})
		case strings.HasPrefix(t, "button "):
			b, err := parseButton(strings.TrimSpace(t[len("button "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
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
			u, err := parseUse(strings.TrimSpace(t[len("use "):]), c.Line.No)
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
	}
	return out, nil
}

// parseFor: `for item in Collection [where cond] [by field desc|asc] [limit n]:`
func parseFor(n *source.Node) (ast.Node, error) {
	head := strings.TrimSuffix(strings.TrimSpace(n.Line.Text[len("for "):]), ":")
	fields := strings.Fields(head)
	if len(fields) < 3 || fields[1] != "in" {
		return nil, &Error{n.Line.No, "for needs `for item in Collection [where cond] [by field desc|asc] [limit n]:`"}
	}
	if !isIdent(fields[0]) || !isIdent(fields[2]) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid for clause %q", head)}
	}
	f := ast.For{Var: fields[0], Coll: fields[2]}

	// remainder after `<var> in <Coll>`
	rest := strings.TrimSpace(head[len(fields[0]):])
	rest = strings.TrimSpace(rest[len("in"):])
	rest = strings.TrimSpace(rest[len(fields[2]):])

	whereS, byS, limitS, err := splitForClauses(rest, n.Line.No)
	if err != nil {
		return nil, err
	}
	if whereS != "" {
		e, err := parseExpr(whereS, n.Line.No)
		if err != nil {
			return nil, err
		}
		f.Where = e
	}
	if byS != "" {
		bp := strings.Fields(byS)
		if len(bp) < 1 || len(bp) > 2 || !isIdent(bp[0]) {
			return nil, &Error{n.Line.No, "ordering is `by field [desc|asc]`"}
		}
		f.Order = bp[0]
		if len(bp) == 2 {
			switch bp[1] {
			case "desc":
				f.Desc = true
			case "asc":
				f.Desc = false
			default:
				return nil, &Error{n.Line.No, fmt.Sprintf("order direction must be `desc` or `asc`, got %q", bp[1])}
			}
		}
	}
	if limitS != "" {
		nlim, err := strconv.Atoi(limitS)
		if err != nil || nlim <= 0 {
			return nil, &Error{n.Line.No, fmt.Sprintf("limit needs a positive integer, got %q", limitS)}
		}
		f.Limit = nlim
	}

	kids, err := parseNodes(n.Children)
	if err != nil {
		return nil, err
	}
	f.Body = kids
	return f, nil
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
	arrow := strings.Index(s, "->")
	if arrow < 0 {
		return nil, &Error{line, `link needs a destination: link "Home" -> "/"`}
	}
	label, err := unquote(strings.TrimSpace(s[:arrow]), line)
	if err != nil {
		return nil, err
	}
	path, err := unquote(strings.TrimSpace(s[arrow+2:]), line)
	if err != nil {
		return nil, err
	}
	return ast.Link{Label: label, Path: path}, nil
}

func parseInput(s string, line int) (ast.Node, error) {
	// `input bind name [placeholder "text"]`
	if !strings.HasPrefix(s, "bind ") {
		return nil, &Error{line, `input needs a binding: input bind stateName`}
	}
	rest := strings.TrimSpace(s[len("bind "):])
	in := ast.Input{}
	if ph := strings.Index(rest, "placeholder "); ph >= 0 {
		p, err := unquote(strings.TrimSpace(rest[ph+len("placeholder "):]), line)
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
	sel := ast.Select{Bind: bind}
	for _, c := range n.Children {
		if !strings.HasPrefix(c.Line.Text, "option ") {
			return nil, &Error{c.Line.No, `select children must be options: option "Label" -> "value"`}
		}
		rest := strings.TrimSpace(c.Line.Text[len("option "):])
		arrow := strings.Index(rest, "->")
		var label, value string
		var err error
		if arrow < 0 {
			label, err = unquote(rest, c.Line.No)
			value = label
		} else {
			label, err = unquote(strings.TrimSpace(rest[:arrow]), c.Line.No)
			if err == nil {
				value, err = unquote(strings.TrimSpace(rest[arrow+2:]), c.Line.No)
			}
		}
		if err != nil {
			return nil, err
		}
		sel.Options = append(sel.Options, ast.Option{Label: label, Value: value})
	}
	return sel, nil
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
	return ast.Form{Action: btn.Action, Args: btn.Args, Submit: litString(btn.Label), Body: kids}, nil
}

// litString flattens label segments to their literal text — used where a label is
// plain (a form's submit button), so interpolation isn't meaningful.
func litString(segs []ast.Seg) string {
	var sb strings.Builder
	for _, s := range segs {
		sb.WriteString(s.Lit)
	}
	return sb.String()
}

// parseUpload: `upload bind url [label "text"]`.
func parseUpload(s string, line int) (ast.Node, error) {
	if !strings.HasPrefix(s, "bind ") {
		return nil, &Error{line, "upload needs a binding: upload bind avatarUrl"}
	}
	rest := strings.TrimSpace(s[len("bind "):])
	up := ast.Upload{Label: "Upload"}
	if lp := strings.Index(rest, "label "); lp >= 0 {
		l, err := unquote(strings.TrimSpace(rest[lp+len("label "):]), line)
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

// parseUse: `use Component(arg, ...)` — invoke a reusable view fragment.
func parseUse(s string, line int) (ast.Node, error) {
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
	return ast.Use{Name: name, Args: args}, nil
}

func parseText(s string, line int) ([]ast.Seg, error) {
	str, err := unquote(s, line)
	if err != nil {
		return nil, err
	}
	var segs []ast.Seg
	var lit strings.Builder
	for i := 0; i < len(str); i++ {
		if str[i] == '{' {
			end := strings.IndexByte(str[i:], '}')
			if end < 0 {
				return nil, &Error{line, "unterminated `{` in text"}
			}
			if lit.Len() > 0 {
				segs = append(segs, ast.Seg{Lit: lit.String()})
				lit.Reset()
			}
			e, err := parseExpr(str[i+1:i+end], line)
			if err != nil {
				return nil, err
			}
			segs = append(segs, ast.Seg{Expr: e})
			i += end
			continue
		}
		lit.WriteByte(str[i])
	}
	if lit.Len() > 0 {
		segs = append(segs, ast.Seg{Lit: lit.String()})
	}
	return segs, nil
}

// parseButton: `"label" -> action` or `"label" -> action(arg, ...)`
func parseButton(s string, line int) (ast.Button, error) {
	arrow := strings.Index(s, "->")
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
	return ast.Button{Label: label, Action: name, Args: args}, nil
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
