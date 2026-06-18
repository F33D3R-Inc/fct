package codegen

import (
	"fmt"
	"strings"
)

// This file holds the target-neutral expression layer used by the non-Go
// backends (Node/Python/Rust). The Go backend keeps its own single-pass lowering
// in expr.go (goExpr → html/template pipelines), the proven v0 path, so this tree
// never affects Go output. Both share exprLex, the FDL expression tokenizer.
//
// One parser, many renderers: parseExpr builds an exNode tree; each backend walks
// it to its own surface syntax (data.count > 100 in JS, etc.). FDL's expression
// grammar is small and identical across targets — only the rendering differs —
// so a shared tree is both less code and more correct than four hand parsers.

// exNode is a parsed FDL expression.
type exNode interface{ exNode() }

// exLit is a numeric, string, or boolean literal. Text is the raw FDL token
// (for str it still carries its surrounding double quotes).
type exLit struct {
	Kind string // "num" | "str" | "bool"
	Text string
}

// exPath is a dotted identifier path (post.id). Local is true when the head
// segment is a loop variable in scope, so a backend can render it as a local
// binding rather than a field of the facet's data object.
type exPath struct {
	Segs  []string
	Local bool
}

// exCall is a method/function call: Recv is the callee path, Args the arguments.
type exCall struct {
	Recv *exPath
	Args []exNode
}

// exUnary is a prefix `!` or `-`.
type exUnary struct {
	Op string
	X  exNode
}

// exBinary is an infix operator: == != < <= > >= && || + - * / %.
type exBinary struct {
	Op   string
	L, R exNode
}

func (exLit) exNode()    {}
func (*exPath) exNode()  {}
func (*exCall) exNode()  {}
func (*exUnary) exNode() {}
func (exBinary) exNode() {}

// parseExpr lexes and parses an FDL expression into a neutral tree. scope lists
// the loop variables currently in scope (their paths render as locals).
func parseExpr(expr string, scope []string) (exNode, error) {
	toks, err := exprLex(expr)
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", expr, err)
	}
	if len(toks) == 0 {
		return nil, fmt.Errorf("empty expression")
	}
	p := &exTreeParser{toks: toks, scope: scope}
	n, err := p.parseOr()
	if err != nil {
		return nil, fmt.Errorf("expression %q: %w", expr, err)
	}
	if !p.atEnd() {
		return nil, fmt.Errorf("expression %q: unexpected %q", expr, p.cur().text)
	}
	return n, nil
}

// exTreeParser is a precedence-climbing parser over exTok (the same tokens
// goExpr's parser consumes) that builds an exNode tree instead of emitting Go.
type exTreeParser struct {
	toks  []exTok
	pos   int
	scope []string
}

func (p *exTreeParser) atEnd() bool { return p.pos >= len(p.toks) }
func (p *exTreeParser) cur() exTok {
	if p.atEnd() {
		return exTok{}
	}
	return p.toks[p.pos]
}
func (p *exTreeParser) acceptOp(op string) bool {
	if !p.atEnd() && p.toks[p.pos].kind == "op" && p.toks[p.pos].text == op {
		p.pos++
		return true
	}
	return false
}
func (p *exTreeParser) acceptKind(kind string) bool {
	if !p.atEnd() && p.toks[p.pos].kind == kind {
		p.pos++
		return true
	}
	return false
}

func (p *exTreeParser) parseOr() (exNode, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptOp("||") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = exBinary{Op: "||", L: l, R: r}
	}
	return l, nil
}

func (p *exTreeParser) parseAnd() (exNode, error) {
	l, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.acceptOp("&&") {
		r, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		l = exBinary{Op: "&&", L: l, R: r}
	}
	return l, nil
}

func (p *exTreeParser) parseCmp() (exNode, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if p.acceptOp(op) {
			r, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			return exBinary{Op: op, L: l, R: r}, nil // non-associative
		}
	}
	return l, nil
}

func (p *exTreeParser) parseAdd() (exNode, error) {
	l, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		if p.acceptOp("+") {
			r, err := p.parseMul()
			if err != nil {
				return nil, err
			}
			l = exBinary{Op: "+", L: l, R: r}
		} else if p.acceptOp("-") {
			r, err := p.parseMul()
			if err != nil {
				return nil, err
			}
			l = exBinary{Op: "-", L: l, R: r}
		} else {
			return l, nil
		}
	}
}

func (p *exTreeParser) parseMul() (exNode, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.acceptOp("*"):
			op = "*"
		case p.acceptOp("/"):
			op = "/"
		case p.acceptOp("%"):
			op = "%"
		default:
			return l, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = exBinary{Op: op, L: l, R: r}
	}
}

