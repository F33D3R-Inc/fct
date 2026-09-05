package ir

import (
	"fmt"

	"facet/internal/ast"
)

// What a value's type is, and what the compiler does with the answer.
//
// # Why this file exists
//
// Every place the language *declares* a type — an entity field, a state cell, a
// component parameter, a record field — the declaration was checked against the
// value at exactly two positions and nowhere else: a `cell` reference argument
// (`use Field("Email", email)`, checked because a reference names a declaration
// whose type is already in a table) and a data-driven option's value
// (`option "{c.name}" -> c.id`, checked because one shape of expression —
// `item.field` over a known row — could be typed by hand at that one site).
//
// Everywhere else the answer was "the compiler does not know the type of an
// expression", so a `use` of a component checked how *many* arguments it was
// handed and never what they were — and the runtime is total (see toStr/toInt),
// so a wrong-typed argument does not fail, it renders wrong. There was no
// diagnostic for a text landing in an `int` parameter, and the only way to see
// one was to read the page.
//
// So the missing thing was not a check, it was the question the check asks. This
// file answers it once — `what type is this expression?` — off the tables the
// builder already keeps, and the `use` site consults it in the same voice as the
// arity check next to it. What the answer is then allowed to refuse is the
// second half of this comment, and it is narrower than it first looks.
//
// # The rule, and why it is exactly this wide
//
// A value position accepts a value of its declared type, or of any type that
// *widens* into it. There is exactly one widening in this language and it is
// into text.
//
// That is not a compromise, it is what the language does. Both interpreters are
// total and both stringify: toStr in runtime/eval.go and the same disjunction in
// facet.js turn an int, a bool, a money amount, a date and an enum member into
// the same characters, so `text "{n}"` prints a count and so does a component
// that interpolates a `text` parameter it was handed one for. The library was
// written against that fact on purpose. facets/layout/sectionheader.fct declares
// `subtitle: text` and explains it in the file:
//
//	"a subtitle is a sentence, sometimes a number, and an int argument converts
//	 into a text parameter for free while a sentence does not convert into an
//	 int at all"
//
// — and facets/layout/section.fct repeats it, and seven call sites across the
// storefront pass a count (a stock level, a line quantity, `count(Category)`)
// into a `text` parameter and render correctly. There is also no cast in the
// language: no `text(x)`, and braces in an argument are a compile error
// (braces.go), so `"" + count(Category)` would be the only way to write those
// seven. Refusing them would not have found a bug, it would have broken working,
// documented code and made a whole layout facet unwritable. So it is allowed.
//
// The reverse is the one that silently lies, and it is refused. toInt("sold
// out") is 0 — total, no diagnostic, from a rendering path with nowhere to put
// one — so a text handed to an `int` parameter does not render oddly, it renders
// a different row. Likewise truthy("false") is true, so text into a `bool`
// parameter is a condition that is always taken. Those are the mistakes that
// cannot be seen by reading the page.
//
// The rest of the lattice collapses, because the storage says so:
//
//   - An enum is a closed *text* type. The builder already stores an enum-typed
//     entity field as "text" (entFieldType), an enum cell holds text, and
//     `Status.active` lowers to a text literal. enum and text are one type.
//   - `money` and `date` are int storage with a rendering convention — money()
//     is a formatter that returns text, a date is unix seconds, and coerceParam
//     parses int/money/date with one call. They are one type with int.
//   - A relation field holds the referenced row's id, an int; reads across it
//     are written `User(m.to).name`. An entity-typed value is an int.
//   - A list and a scalar never unify: `[text]` is not text in any renderer.
//
// # What this does and does not catch
//
// It catches a text where a number was declared, a text where a bool was
// declared, and a list against a scalar — every position at once, since a
// component is expanded per call site and this is the same question asked of
// each argument.
//
// It does not catch an int passed where `text` was declared, which is the shape
// the bug was reported as (`use Byline(author, created, updated)` against
// `Byline(author: text, href: text, at: int)`, with an int timestamp landing in
// `href`). In this language that argument is legitimate — it is the same
// argument `use Section("Categories", count(Category))` passes — and a rule that
// refused it would refuse the library's own documented usage. What went wrong
// there was semantic, not typed: three arguments of the right shapes in the
// wrong order. A type system does not see that one, and pretending otherwise by
// over-tightening the lattice would cost working code and still not see it.
//
// # Unknown is not an error
//
// exprType is partial on purpose. It reports a type only when the tables prove
// one; anything it cannot type is reported as unknown and accepted. A check that
// guesses would refuse working code, and refusing working code is how a type
// rule gets deleted rather than fixed.

