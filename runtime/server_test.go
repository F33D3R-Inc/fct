package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/internal/ir"
)

// jarClient is an HTTP client that keeps cookies, so a sequence of requests
// shares one session (signup -> act, login -> act) the way a browser would.
func jarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// apiCall makes a request (GET when body is "", else POST) and returns the
// decoded JSON object plus the status code.
func apiCall(t *testing.T, c *http.Client, url, body string) (map[string]any, int) {
	t.Helper()
	var (
		res *http.Response
		err error
	)
	if body == "" {
		res, err = c.Get(url)
	} else {
		res, err = c.Post(url, "application/json", strings.NewReader(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return out, res.StatusCode
}

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
	// GET /api/<Entity> is closed unless the app publishes the entity — the
	// framework default is to refuse rather than serve every row to every
	// caller (runtime/apiread.go). These projection tests read entity lists,
	// so they publish everything they declare.
	t.Setenv(apiReadEnv, "*")
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

// resetEntities clears the given entities in both the database and the in-memory
// working set (and resets their id counters), giving an integration test a clean,
// deterministic slate regardless of what an earlier run left behind. Children are
// listed before parents so cascade ordering is irrelevant.
func resetEntities(srv *Server, names ...string) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, n := range names {
		srv.store.Clear(n)
		srv.entities[n] = []any{}
		srv.nextID[n] = 0
	}
}

// getJSON GETs a URL and decodes the JSON object body.
func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return out
}

// Removing a parent cascades to its children — in the database (ON DELETE
// CASCADE on the relation foreign key) and in the live working set the API reads
// through query pushdown. A multi-statement action also persists every statement
// atomically.
func TestCascadeAndTransaction(t *testing.T) {
	requirePostgres(t)
	t.Setenv(apiReadEnv, "*") // see TestAPIProjection: entity lists are published, not default-open
	g, err := compile.String(`
app Rel:
    entity User:
        id: int
        name: text
    entity Message:
        id: int
        to: User
        body: text
    action addUser(name: text):
        add User { name: name }
    action send(to: int, body: text):
        add Message { to: to, body: body }
    action sendTwo(to: int):
        add Message { to: to, body: "one" }
        add Message { to: to, body: "two" }
    action removeUser(id: int):
        remove User(id)
    view M:
        box:
            text "{count(Message)}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(g)
	if err != nil {
		t.Fatal(err)
	}
	// fresh, deterministic state: reset the working set, id counters, and tables.
	resetEntities(srv, "Message", "User")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(path, body string) {
		r, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}

	post("/api/addUser", `{"args":["ada"]}`) // user id depends on prior rows; clear above keeps it 1
	post("/api/sendTwo", `{"args":[1]}`)     // a two-statement action: both rows or neither

	if got := getJSON(t, ts.URL+"/api/Message")["rows"].([]any); len(got) != 2 {
		t.Fatalf("two-statement action should persist 2 rows, got %d", len(got))
	}

	// removing the user cascades to their messages.
	post("/api/removeUser", `{"args":[1]}`)
	if got := getJSON(t, ts.URL+"/api/Message")["rows"].([]any); len(got) != 0 {
		t.Fatalf("removing the parent should cascade-delete its messages, got %d remaining", len(got))
	}
}

// The entity list endpoint pushes `by`/`limit`/`after` down to indexed SQL and
// paginates with an opaque keyset cursor.
func TestQueryPushdownPagination(t *testing.T) {
	requirePostgres(t)
	t.Setenv(apiReadEnv, "*") // see TestAPIProjection: entity lists are published, not default-open
	g, err := compile.String(`
app Feed:
    entity Post:
        id: int
        body: text
    action add1(body: text):
        add Post { body: body }
    view M:
        box:
            for p in Post by id desc limit 50:
                text "{p.body}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(g)
	if err != nil {
		t.Fatal(err)
	}
	resetEntities(srv, "Post")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for i := 0; i < 5; i++ {
		r, _ := http.Post(ts.URL+"/api/add1", "application/json", strings.NewReader(`{"args":["x"]}`))
		r.Body.Close()
	}

	page1 := getJSON(t, ts.URL+"/api/Post?by=id&limit=2")
	if rows := page1["rows"].([]any); len(rows) != 2 {
		t.Fatalf("first page should hold the limit of 2, got %d", len(rows))
	}
	next, ok := page1["next"].(string)
	if !ok || next == "" {
		t.Fatal("a full first page should carry a next cursor")
	}
	page2 := getJSON(t, ts.URL+"/api/Post?by=id&limit=2&after="+next)
	if rows := page2["rows"].([]any); len(rows) != 2 {
		t.Fatalf("second page should hold 2 more, got %d", len(rows))
	}
	// the two pages must not overlap (keyset moved forward).
	first := page1["rows"].([]any)[0].(map[string]any)["id"]
	third := page2["rows"].([]any)[0].(map[string]any)["id"]
	if first == third {
		t.Error("pages overlap: the cursor did not advance")
	}
}

// Migrate is idempotent: after New has reconciled the schema, a dry-run plan is
// empty.
func TestMigrateIdempotent(t *testing.T) {
	requirePostgres(t)
	g, err := compile.String(`
app Mig:
    entity Widget:
        id: int
        label: text
    view M:
        box:
            text "{count(Widget)}"
`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(g); err != nil { // brings the schema up to date
		t.Fatal(err)
	}
	plan, err := Migrate(g, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Errorf("schema should be up to date after New, but plan is:\n%v", plan)
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

// Row-level authorization: a parameterized policy gates a mutation on the
// specific row's owner, so a user may edit only their own post.
func TestRowLevelAuthorization(t *testing.T) {
	requirePostgres(t)
	t.Setenv(apiReadEnv, "*") // see TestAPIProjection: entity lists are published, not default-open
	g, err := compile.String(`
app Own:
    auth
    entity Post:
        id: int
        author: text
        body: text
    policy owns(id: int):
        actor == Post(id).author
    action create(body: text):
        add Post { author: actor, body: body }
    action edit(id: int, body: text):
        requires owns(id)
        set Post(id).body = body
    view M:
        box:
            for p in Post:
                text "{p.body}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(g)
	if err != nil {
		t.Fatal(err)
	}
	resetEntities(srv, "Post", "FacetUser")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ada := jarClient(t)
	bob := jarClient(t)
	apiCall(t, ada, ts.URL+"/api/signup", `{"args":["ada","pw1"]}`)
	apiCall(t, bob, ts.URL+"/api/signup", `{"args":["bob","pw2"]}`)

	// ada writes a post (id 1), then bob may not edit it, but ada may.
	apiCall(t, ada, ts.URL+"/api/create", `{"args":["ada's post"]}`)
	if _, code := apiCall(t, bob, ts.URL+"/api/edit", `{"args":[1,"hacked"]}`); code != http.StatusForbidden {
		t.Errorf("bob editing ada's post should be 403, got %d", code)
	}
	if _, code := apiCall(t, ada, ts.URL+"/api/edit", `{"args":[1,"edited"]}`); code != http.StatusOK {
		t.Errorf("ada editing her own post should be 200, got %d", code)
	}
	rows := getJSON(t, ts.URL+"/api/Post")["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["body"] != "edited" {
		t.Errorf("ada's edit should have applied, got %v", rows)
	}
}

// The account lifecycle: verification and password reset, end to end.
func TestAccountLifecycle(t *testing.T) {
	requirePostgres(t)
	g := mustAuthApp(t)
	srv, err := New(g)
	if err != nil {
		t.Fatal(err)
	}
	resetEntities(srv, "Post", "FacetUser")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	c := jarClient(t)
	// signup issues a verification token; verifyEmail consumes it.
	out, _ := apiCall(t, c, ts.URL+"/api/signup", `{"args":["ada","pw1"]}`)
	vtok, _ := out["verifyToken"].(string)
	if vtok == "" {
		t.Fatal("signup should return a verification token")
	}
	if _, code := apiCall(t, c, ts.URL+"/api/verifyEmail", `{"args":["`+vtok+`"]}`); code != http.StatusOK {
		t.Errorf("verifyEmail with a valid token should be 200, got %d", code)
	}
	if _, code := apiCall(t, c, ts.URL+"/api/verifyEmail", `{"args":["bogus"]}`); code == http.StatusOK {
		t.Error("verifyEmail with a bad token should fail")
	}

	// request a reset, then set a new password and log in with it.
	out, _ = apiCall(t, c, ts.URL+"/api/requestReset", `{"args":["ada"]}`)
	rtok, _ := out["resetToken"].(string)
	if rtok == "" {
		t.Fatal("requestReset should return a reset token")
	}
	if _, code := apiCall(t, c, ts.URL+"/api/resetPassword", `{"args":["ada","`+rtok+`","pw2"]}`); code != http.StatusOK {
		t.Errorf("resetPassword with a valid token should be 200, got %d", code)
	}
	fresh := jarClient(t)
	if _, code := apiCall(t, fresh, ts.URL+"/api/login", `{"args":["ada","pw1"]}`); code == http.StatusOK {
		t.Error("the old password should no longer work")
	}
	if _, code := apiCall(t, fresh, ts.URL+"/api/login", `{"args":["ada","pw2"]}`); code != http.StatusOK {
		t.Errorf("the new password should work, got %d", code)
	}
}

// MFA: enroll TOTP, then a later login demands the second factor.
func TestMFAFlow(t *testing.T) {
	requirePostgres(t)
	g := mustAuthApp(t)
	srv, err := New(g)
	if err != nil {
		t.Fatal(err)
	}
	resetEntities(srv, "Post", "FacetUser")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	c := jarClient(t)
	apiCall(t, c, ts.URL+"/api/signup", `{"args":["ada","pw1"]}`)
	out, code := apiCall(t, c, ts.URL+"/api/enableMFA", `{}`)
	if code != http.StatusOK {
		t.Fatalf("enableMFA should be 200, got %d", code)
	}
	secret, _ := out["secret"].(string)
	if secret == "" {
		t.Fatal("enableMFA should return a TOTP secret")
	}
	code6 := totpCode(secret, time.Now())
	if _, st := apiCall(t, c, ts.URL+"/api/confirmMFA", `{"args":["`+code6+`"]}`); st != http.StatusOK {
		t.Fatalf("confirmMFA with a valid code should be 200, got %d", st)
	}

	// a fresh login now requires the second factor.
	login := jarClient(t)
	out, _ = apiCall(t, login, ts.URL+"/api/login", `{"args":["ada","pw1"]}`)
	if mfa, _ := out["mfa"].(bool); !mfa {
		t.Fatalf("login for an MFA user should ask for the second factor, got %v", out)
	}
	if _, st := apiCall(t, login, ts.URL+"/api/loginMFA", `{"args":["ada","000000"]}`); st == http.StatusOK {
		t.Error("a wrong second factor should be rejected")
	}
	if _, st := apiCall(t, login, ts.URL+"/api/loginMFA", `{"args":["ada","`+totpCode(secret, time.Now())+`"]}`); st != http.StatusOK {
		t.Error("the correct second factor should complete the login")
	}
}

// RBAC: the first user is admin and can promote another; the audit log is
// admin-only.
func TestRBACAndAudit(t *testing.T) {
	requirePostgres(t)
	g := mustAuthApp(t)
	srv, err := New(g)
	if err != nil {
		t.Fatal(err)
	}
	resetEntities(srv, "Post", "FacetUser")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	admin := jarClient(t)
	member := jarClient(t)
	apiCall(t, admin, ts.URL+"/api/signup", `{"args":["root","pw1"]}`) // first -> admin
	apiCall(t, member, ts.URL+"/api/signup", `{"args":["mem","pw2"]}`) // second -> member

	// a member cannot run an admin-gated action.
	if _, code := apiCall(t, member, ts.URL+"/api/wipe", `{}`); code != http.StatusForbidden {
		t.Errorf("a member wiping should be 403, got %d", code)
	}
	// admin promotes the member, whose live session gains the role immediately.
	if _, code := apiCall(t, admin, ts.URL+"/api/setRole", `{"args":["mem","admin"]}`); code != http.StatusOK {
		t.Errorf("admin setRole should be 200, got %d", code)
	}
	if _, code := apiCall(t, member, ts.URL+"/api/wipe", `{}`); code != http.StatusOK {
		t.Errorf("the promoted member should now wipe, got %d", code)
	}

	// the audit feed is admin-only.
	if _, code := apiCall(t, member, ts.URL+"/api/_audit", ""); code == http.StatusForbidden {
		t.Error("the promoted member (now admin) should read the audit log")
	}
	guest := jarClient(t)
	if _, code := apiCall(t, guest, ts.URL+"/api/_audit", ""); code != http.StatusForbidden {
		t.Errorf("a guest reading the audit log should be 403, got %d", code)
	}
	feed, _ := apiCall(t, admin, ts.URL+"/api/_audit", "")
	if entries, _ := feed["entries"].([]any); len(entries) == 0 {
		t.Error("the audit log should have recorded the signups and actions")
	}
}

// mustAuthApp is the shared app for the auth-lifecycle integration tests: auth
// on, an admin-gated action, and a Post entity to mutate.
func mustAuthApp(t *testing.T) *ir.IR {
	t.Helper()
	g, err := compile.String(`
app Acct:
    auth
    entity Post:
        id: int
        body: text
    policy admin:
        role == "admin"
    action wipe:
        requires admin
        clear Post
    view M:
        box:
            text "hi {actor}"
`)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
