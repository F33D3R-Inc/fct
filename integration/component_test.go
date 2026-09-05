package integration

import (
	"strings"
	"testing"
)

// The four things a reusable component could not be parameterized by, in one app:
// the cell a control writes to, the action a button calls, the children a wrapper
// wraps, and the route a link points at. Each was a compile error; a component
// library could not contain a field, a submit button, a card or a nav link.
//
// It is an integration test rather than a compiler one because the claim is not
// that it compiles — it is that the cell the caller named is the cell the rendered
// input actually writes to, the action the caller named is the one the button
// actually dispatches, and the children actually appear inside the wrapper. Those
// are properties of the HTML, and the client has to agree with it.
const parameterizedApp = `app Fields:
    state draft: text @client
    entity Note:
        body: text
    action save(b: text):
        add Note { body: b }
    component TextField(label: text, value: cell text):
        box:
            text "{label}"
            input bind value
    component SubmitButton(label: text, act: action, value: cell text):
        button "{label}" -> act(value)
    component Card(title: text):
        box:
            text "{title}"
            slot
    component NavLink(label: text, href: text):
        link "{label}" -> "{href}"
    view Home at "/":
        use Card("Compose"):
            use TextField("Body", draft)
            use SubmitButton("Post", save, draft)
        use NavLink("elsewhere", "/other")
        use NavLink("off site", "https://example.com/x")
    view Other at "/other":
        text "the other page"
`

func TestAComponentIsParameterizedByCellActionChildrenAndRoute(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, parameterizedApp)

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	// The rendered page only — the IR the page also carries repeats every literal
	// the author wrote, including the destination that must not become a link.
	body := html[strings.Index(html, `<div id="fa-root"`):]
	body = body[:strings.Index(body, `<script type="application/json" id="fa-ir">`)]

	// The cell the call site named is the cell the control writes to.
	if !strings.Contains(body, `data-fa-input="draft"`) {
		t.Errorf("the field is not bound to the caller's cell `draft`:\n%s", body)
	}
	// The action the call site named is the one the button dispatches.
	if !strings.Contains(body, `data-fa-action="save"`) {
		t.Errorf("the submit button does not call the caller's action `save`:\n%s", body)
	}
	// The children the call site wrote are inside the wrapper, not discarded.
	card := body[strings.Index(body, "Compose"):]
	if i := strings.Index(card, "Body"); i < 0 {
		t.Errorf("the block passed to the wrapper did not render inside it:\n%s", body)
	}
	// A computed destination that names a route of this app is a link.
	if !strings.Contains(body, `href="/other"`) {
		t.Errorf("a parameterized destination naming a real route is not a link:\n%s", body)
	}
	// One that does not is not a link at all — the guarantee the compile-time
	// route check gave, kept at the point the value exists.
	if strings.Contains(body, "example.com") {
		t.Errorf("a parameterized destination that is not a route of this app became an href:\n%s", body)
	}

	// And the client renders the same page, character for character — and reaches
	// the same verdict about which destinations may be links. The two halves of
	// the contract are written twice, in two languages, so they are checked here
	// against the same page.
	nodes, clientText, hrefs := renderClientPage(t, html)
	if nodes <= 1 {
		t.Fatalf("the client rendered a blank page")
	}
	if want := serverText(html); clientText != want {
		sw, cw := firstDifference(want, clientText)
		t.Errorf("the client rendered different text than the server sent.\nserver: %s\nclient: %s", sw, cw)
	}
	if len(hrefs) != 1 || hrefs[0] != "/other" {
		t.Errorf("the client disagreed with the server about which computed destinations are links: %q", hrefs)
	}
}
