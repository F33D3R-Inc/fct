package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

const serviceApp = `app A:
    entity Note:
        id: int
        body: text
    service Zodacare at "http://zodacare:8090":
        report(id: int, body: text)
    action post(body: text):
        add Note { body: body }
        call Zodacare.report(1, body)
    view Home at "/":
        box:
            text "hi"
`

func TestServiceCallCompiles(t *testing.T) {
	ir, err := String(serviceApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(ir.Services) != 1 || ir.Services[0].URL != "http://zodacare:8090" {
		t.Fatalf("service missing from IR: %+v", ir.Services)
	}
	if len(ir.Services[0].Ops) != 1 || ir.Services[0].Ops[0].Name != "report" {
		t.Fatalf("service op missing: %+v", ir.Services[0].Ops)
	}
	// A service call is an effect, so the action must run on the authority.
	for _, a := range ir.Actions {
		if a.Name == "post" && a.Placement != "server" {
			t.Errorf("action calling a service must be server-placed, got %q", a.Placement)
		}
	}
}

func TestServiceCallErrors(t *testing.T) {
	cases := []struct {
		name, call, want string
	}{
		{"unknown service", "call Nope.report(1, body)", "unknown service"},
		{"unknown op", "call Zodacare.nope(1, body)", "no operation"},
		{"arg count", "call Zodacare.report(1)", "expects 2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(serviceApp, "call Zodacare.report(1, body)", c.call, 1)
			_, err := String(src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// Request→response: a service op may declare a typed return, and `let x = call …`
// binds it into a local the rest of the action body can use. The op takes a list
// param (`rank(posts: [int])`) and returns a list — the keystone feed shape.
const reqRespApp = `app A:
    service Brain at "http://brain:8080":
        answer(q: int) -> int
        rank(viewer: text, posts: [int]) -> [int]
        report(id: int)
    state result: int = 0
    action ask(q: int):
        let a = call Brain.answer(q)
        result = a
    view Home at "/":
        box:
            text "{result}"
`

func TestServiceRequestResponse(t *testing.T) {
	g, err := String(reqRespApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// The op's typed return survives into the IR.
	var rank, answer ir.ServiceOp
	for _, op := range g.Services[0].Ops {
		switch op.Name {
		case "rank":
			rank = op
		case "answer":
			answer = op
		}
	}
	if answer.Ret != "int" || answer.RetList {
		t.Errorf("answer return = %q list=%v, want int scalar", answer.Ret, answer.RetList)
	}
	if rank.Ret != "int" || !rank.RetList {
		t.Errorf("rank return = %q list=%v, want [int]", rank.Ret, rank.RetList)
	}

	// `ask` binds the result, then assigns it — it must be server-placed (egress),
	// and its call stmt carries the bind + return type for the runtime to decode.
	var ask ir.Action
	for _, a := range g.Actions {
		if a.Name == "ask" {
			ask = a
		}
	}
	if ask.Placement != "server" {
		t.Errorf("an action that calls a service must be server-placed, got %q", ask.Placement)
	}
	var call ir.Stmt
	for _, s := range ask.Body {
		if s.Op == "call" {
			call = s
		}
	}
	if call.Bind != "a" || call.Ret != "int" || call.RetList {
		t.Errorf("bound call stmt = bind:%q ret:%q list:%v, want bind:a ret:int scalar", call.Bind, call.Ret, call.RetList)
	}
}

func TestServiceBindErrors(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"bind a no-return op", "let x = call Brain.report(1)", "returns nothing"},
		{"bind unknown service", "let x = call Nope.answer(1)", "unknown service"},
		{"let without call", "let x = result", "service call"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(reqRespApp, "let a = call Brain.answer(q)\n        result = a", c.body, 1)
			_, err := String(src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
