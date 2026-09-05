package ast

import "fmt"

// Slot placement — the one traversal that decides where an injected tree lands.
//
// Two things splice a foreign tree into a chrome tree: a layout's `slot`, which
// receives the routed view, and a wireframe frame's `slot <name>` (SlotRef),
// which receives a socket's composited content. Both need the same structural
// answer — which nodes a marker may be nested inside, and which of those repeat
// — and two copies of that answer drifting apart is precisely the bug this file
// exists to prevent.
//
// It had already happened. The parser's "a layout must contain a `slot`" check
// did not look inside `for`; the splicer did. A layout with two slots, one of
// them in a loop, therefore passed validation and was then spliced once per row,
// with the loop's row variable capturing the routed view's free names — the same
// `{r}` reading a state cell outside the loop and the layout's row inside it.
//
// So there is one recursion, spliceInto, and it recurses into *every* node that
// carries a body. A marker this traversal cannot see is a marker some later pass
// mishandles in silence. What a marker is replaced by is the caller's business;
// where a marker may be is not.

// spliceInto rewrites nodes, handing every Slot and SlotRef it finds to fill and
// splicing what comes back in the marker's place. inLoop is true when the marker
// sits under a `for`, at any depth — the one structural fact a splice policy
// needs and cannot recover afterwards.
//
// The rewrite is a copy: chrome is spliced once per view that wraps in it, so
// every slice this touches is rebuilt rather than written through.
func spliceInto(nodes []Node, inLoop bool, fill func(marker Node, inLoop bool) ([]Node, error)) ([]Node, error) {
	var out []Node
	for _, n := range nodes {
		switch t := n.(type) {
		case Slot:
			rep, err := fill(t, inLoop)
			if err != nil {
				return nil, err
			}
			out = append(out, rep...)
		case SlotRef:
			rep, err := fill(t, inLoop)
			if err != nil {
				return nil, err
			}
			out = append(out, rep...)
		case Modified:
			// Recurse through the decorator so a slot inside `box class "x":` still
			// receives the tree; a bare styled slot drops the wrapper and splices,
			// since a decorator has exactly one child and cannot hold two.
			inner, err := spliceInto([]Node{t.Inner}, inLoop, fill)
			if err != nil {
				return nil, err
			}
			if len(inner) == 1 {
				t.Inner = inner[0]
				out = append(out, t)
			} else {
				out = append(out, inner...)
			}
		case Box:
			kids, err := spliceInto(t.Children, inLoop, fill)
			if err != nil {
				return nil, err
			}
			out = append(out, Box{Children: kids})
		case Row:
			kids, err := spliceInto(t.Children, inLoop, fill)
			if err != nil {
				return nil, err
			}
			out = append(out, Row{Children: kids})
		case If:
			body, err := spliceInto(t.Body, inLoop, fill)
			if err != nil {
				return nil, err
			}
			out = append(out, If{Cond: t.Cond, Body: body})
		case For:
			// Everything below here repeats, however deep.
			body, err := spliceInto(t.Body, true, fill)
			if err != nil {
				return nil, err
			}
			t.Body = body
			out = append(out, t)
		case Overlay:
			body, err := spliceInto(t.Body, inLoop, fill)
			if err != nil {
				return nil, err
			}
			t.Body = body
			out = append(out, t)
		case Form:
			body, err := spliceInto(t.Body, inLoop, fill)
			if err != nil {
				return nil, err
			}
			t.Body = body
			out = append(out, t)
		case Use:
			// A marker under a `use` makes the injected tree that component's
			// children. That is a real splice into a scope with binders of its own,
			// and the component expansion in internal/ir already alpha-renames those
			// binders apart from the names spliced children mention, so the tree
			// arrives here uncaptured rather than needing a second mechanism.
			body, err := spliceInto(t.Body, inLoop, fill)
			if err != nil {
				return nil, err
			}
			t.Body = body
			out = append(out, t)
		case Tabs:
			tabs := make([]Tab, len(t.Tabs))
			for i, tb := range t.Tabs {
				body, err := spliceInto(tb.Body, inLoop, fill)
				if err != nil {
					return nil, err
				}
				tb.Body = body
				tabs[i] = tb
			}
			t.Tabs = tabs
			out = append(out, t)
		case Match:
			cases := make([]MatchCase, len(t.Cases))
			for i, cs := range t.Cases {
				body, err := spliceInto(cs.Body, inLoop, fill)
				if err != nil {
					return nil, err
				}
				cs.Body = body
				cases[i] = cs
			}
			els, err := spliceInto(t.Else, inLoop, fill)
			if err != nil {
				return nil, err
			}
			t.Cases, t.Else = cases, els
			out = append(out, t)
		default:
			out = append(out, n)
		}
	}
	return out, nil
}

