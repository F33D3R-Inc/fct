package parser

import (
	"fmt"
	"testing"

	"facet/internal/ast"
)

// sexpr renders an ast.Expr as a fully parenthesized S-expression-ish string,
// so a precedence-climbing result can be compared against an exact expected
// grouping without hand-walking Bin/Un nodes in every test case.
func sexpr(e ast.Expr) string {
	switch t := e.(type) {
	case ast.Bin:
		return "(" + sexpr(t.L) + " " + t.Op + " " + sexpr(t.R) + ")"
	case ast.Un:
		return "(" + t.Op + sexpr(t.X) + ")"
	case ast.Ref:
		return t.Name
	case ast.Lit:
		return fmt.Sprintf("%v", t.Val)
	default:
		return fmt.Sprintf("%#v", e)
	}
}

// TestBitwiseTokenization proves the tokenizer's longest-match rule for the
// new multi-character operators: `<<`/`>>` must consume two characters (not
// mis-lex as two separate `<`/`>` tokens), `<=`/`>=` must still win over a
// bare `<`/`>` followed by `=`, and the single-character bitwise operators
// (`& | ^ ~`) — which need no tokenizer change at all, since the existing
// default one-character fallback already handles them — keep working.
func TestBitwiseTokenization(t *testing.T) {
	cases := []struct{ src, want string }{
		{"a << b", "(a << b)"},
		{"a >> b", "(a >> b)"},
		{"a <= b", "(a <= b)"},
		{"a >= b", "(a >= b)"},
		{"a < b", "(a < b)"},
		{"a > b", "(a > b)"},
		{"a & b", "(a & b)"},
		{"a | b", "(a | b)"},
		{"a ^ b", "(a ^ b)"},
		{"~a", "(~a)"},
		// No spaces at all: the longest-match rule must still find the two-char
		// operators inside a run of symbol characters.
		{"a<<b>>c", "((a << b) >> c)"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			e, err := ParseExpr(c.src)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", c.src, err)
			}
			if got := sexpr(e); got != c.want {
				t.Errorf("ParseExpr(%q) = %s, want %s", c.src, got, c.want)
			}
		})
	}
}

// TestFloatLiteralTokenization proves the tokenizer's `<digits>.<digits>`
// rule for a float literal (see tokenize's numeric-literal case): a genuine
// decimal point followed by a digit extends an int literal into a float one,
// distinct at the ast.Lit level ("float" vs "int"); ordinary member access
// (`.field`) and a bare int are both completely unaffected, since neither
// shape has a digit on both sides of a `.`.
func TestFloatLiteralTokenization(t *testing.T) {
	cases := []struct {
		src      string
		wantKind string
		wantVal  any
	}{
		{"3.14", "float", 3.14},
		{"0.5", "float", 0.5},
		{"10.0", "float", 10.0},
		{"3", "int", 3},
		{"42", "int", 42},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			e, err := ParseExpr(c.src)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", c.src, err)
			}
			lit, ok := e.(ast.Lit)
			if !ok {
				t.Fatalf("ParseExpr(%q) = %#v, want an ast.Lit", c.src, e)
			}
			if lit.Kind != c.wantKind {
				t.Errorf("ParseExpr(%q).Kind = %q, want %q", c.src, lit.Kind, c.wantKind)
			}
			if lit.Val != c.wantVal {
				t.Errorf("ParseExpr(%q).Val = %#v (%T), want %#v (%T)", c.src, lit.Val, lit.Val, c.wantVal, c.wantVal)
			}
		})
	}

	// Unary minus over a float literal must produce a Un wrapping a float Lit
	// (`-2.0`), not silently truncate anything at parse time — evaluation of
	// the negation itself is runtime/eval.go's job (see runtime/float_test.go).
	e, err := ParseExpr("-2.5")
	if err != nil {
		t.Fatalf("ParseExpr(-2.5): %v", err)
	}
	un, ok := e.(ast.Un)
	if !ok || un.Op != "-" {
		t.Fatalf("ParseExpr(-2.5) = %#v, want a unary - node", e)
	}
	inner, ok := un.X.(ast.Lit)
	if !ok || inner.Kind != "float" || inner.Val != 2.5 {
		t.Fatalf("ParseExpr(-2.5)'s operand = %#v, want ast.Lit{Kind: \"float\", Val: 2.5}", un.X)
	}

	// A member access after a parenthesized/entity-lookup expression must
	// still work — the tokenizer's float rule only fires within a single
	// contiguous digit run, so `Post(1).field`-shaped source (no digit
	// immediately after the `.`) is never affected.
	e2, err := ParseExpr("a.b")
	if err != nil {
		t.Fatalf("ParseExpr(a.b): %v", err)
	}
	get, ok := e2.(ast.Get)
	if !ok || get.Field != "b" {
		t.Fatalf("ParseExpr(a.b) = %#v, want ast.Get{Field: \"b\"}", e2)
	}
}

