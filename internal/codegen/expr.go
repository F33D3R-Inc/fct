package codegen

import (
	"fmt"
	"strings"
)

// goExpr compiles an FDL expression to a Go-template pipeline string. It supports
//
//	identifier paths     post.id            → .Post.Id        ($var for loop vars)
//	method/func calls    viewer.can_view(p) → (.Viewer.Can_view .P)
//	comparisons          count > 100        → (gt .Count 100)
//	equality             role == "admin"    → (eq .Role "admin")
//	boolean              a && b, c || d, !e → (and .A .B) ...
//	arithmetic           likes + 1          → (add .Likes 1)
//	literals             123, 1.5, "x", true/false
//	grouping             (a || b) && c
//
// Operators lower to Go template builtins (eq/ne/lt/le/gt/ge/and/or/not) and the
// fa arithmetic funcs (add/sub/mul/div/mod), all available in the runtime set.
func goExpr(expr string, scope []string) (string, error) {
	toks, err := exprLex(expr)
	if err != nil {
		return "", fmt.Errorf("expression %q: %w", expr, err)
	}
	if len(toks) == 0 {
		return "", fmt.Errorf("empty expression")
	}
	p := &exprParser{toks: toks, scope: scope}
	out, err := p.parseOr()
	if err != nil {
		return "", fmt.Errorf("expression %q: %w", expr, err)
	}
	if !p.atEnd() {
		return "", fmt.Errorf("expression %q: unexpected %q", expr, p.cur().text)
	}
	return out, nil
}

// ── lexer ───────────────────────────────────────────────────────────────────

type exTok struct {
	kind string // num | str | ident | op | dot | lparen | rparen | comma
	text string
}

func exprLex(s string) ([]exTok, error) {
	var toks []exTok
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(':
			toks = append(toks, exTok{"lparen", "("})
			i++
		case c == ')':
			toks = append(toks, exTok{"rparen", ")"})
			i++
		case c == ',':
			toks = append(toks, exTok{"comma", ","})
			i++
		case c == '.':
			toks = append(toks, exTok{"dot", "."})
			i++
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, exTok{"str", s[i : j+1]})
			i = j + 1
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
				j++
			}
			toks = append(toks, exTok{"num", s[i:j]})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			toks = append(toks, exTok{"ident", s[i:j]})
			i = j
		case has(s, i, "=="), has(s, i, "!="), has(s, i, "<="), has(s, i, ">="), has(s, i, "&&"), has(s, i, "||"):
			toks = append(toks, exTok{"op", s[i : i+2]})
			i += 2
		case c == '<' || c == '>' || c == '!' || c == '+' || c == '-' || c == '*' || c == '/' || c == '%':
			toks = append(toks, exTok{"op", string(c)})
			i++
		default:
			return nil, fmt.Errorf("unexpected character %q", string(c))
		}
	}
	return toks, nil
}

func has(s string, i int, op string) bool { return strings.HasPrefix(s[i:], op) }
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// ── precedence-climbing parser (emits lowered strings) ──────────────────────

type exprParser struct {
	toks  []exTok
	pos   int
	scope []string
}

func (p *exprParser) atEnd() bool { return p.pos >= len(p.toks) }
func (p *exprParser) cur() exTok {
	if p.atEnd() {
		return exTok{}
	}
	return p.toks[p.pos]
}
func (p *exprParser) acceptOp(op string) bool {
	if !p.atEnd() && p.toks[p.pos].kind == "op" && p.toks[p.pos].text == op {
		p.pos++
		return true
	}
	return false
}
func (p *exprParser) acceptKind(kind string) bool {
	if !p.atEnd() && p.toks[p.pos].kind == kind {
		p.pos++
		return true
	}
	return false
}

func (p *exprParser) parseOr() (string, error) {
	l, err := p.parseAnd()
	if err != nil {
		return "", err
	}
	for p.acceptOp("||") {
		r, err := p.parseAnd()
		if err != nil {
			return "", err
		}
		l = "(or " + l + " " + r + ")"
	}
	return l, nil
}

func (p *exprParser) parseAnd() (string, error) {
	l, err := p.parseCmp()
	if err != nil {
		return "", err
	}
	for p.acceptOp("&&") {
		r, err := p.parseCmp()
		if err != nil {
			return "", err
		}
		l = "(and " + l + " " + r + ")"
	}
	return l, nil
}

var cmpFunc = map[string]string{"==": "eq", "!=": "ne", "<": "lt", "<=": "le", ">": "gt", ">=": "ge"}

