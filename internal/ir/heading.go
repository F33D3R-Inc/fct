package ir

import (
	"fmt"

	"facet/internal/ast"
)

// What the compiler can honestly say about a heading level.
//
// A correct document has one `h1` and skips no levels. Neither of those is
// checkable here and neither is claimed, for three separate reasons — any one of
// which would be enough:
//
//   - The level is a value. `heading level "{title}"` inside a component is an
//     `h2` or an `h5` depending on what each call site passed, and call sites are
//     spread across pages, layouts, wireframe sockets and other components.
//   - A component is composed into pages it cannot see. The whole point of a
//     library is that `SectionHeader` does not know what encloses it, so no fact
//     about the outline of the finished page is available where the heading is
//     written.
//   - Only one arm of an `if` or a `match` renders. The rendered sequence of
//     headings is a per-request path through the tree, not the tree, so even an
//     app whose every level is a literal has as many outlines as it has branches.
//
// A rule that fired on the fully-literal subset and went quiet the moment one
// heading became dynamic would be worse than no rule: it would look like
// coverage. So this file checks exactly the three things that are true wherever
// they are written, and stops.

// headingLevel checks and lowers a heading's level.
//
// THE THIRD CHECK IS THE INTERESTING ONE. A heading is a leaf: it has no region
// id, so nothing on the client can re-render it in place. Its *text* still
// updates — each interpolation is a binding the client refills — but the element
// itself, and therefore the level, is written once at first paint. A level read
// straight from a `@client` cell would then be correct on the server render and
// permanently stale afterwards, which is the exact shape of bug this repo has
// paid for repeatedly: something that renders right the first time.
//
// So a level may not read state or a collection. Everything a level legitimately
// wants is still expressible, because the values that vary are the ones that
// arrive through something that IS a region: a component parameter (a `use` is a
// region and re-renders when its arguments' reads change) or a row variable (a
// `for` is a region and re-renders with its rows). The refusal points at those.
//
// It also has a second effect worth stating: an expression with no dependencies
// contains no aggregate, no entity lookup and no state reference, since each of
// those contributes a dependency. That is what keeps Level out of aggIndex,
// walkNodeExprs, rowAggregates and their mirrors in assets/facet.js — the two
// sides address aggregates by position in a shared walk, and a field one side
// walked and the other skipped would shift every number after it.
func (c *viewCtx) headingLevel(t ast.Heading, sc scope) (*Expr, error) {
	if err := c.e.checkPure(t.Level, viewScope(sc.locals), t.Line, "a heading level"); err != nil {
		return nil, err
	}
	if n, ok := constLevel(t.Level); ok && (n < 1 || n > 6) {
		return nil, &BuildError{t.Line, fmt.Sprintf(
			"a heading level is between 1 and 6, but this one is %d — there is no <h%d> element. "+
				"The outer heading of a page is 1 and each nesting step is one deeper; a heading that only needs to look smaller is a `text` with a `class`", n, n)}
	}
	if got := c.e.exprType(t.Level, sc); !c.e.assignable(got, vtype{core: "int"}) {
		return nil, &BuildError{t.Line, fmt.Sprintf(
			"a heading level is a number between 1 and 6, but this one is %s. "+
				"There is no conversion into it: toInt(\"two\") is 0 with no diagnostic anywhere, so a text level would render an element nobody asked for rather than fail", got.label())}
	}
	le := c.e.low(t.Level)
	if deps := sortedKeys(c.e.depsIR(le)); len(deps) > 0 {
		return nil, &BuildError{t.Line, fmt.Sprintf(
			"a heading level cannot read %q: a heading is a leaf with no region of its own, so there is nothing to re-render it when that changes and the level would be right at first paint and stale forever after. "+
				"Pass the level in from something that is a region — a component parameter (`component X(level: int): heading level \"…\"`, called as `use X(3)`) or a row variable inside a `for`", deps[0])}
	}
	return le, nil
}

// constLevel reads a level the author wrote as a number, so the one thing about
// a level that is knowable without running anything can be refused where it is
// written. `-1` is a unary minus over a literal rather than a negative literal,
// and it is exactly the mistake worth catching, so it is folded here.
func constLevel(ex ast.Expr) (int, bool) {
	switch t := ex.(type) {
	case ast.Lit:
		if t.Kind == "int" {
			if n, ok := t.Val.(int); ok {
				return n, true
			}
		}
	case ast.Un:
		if t.Op == "-" {
			if n, ok := constLevel(t.X); ok {
				return -n, true
			}
		}
	}
	return 0, false
}
