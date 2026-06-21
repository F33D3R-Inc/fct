package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// End-to-end PIAL identity: a verify-action calls an identity brain
// (request→response) for a UUID, keeps it in a @private cell, and `establish`es
// the session as the handle. The UUID must never reach the client; the handle
// becomes the rendered identity.
const pialApp = `app PIAL:
    service Elohim at "http://placeholder.invalid":
        verify(handle: text, sig: text) -> text
    state pid: text @private
    policy member:
        pid != ""
    action login(handle: text, sig: text):
        let uuid = call Elohim.verify(handle, sig)
        pid = uuid
        establish actor handle
    view Home at "/":
        box:
            text "signed in as {actor}"
`

func TestPIALIdentityFlow(t *testing.T) {
	const secretUUID = "PIAL-UUID-3f9a-SECRET"
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"result": secretUUID})
	}))
	defer brain.Close()

	g, err := compile.String(pialApp)
	if err != nil {
		t.Fatal(err)
	}
	g.Services[0].URL = brain.URL
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar} // keep the session cookie across login → page
	// log in: the brain returns the UUID, which is stored @private and the session
	// is established as the handle "ada".
	resp, err := client.Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"args":["ada","goodsig"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login returned %d", resp.StatusCode)
	}
	var out struct {
		Deltas map[string]any `json:"deltas"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Deltas["actor"] != "ada" {
		t.Errorf("establish should set actor=ada, got %v", out.Deltas["actor"])
	}
	if _, leaked := out.Deltas["pid"]; leaked {
		t.Error("the @private pid must not appear in the action's deltas")
	}

	// fetch the page as the logged-in session: the rendered identity is the handle,
	// and the secret UUID must appear NOWHERE in the bytes the client receives.
	page, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	html := string(body)
	// the rendered identity is the handle "ada" (in a reactive bind span).
	if !strings.Contains(html, "signed in as") || !strings.Contains(html, ">ada<") {
		t.Error("the page should render the handle as the identity")
	}
	if strings.Contains(html, secretUUID) {
		t.Fatal("LEAK: the @private UUID reached the client page (state bootstrap or HTML)")
	}
}
