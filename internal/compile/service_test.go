package compile

import (
	"strings"
	"testing"
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
