package codegen

import (
	"fmt"

	"github.com/F33D3R-Inc/fct/internal/ast"
)

// checkFieldRefs is the compile-time type-safety pass: every identifier a facet
// references in its looks: block must be a field it declares in what:, or a loop
// variable in scope. A reference to anything else (a typo, a renamed field, a
// field the author forgot to declare) is a COMPILE ERROR with the facet and name
// — not a silent blank at runtime. This is what "the compiler has your back"
// means for FA: the data contract a facet states in what: is enforced.
//
// Roots are extracted by lexing each expression and taking identifier tokens not
// preceded by a dot (so `viewer.can_view(post)` yields roots `viewer` and `post`,
// never the method `can_view`); the boolean literals true/false are not roots.
func checkFieldRefs(facets []*ast.Facet) error {
	for _, f := range facets {
		declared := make(map[string]bool, len(f.Fields)+len(f.State))
		for _, fl := range f.Fields {
			declared[fl.Name] = true
		}
		// State signals (Brick 1) share the looks: name scope with what: fields, so
		// `{count}` resolves to the local signal. checkState already guarantees no
		// name collides between the two blocks.
		for _, s := range f.State {
			declared[s.Name] = true
		}
		// Async queries (Brick 11) resolve to reactive {loading,error,data} values,
		// and `route` (Brick 10) is the built-in client path — both are valid roots
		// in looks: just like signals.
		for _, q := range f.Queries {
			declared[q.Name] = true
		}
		for b := range builtinSignals {
			declared[b] = true
		}
		// Only server-rendered bodies (looks:) are field-checked against what:.
		// A client body (vault decrypt: / media source:) legitimately references
		// values the client runtime produces — decrypted plaintext, player state —
		// which are NOT what: props (what: holds the encrypted envelope), so the
		// data-contract check does not apply there.
		if err := checkNodeRefs(f.Name, f.Looks, declared, nil); err != nil {
			return err
		}
	}
	return nil
}

