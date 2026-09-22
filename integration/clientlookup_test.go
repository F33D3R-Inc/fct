package integration

import (
	"strings"
	"testing"
)

// A client-placed action may read a row by id — `editBody = Post(id).body` —
// which is what lets an edit form be pre-filled from a `@client` cell with no
// round-trip. For that to work in the browser, the rows the lookup reads must
// be on the client: this boots the shipped client over a real page, clicks
// the "load" button, and reads the bound control back.
const prefillApp = `app Prefill:
    entity Post:
        id: int
        title: text
        body: text
    state editTitle: text = "" @client
    state editBody: text = "" @client
    action seed():
        add Post { title: "First", body: "the body" }
    action startEdit(id: int):
        editTitle = Post(id).title
        editBody = Post(id).body
    view Home at "/":
        for p in Post by id:
            button "Load {p.title}" -> startEdit(p.id)
        input bind editTitle
        textarea bind editBody
`

func TestClientActionReadsARowByIdInTheBrowser(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, prefillApp)
	if code, body := a.action("seed"); code != 200 {
		t.Fatalf("seed: %d %s", code, body)
	}
	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	_, after := runClient(t, page, []driveStep{{Sel: `[data-fa-action="startEdit"]`, Do: "click"}})
	title := controlsByBind(after.Controls, "editTitle")
	body := controlsByBind(after.Controls, "editBody")
	if len(title) != 1 || title[0].Value != "First" {
		t.Errorf("after the click the title field holds %+v, want \"First\"", title)
	}
	if len(body) != 1 || !strings.Contains(body[0].Value, "the body") {
		t.Errorf("after the click the body field holds %+v, want \"the body\"", body)
	}
}
