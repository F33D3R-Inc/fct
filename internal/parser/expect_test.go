package parser

import (
	"strings"
	"testing"
)

// `expect` states what a fragment needs its host to provide, in the host's
// own declaration syntax: an entity's fields, an action's signature, a cell's
// type, a route. Only the parts the host alone decides — an action's body, a
// cell's default — are refused.
func TestParseExpectForms(t *testing.T) {
	app, err := Parse(`app Frag:
    expect entity Stream:
        id: int
        title: text
        live: bool
        owner: text @unique
    expect action unsubscribe(stream: int)
    expect action tip(stream: int, cents: money, note: text?) -> bool
    expect state draft: text
    expect view at "/profile/:handle"
    expect event notification: NotificationDTO
    expect policy member
    expect policy owns(id: int)
    component StreamCard(s: Stream):
        text "{s.title}"
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(app.Expects) != 8 {
		t.Fatalf("Expects = %d, want 8", len(app.Expects))
	}
	if p := app.Expects[6].Policy; p == nil || p.Name != "member" || len(p.Params) != 0 {
		t.Errorf("expect policy parsed as %+v", p)
	}
	if p := app.Expects[7].Policy; p == nil || p.Name != "owns" || len(p.Params) != 1 || p.Params[0].Type != "int" {
		t.Errorf("expect policy with params parsed as %+v", p)
	}
	if ev := app.Expects[5].Event; ev == nil || ev.Name != "notification" || ev.Type != "NotificationDTO" {
		t.Errorf("expect event parsed as %+v", ev)
	}
	e := app.Expects[0].Entity
	if e == nil || e.Name != "Stream" || len(e.Fields) != 4 || e.Fields[2].Name != "live" || e.Fields[2].Type != "bool" || !e.Fields[3].Unique {
		t.Errorf("expect entity parsed as %+v", e)
	}
	a := app.Expects[1].Action
	if a == nil || a.Name != "unsubscribe" || len(a.Params) != 1 || a.Params[0].Type != "int" || len(a.Body) != 0 {
		t.Errorf("expect action parsed as %+v", a)
	}
	a2 := app.Expects[2].Action
	if a2 == nil || a2.Ret != "bool" || len(a2.Params) != 3 || !a2.Params[2].Optional {
		t.Errorf("expect action with return parsed as %+v", a2)
	}
	s := app.Expects[3].State
	if s == nil || s.Name != "draft" || s.Type != "text" {
		t.Errorf("expect state parsed as %+v", s)
	}
	if r := app.Expects[4].Route; r != "/profile/:handle" {
		t.Errorf("expect view at parsed as %q", r)
	}
	if len(app.Entities) != 0 || len(app.Actions) != 0 || len(app.States) != 0 {
		t.Errorf("an expectation is not a declaration: entities=%d actions=%d states=%d", len(app.Entities), len(app.Actions), len(app.States))
	}
}

func TestParseExpectRefusals(t *testing.T) {
	cases := []struct{ src, want string }{
		{"app A:\n    expect action pay(cents: money):\n        add X { a: 1 }\n", "no body"},
		{"app A:\n    expect state draft: text = \"\"\n", "no default"},
		{"app A:\n    expect entity Stream:\n", "has no fields"},
		{"app A:\n    expect view at profile\n", "quoted route"},
		{"app A:\n    expect derive x\n", "expect takes"},
		{"app A:\n    expect event notification\n", "payload type"},
		{"app A:\n    expect policy member:\n        actor != \"guest\"\n", "no predicate"},
		{"app A:\n    expect event notification: dto\n", "declared wire type"},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want it to mention %q", c.src, err, c.want)
		}
	}
}
