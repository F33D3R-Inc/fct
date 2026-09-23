package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Two clustered instances over one FacetQL: a session re-keyed by `establish`
// or ended by `revoke` on one instance stops authenticating on the other,
// including a session only the other instance ever held.
const clusterSessionApp = `app CS:
    type TokenDTO:
        token: text
    entity Account:
        id: int
        handle: text
        password: text @password
    policy member:
        actor != "guest"
    action signup(handle: text, password: text) -> TokenDTO:
        add Account { handle: handle, password: password }
        establish actor handle
        return TokenDTO{token: sessionToken}
    action login(handle: text, password: text) -> TokenDTO:
        let id = max(a.id in Account where a.handle == handle)
        check verifyPassword(Account(id).password, password) "wrong password" status 401
        establish actor handle
        return TokenDTO{token: sessionToken}
    action whoami() -> TokenDTO:
        requires member
        return TokenDTO{token: actor}
    action sessionKey() -> TokenDTO:
        requires member
        return TokenDTO{token: session}
    action revokeKey(key: text):
        requires member
        revoke key
    api POST "/api/accounts" -> signup status 201
    api POST "/api/sessions" -> login status 201
    api GET "/api/me" -> whoami
    api GET "/api/me/session" -> sessionKey
    api DELETE "/api/sessions" -> revokeKey status 204
    view V at "/":
        text "x"
`

func bearer(t *testing.T, a *app, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, a.url(path), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// eventually polls until the instance answers want (the bus is asynchronous).
func eventually(t *testing.T, a *app, token string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, _ := bearer(t, a, "GET", "/api/me", token, "")
		if code == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: GET /api/me = %d, want %d", what, code, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestClusteredSessionEndsEverywhere(t *testing.T) {
	e := startEngine(t)
	t.Setenv("FACET_CLUSTER", "1")
	a := startApp(t, e, clusterSessionApp)
	b := startApp(t, e, clusterSessionApp)

	_, body := bearer(t, a, "POST", "/api/accounts", "", `{"handle":"ada","password":"correct horse"}`)
	t1, _ := body["token"].(string)
	eventually(t, b, t1, http.StatusOK, "a session minted on A, used on B")

	// Re-key on A: the old id must die on B too, which had it cached.
	_, body = bearer(t, a, "POST", "/api/sessions", t1, `{"handle":"ada","password":"correct horse"}`)
	t2, _ := body["token"].(string)
	if t2 == "" || t2 == t1 {
		t.Fatalf("login did not re-key (t1 %q, t2 %q)", t1, t2)
	}
	eventually(t, b, t1, http.StatusUnauthorized, "the pre-login id on the peer")
	eventually(t, b, t2, http.StatusOK, "the re-keyed id on the peer")

	// A session only B ever minted and cached, revoked from A.
	_, body = bearer(t, b, "POST", "/api/sessions", "", `{"handle":"ada","password":"correct horse"}`)
	t3, _ := body["token"].(string)
	_, key := bearer(t, b, "GET", "/api/me/session", t3, "")
	if code, _ := bearer(t, a, "DELETE", "/api/sessions", t2, `{"key":"`+key["token"].(string)+`"}`); code != http.StatusNoContent {
		t.Fatalf("revoke on A = %d", code)
	}
	eventually(t, b, t3, http.StatusUnauthorized, "a peer-only session revoked from A")
	eventually(t, a, t3, http.StatusUnauthorized, "the same session on A (rehydration finds nothing)")
}
