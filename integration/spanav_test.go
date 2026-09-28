package integration

import (
	"strings"
	"testing"
)

// Two pages, each with what only it uses: "/" links to "/other", whose
// component and action appear nowhere on "/". A page ships only the IR it
// can use (runtime/pageir.go), so the client must take the component and the
// action from the page it navigates to, not keep the first page's.
const spaNavApp = `app Nav:
    state count: int = 0
    action bump():
        count = count + 1
    component Counter:
        text "counted {count}"
        button "bump" -> bump()
    view Home at "/":
        text "home"
        link "other" -> "/other"
    view Other at "/other":
        text "other page"
        use Counter
`

func TestSPANavigationBringsTheNextPagesComponentsAndActions(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, spaNavApp)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	if strings.Contains(page, `"Counter"`) || strings.Contains(page, `"bump"`) {
		t.Fatalf(`"/" ships /other's component or action; the premise is gone`)
	}

	before, after := runClientAgainst(t, a, page, []driveStep{
		{Sel: `a.fa-link`, Do: "click"},
		{Sel: `button[data-fa-action="bump"]`, Do: "click"},
	})
	if !strings.Contains(before.Text, "home") {
		t.Fatalf("first paint: %q", before.Text)
	}
	if !strings.Contains(after.Text, "otherpage") || !strings.Contains(after.Text, "counted1") {
		t.Fatalf("after navigating and clicking, the page reads %q; want the other page's "+
			"component rendered and its action applied (counted1)", after.Text)
	}
}
