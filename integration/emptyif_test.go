package integration

import (
	"regexp"
	"strings"
	"testing"
)

// An `if` is control flow, not a box — end to end, through both renderers.
//
// internal/ir/build.go lowers `if` to a node that holds children and no element.
// Both renderers used to mint a `<div>` for it regardless: the server wrote the
// opening tag before it evaluated the condition and closed it either way, and
// the client did the same in DOM. So this four-branch component put four
// siblings into the row, three of them empty — in a grid, four tracks — and the
// one branch that rendered landed in whichever column its ordinal fell in.
//
// The client half cannot be checked from Go alone, and it is the half that
// decides what the page looks like a few milliseconds after first paint. So the
// shipped facet.js is booted over the page this server actually sent, and the
// two structures are compared element for element.
const emptyIfGridApp = `app Grid:
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

var (
	openTagRE = regexp.MustCompile(`<[a-z]+(?:\s[^>]*)?>`)
	tagRE     = regexp.MustCompile(`<[^>]*>`)
)

// serverNodeCount counts what the shim's countNodes counts over the same markup:
// every element the server opened, plus every run of text between tags. Comparing
// it with the client's count is the whole question — an element one side creates
// and the other does not is exactly the bug.
func serverNodeCount(markup string) int {
	n := len(openTagRE.FindAllString(markup, -1))
	for _, chunk := range tagRE.Split(markup, -1) {
		if strings.TrimSpace(chunk) != "" {
			n++
		}
	}
	return n
}

func TestAnEmptyIfCreatesNoElementOnEitherSide(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, emptyIfGridApp)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	markup := mountedMarkup(page)
	if markup == "" {
		t.Fatal("the server rendered no mounted markup")
	}

	if n := strings.Count(markup, "<div></div>"); n != 0 {
		t.Errorf("the server wrote %d empty `if` wrappers into the page:\n%s", n, markup)
	}
	// The row holds the component and the label, and nothing else: the component
	// contributes its own `use` wrapper, not one element per branch.
	row := regexp.MustCompile(`(?s)<div class="fa-row x-grid">(.*?)<span class="fa-text">actions</span>`).FindStringSubmatch(markup)
	if row == nil {
		t.Fatalf("the grid row is missing from the page:\n%s", markup)
	}
	if got := len(openTagRE.FindAllString(row[1], -1)); got != 2 {
		t.Errorf("the component put %d elements in the row before the label, want 2 (its own wrapper and the live branch's span); "+
			"a dead `if` branch must not become a sibling in the parent's layout:\n%s", got, row[1])
	}
	// The one `if` that keeps an element is the top-level region the client
	// re-fills — and while its branch is false it must generate no box.
	if !strings.Contains(markup, `<div data-fa-region="f0" style="display:contents"></div>`) {
		t.Errorf("the top-level `if` region is not an empty display:contents anchor:\n%s", markup)
	}

	// And the shipped client builds exactly that, not a structure of its own.
	client, _ := runClient(t, page, nil)
	if want := serverText(page); client.Text != want {
		t.Errorf("the client rendered %q where the server rendered %q", client.Text, want)
	}
	// +1: the client's count includes the mount point the markup sits inside.
	if want := serverNodeCount(markup) + 1; client.Nodes != want {
		t.Errorf("the client built %d nodes over a page of %d — the two renderers disagree about which nodes an `if` creates:\n%s",
			client.Nodes, want, markup)
	}
}
