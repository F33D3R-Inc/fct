package ir

import (
	"fmt"
	"strconv"
	"strings"

	"facet/internal/ast"
	"facet/internal/parser"
)

// This file is the whole answer to one bug class: a `{expr}` written in a quoted
// string that the compiler takes literally.
//
// `{…}` interpolates in node text, in labels and placeholders, in `class`, in
// `meta`, and in a link/image/icon/video destination. In every other quoted
// position — a component argument, `style`, an option/tab/case value, a check
// message, a route path, any text literal inside any expression — the braces and
// everything between them are output, character for character. Until this file,
// the compiler said nothing about it: `use Link("{p.name}", "/p/{p.id}")` compiled, shipped, and
// rendered `{p.name}` onto the page in the server render *and* after hydration.
// The same silence made `box style "width: {n}%"` the reason a progress bar needs
// 51 hardcoded width classes.
//
// The rule is one sentence, and it is the same sentence at every position:
//
//	a literal string whose braces would have rendered a value, had this position
//	interpolated, is a compile error.
//
// # Why the rule is scoped to names in scope, and why that is also the escape hatch
//
// The blunt version of this rule — refuse every `{` in a literal position — is
// wrong, and the tree proves it. `use Code("json", "{\n  \"address\": …")` and
// `use Prose("… `class \"x-rung-c-{tone}\"` is one line on this site …")` are
// correct code on the F33D3R website: a JSON sample and a paragraph *about* FDL
// interpolation, both passed as component arguments. Refusing them would be a
// regression, not a diagnosis.
//
// It would also be an unfixable one. FDL has no escape for a literal brace: in
// text, `{` demands a closing `}` and the inside must parse, and `\{` is not a Go
// string escape so `unquote` rejects it outright. A blanket refusal in literal
// positions would therefore make `{` unwritable there with no way to opt out.
//
// So the refusal is narrowed to exactly the strings that were silently wrong:
// braces that parse as an expression *and* read a name the surrounding scope
// defines. That is precisely the set that would have rendered a value, which is
// precisely the set the author did not mean to see as text. Everything else — a
// CSS block, a JSON sample, prose quoting a name from some other file — reads as
// what it is, text, and keeps compiling.
//
// That narrowing doubles as the escape hatch, and the diagnostics say so: braces
// around a name nothing in scope defines are literal text everywhere. Expression
// positions have a second, exact out — `"{" + "n}"` is two literals neither of
// which contains a brace pair.

// litInterp reports the first `{…}` in s that would have rendered a value had
// this position interpolated: a brace pair whose contents parse as an expression
// and every one of whose free names the scope defines. It returns the offending
// snippet, braces included, for quoting back at the author.
//
// A brace pair that reads no name at all (`{1}`) is left alone: nothing was
// dropped, because there was no value to drop.
func litInterp(s string, defines func(string) bool) (string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return "", false // an unclosed brace is not an interpolation anywhere
		}
		inner := s[i+1 : i+end]
		i += end
		ex, err := parser.ParseExpr(inner)
		if err != nil {
			continue // `{ "a": 1 }`, `{ border: none }` — text, and it stays text
		}
		names := freeNames(ex)
		if len(names) == 0 {
			continue
		}
		ok := true
		for n := range names {
			if !defines(n) {
				ok = false
				break
			}
		}
		if ok {
			return "{" + inner + "}", true
		}
	}
	return "", false
}

// concatForm rewrites an interpolated string into the expression an author should
// have written instead: `"/p/{p.id}"` → `"/p/" + p.id`, `"{p.name}"` → `p.name`.
//
// It is the "what to write" half of the diagnostic, computed rather than
// described, because the two bugs this file exists for cost their authors 27 and
// 51 call sites respectively and the fix is mechanical at every one of them.
func concatForm(s string) string {
	var parts []string
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, strconv.Quote(lit.String()))
			lit.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '{' {
			if end := strings.IndexByte(s[i:], '}'); end >= 0 {
				inner := s[i+1 : i+end]
				if _, err := parser.ParseExpr(inner); err == nil {
					flush()
					parts = append(parts, strings.TrimSpace(inner))
					i += end
					continue
				}
			}
		}
		lit.WriteByte(s[i])
	}
	flush()
	if len(parts) == 0 {
		return `""`
	}
	return strings.Join(parts, " + ")
}

