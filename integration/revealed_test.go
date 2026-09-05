package integration

import (
	"strings"
	"testing"
)

// A branch the authority never rendered, revealed by the actor.
//
// `show` is a `@client` cell, so the first render has it false and paints
// nothing inside the branch — no rows for the region, no value for either
// aggregate. Ticking the box is a state change the client resolves by itself,
// and now it has to render a subtree the render never saw.
//
// The region half already worked: `rowsFor` asks the authority for rows it does
// not have, and its comment names this exact case ("an inactive tab the
// authority did not render"). The aggregate half did not. It scanned the
// collection it had been shipped, found none — clientColls deliberately does not
// ship an aggregate's source, because the render is supposed to ship its answer
// — and returned the length of an empty array. So a revealed `count(...)`
// rendered 0. Not blank, not an error: a confident, wrong number that never
// corrected itself.
//
// Both halves now ask the same authority the same way — and the aggregates have
// to do it themselves. A region in the same branch asks for the whole page, and
// the answer carries every aggregate that render resolved, so a branch holding
// both was already rescued by the region's request. This branch holds no region,
// which is what makes it the case that was actually broken.
const revealedApp = `app Revealed:
    entity Line:
        id: int
        qty: int
        unitPrice: money
    state show: bool = false @client
    action add(qty: int, unitPrice: money):
        add Line { qty: qty, unitPrice: unitPrice }
    view Home at "/":
        box:
            checkbox bind show label "show"
            if show:
                text "count {count(Line)}"
                text "units {sum(l.qty in Line)}"
                text "total {sum(l.qty * l.unitPrice in Line)}"
`

// The same branch with a region in it. Worth keeping separate: the region's own
// request answers the aggregates as a side effect, so this passes even with the
// aggregate half missing — which is precisely why it cannot stand in for the
// test above.
const revealedWithRegionApp = `app RevealedRegion:
    entity Line:
        id: int
        qty: int
        unitPrice: money
    state show: bool = false @client
    action add(qty: int, unitPrice: money):
        add Line { qty: qty, unitPrice: unitPrice }
    view Home at "/":
        box:
            checkbox bind show label "show"
            if show:
                text "count {count(Line)}"
                for l in Line by id:
                    text "row:{l.qty}"
`

func TestARevealedBranchGetsItsAggregatesFromTheAuthority(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, revealedApp)

	// count 2, units 5, total 2*300 + 3*150 = 1050.
	for _, row := range [][]any{{2, 300}, {3, 150}} {
		if code, body := a.action("add", row...); code != 200 {
			t.Fatalf("seeding a line: %d %s", code, body)
		}
	}

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	// The premise: the authority rendered none of this.
	for _, absent := range []string{"count 2", "units 5", "1050"} {
		if strings.Contains(serverText(page), absent) {
			t.Fatalf("the branch was supposed to be hidden, but the page already "+
				"shows %q — this test would prove nothing:\n%s", absent, serverText(page))
		}
	}

	before, after := runClientAgainst(t, a, page, []driveStep{
		{Sel: `[data-fa-input="show"]`},
	})
	if strings.Contains(before.Text, "count") {
		t.Fatalf("the branch was visible before the click: %q", before.Text)
	}

	// Every value in the revealed branch, from the authority: the row count, a
	// reduction over a column, and a reduction over an expression.
	for _, want := range []string{"count2", "units5", "total1050"} {
		if !strings.Contains(after.Text, want) {
			t.Errorf("after revealing the branch, %q is missing — the client "+
				"answered it from an empty collection instead of asking the "+
				"authority. Got: %q", want, after.Text)
		}
	}
}

// A revealed branch holding both a region and an aggregate resolves as one.
//
// The ask is once per state, not once per missing value: this runs inside a
// render, and a render that re-asked every time it found a value it did not have
// would issue one request per aggregate per paint, and again for every paint the
// answer triggered — a loop, not a fetch. Here the region and the count are two
// missing values in one paint, and one answer carries both.
func TestARevealedRegionAndItsAggregateResolveTogether(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, revealedWithRegionApp)

	for _, row := range [][]any{{2, 300}, {3, 150}} {
		if code, body := a.action("add", row...); code != 200 {
			t.Fatalf("seeding a line: %d %s", code, body)
		}
	}

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	_, after := runClientAgainst(t, a, page, []driveStep{
		{Sel: `[data-fa-input="show"]`},
	})

	for _, want := range []string{"count2", "row:2", "row:3"} {
		if !strings.Contains(after.Text, want) {
			t.Errorf("after revealing the branch, %q is missing. Got: %q", want, after.Text)
		}
	}
}
