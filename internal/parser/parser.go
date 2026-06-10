// Package parser turns the lexer's structural token stream into an ast.Facet.
//
// Two layers:
//   - The facet-skeleton parser walks INDENT/DEDENT-delimited blocks
//     (what / looks / when). Here the offside rule is reliable.
//   - The looks-body parser ignores the outer INDENT/DEDENT (HTML line-wraps
//     produce spurious ones) and instead re-derives structure from each line's
//     own indentation, treating ONLY `if` / `for` / `else` lines as structural.
//
// Block keywords: `what` (data), `looks` (template), `when <event>` (handler).
// See README.md (the language reference) and DECISIONS.md (ADR-0002/0003/0005).
package parser

import (
	"fmt"
	"strings"

	"fct.dev/internal/ast"
	"fct.dev/internal/lexer"
)

// Parse lexes and parses src into the facets it declares.
func Parse(src string) ([]*ast.Facet, error) {
	toks, err := lexer.Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var facets []*ast.Facet
	for !p.eof() {
		f, err := p.parseFacet()
		if err != nil {
			return nil, err
		}
		facets = append(facets, f)
	}
	if len(facets) == 0 {
		return nil, &perr{0, 0, "no facet found in source"}
	}
	return facets, nil
}

type parser struct {
	toks []lexer.Token
	pos  int
}

type perr struct {
	line, col int
	msg       string
}

func (e *perr) Error() string { return fmt.Sprintf("%d:%d: %s", e.line, e.col, e.msg) }

// ── token cursor ────────────────────────────────────────────────────────────

func (p *parser) eof() bool { return p.pos >= len(p.toks) }

func (p *parser) peek() (lexer.Token, bool) {
	if p.eof() {
		return lexer.Token{}, false
	}
	return p.toks[p.pos], true
}

func (p *parser) next() lexer.Token {
	t := p.toks[p.pos]
	p.pos++
	return t
}

func (p *parser) at(k lexer.Kind) bool {
	t, ok := p.peek()
	return ok && t.Kind == k
}

func (p *parser) expect(k lexer.Kind, what string) (lexer.Token, error) {
	t, ok := p.peek()
	if !ok {
		return lexer.Token{}, &perr{0, 0, "unexpected end of input, expected " + what}
	}
	if t.Kind != k {
		return lexer.Token{}, &perr{t.Line, t.Col, fmt.Sprintf("expected %s, got %s", what, t.Kind)}
	}
	return p.next(), nil
}

func (p *parser) expectLine(what string) (lexer.Token, error) {
	return p.expect(lexer.LINE, what)
}

// ── facet skeleton ──────────────────────────────────────────────────────────

func (p *parser) parseFacet() (*ast.Facet, error) {
	hdr, err := p.expectLine("`facet <Name>:`")
	if err != nil {
		return nil, err
	}
	name, err := parseFacetHeader(hdr)
	if err != nil {
		return nil, err
	}
	f := &ast.Facet{Name: name, Pos: ast.Pos{Line: hdr.Line, Col: hdr.Col}}

	if _, err := p.expect(lexer.INDENT, "indented facet body"); err != nil {
		return nil, err
	}

	for !p.at(lexer.DEDENT) && !p.eof() {
		t, _ := p.peek()
		if t.Kind != lexer.LINE {
			return nil, &perr{t.Line, t.Col, "expected a section keyword (what/looks/when)"}
		}
		head := t.Text
		switch {
		case head == "what:":
			if err := p.parseWhat(f); err != nil {
				return nil, err
			}
		case head == "looks:":
			if err := p.parseLooks(f); err != nil {
				return nil, err
			}
		case head == "when" || strings.HasPrefix(head, "when "):
			if err := p.parseWhen(f); err != nil {
				return nil, err
			}
		case strings.HasPrefix(head, "facet-id:"):
			id, err := parseFacetIDLine(t)
			if err != nil {
				return nil, err
			}
			f.FacetID = id
			p.next()
		// Helpful migration errors for renamed/removed blocks.
		case head == "data:":
			return nil, &perr{t.Line, t.Col, "`data:` was renamed to `what:`"}
		case head == "render:":
			return nil, &perr{t.Line, t.Col, "`render:` was renamed to `looks:`"}
		case head == "subscribe:":
			return nil, &perr{t.Line, t.Col, "`subscribe:` was removed; subscription is implied by `when <event>:`"}
		case strings.HasPrefix(head, "update on"):
			return nil, &perr{t.Line, t.Col, "`update on <event>:` was renamed to `when <event>:`"}
		case head == "who:":
			if err := p.parseWho(f); err != nil {
				return nil, err
			}
		case head == "auth:":
			return nil, &perr{t.Line, t.Col, "`auth:` was renamed to `who:`"}
		case head == "style:" || head == "error:":
			return nil, &perr{t.Line, t.Col, fmt.Sprintf("`%s` is not supported in v0 (ADR-0003)", strings.TrimSuffix(head, ":"))}
		default:
			return nil, &perr{t.Line, t.Col, fmt.Sprintf("unexpected line in facet body: %q", head)}
		}
	}

	if _, err := p.expect(lexer.DEDENT, "end of facet body"); err != nil {
		return nil, err
	}
	return f, nil
}

