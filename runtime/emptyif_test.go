package runtime

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"facet/internal/compile"
)

// An `if` is control flow, not a box.
//
// internal/ir/build.go's `case ast.If` lowers it to a node that holds children
// and no element, and both renderers used to mint a `<div>` for it anyway —
// written before the condition was tested and closed whatever the answer was. A
// four-branch status component therefore emitted four siblings into its parent,
// three of them empty, and a grid handed every one of them a track: the branch
// that actually rendered came out in whichever column its ordinal fell in
// instead of the one the author laid out.
//
// StatusCell below is that component. Exactly one of its four branches is true,
// so a grid of two columns must receive exactly one child from it.
const emptyIfApp = `app Grid:
    state level: int = 3 @client
    component StatusCell(n: int):
        if n == 1:
            text "one"
        if n == 2:
            text "two"
        if n == 3:
            text "three"
        if n == 4:
            text "four"
    view Home at "/":
        row class "x-grid":
            use StatusCell(level)
            text "actions"
        if level > 99:
            text "never"
`

func TestAnEmptyIfEmitsNoElement(t *testing.T) {
	g, err := compile.String(emptyIfApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")

	if n := strings.Count(html, "<div></div>"); n != 0 {
		t.Errorf("the render emitted %d empty `if` wrappers; an `if` whose branch does not fire must emit nothing at all", n)
	}
	// The grid row's own children: the component's one live branch and the label.
	// Five would mean the three dead branches are still there taking tracks.
	row := regexp.MustCompile(`(?s)<div class="fa-row x-grid">(.*?)<span class="fa-text">actions</span>`).FindStringSubmatch(html)
	if row == nil {
		t.Fatalf("the grid row is not in the page: %s", html)
	}
	if got := strings.Count(row[1], "<div"); got != 1 {
		t.Errorf("the `use` put %d elements in the grid before the label, want 1 (the component's own wrapper); "+
			"a dead `if` branch must not become a sibling in the parent's layout.\ngot: %s", got, row[1])
	}
	if !strings.Contains(row[1], "three") {
		t.Errorf("the live branch did not render: %s", row[1])
	}

	// A top-level `if` is different: it is a region the client re-fills, so its
	// element has to exist even while the branch is false. It must not occupy a
	// slot in the parent's layout while it is empty.
	region := regexp.MustCompile(`<div data-fa-region="(f\d+)"([^>]*)>`).FindStringSubmatch(html)
	if region == nil {
		t.Fatalf("the top-level `if` region is gone; the client has nothing to re-fill: %s", html)
	}
	if !strings.Contains(region[2], `style="display:contents"`) {
		t.Errorf("the `if` region wrapper is %q — without display:contents an empty region still takes a grid track", region[0])
	}
}

// The two renderers, pinned at the source. The server writes markup and the
// client builds DOM; nothing but both files being written to the same rule keeps
// an `if` from generating a box on one side and not the other — and a page whose
// halves disagree reflows the moment the client takes over.
// runtime/link_test.go, classmerge_test.go and attrtext_test.go pin theirs the
// same way; integration/control_test.go boots the real client over a real render.
func TestBothRenderersGiveAnIfNoBoxOfItsOwn(t *testing.T) {
	client, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	server, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("reading the server renderer: %v", err)
	}
	for _, c := range []struct{ inServer, inJS, why string }{
		{
			inServer: "if n.ID == \"\" {\n\t\t\tif show {\n\t\t\t\trd.children(b, n.Children, scope, path)\n\t\t\t}\n\t\t\tbreak\n\t\t}",
			inJS:     "if (!node.id) {\n          const f = document.createDocumentFragment();",
			why:      "an `if` that is not a region has no element on either side",
		},
		{
			inServer: "fmt.Fprintf(b, `<div%s style=\"display:contents\">`, regionAttrs(\"\", n.ID, n))",
			inJS:     `d.setAttribute("style", "display:contents");`,
			why:      "a top-level `if` keeps its re-fill anchor, and it must generate no box while empty",
		},
	} {
		if !strings.Contains(string(server), c.inServer) {
			t.Errorf("runtime/server.go no longer renders `if` as %q — %s", c.inServer, c.why)
		}
		if !strings.Contains(string(client), c.inJS) {
			t.Errorf("runtime/assets/facet.js no longer renders `if` as %q — %s", c.inJS, c.why)
		}
	}
}
