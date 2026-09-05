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
	"==": 3, "!=": 3, "<": 3, "<=": 3, ">": 3, ">=": 3, "in": 3,
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
		if !ok {
			break
		}
		// Operators are symbols (tOp); `in` (membership) is the one word operator.
		op := ""
		if t.kind == tOp {
			op = t.text
		} else if t.kind == tIdent && t.text == "in" {
			op = "in"
		} else {
			break
		}
		prec, isBin := binPrec[op]
		if !isBin || prec < minPrec {
			break
		}
		p.pos++
		right, err := p.parseBinary(prec + 1)
		if err != nil {
			return nil, err
		}
		left = ast.Bin{Op: op, L: left, R: right}
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
			case ref.Name == "count" || ref.Name == "sum" || ref.Name == "exists" || ref.Name == "avg" ||
				((ref.Name == "min" || ref.Name == "max") && !p.argListHasComma()):
				// count/sum/exists/avg are always aggregates. `min`/`max` are also scalar
				// builtins (`min(a, b)`) — they are an aggregate only in the single-argument
				// collection form (`min(Order.amount)`), told apart by the absence of a
				// top-level comma in the argument list.
				ag, err := p.parseAgg(ref.Name)
				if err != nil {
					return nil, err
				}
				atom = ag
			case ref.Name == "pending" || ref.Name == "failed" || ref.Name == "dirty" || ref.Name == "touched":
				as, err := p.parseActState(ref.Name)
				if err != nil {
					return nil, err
				}
				atom = as
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

// argListHasComma reports whether the parenthesised argument list starting at
// p.pos (which is on the opening `(`) contains a top-level comma. It is how a
// `min`/`max` call is disambiguated: the scalar builtin `min(a, b)` has a comma,
// the aggregate `min(Order.amount)` does not. It does not advance the parser.
func (p *exprParser) argListHasComma() bool {
	depth := 0
	for i := p.pos; i < len(p.toks); i++ {
		switch t := p.toks[i]; {
		case t.kind == tLParen:
			depth++
		case t.kind == tRParen:
			depth--
			if depth == 0 {
				return false
			}
		case depth == 1 && t.kind == tOp && t.text == ",":
			return true
		}
	}
	return false
}

// numericAgg reports whether an aggregate op sums/averages/extremes a field (and
// so requires one): sum, avg, min, max. count/exists range over rows, no field.
func numericAgg(op string) bool {
	return op == "sum" || op == "avg" || op == "min" || op == "max"
}

// precArith is the precedence floor an aggregate's reduced value is parsed at:
// high enough that `+ - * / %` and everything tighter bind into it, low enough
// that `in` — which is the token separating the value from the collection it
// ranges over — is left for parseAgg to see. Without the floor,
// `sum(l.qty * l.unitPrice in CartLine …)` would parse as one `in` expression and
// the aggregate would have no collection.
const precArith = 4

