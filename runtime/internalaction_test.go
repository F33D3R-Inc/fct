package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// An @internal action is the authority's own: reachable through `run`,
// tooling, jobs and triggers, and never by a client — /event and
// /api/<action> answer exactly as they do for a name that does not exist, and
// the API schema does not list it.
func TestInternalActionIsNotClientReachable(t *testing.T) {
	g, err := compile.String(`
app X:
    entity XP:
        id: int
        who: text
        points: int
    policy member:
        actor != "guest"
    action award(who: text, points: int) @internal:
        add XP { who: who, points: points }
    action post:
        requires member
        run award(actor, 100)
    view Home at "/":
        text "{count(XP)}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/api/award", "/api/nosuch"} {
		res, err := http.Post(ts.URL+path, "application/json", strings.NewReader(`{"args":["ada",1000000]}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "unknown action") {
			t.Errorf("POST %s = %d %q, want 404 unknown action", path, res.StatusCode, body)
		}
	}
	res, _ := http.Get(ts.URL + "/api")
	schema, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(schema), `"award"`) {
		t.Errorf("the API schema must not advertise an @internal action: %s", schema)
	}
	if n := len(srv.EntityRows("XP")); n != 0 {
		t.Fatalf("a client reached the internal action: %d rows", n)
	}
	// The authority reaches it: through run, and through tooling.
	if _, err := srv.Run("ada", "member", true, "post", nil); err != nil {
		t.Fatalf("run from an action: %v", err)
	}
	if _, err := srv.Run("ada", "member", true, "award", []any{"ada", 5}); err != nil {
		t.Fatalf("tooling: %v", err)
	}
	if n := len(srv.EntityRows("XP")); n != 2 {
		t.Errorf("XP rows = %d, want 2", n)
	}
}
