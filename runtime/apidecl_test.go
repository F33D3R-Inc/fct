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

// Declared `api` routes: path/query/body binding, the action's reply as the
// body, success statuses, check statuses, auth via bearer token, method
// matching, rate classes, and the published contract.
const apiApp = `app W:
    type WorkDTO:
        id: int
        title: text
        shout: text
    entity Work:
        id: int
        title: text
        author: text
    policy member:
        actor != "guest"
    action getWork(id: int) -> WorkDTO:
        check exists(w in Work where w.id == id) "no such work" status 404
        return WorkDTO{id: id, title: Work(id).title, shout: upper(Work(id).title)}
    action listWorks(q: text, limit: int) -> [WorkDTO]:
        return list(WorkDTO{id: w.id, title: w.title, shout: upper(w.title)} in Work where contains(w.title, q) by title limit limit)
    action createWork(title: text) -> Work:
        requires member
        check title != "" "a title is required"
        let id = add Work { title: title, author: actor }
        return Work(id)
    action deleteWork(id: int):
        requires member
        remove Work(id)
    api GET "/api/v2/works/{id}" -> getWork since "2026-09-21"
    api GET "/api/v2/works" -> listWorks rate read
    api POST "/api/v2/works" -> createWork status 201 rate write
    api DELETE "/api/v2/works/{id}" -> deleteWork status 204
    view Home at "/":
        text "{count(Work)}"
`

type apiClient struct {
	t     *testing.T
	base  string
	token string
}

func (c *apiClient) do(method, path, body string) (int, map[string]any, []any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var obj map[string]any
	var arr []any
	if len(raw) > 0 && raw[0] == '[' {
		json.Unmarshal(raw, &arr)
	} else if len(raw) > 0 {
		json.Unmarshal(raw, &obj)
	}
	return resp.StatusCode, obj, arr
}

