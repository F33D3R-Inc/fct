package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	t.Setenv("FACET_RATE_LIMIT_WRITE", "24") // a burst of 6 (a quarter of the minute)
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
	if code, obj, _ := ada.do("POST", "/api/v2/works", `{"title":""}`); code != 422 || errMessage(obj) != "a title is required" {
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
	if code, obj, _ := guest.do("GET", "/api/v2/works/99", ""); code != 404 || errMessage(obj) != "no such work" {
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
	if rl, _ := post["x-rate-limit"].(map[string]any); post["x-auth"] != "session" || rl["limiter"] != "write" || rl["keyed_by"] != "ip" || post["security"] == nil {
		t.Errorf("POST extensions: %v", post)
	}
	if _, has := post["responses"].(map[string]any)["201"]; !has {
		t.Errorf("POST success status missing: %v", post["responses"])
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, want := range []string{"WorkDTO", "Work", "APIErrorDTO", "apiErrorBody"} {
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

// TestDeclaredAPIMessageBody proves a `message` (tagged union) action
// parameter binds the whole POST body — not `{"paramName": {...}}` — and
// that the action can `match` on its discriminant to dispatch per variant,
// exactly the shape a mutation lane like `POST /events` needs (one JSON
// object with its own `type` field, closed set of variants). Also checks the
// published contract references the message's own oneOf schema as the
// requestBody, with a named schema per variant.
func TestDeclaredAPIMessageBody(t *testing.T) {
	src := `app W:
    message ClientEvent:
        | ping()
        | rename(id: int, title: text)
    state lastKind: text = ""
    state lastTitle: text = ""
    action postEvent(ev: ClientEvent) -> text:
        match ev.type:
            case "ping":
                lastKind = "ping"
            case "rename":
                lastKind = "rename"
                lastTitle = ev.title
            else:
                lastKind = "unknown"
        return lastKind
    api POST "/api/v2/events" -> postEvent
    view Home at "/":
        text "{lastKind}"
`
	g, err := compile.String(src)
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
	postEvent := func(body string) (int, string) {
		t.Helper()
		resp, err := http.Post(ts.URL+"/api/v2/events", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var s string
		json.Unmarshal(raw, &s)
		return resp.StatusCode, s
	}
	if code, v := postEvent(`{"type":"rename","id":1,"title":"New name"}`); code != 200 || v != "rename" {
		t.Fatalf("rename event = %d %q", code, v)
	}
	if code, v := postEvent(`{"type":"ping"}`); code != 200 || v != "ping" {
		t.Fatalf("ping event = %d %q", code, v)
	}
	if code, v := postEvent(`{"type":"nope"}`); code != 200 || v != "unknown" {
		t.Fatalf("unknown event = %d %q", code, v)
	}

	resp, err := http.Get(ts.URL + "/api/_contract")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	msg, ok := schemas["ClientEvent"].(map[string]any)
	if !ok {
		t.Fatalf("contract has no ClientEvent schema: %v", schemas)
	}
	if _, ok := msg["oneOf"]; !ok {
		t.Fatalf("ClientEvent schema is not a oneOf: %v", msg)
	}
	if _, ok := schemas["ClientEvent_ping"]; !ok {
		t.Fatalf("contract has no ClientEvent_ping variant schema: %v", schemas)
	}
	if _, ok := schemas["ClientEvent_rename"]; !ok {
		t.Fatalf("contract has no ClientEvent_rename variant schema: %v", schemas)
	}
	paths := doc["paths"].(map[string]any)
	op := paths["/api/v2/events"].(map[string]any)["post"].(map[string]any)
	reqSchema := op["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	// The whole body IS the message: a (maybe-null) reference to it.
	if raw, _ := json.Marshal(reqSchema); string(raw) != `{"oneOf":[{"$ref":"#/components/schemas/ClientEvent"},{"type":"null"}]}` {
		t.Fatalf("requestBody schema = %s, want a reference to ClientEvent (the whole body IS the message)", raw)
	}
}

// TestDeclaredAPIMultipartUpload proves a `bytes`-typed action parameter on
// an `api POST` route accepts multipart/form-data, stores the uploaded file
// through the same mechanism POST /upload uses, and binds the resulting
// public URL as the parameter's value — and that the published contract
// describes the route as multipart/form-data with a binary-format field.
func TestDeclaredAPIMultipartUpload(t *testing.T) {
	t.Setenv("FACET_UPLOAD_DIR", t.TempDir())
	src := `app W:
    entity Work:
        id: int
        posterUrl: text
    action addWork(title: text) -> Work:
        add Work { posterUrl: "" }
    action setPoster(id: int, poster: bytes) -> Work:
        set Work(id).posterUrl = poster
        return Work(id)
    api POST "/api/v2/works" -> addWork status 201
    api POST "/api/v2/works/{id}/poster" -> setPoster
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
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

	addResp, err := http.Post(ts.URL+"/api/v2/works", "application/json", bytes.NewReader([]byte(`{"title":"Hello"}`)))
	if err != nil {
		t.Fatal(err)
	}
	addResp.Body.Close()
	if addResp.StatusCode != 201 {
		t.Fatalf("addWork = %d", addResp.StatusCode)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("poster", "cover.png")
	part.Write([]byte("fake-png-bytes"))
	mw.Close()

	resp, err := http.Post(ts.URL+"/api/v2/works/1/poster", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("setPoster = %d: %s", resp.StatusCode, raw)
	}
	var work map[string]any
	json.Unmarshal(raw, &work)
	url, _ := work["posterUrl"].(string)
	if url == "" || url[:9] != "/uploads/" {
		t.Fatalf("posterUrl = %q, want a stored /uploads/... reference: %s", url, raw)
	}

	resp2, err := http.Get(ts.URL + "/api/_contract")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.NewDecoder(resp2.Body).Decode(&doc)
	resp2.Body.Close()
	paths := doc["paths"].(map[string]any)
	op := paths["/api/v2/works/{id}/poster"].(map[string]any)["post"].(map[string]any)
	content := op["requestBody"].(map[string]any)["content"].(map[string]any)
	mp, ok := content["multipart/form-data"].(map[string]any)
	if !ok {
		t.Fatalf("requestBody content lacks multipart/form-data: %v", content)
	}
	props := mp["schema"].(map[string]any)["properties"].(map[string]any)
	posterSchema := props["poster"].(map[string]any)
	if posterSchema["format"] != "binary" {
		t.Fatalf("poster field schema = %v, want format binary", posterSchema)
	}
}

// TestDeclaredAPIDateTime proves a `datetime`-typed wire type field is a
// real RFC 3339 string end to end: `iso(now())` populates it, it round-trips
// through JSON as a string (never an int the way `date`/`money` do), and the
// published contract describes it as `{"type":"string","format":"date-time"}`
// — the golden contract's own shape for a timestamp field.
func TestDeclaredAPIDateTime(t *testing.T) {
	src := `app W:
    type WorkDTO:
        id: int
        created: datetime
    entity Work:
        id: int
        createdAt: int
    action addWork() -> WorkDTO:
        let id = add Work { createdAt: now() }
        return WorkDTO{id: id, created: iso(Work(id).createdAt)}
    api POST "/api/v2/works" -> addWork status 201
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
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

	resp, err := http.Post(ts.URL+"/api/v2/works", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var work map[string]any
	json.NewDecoder(resp.Body).Decode(&work)
	created, ok := work["created"].(string)
	if !ok {
		t.Fatalf("created = %#v, want a JSON string", work["created"])
	}
	if _, err := time.Parse(time.RFC3339, created); err != nil {
		t.Fatalf("created %q does not parse as RFC 3339: %v", created, err)
	}

	resp2, err := http.Get(ts.URL + "/api/_contract")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.NewDecoder(resp2.Body).Decode(&doc)
	resp2.Body.Close()
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	dto := schemas["WorkDTO"].(map[string]any)
	props := dto["properties"].(map[string]any)
	createdSchema := props["created"].(map[string]any)
	if createdSchema["type"] != "string" || createdSchema["format"] != "date-time" {
		t.Fatalf("created schema = %v, want {type: string, format: date-time}", createdSchema)
	}
}

// TestDeclaredAPIPublicWriteMintsDistinctSessions is the root-cause regression
// for a real bug found while wiring the f33d3r contract: a declared `api`
// route with no `requires` gate (a public signup/login-shaped write) resolved
// its caller with the read-only sidForRequest, which never mints — so every
// anonymous caller reached runActionLocked with sid == "", and
// ensureSession("") stores the FIRST such caller's session under the literal
// "" map key and every later anonymous caller then reuses that exact same
// session (and its `establish`ed identity). Two independent signups here must
// land on two distinct actors, never collide on one shared guest.
func TestDeclaredAPIPublicWriteMintsDistinctSessions(t *testing.T) {
	src := `app W:
    type SessionDTO:
        actor: text
    entity Account:
        id: int
        handle: text
    action signup(handle: text) -> SessionDTO:
        let id = add Account { handle: handle }
        establish actor handle
        return SessionDTO{actor: actor}
    api POST "/api/v2/accounts" -> signup status 201
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
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

	post := func(handle string) (int, map[string]any, []*http.Cookie) {
		resp, err := http.Post(ts.URL+"/api/v2/accounts", "application/json",
			strings.NewReader(`{"handle":"`+handle+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body, resp.Cookies()
	}

	codeA, bodyA, cookiesA := post("alice")
	codeB, bodyB, cookiesB := post("bob")
	if codeA != 201 || codeB != 201 {
		t.Fatalf("signup status = %d, %d, want 201, 201", codeA, codeB)
	}
	if bodyA["actor"] != "alice" || bodyB["actor"] != "bob" {
		t.Fatalf("established actors = %v, %v, want alice, bob (no collision)", bodyA["actor"], bodyB["actor"])
	}
	var sidA, sidB string
	for _, c := range cookiesA {
		if c.Name == "fa_sid" {
			sidA = c.Value
		}
	}
	for _, c := range cookiesB {
		if c.Name == "fa_sid" {
			sidB = c.Value
		}
	}
	if sidA == "" || sidB == "" {
		t.Fatalf("expected each anonymous write to mint its own session cookie, got %q, %q", sidA, sidB)
	}
	if sidA == sidB {
		t.Fatal("two independent anonymous signups minted the SAME session — the collision bug is back")
	}
}
