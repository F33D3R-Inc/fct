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

// An entity list is always a JSON array. A filter that matches nothing
// (`?author=nobody`, `?id=999`) answers `{"rows": []}`, never `{"rows": null}`
// — every F33D3R app served null for an empty filter, and a client iterating
// the rows had to special-case it.
func TestAPIListEmptyFilterIsAnArray(t *testing.T) {
	t.Setenv("FACET_API_READ", "*")
	g, err := compile.String(`
app Listing:
    entity Post:
        id: int
        author: text
        body: text
    action write(body: text):
        add Post { author: actor, body: body }
    view M:
        box:
            text "{count(Post)}"
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
	if _, err := srv.Run("ada", "member", true, "write", []any{"hello"}); err != nil {
		t.Fatal(err)
	}

	raw := func(q string) string {
		res, err := http.Get(ts.URL + "/api/Post" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/Post%s = %d %s", q, res.StatusCode, b)
		}
		return strings.TrimSpace(string(b))
	}
	for _, q := range []string{"?author=nobody", "?id=999"} {
		body := raw(q)
		if strings.Contains(body, "null") {
			t.Errorf("GET /api/Post%s = %s, want an empty array", q, body)
		}
		var out struct{ Rows []any }
		if err := json.Unmarshal([]byte(body), &out); err != nil || out.Rows == nil || len(out.Rows) != 0 {
			t.Errorf("GET /api/Post%s = %s, want {\"rows\": []}", q, body)
		}
	}
	if body := raw("?author=ada"); !strings.Contains(body, `"hello"`) {
		t.Errorf("GET /api/Post?author=ada = %s, want ada's row", body)
	}
}

// The built-in signup answers the same bearer `token` login does: the
// account is signed in the moment it exists, so a native client that signed
// up can act at once instead of logging in a second time.
func TestBuiltinSignupIssuesBearerToken(t *testing.T) {
	g, err := compile.String(`
app Signup:
    auth
    entity Post:
        id: int
        body: text
    policy member:
        actor != "guest"
    action write(body: text):
        requires member
        add Post { body: body }
    view M:
        box:
            text "{count(Post)}"
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

	out, code := apiCall(t, jarClient(t), ts.URL+"/api/signup", `{"args":["ada","password1"]}`)
	if code != http.StatusOK {
		t.Fatalf("signup = %d %v", code, out)
	}
	token, _ := out["token"].(string)
	if token == "" {
		t.Fatalf("signup must answer a bearer token, got %v", out)
	}
	// A cookie-less client acts with the token alone, exactly as after login.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/write", strings.NewReader(`{"args":["first"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("write with the signup token = %d %s", res.StatusCode, body)
	}
	if n := len(srv.EntityRows("Post")); n != 1 {
		t.Errorf("Post rows = %d, want 1", n)
	}
}