// vtype is a value's type as the compiler knows it: a type core plus whether the
// value is a list of that core. The core is a primitive ("int", "text", "bool",
// "money", "date"), an enum name, or an entity name (a row, or a relation field
// holding one's id).
type vtype struct {
	core string
	list bool
}

// known reports whether this is an answer rather than the absence of one.
func (t vtype) known() bool { return t.core != "" }

// label renders a type the way the author wrote it, for a diagnostic.
func (t vtype) label() string {
	if t.list {
		return "[" + t.core + "]"
	}
	return t.core
}

// unify collapses the cores that are the same type wearing different names, so
// compatibility is one string comparison over a canonical form.
//
// An enum is a closed text type; money and date are int storage; an entity-typed
// value is the referenced row's int id. Each of those is a fact stated elsewhere
// in the builder (entFieldType lowers enum fields to "text"; coerceParam parses
// int/money/date identically; a relation column stores an id) — this function is
// the one place they are collected, so a check cannot disagree with the storage.
func (e *env) unify(core string) string {
	switch core {
	case "money", "date":
		return "int"
	case "":
		return ""
	}
	if _, isEnum := e.enums[core]; isEnum {
		return "text"
	}
	if e.entities[core] {
		return "int"
	}
	return core
}

// assignable reports whether a value of type got may be used where want is
// declared. An unknown on either side is accepted: see the note above.
//
// The one conversion allowed across cores is *to* text, and it is allowed
// because the language really does perform it, everywhere, in both interpreters:
// toStr in runtime/eval.go and the same disjunction in facet.js turn every
// scalar into the same characters. The library relies on it by design —
// facets/layout/sectionheader.fct declares `subtitle: text` and says why: "a
// subtitle is a sentence, sometimes a number, and an int argument converts into
// a text parameter for free while a sentence does not convert into an int at
// all" — and seven call sites across the storefront pass a count that way.
//
// The reverse is the one that silently lies. toInt("sold out") is 0, with no
// diagnostic anywhere, so a text handed to an `int` parameter does not render
// oddly, it renders a different row. That asymmetry is the rule.
func (e *env) assignable(got, want vtype) bool {
	if !got.known() || !want.known() {
		return true
	}
	if got.list != want.list {
		return false
	}
	g, w := e.unify(got.core), e.unify(want.core)
	return g == w || w == "text"
}

