package runtime

import (
	"strings"

	"facet/internal/ir"
)

// classText renders an interpolated `class` value, filtering each interpolated
// run to the characters a class token may contain.
//
// It is linkHref's rule applied to the other attribute a value can be placed
// into structurally rather than textually. A class attribute is a
// whitespace-separated token LIST: the author wrote the literal text around the
// hole and gave the value exactly one token to fill, so a value carrying a space
// would quietly add a second class it was never granted — the same shape of bug
// as a path value carrying a `/` and quietly becoming two segments. Filtering the
// interpolated runs, and only those, keeps the author's literal separators and
// makes the data inert.
//
// This is not an HTML-escaping concern and does not replace one: the server
// escapes at the attribute boundary and the client assigns through `className`,
// so a quote in a class value could never have broken out of the attribute on
// either side. What it could do is mean something — and the reason `class`
// interpolates while `style` does not is that "what a class token may contain" is
// a rule that fits on one line, and "what a CSS declaration may safely contain"
// is not.
func classText(segs []ir.Seg, render func([]ir.Seg) string) string {
	var b strings.Builder

	for _, seg := range segs {
		// Dynamic when it carries an expression OR a top-level state binding; only
		// a pure literal is written through unfiltered, so the author's spaces and
		// prefixes survive and nothing else does. Mirrors linkHref.
		if seg.Expr == nil && seg.Bind == "" {
			b.WriteString(seg.Lit)
			continue
		}

		b.WriteString(escapeClassToken(render([]ir.Seg{seg})))
	}

	return b.String()
}

// escapeClassToken drops every character outside the class-token set: A-Z, a-z,
// 0-9, `-` and `_`.
//
// Dropping rather than substituting, because a class name is an identity, not a
// phrase: there is no character that "means" a removed one, and mapping the
// rejected set onto `-` would make `a b` and `a-b` the same class. ASCII only,
// and written out rather than delegated to a Unicode-aware category test, for the
// reason escapePathSegment is: the client has to produce a byte-identical value,
// and a class that differs between first paint and hydration is an element whose
// appearance changes the instant the page becomes interactive. The rule is stated
// here, mirrored in `assets/facet.js`, and a test runs both over the same inputs.
func escapeClassToken(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}

	return b.String()
}
