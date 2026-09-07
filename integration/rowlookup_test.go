package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// A bare `Person(id)` is a row, handed to an entity-typed component. The row is
// recorded for the client like every other lookup — so a field this actor may
// not read (`@requires`) must be stripped before it is recorded, or the gate the
// API and the SSE stream both enforce would leak through the page bootstrap.
func TestLookedUpRowIsGatedBeforeItReachesTheClient(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, `app G:
    auth
    policy admins:
        role == "admin"
    entity Person:
        id: int
        name: text
        salary: money @requires(admins)
    component Card(p: Person):
        box:
            text "{p.name}"
    action hire(name: text, salary: money):
        add Person { name: name, salary: salary }
    view Home at "/":
        box:
            use Card(Person(1))
`)
	// The first account is the admin; it hires, then a fresh guest loads the page.
	if code, body := a.action("signup", "boss", "pw12345678"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("hire", "Grace", 424242); code != 200 {
		t.Fatalf("hire: %d %s", code, body)
	}
	a.newSession()
	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	if !strings.Contains(serverText(page), "Grace") {
		t.Fatalf("the looked-up row did not render: %q", serverText(page))
	}
	state := pageState(t, page)
	raw, _ := json.Marshal(state)
	if strings.Contains(string(raw), "424242") {
		t.Errorf("the gated salary reached a guest's page bootstrap:\n%s", raw)
	}
	run, _ := runClient(t, page, nil)
	if !strings.Contains(run.Text, "Grace") {
		t.Errorf("the client did not render the looked-up row: %q", run.Text)
	}
}
