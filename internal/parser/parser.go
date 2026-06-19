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

// Parse compiles source text to an ast.App. Exactly one `app` per file.
func Parse(src string) (*ast.App, error) {
	roots, err := source.Parse(src)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, &Error{0, "empty source: expected an `app Name:` definition"}
	}
	if len(roots) > 1 {
		return nil, &Error{roots[1].Line.No, "only one `app` definition per file"}
	}
	return parseApp(roots[0])
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
	app := &ast.App{Name: name, Line: n.Line.No}
	for _, c := range n.Children {
		var err error
		switch {
		case c.Line.Text == "auth" || c.Line.Text == "auth:":
			app.Auth = true
		case strings.HasPrefix(c.Line.Text, "entity "):
			var e *ast.Entity
			if e, err = parseEntity(c); err == nil {
				app.Entities = append(app.Entities, e)
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
			err = &Error{c.Line.No, fmt.Sprintf("unexpected %q; expected entity/state/derive/policy/action/job/view", firstWord(c.Line.Text))}
		}
		if err != nil {
			return nil, err
		}
	}
	return app, nil
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
		// A field type is a primitive or an entity name (a relation, stored as the
		// referenced row's id). Entity existence is validated later, in the IR.
		if !isType(ft) && !(isIdent(ft) && isUpper(ft)) {
			return nil, &Error{c.Line.No, fmt.Sprintf("unknown type %q (use int, text, bool, or an entity name)", ft)}
		}
		e.Fields = append(e.Fields, ast.EntityField{Name: fn, Type: ft, Secret: secret, Line: c.Line.No})
	}
	if len(e.Fields) == 0 {
		return nil, &Error{n.Line.No, fmt.Sprintf("entity %q has no fields", name)}
	}
	return e, nil
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
	if eq := strings.IndexByte(after, '='); eq >= 0 {
		typ, defSrc = strings.TrimSpace(after[:eq]), strings.TrimSpace(after[eq+1:])
	} else {
		typ = after
	}
	if !isType(typ) {
		return nil, &Error{n.Line.No, fmt.Sprintf("unknown type %q (use int, text, or bool)", typ)}
	}
	st := &ast.State{Name: name, Type: typ, Placement: place, Line: n.Line.No}
	if defSrc == "" {
		st.Default = defaultFor(typ)
	} else {
		e, err := parseExpr(defSrc, n.Line.No)
		if err != nil {
			return nil, err
		}
		st.Default = e
	}
	return st, nil
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
	if !isType(typ) {
		return nil, &Error{n.Line.No, fmt.Sprintf("unknown type %q (use int, text, or bool)", typ)}
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

// parseAction: `action name(params):` then `requires …` and statement lines.
func parseAction(n *source.Node) (*ast.Action, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "action")), ":")
	name, params, err := parseSignature(head, n.Line.No)
	if err != nil {
		return nil, err
	}
	a := &ast.Action{Name: name, Params: params, Line: n.Line.No}
	for _, c := range n.Children {
		t := c.Line.Text
		switch {
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
			if !isIdent(pn) || !isType(pt) {
				return "", nil, &Error{line, fmt.Sprintf("invalid parameter %q", strings.TrimSpace(p))}
			}
			params = append(params, ast.Param{Name: pn, Type: pt})
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

// parseView: `view Name [at "/path"]:` then a node tree.
func parseView(n *source.Node) (*ast.View, error) {
	head := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(n.Line.Text, "view")), ":")
	name := head
	path := ""
	if i := strings.Index(head, " at "); i >= 0 {
		name = strings.TrimSpace(head[:i])
		p, err := unquote(strings.TrimSpace(head[i+len(" at "):]), n.Line.No)
		if err != nil {
			return nil, err
		}
		path = p
	}
	if !isIdent(name) {
		return nil, &Error{n.Line.No, fmt.Sprintf("invalid view name %q", name)}
	}
	v := &ast.View{Name: name, Path: path, Line: n.Line.No}
	nodes, err := parseNodes(n.Children)
	if err != nil {
		return nil, err
	}
	v.Root = nodes
	return v, nil
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
		case strings.HasPrefix(t, "text "):
			segs, err := parseText(strings.TrimSpace(t[len("text "):]), c.Line.No)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Text{Segs: segs})
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
	label, err := unquote(strings.TrimSpace(s[:arrow]), line)
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

func isType(s string) bool { return s == "int" || s == "text" || s == "bool" }

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
