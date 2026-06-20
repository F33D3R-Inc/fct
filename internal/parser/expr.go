package parser

import (
	"fmt"
	"strconv"
	"strings"

	"facet/internal/ast"
)

// ParseExpr parses a single standalone Facet expression (for the console / test
// runner, which evaluate ad-hoc expressions against a running app).
func ParseExpr(src string) (ast.Expr, error) { return parseExpr(src, 1) }

// parseExpr parses a Facet expression: identifiers, member access (`p.field`),
// entity lookup (`Entity(key).field`), int/text/bool literals, unary ! and -,
// arithmetic (+ - * / %), comparison (== != < <= > >=) and boolean (&& ||),
// with parentheses. Precedence-climbing; produces an ast.Expr every executor
// interprets identically.
func parseExpr(src string, line int) (ast.Expr, error) {
	p := &exprParser{toks: tokenize(src), line: line}
	e, err := p.parseBinary(0)
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, &Error{line, fmt.Sprintf("unexpected %q in expression", p.toks[p.pos].text)}
	}
	return e, nil
}

type tokKind int

const (
	tEOF tokKind = iota
	tNum
	tStr
	tIdent
	tOp
	tLParen
	tRParen
)

type token struct {
	kind tokKind
	text string
}

func tokenize(s string) []token {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(':
			toks = append(toks, token{tLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tRParen, ")"})
			i++
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(s) {
				j++
			}
			toks = append(toks, token{tStr, s[i:j]})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			toks = append(toks, token{tNum, s[i:j]})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(s) && isIdentChar(s[j]) {
				j++
			}
			toks = append(toks, token{tIdent, s[i:j]})
			i = j
		default:
			two := ""
			if i+1 < len(s) {
				two = s[i : i+2]
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||":
				toks = append(toks, token{tOp, two})
				i += 2
			default:
				toks = append(toks, token{tOp, string(c)})
				i++
			}
		}
	}
	return toks
}

type exprParser struct {
	toks []token
	pos  int
	line int
}

var binPrec = map[string]int{
	"||": 1, "&&": 2,
	"==": 3, "!=": 3, "<": 3, "<=": 3, ">": 3, ">=": 3,
	"+": 4, "-": 4,
	"*": 5, "/": 5, "%": 5,
}

func (p *exprParser) peek() (token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return token{kind: tEOF}, false
}

func (p *exprParser) parseBinary(minPrec int) (ast.Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tOp {
			break
		}
		prec, isBin := binPrec[t.text]
		if !isBin || prec < minPrec {
			break
		}
		p.pos++
		right, err := p.parseBinary(prec + 1)
		if err != nil {
			return nil, err
		}
		left = ast.Bin{Op: t.text, L: left, R: right}
	}
	return left, nil
}

func (p *exprParser) parseUnary() (ast.Expr, error) {
	t, ok := p.peek()
	if ok && t.kind == tOp && (t.text == "!" || t.text == "-") {
		p.pos++
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return ast.Un{Op: t.text, X: x}, nil
	}
	return p.parsePostfix()
}