func parseFacetHeader(t lexer.Token) (string, error) {
	s := strings.TrimSuffix(t.Text, ":")
	if s == t.Text {
		return "", &perr{t.Line, t.Col, "facet header must end with ':'"}
	}
	parts := strings.Fields(s)
	if len(parts) != 2 || parts[0] != "facet" {
		return "", &perr{t.Line, t.Col, "expected `facet <Name>:`"}
	}
	if !isIdent(parts[1]) {
		return "", &perr{t.Line, t.Col, fmt.Sprintf("invalid facet name %q", parts[1])}
	}
	return parts[1], nil
}

func parseFacetIDLine(t lexer.Token) (string, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(t.Text, "facet-id:"))
	s, ok := unquote(rest)
	if !ok {
		return "", &perr{t.Line, t.Col, "facet-id must be a double-quoted string"}
	}
	return s, nil
}

// ── who (authorization) ─────────────────────────────────────────────────────

func (p *parser) parseWho(f *ast.Facet) error {
	p.next() // consume `who:`
	if _, err := p.expect(lexer.INDENT, "indented who block"); err != nil {
		return err
	}
	for p.at(lexer.LINE) {
		t := p.next()
		switch {
		case strings.HasPrefix(t.Text, "require:"):
			pol := strings.TrimSpace(strings.TrimPrefix(t.Text, "require:"))
			if !isIdent(pol) {
				return &perr{t.Line, t.Col, fmt.Sprintf("require expects a policy name, got %q", pol)}
			}
			f.Who.Require = append(f.Who.Require, pol)
		case strings.HasPrefix(t.Text, "redact "):
			fields := strings.Fields(strings.TrimPrefix(t.Text, "redact "))
			if len(fields) == 0 {
				return &perr{t.Line, t.Col, "redact expects a field name"}
			}
			red := ast.Redaction{Field: fields[0]}
			switch {
			case len(fields) == 1, len(fields) == 2 && fields[1] == "always":
				// unconditional
			case len(fields) == 3 && fields[1] == "unless":
				if !isIdent(fields[2]) {
					return &perr{t.Line, t.Col, fmt.Sprintf("redact unless expects a policy name, got %q", fields[2])}
				}
				red.UnlessPolicy = fields[2]
			default:
				return &perr{t.Line, t.Col, fmt.Sprintf("bad redact: %q (use `redact <field>` or `redact <field> unless <policy>`)", t.Text)}
			}
			f.Who.Redactions = append(f.Who.Redactions, red)
		default:
			return &perr{t.Line, t.Col, fmt.Sprintf("unknown who directive: %q (expected `require:` or `redact …`)", t.Text)}
		}
	}
	if _, err := p.expect(lexer.DEDENT, "end of who block"); err != nil {
		return err
	}
	return nil
}

// ── what (data) ─────────────────────────────────────────────────────────────