func (p *exTreeParser) parseUnary() (exNode, error) {
	if p.acceptOp("!") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &exUnary{Op: "!", X: x}, nil
	}
	if p.acceptOp("-") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &exUnary{Op: "-", X: x}, nil
	}
	return p.parsePrimary()
}

func (p *exTreeParser) parsePrimary() (exNode, error) {
	t := p.cur()
	switch t.kind {
	case "num":
		p.pos++
		return exLit{Kind: "num", Text: t.text}, nil
	case "str":
		p.pos++
		return exLit{Kind: "str", Text: t.text}, nil
	case "lparen":
		p.pos++
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.acceptKind("rparen") {
			return nil, fmt.Errorf("expected ')'")
		}
		return inner, nil
	case "ident":
		if t.text == "true" || t.text == "false" {
			p.pos++
			return exLit{Kind: "bool", Text: t.text}, nil
		}
		path, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		if p.acceptKind("lparen") {
			args, err := p.parseArgs()
			if err != nil {
				return nil, err
			}
			return &exCall{Recv: path, Args: args}, nil
		}
		return path, nil
	}
	return nil, fmt.Errorf("unexpected %q", t.text)
}

func (p *exTreeParser) parsePath() (*exPath, error) {
	if p.cur().kind != "ident" {
		return nil, fmt.Errorf("expected a name")
	}
	segs := []string{p.cur().text}
	p.pos++
	for p.acceptKind("dot") {
		if p.cur().kind != "ident" {
			return nil, fmt.Errorf("expected a name after '.'")
		}
		segs = append(segs, p.cur().text)
		p.pos++
	}
	return &exPath{Segs: segs, Local: inScope(p.scope, segs[0])}, nil
}

func (p *exTreeParser) parseArgs() ([]exNode, error) {
	if p.acceptKind("rparen") {
		return nil, nil // ()
	}
	var args []exNode
	for {
		a, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		if p.acceptKind("rparen") {
			break
		}
		if !p.acceptKind("comma") {
			return nil, fmt.Errorf("expected ',' or ')' in call")
		}
	}
	return args, nil
}

// ── shared render helpers ───────────────────────────────────────────────────

// renderInfix is the common shape for the C-family targets (Node, Rust): a
// straightforward infix rendering of the tree. data is the name of the root data
// object a non-local path hangs off ("data"); field maps an FDL segment to an
// identifier; logical maps the boolean operators (so Python can pass and/or/not).
type infixStyle struct {
	data    string              // root object for non-local paths
	field   func(string) string // FDL segment → identifier
	logical map[string]string   // "&&"/"||"/"!" → target keyword
	boolLit func(string) string // "true"/"false" → target literal
	call    func(recv string, args []string) string
}

func renderInfix(n exNode, s infixStyle) string {
	switch v := n.(type) {
	case exLit:
		switch v.Kind {
		case "bool":
			return s.boolLit(v.Text)
		default: // num, str (FDL string literals are C-style, reusable as-is)
			return v.Text
		}
	case *exPath:
		return renderPath(v, s)
	case *exCall:
		recv := renderPath(v.Recv, s)
		args := make([]string, len(v.Args))
		for i, a := range v.Args {
			args[i] = renderInfix(a, s)
		}
		return s.call(recv, args)
	case *exUnary:
		op := v.Op
		if m, ok := s.logical[op]; ok {
			op = m
		}
		x := renderInfix(v.X, s)
		if op == "not" { // Python: not <x>
			return "not " + paren(v.X, x)
		}
		return op + paren(v.X, x)
	case exBinary:
		op := v.Op
		if m, ok := s.logical[op]; ok {
			op = m
		}
		return paren(v.L, renderInfix(v.L, s)) + " " + op + " " + paren(v.R, renderInfix(v.R, s))
	}
	return ""
}

func renderPath(p *exPath, s infixStyle) string {
	var b strings.Builder
	if p.Local {
		b.WriteString(s.field(p.Segs[0]))
	} else {
		b.WriteString(s.data + "." + s.field(p.Segs[0]))
	}
	for _, seg := range p.Segs[1:] {
		b.WriteString("." + s.field(seg))
	}
	return b.String()
}

// paren wraps a child rendering in parentheses when it is a binary/unary node, so
// the emitted infix is unambiguous regardless of the target's precedence table.
func paren(n exNode, rendered string) string {
	switch n.(type) {
	case exBinary, *exUnary:
		return "(" + rendered + ")"
	}
	return rendered
}
