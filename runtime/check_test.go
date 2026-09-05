package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A `check` after a `let` validates the bound brain result. When the brain returns
// an empty UUID, the check fails: the action aborts with 422 and — because checks
// precede every mutation — no Account row is written.
const enrollApp = `app Enroll:
    service Verity at "http://placeholder.invalid":
        verify(handle: text, sig: text) -> text
    entity Account:
        id: int
        handle: text
        pid: text
    action enroll(handle: text, sig: text):
        let uuid = call Verity.verify(handle, sig)
        check uuid != "" "device signature rejected"
        add Account { handle: handle, pid: uuid }
    view Home at "/":
        box:
            text "{count(Account)}"
`

func TestPostBindCheckAbortsCleanly(t *testing.T) {
	// A brain that rejects: returns an empty result for the handle "imposter".
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		uuid := ""
		if body["handle"] != "imposter" {
			uuid = "PIAL-" + toStr(body["handle"])
		}
		json.NewEncoder(w).Encode(map[string]any{"result": uuid})
	}))
	defer brain.Close()

	g, err := compile.String(enrollApp)
	if err != nil {
		t.Fatal(err)
	}
	g.Services[0].URL = brain.URL
	// The JSON API publishes an entity only when the app says so — the default is
	// closed (runtime/apiread.go). This test reads Account over it, so it says so.
	t.Setenv(apiReadEnv, "Account")
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	rows := func() int {
		r, _ := getJSON(t, ts.URL+"/api/Account")["rows"].([]any)
		return len(r)
	}

	// rejected enrollment: the post-bind check fails → 422 + message, no row written.
	resp, err := http.Post(ts.URL+"/api/enroll", "application/json", strings.NewReader(`{"args":["imposter","sig"]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(raw)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a failed check should return 422, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "device signature rejected") {
		t.Errorf("expected the check message, got %q", body)
	}
	if rows() != 0 {
		t.Fatal("a failed check must roll back nothing — no Account row should exist")
	}

	// accepted enrollment: the check passes → the row is written.
	resp2, err := http.Post(ts.URL+"/api/enroll", "application/json", strings.NewReader(`{"args":["ada","sig"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("a valid enrollment should succeed, got %d", resp2.StatusCode)
	}
	if rows() != 1 {
		t.Fatalf("the accepted enrollment should write one Account row, got %d", rows())
	}
}