// parsePostfix handles `.field` member access and the `Entity(key).field` form.
func (p *exprParser) parsePostfix() (ast.Expr, error) {
	atom, err := p.parseAtom()
	if err != nil {
		return nil, err
	}
	// A name directly followed by `(` is either an aggregate builtin —
	// `count(Entity)` / `sum(Entity.field)` — or an entity lookup
	// `Entity(key).field`. The builtins are reserved only in this call position,
	// so a state named `count` used as a bare value is unaffected.
	if ref, ok := atom.(ast.Ref); ok {
		if t, ok := p.peek(); ok && t.kind == tLParen {
			switch {
			case ref.Name == "count" || ref.Name == "sum" || ref.Name == "exists":
				ag, err := p.parseAgg(ref.Name)
				if err != nil {
					return nil, err
				}
				atom = ag
			case isBuiltinCall(ref.Name):
				cl, err := p.parseCall(ref.Name)
				if err != nil {
					return nil, err
				}
				atom = cl
			default:
				p.pos++
				key, err := p.parseBinary(0)
				if err != nil {
					return nil, err
				}
				if c, ok := p.peek(); !ok || c.kind != tRParen {
					return nil, &Error{p.line, "missing `)` in entity lookup"}
				}
				p.pos++
				field, err := p.expectDotField()
				if err != nil {
					return nil, err
				}
				atom = ast.EntityGet{Entity: ref.Name, Key: key, Field: field}
			}
		}
	}
	// chained `.field`
	for {
		t, ok := p.peek()
		if !ok || t.kind != tOp || t.text != "." {
			break
		}
		p.pos++
		field, err := p.fieldName()
		if err != nil {
			return nil, err
		}
		atom = ast.Get{Obj: atom, Field: field}
	}
	return atom, nil
}

// parseAgg parses the argument list of an aggregate builtin; p.peek() is at the
// opening `(`. Whole-collection forms: `count(Coll)`, `sum(Coll.field)`. Filtered
// forms (an item variable + predicate): `count(x in Coll where <cond>)` and
// `exists(x in Coll where <cond>)`.
func (p *exprParser) parseAgg(op string) (ast.Expr, error) {
	p.pos++ // consume (
	t, ok := p.peek()
	if !ok || t.kind != tIdent {
		return nil, &Error{p.line, fmt.Sprintf("%s needs a collection: %s(Entity%s)", op, op, aggArgHint(op))}
	}
	name := t.text
	p.pos++

	// Filtered form: `<var> in <Coll> [where <cond>]`. We detect it by a following
	// `in` identifier; otherwise `name` is the collection (whole-collection form).
	itemVar, coll := "", name
	if n, ok := p.peek(); ok && n.kind == tIdent && n.text == "in" {
		p.pos++ // consume `in`
		c, ok := p.peek()
		if !ok || c.kind != tIdent {
			return nil, &Error{p.line, fmt.Sprintf("%s(%s in ...) needs a collection after `in`", op, name)}
		}
		itemVar, coll = name, c.text
		p.pos++
	}

	field := ""
	if op == "sum" {
		if itemVar != "" {
			return nil, &Error{p.line, "filtered sum is not supported yet; use sum(Entity.field) over the whole collection"}
		}
		d, ok := p.peek()
		if !ok || d.kind != tOp || d.text != "." {
			return nil, &Error{p.line, "sum needs a field: sum(Entity.field)"}
		}
		p.pos++
		f, err := p.fieldName()
		if err != nil {
			return nil, err
		}
		field = f
	}

	var where ast.Expr
	if w, ok := p.peek(); ok && w.kind == tIdent && w.text == "where" {
		p.pos++ // consume `where`
		cond, err := p.parseBinary(0)
		if err != nil {
			return nil, err
		}
		where = cond
	}

	c, ok := p.peek()
	if !ok || c.kind != tRParen {
		return nil, &Error{p.line, fmt.Sprintf("missing `)` in %s(...)", op)}
	}
	p.pos++
	return ast.Agg{Op: op, Coll: coll, Field: field, Var: itemVar, Where: where}, nil
}

// parseCall parses a builtin call's argument list; p.peek() is at the opening
// `(`. Arguments are full expressions, comma-separated.
func (p *exprParser) parseCall(name string) (ast.Expr, error) {
	p.pos++ // consume (
	var args []ast.Expr
	if t, ok := p.peek(); ok && t.kind == tRParen {
		p.pos++
		return ast.Call{Name: name}, nil
	}
	for {
		arg, err := p.parseBinary(0)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		t, ok := p.peek()
		if !ok {
			return nil, &Error{p.line, fmt.Sprintf("missing `)` in %s(...)", name)}
		}
		if t.kind == tRParen {
			p.pos++
			break
		}
		if t.kind != tOp || t.text != "," {
			return nil, &Error{p.line, fmt.Sprintf("expected `,` or `)` in %s(...)", name)}
		}
		p.pos++
	}
	return ast.Call{Name: name, Args: args}, nil
}