// parseAgg parses the argument list of an aggregate builtin; p.peek() is at the
// opening `(`. Whole-collection forms: `count(Coll)`, `sum(Coll.field)`,
// `avg(Coll.field)`, `min(Coll.field)`, `max(Coll.field)`. Filtered forms (an item
// variable + predicate): `count(x in Coll where <cond>)`,
// `exists(x in Coll where <cond>)`, `sum(x.field in Coll where <cond>)`, and —
// the general case the other three are special shapes of —
// `sum(<expr over x> in Coll where <cond>)`, e.g.
// `sum(l.qty * l.unitPrice in CartLine where l.owner == actor)`.
//
// The reduced value is parsed as an expression rather than scanned as a name and
// an optional `.field`, so the three forms are one grammar and the shapes are
// told apart afterwards by what the expression turned out to be. A bare
// `x.field` keeps its old lowering (Field, no Sel), which is what makes the
// generalization free for every program written before it.
func (p *exprParser) parseAgg(op string) (ast.Expr, error) {
	p.pos++ // consume (
	if t, ok := p.peek(); !ok || t.kind == tRParen {
		return nil, &Error{p.line, fmt.Sprintf("%s needs a collection: %s(Entity%s)", op, op, aggArgHint(op))}
	}
	head, err := p.parseBinary(precArith)
	if err != nil {
		return nil, err
	}

	itemVar, coll, field := "", "", ""
	var sel ast.Expr

	// Filtered form: `<value> in <Coll> [where <cond>]`, detected by a following `in`.
	if n, ok := p.peek(); ok && n.kind == tIdent && n.text == "in" {
		p.pos++ // consume `in`
		c, ok := p.peek()
		if !ok || c.kind != tIdent {
			return nil, &Error{p.line, fmt.Sprintf("%s(%s in ...) needs a collection after `in`", op, exprSrc(head))}
		}
		coll = c.text
		p.pos++
		switch h := head.(type) {
		case ast.Ref:
			// `count(x in Coll …)` / `exists(x in Coll …)`: the value is the row itself.
			itemVar = h.Name
		case ast.Get:
			if r, isRef := h.Obj.(ast.Ref); isRef {
				itemVar, field = r.Name, h.Field // `sum(x.field in Coll …)`
			}
		}
		if itemVar == "" {
			// An expression over the row. The item variable is the root of the
			// leftmost `x.field` in it — the same place the `x.field` form has always
			// taken it from, read out of a bigger expression. Every other name in the
			// value has to resolve in the enclosing scope, and the builder says so by
			// name if it does not, so a wrong guess is a compile error rather than a
			// wrong answer.
			itemVar = aggRowVar(head)
			if itemVar == "" {
				return nil, &Error{p.line, fmt.Sprintf(
					"%s(... in %s ...) reduces a value read off each row, so the value must read one: %s(x.field * x.other in %s where …)", op, coll, op, coll)}
			}
			sel = head
		}
	} else {
		// Whole-collection form: `count(Coll)` / `sum(Coll.field)`.
		switch h := head.(type) {
		case ast.Ref:
			coll = h.Name
		case ast.Get:
			if r, isRef := h.Obj.(ast.Ref); isRef {
				coll, field = r.Name, h.Field
			}
		}
		if coll == "" {
			return nil, &Error{p.line, fmt.Sprintf(
				"%s over an expression needs an item variable to read the row: %s(x.a * x.b in Entity where …)", op, op)}
		}
	}

	if numericAgg(op) && field == "" && sel == nil {
		if itemVar != "" {
			return nil, &Error{p.line, fmt.Sprintf("%s needs a field: %s(%s.field in %s where …)", op, op, itemVar, coll)}
		}
		return nil, &Error{p.line, fmt.Sprintf("%s needs a field: %s(%s.field)", op, op, coll)}
	}
	if !numericAgg(op) {
		if field != "" {
			return nil, &Error{p.line, fmt.Sprintf("%s ranges over rows, not a field — drop the `.%s`", op, field)}
		}
		if sel != nil {
			return nil, &Error{p.line, fmt.Sprintf("%s ranges over rows, not a value — it counts the rows the filter accepts, so it takes `%s(x in %s where …)`", op, op, coll)}
		}
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
	return ast.Agg{Op: op, Coll: coll, Field: field, Var: itemVar, Where: where, Sel: sel}, nil
}

// aggRowVar reports the name an aggregate's reduced value reads its row through:
// the object of the leftmost member access in it, in source order. It is a
// pre-order walk because "leftmost" has to mean the same thing to a reader as it
// does here — `l.qty * l.unitPrice` reads its row through `l`, and so does
// `abs(l.delta)` and `Product(l.product).price`.
func aggRowVar(ex ast.Expr) string {
	switch t := ex.(type) {
	case ast.Get:
		if r, ok := t.Obj.(ast.Ref); ok {
			return r.Name
		}
		return aggRowVar(t.Obj)
	case ast.EntityGet:
		return aggRowVar(t.Key)
	case ast.Bin:
		if v := aggRowVar(t.L); v != "" {
			return v
		}
		return aggRowVar(t.R)
	case ast.Un:
		return aggRowVar(t.X)
	case ast.Call:
		for _, a := range t.Args {
			if v := aggRowVar(a); v != "" {
				return v
			}
		}
	case ast.ListLit:
		for _, el := range t.Elems {
			if v := aggRowVar(el); v != "" {
				return v
			}
		}
	}
	return ""
}

// exprSrc names an expression well enough for a diagnostic to point at it. Only
// the shapes that can head an aggregate need a name; anything else is described
// rather than spelled.
func exprSrc(ex ast.Expr) string {
	switch t := ex.(type) {
	case ast.Ref:
		return t.Name
	case ast.Get:
		return exprSrc(t.Obj) + "." + t.Field
	}
	return "that value"
}

// parseActState parses `pending(action)` / `failed(action)` — a bare action name
// in parentheses. p.peek() is at the opening `(`.
func (p *exprParser) parseActState(op string) (ast.Expr, error) {
	p.pos++ // consume (
	t, ok := p.peek()
	if !ok || t.kind != tIdent {
		// pending/failed name an action; dirty/touched name a form (state) cell.
		noun := "an action name"
		if op == "dirty" || op == "touched" {
			noun = "a state cell"
		}
		return nil, &Error{p.line, fmt.Sprintf("%s needs %s: %s(name)", op, noun, op)}
	}
	action := t.text
	p.pos++
	c, ok := p.peek()
	if !ok || c.kind != tRParen {
		return nil, &Error{p.line, fmt.Sprintf("missing `)` in %s(...)", op)}
	}
	p.pos++
	return ast.ActState{Op: op, Action: action}, nil
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
		"len", "upper", "lower", "trim", "contains", "take", // string
		"year", "month", "day", // date
		"ago", "compact", "commas": // formatting (render-time text)
		return true
	}
	return false
}

func aggArgHint(op string) string {
	if numericAgg(op) {
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
