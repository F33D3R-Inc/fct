package codegen

import (
	"encoding/json"
	"strconv"

	"github.com/F33D3R-Inc/fct/internal/ast"
)

// RenderIR emits render.json — the neutral, target-independent render program the
// non-Go server runtimes interpret instead of Go html/template. The Go target
// renders from its compiled *.tmpl.html and never reads this; every other target
// (Node/Python/Rust) ships a tiny interpreter over this IR plus a matching
// expression evaluator, so the SAME compiler output drives all of them.
//
// It is deliberately self-contained — facet identity, the render op stream, and
// the when: handler wiring — so a runtime needs only this file and the shared
// manifest, not the reactive-heavy internals of manifest.json. Expressions are
// serialized as a neutral JSON AST (irExpr) so a runtime evaluates a tree rather
// than re-parsing FDL.
//
// See docs/BACKENDS.md.
func RenderIR(facets []*ast.Facet) ([]byte, error) {
	doc := irDoc{Wire: "1"}
	for _, f := range facets {
		if !f.ServerRendered() {
			continue // client-rendered kinds emit no server program
		}
		ops, err := irOps(f.Looks)
		if err != nil {
			return nil, err
		}
		doc.Facets = append(doc.Facets, irFacet{
			Name:    f.Name,
			FacetID: f.DerivedFacetID(),
			Render:  ops,
			When:    irWhens(f.Whens),
		})
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// RenderIRFileName is the on-disk name of the neutral render program.
const RenderIRFileName = "render.json"

type irDoc struct {
	Wire   string    `json:"wire"` // wire-format version this IR targets (matches fa.WireVersion)
	Facets []irFacet `json:"facets"`
}

type irFacet struct {
	Name    string   `json:"name"`
	FacetID string   `json:"facet_id"` // id pattern, e.g. "LikeButton:post:{post.id}" — runtime resolves {…} against data
	Render  []irOp   `json:"render"`
	When    []irWhen `json:"when,omitempty"`
}

// irOp is one render instruction. Op is one of:
//
//	text   literal HTML (V)
//	expr   escaped interpolation of X
//	if     open a conditional on X (truthy)
//	else   alternate branch
//	end    close the nearest if/for
//	for    open a loop binding Var over X (an iterable)
//	child  render child facet Name with Props evaluated in the current scope
type irOp struct {
	Op    string   `json:"op"`
	V     string   `json:"v,omitempty"`
	X     *irExpr  `json:"x,omitempty"`
	Var   string   `json:"var,omitempty"`
	Name  string   `json:"name,omitempty"`
	Props []irProp `json:"props,omitempty"`
}

type irProp struct {
	Name string  `json:"name"`
	X    *irExpr `json:"x,omitempty"`   // value expression (item={item})
	Lit  string  `json:"lit,omitempty"` // literal string value (title="Hi"); X nil
}

// irWhen mirrors a when: handler's wiring — the events that trigger it and the
// fragment mutations applied. The handler body itself is app code in the target
// language; the runtime maps an incoming event type to these mutations.
type irWhen struct {
	Events    []string `json:"events"`
	Mutations []irMut  `json:"mutations"`
}

type irMut struct {
	Op     string `json:"op"`               // replace | append | prepend | remove | replace_all
	Target string `json:"target,omitempty"` // child facet name
}

func irWhens(ws []ast.When) []irWhen {
	var out []irWhen
	for _, w := range ws {
		m := make([]irMut, len(w.Mutations))
		for i, mu := range w.Mutations {
			m[i] = irMut{Op: mu.Op, Target: mu.Target}
		}
		out = append(out, irWhen{Events: w.Events, Mutations: m})
	}
	return out
}

// irOps lowers a flat looks: stream to render ops. The stream is already flat
// (Ctrl markers, with synthesized "end" for block control), so this is a linear
// pass — no scope stack needed beyond what each backend tracks at render time.
func irOps(nodes []ast.Node) ([]irOp, error) {
	var ops []irOp
	var walk func(ns []ast.Node) error
	walk = func(ns []ast.Node) error {
		for _, n := range ns {
			switch v := n.(type) {
			case ast.Text:
				ops = append(ops, irOp{Op: "text", V: v.S})
			case ast.Interp:
				x, err := irExprOf(v.Expr, nil)
				if err != nil {
					return err
				}
				ops = append(ops, irOp{Op: "expr", X: x})
			case ast.Ctrl:
				switch v.Op {
				case "if":
					x, err := irExprOf(v.Expr, nil)
					if err != nil {
						return err
					}
					ops = append(ops, irOp{Op: "if", X: x})
				case "for":
					x, err := irExprOf(v.Iter, nil)
					if err != nil {
						return err
					}
					ops = append(ops, irOp{Op: "for", Var: v.Var, X: x})
				case "else":
					ops = append(ops, irOp{Op: "else"})
				case "end":
					ops = append(ops, irOp{Op: "end"})
				}
			case ast.Child:
				props := make([]irProp, 0, len(v.Props))
				for _, p := range v.Props {
					if p.IsExpr {
						x, err := irExprOf(p.Expr, nil)
						if err != nil {
							return err
						}
						props = append(props, irProp{Name: p.Name, X: x})
					} else {
						props = append(props, irProp{Name: p.Name, Lit: p.Literal})
					}
				}
				ops = append(ops, irOp{Op: "child", Name: v.Name, Props: props})
			case ast.Slot:
				if err := walk(v.Default); err != nil { // best-effort: inline default content
					return err
				}
			}
		}
		return nil
	}
	if err := walk(nodes); err != nil {
		return nil, err
	}
	return ops, nil
}

// ── neutral expression AST (JSON) ───────────────────────────────────────────

// irExpr is the serialized form of an exNode (exprast.go) — a tree a runtime
// evaluates against a data scope. K is the discriminator:
//
//	num   N   numeric literal (decimal text, exact)
//	str   S   string literal value (unquoted, unescaped)
//	bool  B   boolean literal
//	path  Segs+Local   dotted identifier path; Local ⇒ head is a loop var
//	call  Recv+Args    method/function call
//	unary Op+X         prefix ! or -
//	bin   Op+L+R       infix operator
type irExpr struct {
	K     string    `json:"k"`
	N     string    `json:"n,omitempty"`
	S     string    `json:"s,omitempty"`
	B     bool      `json:"b,omitempty"`
	Segs  []string  `json:"segs,omitempty"`
	Local bool      `json:"local,omitempty"`
	Op    string    `json:"op,omitempty"`
	Recv  *irExpr   `json:"recv,omitempty"`
	Args  []*irExpr `json:"args,omitempty"`
	X     *irExpr   `json:"x,omitempty"`
	L     *irExpr   `json:"l,omitempty"`
	R     *irExpr   `json:"r,omitempty"`
}

// irExprOf parses an FDL expression and serializes it to the neutral JSON AST.
func irExprOf(expr string, scope []string) (*irExpr, error) {
	n, err := parseExpr(expr, scope)
	if err != nil {
		return nil, err
	}
	return marshalExpr(n)
}

func marshalExpr(n exNode) (*irExpr, error) {
	switch v := n.(type) {
	case exLit:
		switch v.Kind {
		case "num":
			return &irExpr{K: "num", N: v.Text}, nil
		case "bool":
			return &irExpr{K: "bool", B: v.Text == "true"}, nil
		default: // str — unquote to the literal value
			s, err := strconv.Unquote(v.Text)
			if err != nil {
				s = v.Text
			}
			return &irExpr{K: "str", S: s}, nil
		}
	case *exPath:
		return &irExpr{K: "path", Segs: v.Segs, Local: v.Local}, nil
	case *exCall:
		recv, err := marshalExpr(v.Recv)
		if err != nil {
			return nil, err
		}
		args := make([]*irExpr, len(v.Args))
		for i, a := range v.Args {
			args[i], err = marshalExpr(a)
			if err != nil {
				return nil, err
			}
		}
		return &irExpr{K: "call", Recv: recv, Args: args}, nil
	case *exUnary:
		x, err := marshalExpr(v.X)
		if err != nil {
			return nil, err
		}
		return &irExpr{K: "unary", Op: v.Op, X: x}, nil
	case exBinary:
		l, err := marshalExpr(v.L)
		if err != nil {
			return nil, err
		}
		r, err := marshalExpr(v.R)
		if err != nil {
			return nil, err
		}
		return &irExpr{K: "bin", Op: v.Op, L: l, R: r}, nil
	}
	return nil, nil
}
