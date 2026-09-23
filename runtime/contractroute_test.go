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

	"facet/internal/compile"
)

const contractApp = `app C:
    type WorkDTO:
        id: int
        title: text
    entity Work:
        id: int
        title: text
    action getWork(id: int) -> WorkDTO:
        check exists(w in Work where w.id == id) "no such work" status 404
        return WorkDTO{id: id, title: Work(id).title}
    api GET "/api/v2/works/{id}" -> getWork rate read since "2026-09-18"
    contract "/api/v2/contract" rate read since "2026-09-13"
    view Home at "/":
        text "x"
`

func fetchJSON(t *testing.T, url string, hdr ...string) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

// `contract "/path"` serves the published contract (conditional, cacheable),
// its version, the recorded history — current always listed — and the
// server-computed diff between two recorded versions; the routes and the
// runtime's own schemas for them are in the contract itself.
func TestContractRoutes(t *testing.T) {
	g, err := compile.String(contractApp)
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

	code, doc, hdr := fetchJSON(t, ts.URL+"/api/v2/contract")
	if code != 200 || hdr.Get("ETag") == "" || !strings.Contains(hdr.Get("Cache-Control"), "public") {
		t.Fatalf("GET contract = %d %v", code, hdr)
	}
	version := doc["info"].(map[string]any)["version"].(string)
	if len(version) != 64 || doc["x-schema-version"] != float64(1) {
		t.Fatalf("info.version %q / x-schema-version %v", version, doc["x-schema-version"])
	}
	for _, k := range []string{"components", "info", "openapi", "paths", "security", "x-facet-catalog", "x-mutation-events", "x-schema-version", "x-stream-events"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("document lacks required %q", k)
		}
	}
	if code, _, _ := fetchJSON(t, ts.URL+"/api/v2/contract", "If-None-Match", hdr.Get("ETag")); code != 304 {
		t.Fatalf("conditional GET = %d, want 304", code)
	}
	// The same document /api/_contract answers.
	_, same, _ := fetchJSON(t, ts.URL+"/api/_contract")
	if same["info"].(map[string]any)["version"] != version {
		t.Fatal("/api/_contract and the declared contract route disagree")
	}
	paths := doc["paths"].(map[string]any)
	for p, id := range map[string]string{"/api/v2/contract": "getContract", "/api/v2/contract/version": "getContractVersion",
		"/api/v2/contract/history": "getContractHistory", "/api/v2/contract/diff": "getContractDiff"} {
		op, _ := paths[p].(map[string]any)["get"].(map[string]any)
		if op == nil || op["operationId"] != id || op["x-auth"] != "none" || op["x-since"] != "2026-09-13" {
			t.Fatalf("%s = %v", p, op)
		}
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, s := range []string{"APIContractDocumentDTO", "APIContractStreamEventDTO", "APIContractMutationEventDTO", "APIContractVersionDTO",
		"APIContractHistoryDTO", "APIContractHistoryEntryDTO", "ContractDiffDTO", "ContractFieldsDiffDTO", "FacetCatalogBindingDTO"} {
		if schemas[s] == nil {
			t.Fatalf("schema %s missing", s)
		}
	}

	_, ver, _ := fetchJSON(t, ts.URL+"/api/v2/contract/version")
	if ver["version"] != version || ver["schema_version"] != float64(1) || ver["generated_at"] == nil {
		t.Fatalf("version = %v", ver)
	}
	_, hist, _ := fetchJSON(t, ts.URL+"/api/v2/contract/history")
	vs := hist["versions"].([]any)
	if hist["current"] != version || len(vs) != 1 || vs[0].(map[string]any)["current"] != true {
		t.Fatalf("history = %v", hist)
	}

	// An older document this deployment served: the diff reports what changed.
	older := strings.Replace(contractApp, "        title: text\n    entity", "        title: text\n        slug: text\n    entity", 1)
	older = strings.Replace(older, "WorkDTO{id: id, title: Work(id).title}", "WorkDTO{id: id, title: Work(id).title, slug: \"s\"}", 1)
	older = strings.Replace(older, "    api GET \"/api/v2/works/{id}\"", "    api GET \"/api/v2/old/{id}\" -> getWork\n    api GET \"/api/v2/works/{id}\"", 1)
	og, err := compile.String(older)
	if err != nil {
		t.Fatalf("compile older: %v", err)
	}
	oldDoc := buildContract(og, 1)
	oldRaw, _ := json.Marshal(oldDoc)
	oldVersion := oldDoc["info"].(map[string]any)["version"].(string)
	srv.mu.Lock()
	srv.insertReserved(contractEntity, record{"version": oldVersion, "schema_version": 1, "schema_hash": "x", "first_seen": 1, "document": string(oldRaw)})
	srv.mu.Unlock()

	_, hist, _ = fetchJSON(t, ts.URL+"/api/v2/contract/history")
	if vs := hist["versions"].([]any); len(vs) != 2 || vs[0].(map[string]any)["version"] != version || vs[1].(map[string]any)["version"] != oldVersion {
		t.Fatalf("history (newest first) = %v", hist)
	}
	code, diff, _ := fetchJSON(t, ts.URL+"/api/v2/contract/diff?from="+oldVersion)
	if code != 200 || diff["identical"] != false {
		t.Fatalf("diff = %d %v", code, diff)
	}
	raw, _ := json.Marshal(diff)
	if !strings.Contains(string(raw), `"routes_removed":["GET /api/v2/old/{id}"]`) ||
		!strings.Contains(string(raw), `"name":"WorkDTO"`) || !strings.Contains(string(raw), `"removed":[{"name":"slug"`) {
		t.Fatalf("diff content: %s", raw)
	}
	if _, same, _ := fetchJSON(t, ts.URL+"/api/v2/contract/diff?from=current&to="+version); same["identical"] != true {
		t.Fatalf("current vs itself = %v", same)
	}
	if code, body, _ := fetchJSON(t, ts.URL+"/api/v2/contract/diff"); code != 400 || errMessage(body) == "" {
		t.Fatalf("diff without from = %d %v", code, body)
	}
	if code, body, _ := fetchJSON(t, ts.URL+"/api/v2/contract/diff?from=nope"); code != 404 || body["error"].(map[string]any)["code"] != "not_found" {
		t.Fatalf("diff from an unknown version = %d %v", code, body)
	}

	// A declared route may not shadow the contract's.
	clash := strings.Replace(contractApp, "    contract \"/api/v2/contract\"", "    api GET \"/api/v2/contract/version\" -> getWork\n    contract \"/api/v2/contract\"", 1)
	if _, err := compile.String(clash); err == nil || !strings.Contains(err.Error(), "served by the contract declaration") {
		t.Fatalf("clash: %v", err)
	}
}