func TestDeclaredAPIRoutes(t *testing.T) {
	t.Setenv("FACET_RATE_LIMIT_WRITE", "6")
	g, err := compile.String(apiApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	guest := &apiClient{t: t, base: ts.URL}

	// No session: a gated route is 401 with the contract's error body.
	if code, obj, _ := guest.do("POST", "/api/v2/works", `{"title":"Hello"}`); code != 401 || obj["error"] == nil {
		t.Fatalf("guest POST = %d %v, want 401 with an error body", code, obj)
	}
	// A session: the dev `?as=` identity mints a cookie; its signed value is
	// the bearer token a native client would get from the login reply.
	resp, err := http.Get(ts.URL + "/?as=ada")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var token string
	for _, c := range resp.Cookies() {
		if c.Name == "fa_sid" {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no session cookie")
	}
	ada := &apiClient{t: t, base: ts.URL, token: token}

	code, obj, _ := ada.do("POST", "/api/v2/works", `{"title":"Hello"}`)
	if code != 201 || obj["title"] != "Hello" || obj["author"] != "ada" {
		t.Fatalf("create = %d %v", code, obj)
	}
	if code, obj, _ := ada.do("POST", "/api/v2/works", `{"title":""}`); code != 422 || obj["error"] != "a title is required" {
		t.Fatalf("a failed check is 422 with its message: %d %v", code, obj)
	}
	// The write class is metered on its own: a burst of writes is refused
	// with 429 once its budget is spent, while reads below stay unaffected.
	limited := false
	for i := 0; i < 20 && !limited; i++ {
		code, _, _ := ada.do("POST", "/api/v2/works", `{"title":"More"}`)
		limited = code == 429
	}
	if !limited {
		t.Fatal("a burst of writes should be rate limited")
	}

	code, obj, _ = guest.do("GET", "/api/v2/works/1", "")
	if code != 200 || obj["shout"] != "HELLO" || toInt(obj["id"]) != 1 {
		t.Fatalf("GET by path param = %d %v", code, obj)
	}
	// Conditional GET: the same answer under its ETag is a 304 with no body.
	req, _ := http.NewRequest("GET", ts.URL+"/api/v2/works/1", nil)
	first, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("a declared GET must carry an ETag")
	}
	req.Header.Set("If-None-Match", etag)
	again, _ := http.DefaultClient.Do(req)
	again.Body.Close()
	if again.StatusCode != 304 {
		t.Fatalf("matching If-None-Match should be 304, got %d", again.StatusCode)
	}
	if code, obj, _ := guest.do("GET", "/api/v2/works/99", ""); code != 404 || obj["error"] != "no such work" {
		t.Fatalf("a check with status 404 answers 404: %d %v", code, obj)
	}
	code, _, arr := guest.do("GET", "/api/v2/works?q=ell&limit=5", "")
	if code != 200 || len(arr) != 1 {
		t.Fatalf("GET list with query params = %d %v", code, arr)
	}
	if code, _, _ := guest.do("PUT", "/api/v2/works/1", ""); code != 405 {
		t.Fatalf("an undeclared method on a declared path is 405, got %d", code)
	}
	if code, _, _ := ada.do("DELETE", "/api/v2/works/1", ""); code != 204 {
		t.Fatalf("delete = %d, want 204", code)
	}
	if code, _, _ := guest.do("GET", "/api/v2/works/1", ""); code != 404 {
		t.Fatalf("after delete the row is gone: %d", code)
	}

	// The contract describes exactly these routes.
	code, doc, _ := guest.do("GET", "/api/_contract", "")
	if code != 200 {
		t.Fatalf("contract = %d", code)
	}
	paths := doc["paths"].(map[string]any)
	byID := paths["/api/v2/works/{id}"].(map[string]any)
	get := byID["get"].(map[string]any)
	if get["x-auth"] != "none" || get["x-since"] != "2026-09-21" || get["x-conditional-get"] != true {
		t.Errorf("GET {id} extensions: %v", get)
	}
	if _, has := get["responses"].(map[string]any)["404"]; !has {
		t.Errorf("the 404 a check can answer must be in the contract: %v", get["responses"])
	}
	post := paths["/api/v2/works"].(map[string]any)["post"].(map[string]any)
	if post["x-auth"] != "session" || post["x-rate-limit"] != "write" || post["security"] == nil {
		t.Errorf("POST extensions: %v", post)
	}
	if _, has := post["responses"].(map[string]any)["201"]; !has {
		t.Errorf("POST success status missing: %v", post["responses"])
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, want := range []string{"WorkDTO", "Work", "APIError"} {
		if schemas[want] == nil {
			t.Errorf("schema %s missing from the contract", want)
		}
	}
}

func TestDeclaredAPIGates(t *testing.T) {
	base := "app G:\n    entity Work:\n        id: int\n        title: text\n    state n: int = 0 @client\n    action getWork(id: int) -> Work:\n        return Work(id)\n    action bump():\n        n = n + 1\n" +
		"    action noReply(id: int):\n        remove Work(id)\n" + "DECL" + "    view Home at \"/\":\n        text \"x\"\n"
	cases := []struct{ name, decl, want string }{
		{"unknown action", "    api GET \"/w/{id}\" -> nope\n", "unknown action"},
		{"client-state action", "    api POST \"/bump\" -> bump\n", "client-only state"},
		{"path param not a parameter", "    api GET \"/w/{slug}\" -> getWork\n", "is not a parameter"},
		{"redeclared", "    api GET \"/w/{id}\" -> getWork\n    api GET \"/w/{id}\" -> getWork\n", "redeclared"},
		{"204 with a reply", "    api GET \"/w/{id}\" -> getWork status 204\n", "answers 204"},
		{"bad method", "    api FETCH \"/w\" -> getWork\n", "must be GET, POST"},
		{"bad status", "    api GET \"/w/{id}\" -> getWork status 404\n", "2xx"},
		{"bad rate class", "    api GET \"/w/{id}\" -> getWork rate fast\n", "must be read, write or auth"},
		{"check status out of range", "    api GET \"/w/{id}\" -> getWork\n    action g2(id: int) -> Work:\n        check id > 0 \"bad\" status 200\n        return Work(id)\n", "400-599"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := compile.String(strings.Replace(base, "DECL", c.decl, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