// exprType computes the type of a view expression from the declaration tables,
// or reports that it does not know.
//
// It is a method on env rather than viewCtx because the same question is asked
// of a component body, a page and an expansion, and the tables it reads are the
// app's, not the render context's; only the local variable types come from the
// scope.
func (e *env) exprType(ex ast.Expr, sc scope) vtype {
	switch t := ex.(type) {
	case ast.Lit:
		switch t.Kind {
		case "int", "text", "bool":
			return vtype{core: t.Kind}
		}

	case ast.Ref:
		// A local shadows everything, exactly as it does in both renderers.
		if v, ok := sc.varTypes[t.Name]; ok {
			return v
		}
		if _, ok := e.states[t.Name]; ok {
			return vtype{core: e.stateTypes[t.Name], list: e.stateList[t.Name]}
		}
		if v, ok := e.inlineType[t.Name]; ok { // a derive or a zero-arg policy
			return v
		}
		switch t.Name {
		case "actor", "role", "tenantRole", "route":
			return vtype{core: "text"}
		case "verified":
			return vtype{core: "bool"}
		case "tenant":
			return vtype{core: "int"}
		}

	case ast.ActState:
		// failed() is the last error message; the other three are flags.
		if t.Op == "failed" {
			return vtype{core: "text"}
		}
		return vtype{core: "bool"}

	case ast.Get:
		if r, ok := t.Obj.(ast.Ref); ok {
			// `Status.active` is a member of a closed text type, not a field access.
			if _, isEnum := e.enums[r.Name]; isEnum {
				return vtype{core: r.Name}
			}
			// A record-typed action local (`let v = call …`).
			if rb, isRec := e.locRecords[r.Name]; isRec && !rb.list {
				if f, ok := e.records[rb.rec][t.Field]; ok {
					return vtype{core: f.typ, list: f.list}
				}
				return vtype{}
			}
		}
		if ot := e.exprType(t.Obj, sc); ot.known() && !ot.list && e.entities[ot.core] {
			return vtype{core: e.entFieldType[ot.core][t.Field]}
		}

	case ast.EntityGet:
		return vtype{core: e.entFieldType[t.Entity][t.Field]}

	case ast.Agg:
		switch t.Op {
		case "count":
			return vtype{core: "int"}
		case "exists":
			return vtype{core: "bool"}
		default: // sum | avg | min | max reduce one numeric value per row
			if t.Sel != nil {
				// The reduced value is an expression over the row, so the
				// aggregate's type is that expression's — read with the item
				// variable bound to a row of the collection, which is what makes
				// `sum(l.qty * l.unitPrice in …)` money rather than untyped.
				inner := sc.with(t.Var)
				inner.varTypes[t.Var] = vtype{core: t.Coll}
				return e.exprType(t.Sel, inner)
			}
			return vtype{core: e.entFieldType[t.Coll][t.Field]}
		}

	case ast.Call:
		switch t.Name {
		case "upper", "lower", "trim", "ago", "compact", "commas", "take":
			return vtype{core: "text"}
		case "money":
			// money() is the *formatter*: it renders int cents as text. The `money`
			// type is what it reads, not what it returns.
			return vtype{core: "text"}
		case "contains":
			return vtype{core: "bool"}
		case "len", "year", "month", "day", "rand":
			return vtype{core: "int"}
		case "now":
			return vtype{core: "date"}
		case "abs", "floor", "round":
			// These preserve the numeric flavour they are handed: abs() of a money
			// amount is a money amount. Anything non-numeric is not their input, so
			// nothing is claimed about it. (Arity is checked separately, and this
			// may run before it is, so a malformed call types nothing.)
			if len(t.Args) == 1 {
				return e.numeric(e.exprType(t.Args[0], sc))
			}
		case "min", "max":
			if len(t.Args) != 2 {
				break
			}
			a, b := e.numeric(e.exprType(t.Args[0], sc)), e.numeric(e.exprType(t.Args[1], sc))
			if a == b {
				return a
			}
			if a.known() && b.known() {
				return vtype{core: "int"}
			}
		}

	case ast.ListLit:
		// A list literal's element type is the one every element agrees on; a mixed
		// list is not typed rather than being typed wrongly.
		var el vtype
		for i, x := range t.Elems {
			xt := e.exprType(x, sc)
			if i == 0 {
				el = xt
				continue
			}
			if xt != el {
				return vtype{}
			}
		}
		if !el.known() || el.list {
			return vtype{}
		}
		return vtype{core: el.core, list: true}

	case ast.Bin:
		switch t.Op {
		case "==", "!=", "<", "<=", ">", ">=", "&&", "||", "in":
			return vtype{core: "bool"}
		case "+":
			// `+` is concatenation when either side is text and addition otherwise —
			// the same disjunction eval.go makes at runtime.
			l, r := e.exprType(t.L, sc), e.exprType(t.R, sc)
			if (l.known() && !l.list && e.unify(l.core) == "text") || (r.known() && !r.list && e.unify(r.core) == "text") {
				return vtype{core: "text"}
			}
			return e.arith(l, r)
		case "-", "*", "/", "%":
			return e.arith(e.exprType(t.L, sc), e.exprType(t.R, sc))
		}

	case ast.Un:
		switch t.Op {
		case "!":
			return vtype{core: "bool"}
		case "-":
			return e.numeric(e.exprType(t.X, sc))
		}
	}
	return vtype{}
}

// numeric keeps a numeric flavour (int, money, date, or an entity id) and drops
// everything else, so a builtin that only makes sense over numbers claims
// nothing about a value that is not one.
func (e *env) numeric(t vtype) vtype {
	if t.list || e.unify(t.core) != "int" {
		return vtype{}
	}
	return t
}

// arith types an arithmetic operation. Two operands of the same numeric flavour
// keep it (money - money is money); anything else that is still numeric is a
// plain int, and a non-numeric operand types nothing.
func (e *env) arith(l, r vtype) vtype {
	l, r = e.numeric(l), e.numeric(r)
	if l == r {
		return l
	}
	if l.known() && r.known() {
		return vtype{core: "int"}
	}
	return vtype{}
}