// The recorded history survives a restart on the same store, and the data
// schema version rises only when the entity schema changes.
func TestContractHistoryPersists(t *testing.T) {
	store := newMemStore()
	boot := func(src string) *Server {
		g, err := compile.String(src)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		s := newServer(g)
		if err := s.attachStore(store); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a := boot(contractApp)
	va := a.contractVersion()
	// Same entities, a new route: a new contract version, the same schema version.
	b := boot(strings.Replace(contractApp, "    contract", "    api GET \"/api/v2/w/{id}\" -> getWork\n    contract", 1))
	if b.contractVersion() == va || b.schemaVersion != 1 {
		t.Fatalf("route-only change: version %s schema %d", b.contractVersion(), b.schemaVersion)
	}
	// A new entity field: the schema version rises.
	c := boot(strings.Replace(contractApp, "        title: text\n    action", "        title: text\n        views: int\n    action", 1))
	if c.schemaVersion != 2 {
		t.Fatalf("schema change: schema version %d, want 2", c.schemaVersion)
	}
	c.mu.Lock()
	n := len(c.contractRows())
	c.mu.Unlock()
	if n != 3 {
		t.Fatalf("recorded versions = %d, want 3", n)
	}
	// Booting the first app again records nothing new and keeps its schema version.
	d := boot(contractApp)
	d.mu.Lock()
	n = len(d.contractRows())
	d.mu.Unlock()
	if d.contractVersion() != va || d.schemaVersion != 1 || n != 3 {
		t.Fatalf("reboot: version %s schema %d rows %d", d.contractVersion(), d.schemaVersion, n)
	}
}

// `auth <scheme> bearer <param>`: the credential binds from the
// Authorization header only, the route is published under that scheme, and
// the action cannot also demand a session.
func TestBearerAuthRoute(t *testing.T) {
	src := `app B:
    entity Token:
        id: int
        secret: text
        owner: text
    type WhoDTO:
        owner: text
    action devWho(access_token: text) -> WhoDTO:
        check exists(t in Token where t.secret == access_token) "invalid token" status 401
        let tid = max(t.id in Token where t.secret == access_token)
        return WhoDTO{owner: Token(tid).owner}
    api GET "/api/v2/dev/who" -> devWho auth dev_token bearer access_token rate read since "2026-09-21"
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
	srv.mu.Lock()
	srv.insertReserved("Token", record{"secret": "tok-1", "owner": "ada"})
	srv.mu.Unlock()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if code, body, hdr := fetchJSON(t, ts.URL+"/api/v2/dev/who"); code != 401 || hdr.Get("WWW-Authenticate") != "Bearer" || errMessage(body) == "" {
		t.Fatalf("no token = %d %v", code, body)
	}
	if code, _, _ := fetchJSON(t, ts.URL+"/api/v2/dev/who?access_token=tok-1"); code != 401 {
		t.Fatalf("a query token must not authenticate: %d", code)
	}
	if code, body, _ := fetchJSON(t, ts.URL+"/api/v2/dev/who", "Authorization", "Bearer nope"); code != 401 {
		t.Fatalf("bad token = %d %v", code, body)
	}
	code, body, _ := fetchJSON(t, ts.URL+"/api/v2/dev/who", "Authorization", "Bearer tok-1")
	if code != 200 || body["owner"] != "ada" {
		t.Fatalf("good token = %d %v", code, body)
	}
	op := buildContract(g, 0)["paths"].(map[string]map[string]any)["/api/v2/dev/who"]["get"].(map[string]any)
	if op["x-auth"] != "dev_token" || op["parameters"] != nil || op["security"] == nil {
		t.Fatalf("contract op = %v", op)
	}
	bad := strings.Replace(src, "    action devWho(access_token: text) -> WhoDTO:\n", "    policy member:\n        actor != \"guest\"\n    action devWho(access_token: text) -> WhoDTO:\n        requires member\n", 1)
	if _, err := compile.String(bad); err == nil || !strings.Contains(err.Error(), "cannot also require a session") {
		t.Fatalf("bearer + requires: %v", err)
	}
	if _, err := compile.String(strings.Replace(src, "bearer access_token", "bearer nope", 1)); err == nil || !strings.Contains(err.Error(), "must be a text parameter") {
		t.Fatalf("unknown bearer param: %v", err)
	}
}

// A service operation declared `-> bytes` answers a file: the runtime stores
// the reply body, an action returns it, and a declared route answers it as
// the response itself — a private download with its own Content-Type. A
// renderer that does not answer is 502.
func TestFileReplyFromService(t *testing.T) {
	var got map[string]any
	renderer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/storyCard" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "image/webp")
		w.Write([]byte("RIFF....WEBPVP8 card"))
	}))
	defer renderer.Close()
	src := `app F:
    entity Work:
        id: int
        body: text
    service Caeor at "` + renderer.URL + `":
        storyCard(body: text) -> bytes
    action card(id: int) -> bytes:
        check exists(w in Work where w.id == id) "no such work" status 404
        let c = call Caeor.storyCard(Work(id).body)
        return c
    api GET "/api/v2/works/{id}/story-card" -> card rate read since "2026-09-18"
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	t.Setenv("FACET_UPLOAD_DIR", t.TempDir())
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	srv.uploadDir = t.TempDir()
	srv.mu.Lock()
	srv.insertReserved("Work", record{"body": "hello world"})
	srv.mu.Unlock()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v2/works/1/story-card")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/webp" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment; filename=") || string(body) != "RIFF....WEBPVP8 card" {
		t.Fatalf("story card = %d %v %q", resp.StatusCode, resp.Header, body)
	}
	if got["body"] != "hello world" {
		t.Fatalf("renderer received %v", got)
	}
	if code, _, _ := fetchJSON(t, ts.URL+"/api/v2/works/9/story-card"); code != 404 {
		t.Fatalf("unknown work = %d", code)
	}
	op := buildContract(g, 0)["paths"].(map[string]map[string]any)["/api/v2/works/{id}/story-card"]["get"].(map[string]any)
	ok := op["responses"].(map[string]any)["200"].(map[string]any)
	if ok["content"] != nil || op["x-conditional-get"] != nil || op["responses"].(map[string]any)["502"] == nil {
		t.Fatalf("contract op = %v", op)
	}
	renderer.Close()
	if code, body, _ := fetchJSON(t, ts.URL+"/api/v2/works/1/story-card"); code != 502 {
		t.Fatalf("renderer down = %d %v", code, body)
	}
}

