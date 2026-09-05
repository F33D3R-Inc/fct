package runtime

import (
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

const gateApp = `app Dir:
    auth
    policy admins:
        role == "admin"
    entity Person:
        id: int
        name: text
        salary: money @requires(admins)
    action add(name: text, salary: money):
        add Person { name: name, salary: salary }
    view Home at "/":
        box:
            text "{count(Person)}"
`

func newGateServer(t *testing.T) *Server {
	t.Helper()
	g, err := compile.String(gateApp)
	if err != nil {
		t.Fatal(err)
	}
	// The JSON API publishes an entity only when the app says so — the default is
	// closed (runtime/apiread.go). This test reads Person over it, so it says so.
	t.Setenv(apiReadEnv, "Person")
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	// seed one row under system (admin) authority
	if _, status, msg := srv.runAction(systemSID, srv.byAction["add"], []any{"Ada", 9999}); status != 200 {
		t.Fatalf("seed add failed: %d %s", status, msg)
	}
	return srv
}

func TestFieldGatePerActor(t *testing.T) {
	srv := newGateServer(t)
	// admin passes the policy → sees every field (nothing dropped)
	if drop := srv.gateForActor("Person", map[string]any{"role": "admin"}); len(drop) != 0 {
		t.Fatalf("admin should see all fields, dropped %v", drop)
	}
	// a guest fails the policy → the gated field is dropped
	drop := srv.gateForActor("Person", map[string]any{"role": "guest"})
	if !drop["salary"] {
		t.Fatalf("guest should be denied salary, dropped %v", drop)
	}
	if drop["name"] {
		t.Fatal("non-gated name must never be dropped")
	}
}

func TestFieldGateAPIStripsForGuest(t *testing.T) {
	srv := newGateServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// A guest (no session) hits the API: the gated salary is absent, the rest stays.
	body := getJSON(t, ts.URL+"/api/Person")
	rows, _ := body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	row := rows[0].(map[string]any)
	if _, ok := row["salary"]; ok {
		t.Fatal("guest must not receive the gated salary field")
	}
	if row["name"] != "Ada" {
		t.Fatalf("non-gated field should be present, got %v", row["name"])
	}
}

func TestFieldGateNeverStreamsOverSSE(t *testing.T) {
	srv := newGateServer(t)
	// sseSafe strips gated fields unconditionally — the stream has no actor.
	deltas := map[string]any{"Person": srv.entities["Person"]}
	safe := srv.sseSafe(deltas)
	rows := safe["Person"].([]any)
	if _, ok := rows[0].(record)["salary"]; ok {
		t.Fatal("gated salary must never appear in an SSE delta")
	}
	if rows[0].(record)["name"] != "Ada" {
		t.Fatal("non-gated field should remain in the SSE delta")
	}
	// the original working set is untouched (strip copies, never mutates)
	if _, ok := srv.entities["Person"][0].(record)["salary"]; !ok {
		t.Fatal("stripping must not mutate the in-memory rows")
	}
}