func (p *parser) parseWhat(f *ast.Facet) error {
	p.next() // consume `what:`
	if _, err := p.expect(lexer.INDENT, "indented what block"); err != nil {
		return err
	}
	for p.at(lexer.LINE) {
		t := p.next()
		fld, err := parseField(t)
		if err != nil {
			return err
		}
		f.Fields = append(f.Fields, fld)
	}
	if _, err := p.expect(lexer.DEDENT, "end of what block"); err != nil {
		return err
	}
	return nil
}

func parseField(t lexer.Token) (ast.Field, error) {
	if strings.Contains(t.Text, "=") {
		return ast.Field{}, &perr{t.Line, t.Col, "computed fields (`=`) are not supported in v0 (ADR-0003)"}
	}
	name, typ, found := strings.Cut(t.Text, ":")
	if !found {
		return ast.Field{}, &perr{t.Line, t.Col, fmt.Sprintf("expected `name: Type`, got %q", t.Text)}
	}
	name = strings.TrimSpace(name)
	typ = strings.TrimSpace(typ)
	if !isIdent(name) {
		return ast.Field{}, &perr{t.Line, t.Col, fmt.Sprintf("invalid field name %q", name)}
	}
	if !isIdent(typ) {
		return ast.Field{}, &perr{t.Line, t.Col, fmt.Sprintf("invalid type %q", typ)}
	}
	return ast.Field{Name: name, Type: typ, Pos: ast.Pos{Line: t.Line, Col: t.Col}}, nil
}

// ── when (event handler) ────────────────────────────────────────────────────

func (p *parser) parseWhen(f *ast.Facet) error {
	hdr := p.next() // `when E1, E2:`
	if !strings.HasSuffix(hdr.Text, ":") {
		return &perr{hdr.Line, hdr.Col, "when header must end with ':'"}
	}
	body := strings.TrimSuffix(strings.TrimPrefix(hdr.Text, "when"), ":")
	var events []string
	for _, e := range strings.Split(body, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		events = append(events, e)
	}
	if len(events) == 0 {
		return &perr{hdr.Line, hdr.Col, "when needs at least one event"}
	}
	w := ast.When{Events: events, Pos: ast.Pos{Line: hdr.Line, Col: hdr.Col}}

	if _, err := p.expect(lexer.INDENT, "indented when block"); err != nil {
		return err
	}
	for p.at(lexer.LINE) {
		t := p.next()
		m, err := parseMutation(t)
		if err != nil {
			return err
		}
		w.Mutations = append(w.Mutations, m)
	}
	if _, err := p.expect(lexer.DEDENT, "end of when block"); err != nil {
		return err
	}
	f.Whens = append(f.Whens, w)
	return nil
}

func parseMutation(t lexer.Token) (ast.Mutation, error) {
	fields := strings.Fields(t.Text)
	pos := ast.Pos{Line: t.Line, Col: t.Col}
	if len(fields) == 0 {
		return ast.Mutation{}, &perr{t.Line, t.Col, "empty mutation"}
	}
	op := fields[0]
	switch op {
	case "replace_all":
		return ast.Mutation{Op: op, Pos: pos}, nil
	case "replace", "append", "prepend", "remove":
		if len(fields) < 2 {
			return ast.Mutation{}, &perr{t.Line, t.Col, op + " requires a target facet"}
		}
		m := ast.Mutation{Op: op, Target: fields[1], Pos: pos}
		if len(fields) > 2 {
			if fields[2] != "with" {
				return ast.Mutation{}, &perr{t.Line, t.Col, fmt.Sprintf("expected `with`, got %q", fields[2])}
			}
			m.With = strings.TrimSpace(strings.Join(fields[3:], " "))
			if m.With == "" {
				return ast.Mutation{}, &perr{t.Line, t.Col, "`with` requires an expression"}
			}
		}
		return m, nil
	default:
		return ast.Mutation{}, &perr{t.Line, t.Col, fmt.Sprintf("unknown mutation %q", op)}
	}
}

// ── looks (template) ────────────────────────────────────────────────────────

type rawLine struct {
	indent int
	text   string
	line   int
}