// `return expr status N` gives a reply its own success status: a route
// declared 201 answers 200 when the action says so (already there), and the
// contract lists both outcomes with the same reply shape.
func TestReturnStatus(t *testing.T) {
	src := `app R:
    type InviteDTO:
        token: text
    entity Invite:
        id: int
        token: text
    action createInvite(token: text) -> InviteDTO:
        if exists(i in Invite where i.token == token):
            return InviteDTO{token: token} status 200
        add Invite { token: token }
        return InviteDTO{token: token}
    api POST "/api/v2/invites" -> createInvite status 201
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
	c := &apiClient{t: t, base: ts.URL}
	if code, body, _ := c.do("POST", "/api/v2/invites", `{"token":"t1"}`); code != 201 || body["token"] != "t1" {
		t.Fatalf("first = %d %v, want 201", code, body)
	}
	if code, body, _ := c.do("POST", "/api/v2/invites", `{"token":"t1"}`); code != 200 || body["token"] != "t1" {
		t.Fatalf("again = %d %v, want 200", code, body)
	}
	// The generic projection still sees the plain reply value.
	resp, _ := http.Post(ts.URL+"/api/createInvite", "application/json", strings.NewReader(`{"args":["t1"]}`))
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if v, _ := env["value"].(map[string]any); v["token"] != "t1" {
		t.Fatalf("generic projection value = %v", env)
	}
	op := buildContract(g, 0)["paths"].(map[string]map[string]any)["/api/v2/invites"]["post"].(map[string]any)
	rs := op["responses"].(map[string]any)
	for _, code := range []string{"200", "201"} {
		r, _ := rs[code].(map[string]any)
		if r == nil || r["content"] == nil {
			t.Fatalf("response %s = %v", code, rs[code])
		}
	}
	if _, err := compile.String(strings.Replace(src, "status 200\n", "status 404\n", 1)); err == nil || !strings.Contains(err.Error(), "2xx") {
		t.Fatalf("non-2xx return status: %v", err)
	}
}

// fileDigest(file) is the sha256 of a stored upload's bytes: the second
// upload of the same content is spotted (answered 200, nothing queued) while
// a different file is queued (202) — without naming files by their content.
func TestFileDigestDuplicateUpload(t *testing.T) {
	src := `app V:
    type JobDTO:
        upload_id: text
        status: text
    entity Job:
        id: int
        master_url: text
        digest: text
    action uploadVideo(file: bytes) -> JobDTO:
        let d = fileDigest(file)
        check d != "" "the upload is missing" status 400
        if exists(j in Job where j.digest == d):
            let prev = max(j.id in Job where j.digest == d)
            return JobDTO{upload_id: "" + prev, status: "duplicate"} status 200
        let id = add Job { master_url: file, digest: d }
        return JobDTO{upload_id: "" + id, status: "queued"}
    api POST "/api/v2/media/videos" -> uploadVideo status 202
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
	srv.uploadDir = t.TempDir()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	upload := func(content string) (int, map[string]any) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "clip.mp4")
		fw.Write([]byte(content))
		mw.Close()
		resp, err := http.Post(ts.URL+"/api/v2/media/videos", mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, body := upload("frames-A"); code != 202 || body["status"] != "queued" {
		t.Fatalf("first = %d %v", code, body)
	}
	if code, body := upload("frames-A"); code != 200 || body["status"] != "duplicate" || body["upload_id"] != "1" {
		t.Fatalf("same bytes again = %d %v", code, body)
	}
	if code, body := upload("frames-B"); code != 202 || body["upload_id"] != "2" {
		t.Fatalf("other bytes = %d %v", code, body)
	}
	if srv.fileDigest("/uploads/../../etc/passwd") != "" || srv.fileDigest("https://x/y") != "" {
		t.Fatal("fileDigest must only read this server's uploads")
	}
}

