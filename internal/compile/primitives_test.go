package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// The view primitives f33d3r.com needs: icon nav, count/status badges, and a
// tabbed feed (Following/Trending/…) whose selection is local @client state.
const primitivesApp = `app P:
    state feedTab: text = "following" @client
    entity Tweet:
        id: int
        body: text
    view Home at "/":
        box:
            icon "home"
            badge "{count(Tweet)}"
            tabs bind feedTab:
                tab "Following" -> "following":
                    for t in Tweet:
                        text "{t.body}"
                tab "Trending" -> "trending":
                    text "trending"
`

func kindCounts(nodes []ir.Node) map[string]int {
	m := map[string]int{}
	var walk func([]ir.Node)
	walk = func(ns []ir.Node) {
		for _, n := range ns {
			m[n.Kind]++
			walk(n.Children)
		}
	}
	walk(nodes)
	return m
}

func TestViewPrimitives(t *testing.T) {
	g := mustCompile(t, primitivesApp)
	k := kindCounts(g.View)
	for _, want := range []string{"icon", "badge", "tabs", "tab"} {
		if k[want] == 0 {
			t.Errorf("view should contain a %q node, kinds=%v", want, k)
		}
	}
	if k["tab"] != 2 {
		t.Errorf("expected 2 tab nodes, got %d", k["tab"])
	}
	// Switching tabs is a client state change, so the tabs region depends on its
	// bound cell; and because a tab body reads Tweet, it also refreshes on Tweet.
	if len(g.DepGraph["feedTab"]) == 0 {
		t.Errorf("tabs region should depend on its bound cell feedTab, deps=%v", g.DepGraph)
	}
	if len(g.DepGraph["Tweet"]) == 0 {
		t.Errorf("tabs region should depend on Tweet (read in a tab body), deps=%v", g.DepGraph)
	}
}

func TestTabsRequireClientState(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"authoritative bind rejected",
			`app B:
    state feedTab: text = "a"
    view H at "/":
        tabs bind feedTab:
            tab "A" -> "a":
                text "a"
`,
			"requires a @client state",
		},
		{
			"unknown bind rejected",
			`app B:
    view H at "/":
        tabs bind nope:
            tab "A" -> "a":
                text "a"
`,
			"unknown state",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := String(c.src); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