func (p *parser) parseLooks(f *ast.Facet) error {
	p.next() // consume `looks:`
	if _, err := p.expect(lexer.INDENT, "indented looks block"); err != nil {
		return err
	}
	// Collect raw lines until the matching DEDENT, flattening any nested
	// INDENT/DEDENT that HTML line-wraps or control bodies produced.
	var lines []rawLine
	depth := 1
	for !p.eof() && depth > 0 {
		t := p.next()
		switch t.Kind {
		case lexer.INDENT:
			depth++
		case lexer.DEDENT:
			depth--
		case lexer.LINE:
			lines = append(lines, rawLine{indent: t.Col - 1, text: t.Text, line: t.Line})
		}
	}
	nodes, err := parseLooksLines(lines)
	if err != nil {
		return err
	}
	f.Looks = nodes
	return nil
}

// parseLooksLines builds the flat node stream from collected looks lines.
func parseLooksLines(lines []rawLine) ([]ast.Node, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	nodes, _, err := parseLooksBlock(lines, 0, lines[0].indent)
	return nodes, err
}

// parseLooksBlock consumes lines whose indent >= minIndent, returning the nodes
// and the index of the first line that fell out of the block.
func parseLooksBlock(lines []rawLine, i, minIndent int) ([]ast.Node, int, error) {
	var nodes []ast.Node
	for i < len(lines) && lines[i].indent >= minIndent {
		ln := lines[i]
		op, a, b, isCtrl := matchControl(ln.text)
		if !isCtrl {
			// Block-form child facet: <Card …> on its own line, … </Card>.
			if child, ok := matchBlockChildOpen(ln); ok {
				ni, err := parseChildBody(lines, i+1, ln.indent, &child)
				if err != nil {
					return nil, 0, err
				}
				nodes = append(nodes, child)
				i = ni
				continue
			}
			// Raw HTML content line: reconstruct indentation, parse inline holes,
			// keep the trailing newline so the emitted HTML stays readable.
			inline, err := parseInline(ln)
			if err != nil {
				return nil, 0, err
			}
			nodes = append(nodes, ast.Text{S: strings.Repeat(" ", ln.indent)})
			nodes = append(nodes, inline...)
			nodes = append(nodes, ast.Text{S: "\n"})
			i++
			continue
		}

		switch op {
		case "if":
			nodes = append(nodes, ast.Ctrl{Op: "if", Expr: a, Pos: ast.Pos{Line: ln.line, Col: ln.indent + 1}})
			body, ni, err := parseLooksBlock(lines, i+1, ln.indent+1)
			if err != nil {
				return nil, 0, err
			}
			nodes = append(nodes, body...)
			i = ni
			// optional `else:` at the same indent as the `if`
			if i < len(lines) && lines[i].indent == ln.indent {
				if eop, _, _, ok := matchControl(lines[i].text); ok && eop == "else" {
					nodes = append(nodes, ast.Ctrl{Op: "else"})
					ebody, ni2, err := parseLooksBlock(lines, i+1, lines[i].indent+1)
					if err != nil {
						return nil, 0, err
					}
					nodes = append(nodes, ebody...)
					i = ni2
				}
			}
			nodes = append(nodes, ast.Ctrl{Op: "end"})
		case "for":
			nodes = append(nodes, ast.Ctrl{Op: "for", Var: a, Iter: b, Pos: ast.Pos{Line: ln.line, Col: ln.indent + 1}})
			body, ni, err := parseLooksBlock(lines, i+1, ln.indent+1)
			if err != nil {
				return nil, 0, err
			}
			nodes = append(nodes, body...)
			nodes = append(nodes, ast.Ctrl{Op: "end"})
			i = ni
		case "slot":
			body, ni, err := parseLooksBlock(lines, i+1, ln.indent+1)
			if err != nil {
				return nil, 0, err
			}
			nodes = append(nodes, ast.Slot{Default: body, Pos: ast.Pos{Line: ln.line, Col: ln.indent + 1}})
			i = ni
		case "else":
			return nil, 0, &perr{ln.line, ln.indent + 1, "`else` without a matching `if`"}
		default:
			return nil, 0, &perr{ln.line, ln.indent + 1, "unknown control line"}
		}
	}
	return nodes, i, nil
}