// checkNodeRefs walks a node stream linearly, tracking loop scope exactly as
// codegen does (for-loops push a variable until their end), and validates every
// expression's roots. It recurses into block-child fill content (which renders in
// the same scope).
func checkNodeRefs(facet string, nodes []ast.Node, declared map[string]bool, scope []string) error {
	forStack := make([]bool, 0, 4) // true => the open block is a for (pops scope)
	for _, n := range nodes {
		switch v := n.(type) {
		case ast.Interp:
			if err := checkRoots(facet, v.Expr, declared, scope); err != nil {
				return err
			}
		case ast.Ctrl:
			switch v.Op {
			case "if":
				if v.Expr != "" {
					if err := checkRoots(facet, v.Expr, declared, scope); err != nil {
						return err
					}
				}
				forStack = append(forStack, false)
			case "for":
				if err := checkRoots(facet, v.Iter, declared, scope); err != nil {
					return err
				}
				scope = append(scope, v.Var)
				forStack = append(forStack, true)
			case "else":
				// no scope change
			case "end":
				if n := len(forStack); n > 0 {
					if forStack[n-1] && len(scope) > 0 {
						scope = scope[:len(scope)-1]
					}
					forStack = forStack[:n-1]
				}
			}
		case ast.Child:
			for _, p := range v.Props {
				if p.IsExpr {
					if err := checkRoots(facet, p.Expr, declared, scope); err != nil {
						return err
					}
				}
			}
			if len(v.Children) > 0 {
				sub := append([]string(nil), scope...) // isolate child scope
				if err := checkNodeRefs(facet, v.Children, declared, sub); err != nil {
					return err
				}
			}
			for _, name := range sortedFillNames(v.Fills) {
				sub := append([]string(nil), scope...) // isolate fill scope
				if err := checkNodeRefs(facet, v.Fills[name], declared, sub); err != nil {
					return err
				}
			}
		case ast.Slot:
			// Slot default content renders in this facet's own scope.
			if err := checkNodeRefs(facet, v.Default, declared, scope); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkComputed validates the `what:` block: no duplicate field names, and every
// computed field's expression references only plain fields or computed fields
// declared *before* it (a forward or self reference would emit a Go-template
// `$var` used before definition). This is the compile-time contract for derived
// values — a typo or bad order is an error here, not a blank at render.
func checkComputed(facets []*ast.Facet) error {
	for _, f := range facets {
		plain := make(map[string]bool)
		all := make(map[string]bool)
		for _, fl := range f.Fields {
			if !fl.IsComputed() {
				plain[fl.Name] = true
			}
		}
		// A computed field may also derive from a state signal (Brick 5): such a
		// field recomputes client-side when the signal changes. Signals have no
		// ordering constraint among computed fields (they always have a value).
		signal := make(map[string]bool, len(f.State))
		for _, s := range f.State {
			signal[s.Name] = true
		}
		seen := make(map[string]bool) // computed fields declared so far
		for _, fl := range f.Fields {
			if all[fl.Name] {
				return fmt.Errorf("facet %s: duplicate field %q in what:", f.Name, fl.Name)
			}
			all[fl.Name] = true
			if !fl.IsComputed() {
				continue
			}
			for _, r := range exprRoots(fl.Expr) {
				if plain[r] || seen[r] || signal[r] {
					continue
				}
				return fmt.Errorf("facet %s: computed field %q references %q, which is not a field declared before it "+
					"(declare it in what:, fix the name, or move it above %q)", f.Name, fl.Name, r, fl.Name)
			}
			seen[fl.Name] = true
		}
	}
	return nil
}

// checkState validates the `state:` block (Brick 1 of docs/REACTIVITY.md): every
// state field carries an initial value, names are unique within state:, and they
// do not collide with a `what:` field — `state` and `what` share one name scope
// in looks:, so a clash would make `{count}` ambiguous. Reactivity bindings are a
// later brick; this is the language-surface contract.
func checkState(facets []*ast.Facet) error {
	for _, f := range facets {
		what := make(map[string]bool, len(f.Fields))
		for _, fl := range f.Fields {
			what[fl.Name] = true
		}
		seen := make(map[string]bool, len(f.State))
		for _, s := range f.State {
			if !s.IsComputed() { // Expr == "" → no initial value
				return fmt.Errorf("facet %s: state field %q needs an initial value", f.Name, s.Name)
			}
			if seen[s.Name] {
				return fmt.Errorf("facet %s: duplicate state field %q", f.Name, s.Name)
			}
			if what[s.Name] {
				return fmt.Errorf("facet %s: state field %q also declared in what: — give one a different name (they share the looks: scope)", f.Name, s.Name)
			}
			if builtinSignals[s.Name] {
				return fmt.Errorf("facet %s: state field %q is a reserved built-in signal — rename it", f.Name, s.Name)
			}
			seen[s.Name] = true
		}
	}
	return nil
}

// checkActions validates the `actions:` block (Brick 3 of docs/REACTIVITY.md):
// action names are unique; every assignment target is a declared *state* signal
// (what: fields are server-authoritative and immutable on the client); every
// assignment expression references only declared names (what: + state); and every
// `on:<event>="name"` wiring in looks: names a declared action. Together this is
// the typed, checked contract for the client reactive loop — handler → action →
// signal mutation → bound DOM node.
func checkActions(facets []*ast.Facet) error {
	for _, f := range facets {
		// Expression scope: what: fields (incl. computed) + state signals.
		declared := make(map[string]bool, len(f.Fields)+len(f.State))
		for _, fl := range f.Fields {
			declared[fl.Name] = true
		}
		state := make(map[string]bool, len(f.State))
		for _, s := range f.State {
			declared[s.Name] = true
			state[s.Name] = true
		}
		// Actions may READ queries (Brick 11) and route (Brick 10) — they are
		// reactive values — but may not assign to them (read-only, like what:).
		for _, q := range f.Queries {
			declared[q.Name] = true
		}
		for b := range builtinSignals {
			declared[b] = true
		}

		names := make(map[string]bool, len(f.Actions))
		for _, a := range f.Actions {
			if names[a.Name] {
				return fmt.Errorf("facet %s: duplicate action %q", f.Name, a.Name)
			}
			names[a.Name] = true
			for _, m := range a.Mutations {
				if !state[m.Target] {
					if declared[m.Target] {
						return fmt.Errorf("facet %s: action %q assigns to %q, which is a what: field — only state signals are mutable on the client (declare %q in state:)", f.Name, a.Name, m.Target, m.Target)
					}
					return fmt.Errorf("facet %s: action %q assigns to undeclared signal %q (declare it in state:)", f.Name, a.Name, m.Target)
				}
				for _, r := range exprRoots(m.Expr) {
					if !declared[r] {
						return fmt.Errorf("facet %s: action %q uses %q, which is not a what: field or state signal", f.Name, a.Name, r)
					}
				}
			}
		}

		// Every `on:<event>="name"` in looks: must reference a declared action.
		for _, h := range scanHandlers(f) {
			if !names[h.Action] {
				return fmt.Errorf("facet %s: on:%s=%q references an undeclared action (declare %q in actions:)", f.Name, h.Event, h.Action, h.Action)
			}
		}

		// Effects (Brick 7): each dependency is a state signal and the action exists.
		for _, eff := range f.Effects {
			if !names[eff.Action] {
				return fmt.Errorf("facet %s: effect runs undeclared action %q (declare it in actions:)", f.Name, eff.Action)
			}
			for _, d := range eff.Deps {
				if !state[d] {
					return fmt.Errorf("facet %s: effect depends on %q, which is not a state signal", f.Name, d)
				}
			}
		}
	}
	return nil
}

// checkQueries validates the `query:` block (Brick 11): every query has a valid
// identifier name and a non-empty URL, names are unique, and they do not collide
// with a what: field, a state signal, or a reserved built-in — the query name
// becomes a reactive root in the same looks: scope.
func checkQueries(facets []*ast.Facet) error {
	for _, f := range facets {
		taken := make(map[string]bool, len(f.Fields)+len(f.State))
		for _, fl := range f.Fields {
			taken[fl.Name] = true
		}
		for _, s := range f.State {
			taken[s.Name] = true
		}
		seen := make(map[string]bool, len(f.Queries))
		for _, q := range f.Queries {
			if q.URL == "" {
				return fmt.Errorf("facet %s: query %q needs a URL (e.g. `%s from \"/api/...\"`)", f.Name, q.Name, q.Name)
			}
			if seen[q.Name] {
				return fmt.Errorf("facet %s: duplicate query %q", f.Name, q.Name)
			}
			if taken[q.Name] || builtinSignals[q.Name] {
				return fmt.Errorf("facet %s: query %q collides with a field, signal, or built-in — give it a different name (they share the looks: scope)", f.Name, q.Name)
			}
			seen[q.Name] = true
		}
	}
	return nil
}

// checkBindValues validates `bind:value`/`bind:checked` form bindings (Brick 9):
// the target of a two-way binding must be a declared state signal — what: fields
// are server-authoritative and a query/route value is read-only, so neither can
// receive a user's keystrokes.
func checkBindValues(facets []*ast.Facet) error {
	for _, f := range facets {
		state := make(map[string]bool, len(f.State))
		for _, s := range f.State {
			state[s.Name] = true
		}
		for _, in := range scanInputs(f) {
			if !state[in.Signal] {
				return fmt.Errorf("facet %s: bind:%s=%q must target a state signal (declare %q in state:)", f.Name, in.Prop, in.Signal, in.Signal)
			}
		}
	}
	return nil
}

func checkRoots(facet, expr string, declared map[string]bool, scope []string) error {
	for _, r := range exprRoots(expr) {
		if declared[r] || inScope(scope, r) {
			continue
		}
		return fmt.Errorf("facet %s: %q is used in looks: but not declared in what: "+
			"(add `%s: <Type>` to what:, or fix the name)", facet, r, r)
	}
	return nil
}

// exprRoots returns the root identifiers an expression references — identifier
// tokens not following a dot (path roots and call arguments), minus boolean
// literals. If the expression doesn't lex, it returns nil; goExpr will surface a
// precise error later.
func exprRoots(expr string) []string {
	toks, err := exprLex(expr)
	if err != nil {
		return nil
	}
	var roots []string
	for i, t := range toks {
		if t.kind != "ident" {
			continue
		}
		if i > 0 && toks[i-1].kind == "dot" {
			continue // a field/method after a dot, not a root
		}
		if t.text == "true" || t.text == "false" {
			continue
		}
		roots = append(roots, t.text)
	}
	return roots
}
