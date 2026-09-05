package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// Where a layout's `slot` may sit.
//
// The bug these pin: the parser's "a layout must contain a `slot`" check did not
// look inside `for`, while the splicer that inlines the routed view did. A layout
// with two slots — one of them in a loop — therefore passed validation, and the
// routed view was then spliced once per row, its free names captured by the
// layout's row variable. Both sides now come from one traversal (ast.SpliceLayout),
// so the check cannot see a different set of slots than the splice acts on.
func TestWhereALayoutSlotMaySit(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{{
		name: "a slot in a repeating context is refused, and the error says so",
		src: `app A:
    entity Row:
        label: text
    state r: text = "STATE-R" @client
    layout L:
        box:
            for r in Row:
                slot
            box:
                slot
    view V at "/" in L:
        text "VIEW={r}"
`,
		want: "has a `slot` inside a `for`",
	}, {
		name: "a layout wraps one view, so it has one slot",
		src: `app A:
    layout L:
        box:
            slot
        box:
            slot
    view V at "/" in L:
        text "hi"
`,
		want: "has more than one `slot`",
	}, {
		name: "a layout with no slot is still refused",
		src: `app A:
    layout L:
        box:
            text "chrome"
    view V at "/" in L:
        text "hi"
`,
		want: "must contain a `slot`",
	}, {
		name: "a named slot belongs to a wireframe frame, not a layout",
		src: `app A:
    layout L:
        box:
            slot feed
    view V at "/" in L:
        text "hi"
`,
		want: "a named slot belongs to a wireframe frame",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(c.src)
			if err == nil {
				t.Fatalf("expected a compile error containing %q, got none", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected a compile error containing %q, got: %v", c.want, err)
			}
		})
	}
}

// An `if` is chrome choosing whether to render the view, not repeating it, and it
// binds no names — so it stays legal. The refusal above is about repetition, not
// about depth.
func TestALayoutSlotMaySitUnderAnIf(t *testing.T) {
	g := mustCompile(t, `
app A:
    state on: bool = true @client
    layout L:
        box:
            if on:
                slot
    view V at "/" in L:
        text "hi"
`)
	if len(g.Pages) != 1 {
		t.Fatalf("expected one page, got %d", len(g.Pages))
	}
}

// The one splice path that still lands a foreign tree under binders: a layout
// whose slot sits inside a `use`, making the routed view that component's
// children. It needs no new machinery — the view becomes ordinary `use` children,
// so the capture-avoiding expansion in internal/ir renames the callee's binders
// apart (`r` → `r$c1`) and the view's `{r}` keeps meaning the state cell it was
// written against.
func TestALayoutSlotInsideAUseIsCaptureAvoiding(t *testing.T) {
	g := mustCompile(t, `
app A:
    entity Row:
        label: text
    state r: text = "STATE-R" @client
    component Panel():
        box:
            for r in Row:
                text "PANEL-ROW={r.label}"
            slot
    layout L:
        box:
            use Panel():
                slot
    view V at "/" in L:
        text "VIEW={r}"
`)
	if len(g.Components) != 1 {
		t.Fatalf("expected one expanded component, got %d", len(g.Components))
	}
	var loopVars, refs []string
	var walk func([]ir.Node)
	walk = func(ns []ir.Node) {
		for _, n := range ns {
			if n.Var != "" {
				loopVars = append(loopVars, n.Var)
			}
			for _, segs := range n.SegLists() {
				for _, sg := range segs {
					collectRefs(sg.Expr, &refs)
				}
			}
			walk(n.Children)
		}
	}
	walk(g.Components[0].View)

	for _, v := range loopVars {
		if v == "r" {
			t.Fatalf("the component's own loop binder is still %q, so it shadows the spliced view's %q", v, "r")
		}
		if !strings.Contains(v, "$c") {
			t.Fatalf("expected the loop binder renamed apart, got %q", v)
		}
	}
	if !contains(refs, "r") {
		t.Fatalf("the routed view's reference to the state cell %q did not survive the splice; refs were %v", "r", refs)
	}
}

func collectRefs(x *ir.Expr, out *[]string) {
	if x == nil {
		return
	}
	if x.Name != "" {
		*out = append(*out, x.Name)
	}
	collectRefs(x.Obj, out)
	collectRefs(x.Key, out)
	collectRefs(x.Where, out)
	collectRefs(x.L, out)
	collectRefs(x.R, out)
	collectRefs(x.X, out)
	for _, a := range x.Args {
		collectRefs(a, out)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
