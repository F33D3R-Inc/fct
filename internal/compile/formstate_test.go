package compile

import "testing"

// pending(action) / failed(action) are reactive client values; a region or binding
// that reads them must depend on the synthetic "@act:<action>" key, so the dispatch
// loop refreshes exactly the spinner/error UI when the action's status changes.
func TestActionStateDeps(t *testing.T) {
	g := mustCompile(t, `app F:
    state draft: text = "" @client
    entity Post:
        id: int
        body: text
    action post(body: text):
        check body != "" "say something"
        add Post { body: body }
    view Home at "/":
        box:
            input bind draft placeholder "what's happening?"
            button "post" -> post(draft)
            if pending(post):
                text "Posting..."
            text "{failed(post)}"
`)
	deps := g.DepGraph["@act:post"]
	if len(deps) < 2 {
		t.Errorf("pending(post)+failed(post) should feed at least the if-region and the failed() binding; got %v", deps)
	}
}
