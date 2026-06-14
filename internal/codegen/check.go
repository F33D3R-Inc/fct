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
		declared := make(map[string]bool, len(f.Fields))
		for _, fl := range f.Fields {
			declared[fl.Name] = true
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
				if plain[r] || seen[r] {
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