func (p *exprParser) parseCmp() (string, error) {
	l, err := p.parseAdd()
	if err != nil {
		return "", err
	}
	for _, op := range []string{"==", "!=", "<", "<=", ">", ">="} {
		if p.acceptOp(op) {
			r, err := p.parseAdd()
			if err != nil {
				return "", err
			}
			return "(" + cmpFunc[op] + " " + l + " " + r + ")", nil // non-associative
		}
	}
	return l, nil
}

func (p *exprParser) parseAdd() (string, error) {
	l, err := p.parseMul()
	if err != nil {
		return "", err
	}
	for {
		if p.acceptOp("+") {
			r, err := p.parseMul()
			if err != nil {
				return "", err
			}
			l = "(add " + l + " " + r + ")"
		} else if p.acceptOp("-") {
			r, err := p.parseMul()
			if err != nil {
				return "", err
			}
			l = "(sub " + l + " " + r + ")"
		} else {
			return l, nil
		}
	}
}

func (p *exprParser) parseMul() (string, error) {
	l, err := p.parseUnary()
	if err != nil {
		return "", err
	}
	for {
		switch {
		case p.acceptOp("*"):
			r, err := p.parseUnary()
			if err != nil {
				return "", err
			}
			l = "(mul " + l + " " + r + ")"
		case p.acceptOp("/"):
			r, err := p.parseUnary()
			if err != nil {
				return "", err
			}
			l = "(div " + l + " " + r + ")"
		case p.acceptOp("%"):
			r, err := p.parseUnary()
			if err != nil {
				return "", err
			}
			l = "(mod " + l + " " + r + ")"
		default:
			return l, nil
		}
	}
}

func (p *exprParser) parseUnary() (string, error) {
	if p.acceptOp("!") {
		x, err := p.parseUnary()
		if err != nil {
			return "", err
		}
		return "(not " + x + ")", nil
	}
	if p.acceptOp("-") {
		x, err := p.parseUnary()
		if err != nil {
			return "", err
		}
		return "(sub 0 " + x + ")", nil
	}
	return p.parsePrimary()
}

func (p *exprParser) parsePrimary() (string, error) {
	t := p.cur()
	switch t.kind {
	case "num":
		p.pos++
		return t.text, nil
	case "str":
		p.pos++
		return t.text, nil // FDL and Go template string literals share syntax
	case "lparen":
		p.pos++
		inner, err := p.parseOr()
		if err != nil {
			return "", err
		}
		if !p.acceptKind("rparen") {
			return "", fmt.Errorf("expected ')'")
		}
		return inner, nil
	case "ident":
		if t.text == "true" || t.text == "false" {
			p.pos++
			return t.text, nil
		}
		path, err := p.parsePath()
		if err != nil {
			return "", err
		}
		if p.acceptKind("lparen") { // method / function call → Go template `call`
			args, err := p.parseArgs()
			if err != nil {
				return "", err
			}
			if args == "" {
				return "(call " + path + ")", nil
			}
			return "(call " + path + " " + args + ")", nil
		}
		return path, nil
	}
	return "", fmt.Errorf("unexpected %q", t.text)
}

func (p *exprParser) parsePath() (string, error) {
	if p.cur().kind != "ident" {
		return "", fmt.Errorf("expected a name")
	}
	segs := []string{p.cur().text}
	p.pos++
	for p.acceptKind("dot") {
		if p.cur().kind != "ident" {
			return "", fmt.Errorf("expected a name after '.'")
		}
		segs = append(segs, p.cur().text)
		p.pos++
	}
	return lowerPath(segs, p.scope), nil
}

func (p *exprParser) parseArgs() (string, error) {
	if p.acceptKind("rparen") {
		return "", nil // ()
	}
	var args []string
	for {
		a, err := p.parseOr()
		if err != nil {
			return "", err
		}
		args = append(args, a)
		if p.acceptKind("rparen") {
			break
		}
		if !p.acceptKind("comma") {
			return "", fmt.Errorf("expected ',' or ')' in call")
		}
	}
	return strings.Join(args, " "), nil
}

// lowerPath lowers a dotted FDL path to a Go template path: the first segment is
// $var if it's a loop variable in scope, else .Title(seg); the rest are
// .Title(seg).
func lowerPath(segs, scope []string) string {
	var b strings.Builder
	if inScope(scope, segs[0]) {
		b.WriteString("$" + segs[0])
	} else {
		b.WriteString("." + GoName(segs[0]))
	}
	for _, s := range segs[1:] {
		b.WriteString("." + GoName(s))
	}
	return b.String()
}
