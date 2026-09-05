package compile

import (
	"strings"
	"testing"
)

// controlApp is one page with a cell of every type a control could be pointed at,
// so a control can be dropped into it and the cell rule asked about directly.
const controlApp = `app C:
    enum Plan: free, pro
    state name: text @client
    state n: int @client
    state ok: bool @client
    state plan: Plan @client
    state tags: [text] @client
    state owned: text
    view Home at "/":
        box:
            %s
`

func controlSrc(node string) string {
	return strings.Replace(controlApp, "%s", strings.ReplaceAll(node, "\n", "\n            "), 1)
}

// A control's cell type is checked where the control is written.
//
// This is the whole reason a control can be trusted with a cell: the compiler
// knows what the control can produce and what the cell can hold, so a checkbox
// over a text cell is a compile error rather than a cell that quietly holds
// `true` in one place and `"true"` in another.
func TestAControlChecksTheTypeOfTheCellItBinds(t *testing.T) {
	for _, c := range []struct{ name, node, want string }{
		{"checkbox on text", `checkbox bind name`,
			"checkbox binds \"name\", which is text, but a checkbox toggles a bool cell"},
		{"toggle on int", `toggle bind n`,
			"toggle binds \"n\", which is int, but a toggle flips a bool cell"},
		{"textarea on bool", `textarea bind ok`,
			"textarea binds \"ok\", which is bool, but a textarea edits a text cell"},
		{"radio on int", "radio bind n:\n    option \"a\" -> \"a\"",
			"radio binds \"n\", which is int, but a radio group stores one of its option values"},
		{"a control over a list cell", `checkbox bind tags`,
			"checkbox binds \"tags\", which is [text]; a control writes one value, not a list"},
		{"a control over an authoritative cell", `checkbox bind owned`,
			"checkbox binds \"owned\", which is authoritative; two-way input requires a @client state"},
		{"a control over no cell at all", `toggle bind nope`,
			`toggle binds unknown state "nope"`},
		{"a radio with nothing to choose from", `radio bind name`,
			`radio on "name" needs options`},
		{"password on int", `password bind n`,
			"password binds \"n\", which is int, but a password box edits a text cell"},
		{"newpassword on bool", `newpassword bind ok`,
			"newpassword binds \"ok\", which is bool, but a password box edits a text cell"},
		{"a password over an authoritative cell", `password bind owned`,
			"password binds \"owned\", which is authoritative; two-way input requires a @client state"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(controlSrc(c.node))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got: %v", c.want, err)
			}
		})
	}
}

// The controls that are correctly bound compile, lower to the kind the renderers
// switch on, and carry the cell they write.
func TestEveryControlLowersToItsBoundCell(t *testing.T) {
	g, err := String(controlSrc(
		"textarea bind name placeholder \"why?\"\n" +
			"checkbox bind ok label \"Email me\"\n" +
			"toggle bind ok label \"Dark\"\n" +
			"password bind name placeholder \"Password\"\n" +
			"newpassword bind name placeholder \"Choose one\"\n" +
			"radio bind plan"))
	if err != nil {
		t.Fatalf("the controls should compile, got: %v", err)
	}

	kinds := map[string]string{} // kind -> bound cell
	var opts int

	for _, n := range g.View[0].Children {
		kinds[n.Kind+"/"+n.Value] = n.Bind
		if n.Kind == "radio" {
			opts = len(n.Options)
		}
		if n.ID == "" {
			t.Errorf("%s has no binding id, so nothing can refresh it when its cell changes", n.Kind)
		}
	}

	for kind, want := range map[string]string{
		"textarea/":       "name",
		"checkbox/":       "ok",
		"checkbox/switch": "ok", // `toggle` is a checkbox variant, not a node kind
		"radio/":          "plan",
		// A password box is an `input` carrying the autocomplete token the
		// renderers write verbatim — the variant IS the token, so there is no
		// keyword-to-attribute mapping for the two sides to disagree about.
		"input/current-password": "name",
		"input/new-password":     "name",
	} {
		if got := kinds[kind]; got != want {
			t.Errorf("no %q node bound to %q (got %q); nodes: %v", kind, want, got, kinds)
		}
	}

	// A radio over an enum cell takes its choices from the enum, like a select.
	if opts != 2 {
		t.Errorf("radio over an enum cell has %d options, want the enum's 2", opts)
	}

	// Every control is reachable from its cell in the dependency graph, which is
	// what makes it re-render when anything else writes that cell.
	for _, cell := range []string{"name", "ok", "plan"} {
		if len(g.DepGraph[cell]) == 0 {
			t.Errorf("nothing depends on %q — a control bound to it will not refresh", cell)
		}
	}
}

// `route` — the path being rendered — is readable in a view and nowhere else.
//
// Not because reading it elsewhere is dangerous, but because elsewhere it has no
// answer: an action the authority runs, a derive it folds into a policy gate, a
// check it evaluates before a write — none of them is rendering a page. Allowing
// it there would return an empty string, which is the shape of bug that costs a
// week to find.
func TestRouteIsARenderTimeName(t *testing.T) {
	viewSrc := `app R:
    view Home at "/":
        box:
            if route == "/":
                text "home, at {route}"
`
	if _, err := String(viewSrc); err != nil {
		t.Fatalf("`route` should be readable in a view, got: %v", err)
	}

	for _, c := range []struct{ name, src, want string }{
		{"in a derive", `app R:
    derive here: text = route
    view Home at "/":
        text "{here}"
`, `unknown reference "route"`},
		{"in an action", `app R:
    entity E:
        id: int
        n: text
    action go:
        add E { n: route }
    view Home at "/":
        button "x" -> go()
`, `unknown reference "route"`},
		{"as a state cell", `app R:
    state route: text @client
    view Home at "/":
        text "{route}"
`, "collides with the built-in `route`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got: %v", c.want, err)
			}
		})
	}
}
