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