// A service's base URL can be overridden per deployment (`env VAR`) and its
// calls carry headers valued from the environment (`header "…" env VAR`).
func TestServiceEnvURLAndHeaders(t *testing.T) {
	var gotKey string
	real := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Internal-Key")
		w.Write([]byte(`{"result":"ok:` + r.URL.Path + `"}`))
	}))
	defer real.Close()
	t.Setenv("THEMIS_URL", real.URL+"/v1")
	t.Setenv("INTERNAL_API_KEY", "k-123")
	src := `app S:
    service Themis at "http://themis.invalid:1" env THEMIS_URL header "X-Internal-Key" env INTERNAL_API_KEY:
        onboard(pial_id: text) -> text
    action go(p: text) -> text:
        let r = call Themis.onboard(p)
        return r
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, _ := NewInMemory(g)
	defer srv.Shutdown()
	v, err := srv.RunValue("ada", "member", true, "go", []any{"p1"})
	if err != nil || v != "ok:/v1/onboard" || gotKey != "k-123" {
		t.Fatalf("call = %v %v, header %q", v, err, gotKey)
	}
	if _, err := compile.String(strings.Replace(src, `header "X-Internal-Key" env INTERNAL_API_KEY`, `header "X-Internal-Key" "k"`, 1)); err == nil {
		t.Fatal("a header value written in source must be refused")
	}
}
