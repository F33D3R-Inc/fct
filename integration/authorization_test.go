package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// A field gated by a policy, and an action to write one.
//
// `salary` is readable only by an admin. The first account to sign up becomes
// the admin, so the second is an ordinary member — which is the actor these
// tests use to ask "what does a visitor actually receive?"
const gatedApp = `app Payroll:
    auth
    policy admins:
        role == "admin"
    entity Person:
        id: int
        name: text
        salary: money @requires(admins)
    action hire(name: text, salary: money):
        add Person { name: name, salary: salary }
    view Home at "/":
        box:
            text "people: {count(Person)}"
            for p in Person by id desc limit 20:
                text "{p.name}"
`

// A gated field must not reach an actor who may not read it — by ANY path.
//
// This is the shape the bug took: `gateForActor` is called from exactly one
// handler, the JSON API's entity GET. The page render walks a different path
// (fullStore -> scope -> clientSafe -> fa-state) whose only filter drops
// @private *state cells*, so the gate was implemented, enforced on one route,
// and silently bypassed on the other. Reproduced with this exact app: the API
// returned {"id":1,"name":"Alice"} while the page carried salary 250000.
//
// The test asks each path the same question about the same actor, because the
// only durable fix is one function that answers it — two filters is how the
// paths came to disagree.
func TestAGatedFieldReachesNoPathItIsNotAllowedOn(t *testing.T) {
	e := startEngine(t)
	// Publishing the collection is a separate decision from gating a field: this
	// test is about the FIELD gate, so it publishes Person and then asks what a
	// member receives of a published row (see fct/runtime/apiread.go).
	t.Setenv("FACET_API_READ", "Person")
	a := startApp(t, e, gatedApp)

	// First account is the admin; it writes the row.
	if code, body := a.action("signup", "boss", "hunter2hunter2"); code != 200 {
		t.Fatalf("admin signup: %d %s", code, body)
	}
	if code, body := a.action("hire", "Alice", 250000); code != 200 {
		t.Fatalf("hire: %d %s", code, body)
	}

	// A different visitor entirely: new cookie jar, new session, plain member.
	a.newSession()
	if code, body := a.action("signup", "snooper", "hunter2hunter2"); code != 200 {
		t.Fatalf("member signup: %d %s", code, body)
	}

	const secret = "250000"

	t.Run("json api", func(t *testing.T) {
		code, body := a.get("/api/Person")
		if code != 200 {
			t.Fatalf("GET /api/Person: %d", code)
		}
		if strings.Contains(body, secret) {
			t.Errorf("the API handed a gated field to a member: %s", body)
		}
		if !strings.Contains(body, "Alice") {
			t.Errorf("the ungated field is missing too; the gate is dropping the row: %s", body)
		}
	})

	t.Run("html page", func(t *testing.T) {
		code, html := a.get("/")
		if code != 200 {
			t.Fatalf("GET /: %d", code)
		}

		state := pageState(t, html)
		payload, _ := json.Marshal(state)

		if strings.Contains(string(payload), secret) {
			t.Errorf("the page handed a gated field to a member.\n"+
				"\n"+
				"THIS IS A KNOWN OPEN BUG, and this test is its acceptance criterion.\n"+
				"The JSON API strips this field for the same actor on the same row;\n"+
				"the page ships it. See the AGENT_LOG entry \"fct ships the whole\n"+
				"database to every browser\". A fix is in flight; this test going\n"+
				"green is what done looks like.\n"+
				"\n"+
				"fa-state was: %s", payload)
		}
	})
}