// isBuiltinCall reports whether name is an invocable builtin in call position:
// the effectful clock/RNG plus the pure standard library (string/math/date).
func isBuiltinCall(name string) bool {
	switch name {
	case "now", "rand", // effectful (pinned to the authority)
		"abs", "min", "max", "floor", "round", "money", // math / money
		"len", "upper", "lower", "trim", "contains", // string
		"year", "month", "day": // date
		return true
	}
	return false
}

func aggArgHint(op string) string {
	if op == "sum" {
		return ".field"
	}
	return ""
}

func (p *exprParser) expectDotField() (string, error) {
	t, ok := p.peek()
	if !ok || t.kind != tOp || t.text != "." {
		return "", &Error{p.line, "expected .field after Entity(key)"}
	}
	p.pos++
	return p.fieldName()
}

func (p *exprParser) fieldName() (string, error) {
	t, ok := p.peek()
	if !ok || t.kind != tIdent {
		return "", &Error{p.line, "expected a field name after `.`"}
	}
	p.pos++
	return t.text, nil
}

func (p *exprParser) parseAtom() (ast.Expr, error) {
	t, ok := p.peek()
	if !ok {
		return nil, &Error{p.line, "unexpected end of expression"}
	}
	switch t.kind {
	case tLParen:
		p.pos++
		e, err := p.parseBinary(0)
		if err != nil {
			return nil, err
		}
		if c, ok := p.peek(); !ok || c.kind != tRParen {
			return nil, &Error{p.line, "missing closing `)`"}
		}
		p.pos++
		return e, nil
	case tOp:
		if t.text == "[" {
			return p.parseListLit()
		}
		return nil, &Error{p.line, fmt.Sprintf("unexpected %q in expression", t.text)}
	case tNum:
		p.pos++
		if strings.Contains(t.text, ".") {
			return nil, &Error{p.line, "floats are not supported yet; use int"}
		}
		n, err := strconv.Atoi(t.text)
		if err != nil {
			return nil, &Error{p.line, fmt.Sprintf("bad number %q", t.text)}
		}
		return ast.Lit{Kind: "int", Val: n}, nil
	case tStr:
		p.pos++
		v, err := strconv.Unquote(t.text)
		if err != nil {
			return nil, &Error{p.line, fmt.Sprintf("bad string %q", t.text)}
		}
		return ast.Lit{Kind: "text", Val: v}, nil
	case tIdent:
		p.pos++
		switch t.text {
		case "true":
			return ast.Lit{Kind: "bool", Val: true}, nil
		case "false":
			return ast.Lit{Kind: "bool", Val: false}, nil
		}
		return ast.Ref{Name: t.text}, nil
	default:
		return nil, &Error{p.line, fmt.Sprintf("unexpected %q in expression", t.text)}
	}
}

// parseListLit parses a `[a, b, c]` list literal; p.peek() is at the opening `[`.
func (p *exprParser) parseListLit() (ast.Expr, error) {
	p.pos++ // consume [
	var elems []ast.Expr
	if t, ok := p.peek(); ok && t.kind == tOp && t.text == "]" {
		p.pos++
		return ast.ListLit{}, nil
	}
	for {
		e, err := p.parseBinary(0)
		if err != nil {
			return nil, err
		}
		elems = append(elems, e)
		t, ok := p.peek()
		if !ok {
			return nil, &Error{p.line, "missing closing `]` in list"}
		}
		if t.kind == tOp && t.text == "]" {
			p.pos++
			break
		}
		if t.kind != tOp || t.text != "," {
			return nil, &Error{p.line, "expected `,` or `]` in list literal"}
		}
		p.pos++
	}
	return ast.ListLit{Elems: elems}, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}