// SpliceLayout substitutes a routed view's nodes for the `slot` in a layout's
// tree, producing the single node tree a page is lowered from — the layout's
// chrome surrounding the view, so the runtime needs no layout concept at all.
//
// It is also the layout's validation. The parser calls it with a nil view to
// find out whether a layout is well-formed, and internal/ir calls it with the
// real view to perform the splice; both get the same answer because it is the
// same traversal deciding, not two functions that happen to agree. Passing a nil
// view is meaningful precisely because the checks below do not depend on what is
// being spliced, only on where.
//
// Two rules, both following from the same fact — a layout wraps exactly one
// routed view, and that view is one tree:
//
//   - Exactly one `slot`. Two would render the page's whole content twice, and
//     the second copy would duplicate every page-local address the view mints:
//     its bindings, its region ids, its inputs. (A component has at most one for
//     the same reason; a layout has exactly one, because there is no call site
//     that could decline to pass a view.)
//   - No `slot` in a repeating context. A `for` would splice the view once per
//     row — a count decided by data, so an empty table renders a route that
//     resolved as nothing at all — and the loop's row variable would capture the
//     view's free names, quietly re-pointing the author's page at a row it was
//     never written against.
//
// A `slot` under an `if` stays legal: that is chrome choosing whether to render
// the view, not repeating it, and it binds no names.
func SpliceLayout(name string, layout, view []Node) ([]Node, error) {
	n := 0
	out, err := spliceInto(layout, false, func(marker Node, inLoop bool) ([]Node, error) {
		if ref, isRef := marker.(SlotRef); isRef {
			return nil, fmt.Errorf("layout %q uses `slot %s`; a named slot belongs to a wireframe frame, and a layout has one anonymous `slot` for the view it wraps", name, ref.Name)
		}
		if inLoop {
			return nil, fmt.Errorf("layout %q has a `slot` inside a `for`; a layout's slot may not sit in a repeating context, since the view it wraps is one tree and a `for` would render it once per row", name)
		}
		n++
		if n > 1 {
			return nil, fmt.Errorf("layout %q has more than one `slot`; a layout has exactly one, since the view it wraps is one tree", name)
		}
		return view, nil
	})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("layout %q must contain a `slot`", name)
	}
	return out, nil
}

// SpliceFrame returns a wireframe frame as a plain node tree: a copy of the
// frame with each `slot <name>` replaced by the content of the facets snapped
// into that socket. An unfilled socket renders nothing.
//
// It shares spliceInto with SpliceLayout, so the two answer "where may a slot
// appear" identically and a socket's fill cannot land in a loop either — the
// content snapped into a socket is one tree for the same reason a routed view
// is.
func SpliceFrame(frame []Node, fill map[string][]Node) ([]Node, error) {
	return spliceInto(frame, false, func(marker Node, inLoop bool) ([]Node, error) {
		ref, isRef := marker.(SlotRef)
		if !isRef {
			return nil, fmt.Errorf("a wireframe frame uses `slot <name>`, not a bare `slot`")
		}
		if inLoop {
			return nil, fmt.Errorf("`slot %s` sits inside a `for`; a socket's content may not land in a repeating context, since what snaps into a socket is one tree and a `for` would render it once per row", ref.Name)
		}
		return fill[ref.Name], nil
	})
}
