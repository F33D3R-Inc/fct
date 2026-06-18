package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"facet/internal/compile"
)

// requirePostgres skips a server integration test unless FACET_DATABASE_URL
// points at a Postgres database (the only backend). Run them with, e.g.:
//
//	FACET_DATABASE_URL=postgres://user:pw@localhost:5432/facet_test go test ./runtime
func requirePostgres(t *testing.T) {
	t.Helper()
	if url := os.Getenv("FACET_DATABASE_URL"); !strings.HasPrefix(url, "postgres") {
		t.Skip("set FACET_DATABASE_URL=postgres://… to run server integration tests")
	}
}

// The JSON API is a second projection of the same graph: the schema describes
// the invocable server actions, POST runs one (policies enforced exactly as on
// the web channel), and GET lists an entity's durable rows.
func TestAPIProjection(t *testing.T) {
	requirePostgres(t)
	g, err := compile.String(`
app ApiApp:
    entity User:
        id: int
        name: text
    policy admin:
        actor == "admin"
    action addUser(name: text):
        add User { name: name }
    action wipe:
        requires admin
        clear User
    view M:
        box:
            text "{count(User)}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// schema lists the invocable server action.
	res, err := http.Get(ts.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	schema, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(schema), "addUser") {
		t.Errorf("schema should advertise addUser, got: %s", schema)
	}

	post := func(path, body string) *http.Response {
		r, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	post("/api/addUser", `{"args":["ada"]}`).Body.Close()
	post("/api/addUser", `{"args":["alan"]}`).Body.Close()

	// GET the entity rows back.
	res, err = http.Get(ts.URL + "/api/User")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Rows []map[string]any `json:"rows"`
	}
	json.NewDecoder(res.Body).Decode(&got)
	res.Body.Close()
	if len(got.Rows) != 2 {
		t.Fatalf("want 2 users after two POSTs, got %d (%v)", len(got.Rows), got.Rows)
	}

	// policy is enforced on the API just like the web: a guest cannot wipe.
	r := post("/api/wipe", `{}`)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("guest wipe should be 403, got %d", r.StatusCode)
	}
}

// An `on start` job runs its server action once when the server starts, with no
// client involved — its effect is visible immediately over the API.
func TestOnStartJob(t *testing.T) {
	requirePostgres(t)
	g, err := compile.String(`
app JobApp:
    entity Thing:
        id: int
        at: int
    action seedOne:
        add Thing { at: now() }
    job seed on start -> seedOne
    view M:
        box:
            text "{count(Thing)}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(g)
	if err != nil {
		t.Fatal(err)
	}
	srv.StartJobs() // the on-start job runs synchronously here

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/Thing")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Rows []map[string]any `json:"rows"`
	}
	json.NewDecoder(res.Body).Decode(&got)
	res.Body.Close()
	if len(got.Rows) != 1 {
		t.Fatalf("on-start job should have seeded 1 row, got %d", len(got.Rows))
	}
}
