package integration

import (
	"strings"
	"testing"
)

// An aggregate a component evaluates for each row it is used for — through an
// inlined zero-argument derive, inside a class interpolation and an `if` — is
// the same question on the client as on the server: the browser's render of a
// thread keeps each bubble's mine/theirs class and its "Seen" receipt.
const componentAggApp = `app Bubbles:
    entity Person:
        id: int
        name: text
        read_at: int
    entity Msg:
        id: int
        sender: int
        at: int
        body: text
    derive me: int = max(p.id in Person where p.name == "ada")
    action seed():
        add Person { name: "ada", read_at: 0 }
        add Person { name: "grace", read_at: 2 }
        add Msg { sender: 1, at: 1, body: "one" }
        add Msg { sender: 2, at: 2, body: "two" }
        add Msg { sender: 1, at: 3, body: "three" }
    component Bubble(id: int):
        for m in Msg where m.id == id limit 1:
            row class "x-mine-{m.sender == me}":
                text "{m.body}"
                if m.sender == me && exists(o in Person where o.id != me && o.read_at >= m.at):
                    text "Seen"
    view Home at "/":
        for m in Msg by id:
            use Bubble(m.id)
`

func TestComponentAggregatesPerRowOnTheClient(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, componentAggApp)
	if code, body := a.action("seed"); code != 200 {
		t.Fatalf("seed: %d %s", code, body)
	}
	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	m := mountedMarkup(page)
	if strings.Count(m, "x-mine-true") != 2 || strings.Count(m, "x-mine-false") != 1 || strings.Count(serverText(page), "Seen") != 1 {
		t.Fatalf("server render: %s", m)
	}
	run, _ := runClient(t, page, nil)
	mine := 0
	for _, at := range run.Attrs {
		if at.Name == "class" && strings.Contains(at.Value, "x-mine-true") {
			mine++
		}
	}
	if mine != 2 || strings.Count(run.Text, "Seen") != 1 {
		t.Errorf("client render: %d mine bubbles (want 2), text %q (want one Seen): %v", mine, run.Text, run.Attrs)
	}
}