// braceErr is the single diagnostic for the whole class. Every position that does
// not interpolate reports through it, so the shape of the message — where, the
// offending string, the snippet that was dropped, what to write, and the escape
// hatch — is stated once and cannot drift apart per site.
func braceErr(line int, pos, s, snip, fix string) error {
	return &BuildError{line, fmt.Sprintf(
		"%s does not interpolate, so %q renders `%s` as literal text — %s. "+
			"`{…}` interpolates in node text, labels, placeholders, `class`, `meta` and link/image destinations, and nowhere else; "+
			"braces around a name the scope does not define are literal everywhere",
		pos, s, snip, fix)}
}

// checkLiteral is the one call every literal-string position makes.
func (e *env) checkLiteral(s string, locals map[string]bool, line int, pos, fix string) error {
	snip, bad := litInterp(s, func(n string) bool { return e.resolves(n, locals) })
	if !bad {
		return nil
	}
	return braceErr(line, pos, s, snip, fix)
}

// checkLiteralExpr applies the rule to every text literal inside an expression.
// It hangs off `check`, the one funnel every expression in the language passes
// through with its scope, so a `use` argument, an action argument, a `set` value,
// a `where` operand and a policy argument are all covered by one call rather than
// by a check bolted onto each of them.
func (e *env) checkLiteralExpr(ex ast.Expr, locals map[string]bool, line int) error {
	for _, s := range textLits(ex) {
		if err := e.checkLiteral(s, locals, line,
			"a text literal", "write the value as an expression: "+concatForm(s)); err != nil {
			return err
		}
	}
	return nil
}

// textLits collects every text literal an expression contains, at any depth.
func textLits(ex ast.Expr) []string {
	var out []string
	var walk func(ast.Expr)
	walk = func(ex ast.Expr) {
		switch t := ex.(type) {
		case ast.Lit:
			if t.Kind == "text" {
				if s, ok := t.Val.(string); ok {
					out = append(out, s)
				}
			}
		case ast.ListLit:
			for _, el := range t.Elems {
				walk(el)
			}
		case ast.Get:
			walk(t.Obj)
		case ast.EntityGet:
			walk(t.Key)
		case ast.Agg:
			if t.Where != nil {
				walk(t.Where)
			}
		case ast.Call:
			for _, a := range t.Args {
				walk(a)
			}
		case ast.Bin:
			walk(t.L)
			walk(t.R)
		case ast.Un:
			walk(t.X)
		}
	}
	walk(ex)
	return out
}

// checkUseArgs applies the rule to a `use`'s arguments before they are lowered.
//
// The generic sweep in `check` would catch these too, but this position deserves
// the better message: it is where the bug was found (27 call sites in one app),
// the fix is mechanical, and the compiler knows enough here to write the fixed
// call out in full — component, parameter, and the concatenation to replace the
// template with.
func (e *env) checkUseArgs(u ast.Use, params []ast.Param, locals map[string]bool) error {
	for i, arg := range u.Args {
		if i >= len(params) {
			break
		}
		for _, s := range textLits(arg) {
			pos := fmt.Sprintf("argument %d (%s) of component %q", i+1, params[i].Name, u.Name)
			fix := fmt.Sprintf(
				"a component argument is an expression, not a template: pass `%s` (the component interpolates it where it is used)",
				concatForm(s))
			if err := e.checkLiteral(s, locals, u.Line, pos, fix); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkOptionLit applies the rule to an option's stored value.
//
// It is a method on viewCtx rather than a line in each of the two branches of
// lowerOptions because a literal option value is written in both — the static
// list and the `for`-driven one whose value half was left quoted — and the
// second is where the row variable is in scope, so it is where the mistake is
// easiest to make.
func (c *viewCtx) checkOptionLit(o ast.Option, sc scope) error {
	return c.e.checkLiteral(o.Value, viewScope(sc.locals), o.Line, "an option value",
		"an option's value is the identity it stores — the thing enum defaulting and the field typo-check rest on — so it is a compile-time literal; "+
			"for a value that comes from the row, drop the quotes and write the expression: `option \"Label\" -> "+concatForm(o.Value)+"`")
}

// checkThemeVar applies the rule to one theme token's value. A token is a CSS
// value baked into the stylesheet at compile time — there is one stylesheet per
// app, not one per render — so nothing in it can vary with data.
func (e *env) checkThemeVar(tv ast.ThemeVar) error {
	return e.checkLiteral(tv.Value, nil, tv.Line, fmt.Sprintf("theme token %q", tv.Name),
		"a theme token is a CSS value emitted once into the page's stylesheet, so it cannot read the app's data — switch whole palettes with `theme <name>:` blocks and the built-in `theme` cell instead")
}
