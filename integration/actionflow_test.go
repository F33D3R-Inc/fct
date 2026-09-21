package integration

import (
	"strings"
	"testing"
)

// The browser runs a client-placed action's body itself (assets/facet.js's
// execBody), and that body can now branch, loop over rows, and bind locals —
// the same statements the authority interprets in runtime/server.go. This
// drives the shipped client under node against a page the server sent: a
// click runs the action in the browser with no network, and what the page
// says afterwards is what the interpreter computed.
//
// It also pins the one behaviour that changed for every client action: a
// `check` used to be skipped in the browser outright (runClient only ever ran
// assigns). Now it refuses the action, leaves every cell as it was, and
// surfaces its message the way the authority's refusal would.
const flowClientApp = `app FlowClient:
    entity Item:
        id: int
        n: int
    state picked: int = 0 @client
    state label: text = "none" @client
    action pick(min: int):
        check min >= 0 "min cannot be negative"
        let floor = min * 2
        for i in Item where i.n >= floor by n desc limit 1:
            if i.n > 10:
                picked = i.id
                label = "big"
            else:
                picked = i.id
                label = "small"
    action bad():
        check picked < 0 "picked cannot be negative"
        label = "never"
    action seed():
        add Item { n: 3 }
        add Item { n: 12 }
        add Item { n: 7 }
    view Home at "/":
        text "picked={picked} label={label}"
        button "pick" -> pick(2)
        button "bad" -> bad()
`

func TestClientActionBranchesLoopsAndBindsInTheBrowser(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, flowClientApp)
	if code, body := a.action("seed"); code != 200 {
		t.Fatalf("seed: %d %s", code, body)
	}
	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	if got := serverText(page); !strings.Contains(got, "picked=0label=none") {
		t.Fatalf("first paint should show the initial cells, got %q", got)
	}

	t.Run("for/by/limit picks the row and if/else labels it", func(t *testing.T) {
		// min=2 → floor=4 → rows with n>=4 are 12 and 7; by n desc limit 1 → 12,
		// which is > 10, so the else branch is not taken.
		_, after := runClient(t, page, []driveStep{{Sel: `[data-fa-action="pick"]`, Do: "click"}})
		if !strings.Contains(after.Text, "picked=2label=big") {
			t.Errorf("after the click the page says %q, want picked=2label=big", after.Text)
		}
	})

	t.Run("a failed check applies nothing and says why", func(t *testing.T) {
		_, after := runClient(t, page, []driveStep{{Sel: `[data-fa-action="bad"]`, Do: "click"}})
		if !strings.Contains(after.Text, "picked=0label=none") {
			t.Errorf("a refused action must leave every cell as it was, page says %q", after.Text)
		}
		if !strings.Contains(after.Text, "pickedcannotbenegative") {
			t.Errorf("the check's message should be on the page, got %q", after.Text)
		}
	})
}