// matchControl reports whether a looks line is a block control line and returns
// its parts. Block control lines end with ':'. Returns (op, a, b, ok):
//   - if:   a = condition
//   - for:  a = loop var, b = iterable
//   - else: a, b empty
func matchControl(text string) (op, a, b string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasSuffix(t, ":") {
		return "", "", "", false
	}
	t = strings.TrimSpace(strings.TrimSuffix(t, ":"))
	switch {
	case t == "else":
		return "else", "", "", true
	case t == "slot":
		return "slot", "", "", true
	case strings.HasPrefix(t, "if "):
		return "if", strings.TrimSpace(t[3:]), "", true
	case strings.HasPrefix(t, "for "):
		rest := strings.TrimSpace(t[4:])
		v, iter, found := strings.Cut(rest, " in ")
		if !found {
			return "", "", "", false
		}
		return "for", strings.TrimSpace(v), strings.TrimSpace(iter), true
	}
	return "", "", "", false
}

// parseInline splits a content line into Text / Interp / inline-Ctrl / Child
// nodes. It scans for two things: `{…}` holes and `<Capitalized …/>` child-facet
// calls. Lowercase tags (`<div>`) are ordinary HTML text. Braces do not nest.
func parseInline(ln rawLine) ([]ast.Node, error) {
	var nodes []ast.Node
	s := ln.text
	i, start := 0, 0
	flush := func(end int) {
		if end > start {
			nodes = append(nodes, ast.Text{S: s[start:end]})
		}
	}
	for i < len(s) {
		switch {
		case s[i] == '{':
			flush(i)
			closeIdx := strings.IndexByte(s[i:], '}')
			if closeIdx < 0 {
				return nil, &perr{ln.line, ln.indent + i + 1, "unterminated `{` interpolation"}
			}
			inner := strings.TrimSpace(s[i+1 : i+closeIdx])
			nodes = append(nodes, classifyHole(inner, ln))
			i += closeIdx + 1
			start = i
		case s[i] == '<' && i+1 < len(s) && isUpperByte(s[i+1]):
			flush(i)
			child, next, err := parseChildTag(s, i, ln)
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, child)
			i = next
			start = next
		default:
			i++
		}
	}
	flush(len(s))
	return nodes, nil
}

// parseTagOpen parses a child tag <Name attr="v" .../> or <Name ...> at
// s[pos]=='<'. Returns the name, props, whether it self-closes, and the index
// just past the terminating '>'.
func parseTagOpen(s string, pos int, ln rawLine) (name string, props []ast.Prop, selfClose bool, end int, err error) {
	fail := func(off int, msg string) (string, []ast.Prop, bool, int, error) {
		return "", nil, false, 0, &perr{ln.line, ln.indent + off + 1, msg}
	}
	i := pos + 1 // past '<'
	ns := i
	for i < len(s) && isIdentByte(s[i]) {
		i++
	}
	name = s[ns:i]
	if name == "" || !isUpperByte(name[0]) {
		return fail(pos, "expected a capitalized facet name")
	}
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			return fail(pos, fmt.Sprintf("unterminated tag <%s …>", name))
		}
		if s[i] == '/' {
			if i+1 < len(s) && s[i+1] == '>' {
				return name, props, true, i + 2, nil
			}
			return fail(i, "expected '/>'")
		}
		if s[i] == '>' {
			return name, props, false, i + 1, nil
		}
		as := i
		for i < len(s) && isAttrNameByte(s[i]) {
			i++
		}
		if i == as {
			return fail(i, fmt.Sprintf("malformed attribute in <%s …>", name))
		}
		attr := s[as:i]
		if i >= len(s) || s[i] != '=' {
			return fail(i, fmt.Sprintf("attribute %q needs a =\"value\"", attr))
		}
		i++ // '='
		if i >= len(s) || s[i] != '"' {
			return fail(i, fmt.Sprintf("attribute %q value must be double-quoted", attr))
		}
		i++ // opening quote
		vs := i
		for i < len(s) && s[i] != '"' {
			i++
		}
		if i >= len(s) {
			return fail(vs, fmt.Sprintf("unterminated value for attribute %q", attr))
		}
		props = append(props, makeProp(attr, s[vs:i]))
		i++ // closing quote
	}
}

