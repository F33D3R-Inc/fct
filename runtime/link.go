package runtime

import (
	"strings"

	"facet/internal/ir"
)

// linkHref renders an interpolated destination, escaping each interpolated value
// for the path segment it lands in.
//
// The escaping is why this is not just `segsToString`. An interpolated value is
// data — a handle, a title, a row id — being placed into a URL path, where `/`,
// `?`, `#` and `%` all mean something. A handle of `a/b` would silently become
// two path segments and route somewhere else; one containing `?` would turn the
// rest of the path into a query string. Escaping the interpolated runs and only
// those keeps the literal `/` separators the author wrote while making the data
// inert.
//
// HTML-escaping happens after this, at the attribute boundary, and is a
// different concern: it stops the value breaking out of the attribute, not out
// of the path.
// A route expression (`link "x" -> "{href}"`) is the other form and does not go
// through here at all: its value is not data landing in a segment, it is the
// whole path, so escaping its separators would destroy it. It is rendered with
// segsToString and then checked against the app's routes — see isAppRoute.
func linkHref(segs []ir.Seg, render func([]ir.Seg) string) string {
	var b strings.Builder

	for _, seg := range segs {
		// A segment is dynamic when it carries an expression OR a top-level
		// state binding; only a pure literal is written through unescaped, so
		// the author's `/` separators survive and nothing else does.
		if seg.Expr == nil && seg.Bind == "" {
			b.WriteString(seg.Lit)
			continue
		}

		b.WriteString(escapePathSegment(render([]ir.Seg{seg})))
	}

	return b.String()
}

// escapePathSegment percent-encodes everything outside RFC 3986's unreserved
// set: A-Z, a-z, 0-9, and `-._~`.
//
// Written out rather than delegated to `url.PathEscape`, because the client has
// to produce a byte-identical href and neither language's built-in matches the
// other. Go's `PathEscape` leaves `$&+:=@` unescaped in a path segment;
// JavaScript's `encodeURIComponent` escapes those and leaves `!*'()` alone. A
// handle containing `=` would therefore render one href on first paint and a
// different one the instant the client took over — a link that changes under the
// cursor, and exactly the kind of divergence that is invisible until someone
// clicks it.
//
// So the rule is stated here and mirrored in `assets/facet.js`, and a test runs
// both over the same inputs. Escaping more than strictly necessary is free; a
// disagreement is not.
func escapePathSegment(s string) string {
	const hex = "0123456789ABCDEF"

	var b strings.Builder

	for i := 0; i < len(s); i++ {
		c := s[i]

		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}

	return b.String()
}

// isAppRoute reports whether a path is served by one of this app's routes.
//
// It is the runtime half of the contract a parameterized link destination is
// checked under. A literal destination is proven against the route table at
// compile time; a computed one cannot be, so it is proven here, against the same
// table, at the moment its value exists. A destination that is not a route of
// this app — an off-site URL, a `javascript:` payload, a typo — is not turned
// into a link at all, so parameterizing a destination can never widen where a
// link may point.
//
// External links did not change this. An absolute URL is expressible now, but
// only as text the AUTHOR wrote, checked against the scheme allowlist at compile
// time and marked ir.Node.External. A destination that arrives as a VALUE still
// comes through here and still may only name a route of this app, because the day
// a runtime value can become an arbitrary anchor is the day a `javascript:`
// payload in a database row becomes a link.
func isAppRoute(routes []ir.Route, href string) bool {
	path := hrefPath(href)
	if path == "" {
		return false
	}
	for _, rt := range routes {
		if _, ok := matchRoute(rt.Path, path); ok {
			return true
		}
	}
	return false
}

// hrefPath is the route-bearing half of a destination: everything before the
// `#`.
//
// A fragment is a position inside a page, not a route, so it must come off
// before anything asks the route table a question. Without this `/docs#install`
// is not `/docs` — it is a one-segment path spelled `docs#install`, which matches
// no route, so a route expression yielding it renders as inert text and a guarded
// route's policy is never consulted. A bare `#install` reduces to the empty
// string, which is not a route at all: it is wherever the reader already is.
func hrefPath(href string) string {
	if i := strings.IndexByte(href, '#'); i >= 0 {
		return href[:i]
	}
	return href
}

// safeExternalHref reports whether a rendered destination is one this runtime
// will make an off-site anchor of.
//
// This is the render-time half of a guarantee the compiler already gives. A node
// is marked External only from a scheme and host the author wrote as literal
// text, so in a program built by this compiler the answer here is always yes —
// and that is exactly why it is worth asking. It states the property in terms of
// the string that reaches the reader's browser rather than a boolean set
// somewhere else in the pipeline, so an IR arriving from anywhere (a cached
// build, a generated one, a future front end) cannot turn `javascript:alert(1)`
// into an anchor by setting a flag. A destination that fails renders as inert
// text, the same way a route expression that names no route does.
//
// The scheme set is the compiler's allowlist and must stay identical to it, and
// to the mirror in assets/facet.js — an href the server makes a link of and the
// client makes text of (or the reverse) is a link that changes on hydration.
func safeExternalHref(href string) bool {
	for _, scheme := range []string{"https://", "http://"} {
		if !hasPrefixFold(href, scheme) {
			continue
		}
		// A host is required: `https:///x` addresses this origin, not another.
		rest := href[len(scheme):]
		for i := 0; i < len(rest); i++ {
			if rest[i] == '/' || rest[i] == '?' || rest[i] == '#' {
				return i > 0
			}
		}
		return rest != ""
	}
	if hasPrefixFold(href, "mailto:") {
		return len(href) > len("mailto:")
	}
	return false
}

// hasPrefixFold is a case-insensitive prefix test over ASCII, and only ASCII.
//
// `strings.EqualFold` and JavaScript's `toLowerCase` do not agree on every input
// — `\u0130` lowercases to one rune in Go and two UTF-16 units in JS — and the
// two sides of this runtime have to answer identically or a link changes the
// instant the client takes over. Every prefix compared here is ASCII, so folding
// only A-Z is both sufficient and something both languages can spell the same way.
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}