// TestBitwisePrecedence pins down the exact precedence table chosen for `&
// | ^ << >>` and unary `~`: loosest to tightest, `|| , && , | , ^ , & ,
// comparison , << >> , + - , * / %` — the same relative order most C-family
// languages use (shifts tighter than `&`, which is tighter than `^`, which is
// tighter than `|`, and all four looser than `+ - * /`), collapsed against
// this language's own choice to keep every comparison operator at one level
// rather than splitting `==`/`!=` from `<`/`<=`/`>`/`>=` as C does.
//
// Some of these are the classic C "gotcha" groupings — `a == b & c` really
// does mean `(a == b) & c`, not `a == (b & c)`, because `&` is looser than
// `==` — verified here at the pure parse-tree level, where there is no type
// checker to object to the (real, C-inherited) surprise of feeding a
// comparison's bool result into a bitwise operator; internal/compile's
// checkBitwiseTypes rejects that shape once you try to use it inside an
// actual proc (see internal/compile/proc_test.go's precedenceApp, which
// tests the same table's int-only slice end to end through the compiler).
func TestBitwisePrecedence(t *testing.T) {
	cases := []struct{ src, want string }{
		// Within the bitwise family: & tighter than ^ tighter than |.
		{"a & b | c ^ d", "((a & b) | (c ^ d))"},
		{"a | b & c", "(a | (b & c))"},
		{"a ^ b & c", "(a ^ (b & c))"},
		// Shifts tighter than &, and additive tighter than shifts.
		{"a << 1 & b", "((a << 1) & b)"},
		{"a + b << 1", "((a + b) << 1)"},
		{"a - b >> 1", "((a - b) >> 1)"},
		// Multiplicative tighter than additive, unaffected by this change
		// (regression: the renumbered table must preserve the old ordering).
		{"a + b * c", "(a + (b * c))"},
		// Comparison binds TIGHTER than the bitwise family — the C gotcha.
		{"a == b & c", "((a == b) & c)"},
		{"a & b == c", "(a & (b == c))"},
		// `|` looser than `&&`... no: `|` is TIGHTER than `&&` (matching C:
		// `&&`/`||` are the loosest operators of all), so `a || b && c | d`
		// groups the bitwise-or with `c` first, then `&&`, then `||`.
		{"a || b && c | d", "(a || (b && (c | d)))"},
		// Existing boolean precedence, unaffected by inserting bitwise ops
		// between it and comparison (regression).
		{"a || b && c", "(a || (b && c))"},
		// Unary ~ binds like unary - and ! (tighter than any binary operator).
		{"~a & b", "((~a) & b)"},
		{"-a << 1", "((-a) << 1)"},
		{"~a == b", "((~a) == b)"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			e, err := ParseExpr(c.src)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", c.src, err)
			}
			if got := sexpr(e); got != c.want {
				t.Errorf("ParseExpr(%q) = %s, want %s", c.src, got, c.want)
			}
		})
	}
}

// A lower-case name in call position that is not a builtin is a call — of a
// parameterized derive, which only the builder can resolve — and a list whose
// value reads its row only through such a call takes its item variable from the
// filter: the leftmost member access on a name no nested aggregate binds.
func TestProjectionCallAndItemVariable(t *testing.T) {
	ex, err := ParseExpr("card(w, me)")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := ex.(ast.Call); !ok || c.Name != "card" || len(c.Args) != 2 {
		t.Fatalf("card(w, me) should parse as a two-argument call, got %#v", ex)
	}
	for src, want := range map[string]string{
		"list(card(a, me) in Account where a.handle != \"\")":                                           "a",
		"list(card(a, me) in Account where exists(b in Block where b.owner == me && b.target == a.id))": "a",
		"list(card(Account(v.account), me) in View where v.id > 0)":                                     "v",
	} {
		ex, err := ParseExpr(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if ag, ok := ex.(ast.Agg); !ok || ag.Var != want {
			t.Errorf("%s: item variable = %#v, want %q", src, ex, want)
		}
	}
}

func TestExponentFloatLiterals(t *testing.T) {
	for src, want := range map[string]float64{"1e22": 1e22, "2.5E-3": 2.5e-3, "7e+2": 700, "0e0": 0} {
		ex, err := ParseExpr(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		lit, ok := ex.(ast.Lit)
		if !ok || lit.Kind != "float" || lit.Val != want {
			t.Fatalf("%s parsed as %#v, want float %v", src, ex, want)
		}
	}
	// An `e` with no digit after it is not an exponent: `2e` is still an
	// int followed by an identifier, exactly as before.
	if toks := tokenize("2e"); len(toks) < 2 || toks[0].text != "2" {
		t.Fatalf("2e tokenized as %v", toks)
	}
}