// parseChildTag parses an inline, self-closing child call <Name .../> at s[pos].
// Block form (<Name>…</Name>) must be alone on its line — see matchBlockChildOpen.
func parseChildTag(s string, pos int, ln rawLine) (ast.Child, int, error) {
	name, props, selfClose, end, err := parseTagOpen(s, pos, ln)
	if err != nil {
		return ast.Child{}, 0, err
	}
	if !selfClose {
		return ast.Child{}, 0, &perr{ln.line, ln.indent + pos + 1,
			fmt.Sprintf("inline child <%s> must be self-closing (<%s …/>); block form must be alone on its line", name, name)}
	}
	return ast.Child{Name: name, Props: props, Pos: ast.Pos{Line: ln.line, Col: ln.indent + pos + 1}}, end, nil
}

// matchBlockChildOpen reports whether a line is a lone block-form child open tag
// (<Card …> on its own line, not self-closing). Children are filled by
// parseChildBody.
func matchBlockChildOpen(ln rawLine) (ast.Child, bool) {
	t := strings.TrimSpace(ln.text)
	if len(t) < 2 || t[0] != '<' || !isUpperByte(t[1]) {
		return ast.Child{}, false
	}
	name, props, selfClose, end, err := parseTagOpen(t, 0, ln)
	if err != nil || selfClose || end != len(t) {
		return ast.Child{}, false // malformed, self-closing, or trailing content → not a block open
	}
	return ast.Child{Name: name, Props: props, Pos: ast.Pos{Line: ln.line, Col: ln.indent + 1}}, true
}

// parseChildBody collects a block-form child's body — the lines indented under
// the open tag, up to its `</Name>` close line — as the child's slot content.
// Returns the index past the close line.
func parseChildBody(lines []rawLine, i, openIndent int, child *ast.Child) (int, error) {
	bodyStart := i
	for i < len(lines) && lines[i].indent > openIndent {
		i++
	}
	closeTag := "</" + child.Name + ">"
	if i >= len(lines) || lines[i].indent != openIndent || strings.TrimSpace(lines[i].text) != closeTag {
		return 0, &perr{child.Pos.Line, child.Pos.Col, fmt.Sprintf("unclosed <%s>; expected %s", child.Name, closeTag)}
	}
	if i > bodyStart {
		body, _, err := parseLooksBlock(lines, bodyStart, lines[bodyStart].indent)
		if err != nil {
			return 0, err
		}
		child.Children = body
	}
	return i + 1, nil // past the close line
}

// makeProp classifies an attribute value: a pure `{expr}` becomes an expression
// prop; anything else is a literal string.
func makeProp(name, val string) ast.Prop {
	t := strings.TrimSpace(val)
	if len(t) >= 2 && t[0] == '{' && t[len(t)-1] == '}' && strings.IndexByte(t[1:len(t)-1], '{') < 0 {
		return ast.Prop{Name: name, IsExpr: true, Expr: strings.TrimSpace(t[1 : len(t)-1])}
	}
	return ast.Prop{Name: name, Literal: val}
}

func isUpperByte(b byte) bool { return b >= 'A' && b <= 'Z' }

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isAttrNameByte(b byte) bool { return isIdentByte(b) || b == '-' }

func classifyHole(inner string, ln rawLine) ast.Node {
	switch {
	case inner == "end":
		return ast.Ctrl{Op: "end"}
	case inner == "else":
		return ast.Ctrl{Op: "else"}
	case strings.HasPrefix(inner, "if "):
		return ast.Ctrl{Op: "if", Expr: strings.TrimSpace(inner[3:])}
	default:
		return ast.Interp{Expr: inner, Pos: ast.Pos{Line: ln.line}}
	}
}

// ── small helpers ───────────────────────────────────────────────────────────

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// unquote strips one pair of surrounding double quotes; ok is false if the
// string is not double-quoted.
func unquote(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", false
	}
	return s[1 : len(s)-1], true
}