// declType reads a declared type — a parameter's, a field's — as a vtype, and
// reports unknown for a core the language does not have. A declaration the
// builder cannot resolve to a real type constrains nothing: the check's job is
// to catch a wrong argument, not to be the place a bad declaration is finally
// noticed.
func (e *env) declType(core string, list bool) vtype {
	if !isPrimitive(core) {
		if _, isEnum := e.enums[core]; !isEnum && !e.entities[core] {
			return vtype{}
		}
	}
	return vtype{core: core, list: list}
}

// paramTypes is the scope entry set a component body renders in: each value
// parameter bound to its declared type. A reference parameter carries no value —
// it is substituted into the body as the name of a real cell or action, whose
// type the tables already hold — so it contributes nothing here.
func (e *env) paramTypes(ps []ast.Param) map[string]vtype {
	out := map[string]vtype{}
	for _, p := range ps {
		if p.Ref != ast.RefValue {
			continue
		}
		if t := e.declType(p.Type, p.List); t.known() {
			out[p.Name] = t
		}
	}
	return out
}

// checkArgType is the value-parameter half of what a `use` checks about its
// arguments — the other halves being how many there are and, for a reference
// parameter, which declaration it names.
//
// It sits beside those rather than in a pass of its own because this is the one
// point where the parameter's declaration and the caller's expression are both
// in hand: a component is expanded per call site, so the caller's scope — the
// row a `for` binds, the cells, the enclosing component's own parameters — is
// exactly the scope the argument was written in.
func (e *env) checkArgType(u ast.Use, i int, p ast.Param, sc scope) error {
	want := e.declType(p.Type, p.List)
	got := e.exprType(u.Args[i], sc)
	if !e.assignable(got, want) {
		return &BuildError{u.Line, fmt.Sprintf(
			"component %q parameter %q is %s, but argument %d is %s",
			u.Name, p.Name, want.label(), i+1, got.label())}
	}
	// An entity-typed parameter is a ROW — `component PostCard(t: Tweet)` reads
	// `t.body` — and unify() cannot see that: it collapses an entity core to the
	// int id a relation column stores, so an id (`p.id`, a relation field `p.by`,
	// or a bare `5`) unifies with the row it is not. Handed one, the component
	// would render every field as nothing, with no diagnostic anywhere. So the
	// argument has to *be* a row: the variable a `for` binds, or an enclosing
	// component's own entity parameter. Nothing else in the language produces one
	// (`Post(id)` always takes a `.field`).
	if want.known() && !want.list && e.entities[want.core] && !isRowRef(u.Args[i], sc, want.core) {
		return &BuildError{u.Line, fmt.Sprintf(
			"component %q parameter %q is a %s row, but argument %d (%s) is not one — pass the row itself, the variable a `for x in %s` binds, not its id",
			u.Name, p.Name, want.core, i+1, describeArg(u.Args[i], got), want.core)}
	}
	return nil
}

// isRowRef reports whether ex names a row of the given entity in this scope: a
// `for` item variable over it, or an entity-typed component parameter.
func isRowRef(ex ast.Expr, sc scope, entity string) bool {
	r, ok := ex.(ast.Ref)
	if !ok {
		return false
	}
	t, ok := sc.varTypes[r.Name]
	return ok && !t.list && t.core == entity
}

// describeArg names an argument for the row diagnostic: what it is, then its type.
func describeArg(ex ast.Expr, got vtype) string {
	switch t := ex.(type) {
	case ast.Get:
		if r, ok := t.Obj.(ast.Ref); ok {
			if got.known() {
				return fmt.Sprintf("`%s.%s`, %s", r.Name, t.Field, idOrType(got))
			}
			return fmt.Sprintf("`%s.%s`", r.Name, t.Field)
		}
	case ast.Lit:
		return fmt.Sprintf("the literal %v", t.Val)
	case ast.Ref:
		return fmt.Sprintf("`%s`, %s", t.Name, idOrType(got))
	}
	if got.known() {
		return got.label()
	}
	return "an expression"
}

func idOrType(got vtype) string {
	if got.core == "int" {
		return "an int"
	}
	return "a " + got.label()
}
