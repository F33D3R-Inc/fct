package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// pageState returns the parsed #fa-state bootstrap of a rendered page — the
// exact document the browser hydrates from.
func pageState(t *testing.T, html string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`(?s)<script[^>]*id="fa-state"[^>]*>(.*?)</script>`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("page has no #fa-state block")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(m[1]), &out); err != nil {
		t.Fatalf("fa-state is not JSON: %v", err)
	}
	return out
}

// pageRegions returns the region result sets a page shipped, keyed by render path.
func pageRegions(t *testing.T, html string) map[string][]any {
	t.Helper()
	raw, _ := pageState(t, html)["@regions"].(map[string]any)
	out := map[string][]any{}
	for k, v := range raw {
		rows, _ := v.([]any)
		out[k] = rows
	}
	return out
}

// getPage fetches a page and returns its HTML plus the CSRF token it minted, so
// a follow-up POST can authenticate as the same session.
func getPage(t *testing.T, c *http.Client, url string) (string, string) {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	token := ""
	if m := regexp.MustCompile(`name="fa-csrf" content="([^"]*)"`).FindStringSubmatch(html); m != nil {
		token = m[1]
	}
	return html, token
}

// postRegion drives the region endpoint the way the client does.
func postRegion(t *testing.T, c *http.Client, base, csrf, path, key string, state map[string]any) (map[string]any, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"path": path, "key": key, "state": state})
	req, err := http.NewRequest(http.MethodPost, base+"/region", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-Facet-CSRF", csrf)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return out, res.StatusCode
}

// onlyRegion returns the single region a page shipped, failing if there is not
// exactly one — so a test asserting on "the list" cannot silently assert on the
// wrong one after the view changes.
func onlyRegion(t *testing.T, regions map[string][]any) (string, []any) {
	t.Helper()
	if len(regions) != 1 {
		t.Fatalf("want exactly one region, got %d: %v", len(regions), regions)
	}
	for k, v := range regions {
		return k, v
	}
	return "", nil
}

// ── a list region resolves through the store ────────────────────────────────

// spyStore records every Query it is asked, so a test can assert that a list
// region became a pushed-down read rather than a scan of the working set.
type spyStore struct {
	Store
	queries []Query
	counts  []Query
	groups  []spyGroup
}

// spyGroup is one grouped cardinality read, with the values it pinned — the
// difference between "twenty answers in one request" and "every answer in the
// table to use twenty".
type spyGroup struct {
	query   Query
	groupBy string
	values  []any
}

func (s *spyStore) Query(q Query) ([]any, string, error) {
	s.queries = append(s.queries, q)
	return s.Store.Query(q)
}

func (s *spyStore) Count(q Query) (int, error) {
	s.counts = append(s.counts, q)
	return s.Store.Count(q)
}

func (s *spyStore) CountBy(q Query, groupBy string, values []any) (map[string]int, error) {
	s.groups = append(s.groups, spyGroup{q, groupBy, values})
	return s.Store.CountBy(q, groupBy, values)
}

const listApp = `app Feed:
    state shown: int = 2 @client
    entity Post:
        id: int
        author: text
        body: text
    action add(author: text, body: text):
        add Post { author: author, body: body }
    view Home at "/":
        box:
            for p in Post where p.author == "ada" by id desc limit shown:
                text "{p.body}"
`

// A top-level list region is a query against the database, not a filter over
// RAM: the node's own where/by/limit reach the store, and the rows the page
// renders (and ships) are the store's answer. The proof that it is not reading
// the mirror is that the mirror is emptied first and the page still renders.
func TestListRegionResolvesThroughTheStore(t *testing.T) {
	g, err := compile.String(listApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]any{{"ada", "one"}, {"bob", "not mine"}, {"ada", "two"}, {"ada", "three"}} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["add"], row); status != 200 {
			t.Fatalf("seed failed: %d %s", status, msg)
		}
	}
	spy := &spyStore{Store: srv.store}
	srv.store = spy
	// Empty the in-memory working set. Anything the page still renders came from
	// the store; anything that vanishes was being read out of RAM.
	srv.mu.Lock()
	srv.entities["Post"] = []any{}
	srv.mu.Unlock()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")

	if len(spy.queries) != 1 {
		t.Fatalf("want exactly one store query for one list region, got %d: %#v", len(spy.queries), spy.queries)
	}
	q := spy.queries[0]
	if q.Entity != "Post" || q.Order != "id" || !q.Desc || q.Limit != 2 {
		t.Fatalf("the node's own by/desc/limit must reach the store, got %+v", q)
	}
	if q.Where == nil {
		t.Fatal("the node's `where` must be pushed down, got a query with no predicate")
	}
	if q.ItemVar != "p" {
		t.Fatalf("the predicate's item var must be the loop variable, got %q", q.ItemVar)
	}

	// The rendered page shows ada's two newest posts and nothing of bob's.
	if !strings.Contains(html, "three") || !strings.Contains(html, "two") {
		t.Fatalf("the list should render ada's two newest posts, got: %s", html)
	}
	if strings.Contains(html, "not mine") {
		t.Fatal("the pushed-down predicate should have excluded bob's post")
	}
	if strings.Contains(html, ">one<") {
		t.Fatal("the pushed-down limit should have excluded the third-newest post")
	}

	// And the client receives that result set, not the collection.
	state := pageState(t, html)
	if _, ok := state["Post"]; ok {
		t.Fatal("fa-state must not carry the Post collection")
	}
	_, rows := onlyRegion(t, pageRegions(t, html))
	if len(rows) != 2 {
		t.Fatalf("the region should carry exactly the two rendered rows, got %d", len(rows))
	}
}

// A `for` over a `[T]` state cell has no table to query: it is local data the
// client already holds, so it keeps the in-memory path and is not shipped twice.
func TestListOverStateCellStaysInMemory(t *testing.T) {
	g, err := compile.String(`app Local:
    state tags: [text] = ["b", "a"] @client
    view Home at "/":
        box:
            for t in tags:
                text "{t}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")
	if !strings.Contains(html, ">b<") || !strings.Contains(html, ">a<") {
		t.Fatalf("a list over a state cell must still render, got: %s", html)
	}
	if regions := pageRegions(t, html); len(regions) != 0 {
		t.Fatalf("a state-cell list is not a region result set, got %v", regions)
	}
}

// ── the field gate applies on every path that hands rows to a client ─────────

const leakApp = `app Dir:
    auth
    policy admins:
        role == "admin"
    entity Person:
        id: int
        name: text
        salary: money @requires(admins)
    action add(name: text, salary: money):
        add Person { name: name, salary: salary }
    view Home at "/":
        box:
            text "people: {count(Person)}"
            for p in Person by id limit 10:
                text "{p.name}"
`

// The regression this locks down: `@requires(admins)` was enforced by the JSON
// API and walked around by the page, which shipped the gated value in plaintext
// inside fa-state. A gated field must be absent from *every* door a row leaves
// by — the API, the page bootstrap (region rows and collections alike), the
// region endpoint, and the live stream.
func TestGatedFieldNeverReachesADeniedActor(t *testing.T) {
	g, err := compile.String(leakApp)
	if err != nil {
		t.Fatal(err)
	}
	// The JSON API publishes an entity only when the app says so — the default is
	// closed (runtime/apiread.go). This test reads Person over it, so it says so.
	t.Setenv(apiReadEnv, "Person")
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, status, msg := srv.runAction(systemSID, srv.byAction["add"], []any{"Alice", 250000}); status != 200 {
		t.Fatalf("seed failed: %d %s", status, msg)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := jarClient(t)

	// 1. the JSON API (this one was already correct)
	api, _ := apiCall(t, c, ts.URL+"/api/Person", "")
	rows, _ := api["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 row from the API, got %d", len(rows))
	}
	if _, leaked := rows[0].(map[string]any)["salary"]; leaked {
		t.Error("API: a non-admin must not receive the gated salary")
	}

	// 2. the page bootstrap — both the region result set and the aggregate residue
	html, csrf := getPage(t, c, ts.URL+"/")
	if strings.Contains(html, "250000") {
		t.Error("page: the gated salary appears somewhere in the served HTML")
	}
	_, regionRows := onlyRegion(t, pageRegions(t, html))
	if len(regionRows) != 1 {
		t.Fatalf("want 1 region row, got %d", len(regionRows))
	}
	if _, leaked := regionRows[0].(map[string]any)["salary"]; leaked {
		t.Error("fa-state: the region rows carry the gated salary")
	}
	if regionRows[0].(map[string]any)["name"] != "Alice" {
		t.Error("fa-state: a non-gated field must survive the gate")
	}
	if coll, ok := pageState(t, html)["Person"].([]any); ok {
		for _, r := range coll {
			if _, leaked := r.(map[string]any)["salary"]; leaked {
				t.Error("fa-state: the aggregate residue collection carries the gated salary")
			}
		}
	}

	// 3. the region endpoint
	key, _ := onlyRegion(t, pageRegions(t, html))
	body, status := postRegion(t, c, ts.URL, csrf, "/", key, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("region request failed: %d", status)
	}
	got, _ := body["rows"].([]any)
	if len(got) != 1 {
		t.Fatalf("want 1 region row, got %d", len(got))
	}
	if _, leaked := got[0].(map[string]any)["salary"]; leaked {
		t.Error("/region: the response carries the gated salary")
	}

	// 4. the live stream (no actor at all: every gate fails closed)
	safe := srv.sseSafe(map[string]any{"Person": srv.entities["Person"]})
	if _, leaked := safe["Person"].([]any)[0].(record)["salary"]; leaked {
		t.Error("SSE: the gated salary must never be streamed")
	}

	// The authority itself still holds the value — the gate is a projection, not
	// a deletion.
	if _, ok := srv.entities["Person"][0].(record)["salary"]; !ok {
		t.Fatal("gating must not mutate the authority's own rows")
	}
}

// ── the region endpoint's authorization ─────────────────────────────────────

const regionAuthApp = `app Guarded:
    auth
    state q: text = "" @client
    entity Note:
        id: int
        owner: text
        body: text
    policy member:
        actor != "guest"
    action add(owner: text, body: text):
        add Note { owner: owner, body: body }
    view Home at "/":
        box:
            for n in Note where n.owner == actor by id limit 10:
                text "{n.body}"
    view Vault at "/vault" requires member:
        box:
            for n in Note by id limit 10:
                text "{n.body}"
`

func newRegionAuthServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	g, err := compile.String(regionAuthApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]any{{"ada", "ada's note"}, {"bob", "bob's secret"}} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["add"], row); status != 200 {
			t.Fatalf("seed failed: %d %s", status, msg)
		}
	}
	return srv, httptest.NewServer(srv.Handler())
}

// The region endpoint is the page's authority, not a hole beside it: it needs
// the same session, refuses without the anti-forgery token, enforces the page's
// route guard, and answers only for regions that exist.
func TestRegionEndpointAuthorization(t *testing.T) {
	srv, ts := newRegionAuthServer(t)
	defer ts.Close()
	_ = srv
	c := jarClient(t)
	html, csrf := getPage(t, c, ts.URL+"/")
	key, _ := onlyRegion(t, pageRegions(t, html))

	// A cookie-authenticated request with no CSRF token is refused: this endpoint
	// reads on the session's authority, so it must not be callable cross-origin.
	if _, status := postRegion(t, c, ts.URL, "", "/", key, map[string]any{}); status != http.StatusForbidden {
		t.Errorf("missing CSRF token should be 403, got %d", status)
	}
	// A route the actor may not enter is not readable a region at a time either.
	if _, status := postRegion(t, c, ts.URL, csrf, "/vault", "/0/0", map[string]any{}); status != http.StatusForbidden {
		t.Errorf("a guarded page's region should be 403 for a guest, got %d", status)
	}
	// Unknown page / unknown region are 404, not an empty success that would read
	// as "this region legitimately has no rows".
	if _, status := postRegion(t, c, ts.URL, csrf, "/nope", key, map[string]any{}); status != http.StatusNotFound {
		t.Errorf("unknown page should be 404, got %d", status)
	}
	if _, status := postRegion(t, c, ts.URL, csrf, "/", "/9/9/9", map[string]any{}); status != http.StatusNotFound {
		t.Errorf("unknown region should be 404, got %d", status)
	}
	// GET is not a way in.
	res, err := c.Get(ts.URL + "/region")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /region should be 405, got %d", res.StatusCode)
	}
}

// The client's state is authoritative over the cells the compiler placed on the
// client, and over nothing else. A body claiming a different actor, a different
// role, or a collection of its own must change neither who the request is nor
// which rows it may see.
func TestRegionRefusesForgedIdentityAndData(t *testing.T) {
	_, ts := newRegionAuthServer(t)
	defer ts.Close()
	c := jarClient(t)
	// Sign in as ada, whose region should hold exactly her own note.
	if _, status := apiCall(t, c, ts.URL+"/api/signup", `{"args":["ada","hunter2hunter2"]}`); status != http.StatusOK {
		t.Fatalf("signup failed: %d", status)
	}
	html, csrf := getPage(t, c, ts.URL+"/")
	key, _ := onlyRegion(t, pageRegions(t, html))

	forged := map[string]any{
		"actor": "bob",
		"role":  "admin",
		"Note":  []any{map[string]any{"id": 99, "owner": "ada", "body": "injected"}},
	}
	body, status := postRegion(t, c, ts.URL, csrf, "/", key, forged)
	if status != http.StatusOK {
		t.Fatalf("region request failed: %d", status)
	}
	rows, _ := body["rows"].([]any)
	for _, r := range rows {
		got := r.(map[string]any)
		if got["owner"] != "ada" {
			t.Errorf("a forged actor changed which rows came back: %v", got)
		}
		if got["body"] == "injected" {
			t.Error("a collection supplied by the client was treated as data")
		}
	}
	if len(rows) != 1 {
		t.Fatalf("ada should see exactly her own note, got %d rows", len(rows))
	}
}

// A client-placed state cell IS honoured — that is the whole point of the round
// trip: the search box's value only exists on the client, and the region is
// re-answered against it.
func TestRegionAnswersUnderClientState(t *testing.T) {
	g, err := compile.String(`app Search:
    state q: text = "" @client
    entity Post:
        id: int
        body: text
    action add(body: text):
        add Post { body: body }
    view Home at "/":
        box:
            for p in Post where contains(lower(p.body), lower(q)) by id limit 10:
                text "{p.body}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"alpha", "beta", "gamma"} {
		if _, status, _ := srv.runAction(systemSID, srv.byAction["add"], []any{b}); status != 200 {
			t.Fatalf("seed %q failed", b)
		}
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := jarClient(t)
	html, csrf := getPage(t, c, ts.URL+"/")
	key, seeded := onlyRegion(t, pageRegions(t, html))
	if len(seeded) != 3 {
		t.Fatalf("an empty query matches everything, want 3 rows, got %d", len(seeded))
	}
	body, status := postRegion(t, c, ts.URL, csrf, "/", key, map[string]any{"q": "ET"})
	if status != http.StatusOK {
		t.Fatalf("region request failed: %d", status)
	}
	rows, _ := body["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["body"] != "beta" {
		t.Fatalf(`typing "ET" should leave only "beta", got %v`, rows)
	}
}

// ── the pushdown split ──────────────────────────────────────────────────────

// The half of a `where` the store can answer is separated from the half it
// cannot, after the closed-over scope is folded to literals — so an indexed
// equality is still pushed down even when it sits beside a substring test.
func TestSplitPredicateFoldsScopeAndSplitsResidual(t *testing.T) {
	g, err := compile.String(`app Split:
    state q: text = "" @client
    entity Post:
        id: int
        author: text
        body: text
    view Home at "/u/:handle":
        box:
            for p in Post where p.author == handle && contains(p.body, q) by id limit 5:
                text "{p.body}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	node := findList(t, g.Pages[0].View)
	ent, _ := srv.entityByName("Post")
	push, residual := srv.splitPredicate(ent, node, map[string]any{"handle": "ada", "q": "x"})
	if push == nil {
		t.Fatal("the sargable equality must be pushed down")
	}
	if !pushable(push, node.Var) {
		t.Fatalf("the pushed half must compile to the store's subset, got %+v", push)
	}
	if push.Kind != "bin" || push.Op != "==" || push.R == nil || push.R.Kind != "lit" || toStr(push.R.Val) != "ada" {
		t.Fatalf("the outer `handle` must be folded to the literal it evaluates to, got %+v", push)
	}
	if residual == nil || residual.Kind != "call" {
		t.Fatalf("the substring test must stay behind as a residual, got %+v", residual)
	}
}

// A route parameter arrives as text; comparing it against an int column has to
// push down as an int, or a typed store answers an empty page for a row that
// exists. (This is the same class of bug as toInt("1") == 0.)
func TestPushedLiteralIsTypedByTheColumn(t *testing.T) {
	g, err := compile.String(`app Detail:
    entity Post:
        id: int
        body: text
    view One at "/post/:id":
        box:
            for p in Post where p.id == id by id limit 1:
                text "{p.body}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	node := findList(t, g.Pages[0].View)
	ent, _ := srv.entityByName("Post")
	push, residual := srv.splitPredicate(ent, node, map[string]any{"id": "7"})
	if residual != nil {
		t.Fatalf("an id equality is fully pushable, got residual %+v", residual)
	}
	if push == nil || push.R == nil || push.R.VType != "int" {
		t.Fatalf("the literal must be retyped to the column, got %+v", push)
	}
	if toInt(push.R.Val) != 7 {
		t.Fatalf("want the literal 7, got %v", push.R.Val)
	}
}

// findList returns the first list node in a view tree.
func findList(t *testing.T, nodes []ir.Node) ir.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Kind == "list" {
			return n
		}
		if len(n.Children) > 0 {
			if got := findListMaybe(n.Children); got != nil {
				return *got
			}
		}
	}
	t.Fatal("no list node in the view")
	return ir.Node{}
}

func findListMaybe(nodes []ir.Node) *ir.Node {
	for _, n := range nodes {
		if n.Kind == "list" {
			row := n
			return &row
		}
		if got := findListMaybe(n.Children); got != nil {
			return got
		}
	}
	return nil
}

// ── materialized aggregates: the address both sides compute ─────────────────

const aggApp = `app Counted:
    entity Post:
        id: int
        body: text
    entity Like:
        id: int
        post: Post
    action addPost(body: text):
        add Post { body: body }
    action addLike(post: int):
        add Like { post: post }
    view Home at "/":
        box:
            text "posts: {count(Post)}"
            for p in Post by id limit 5:
                text "{p.body} has {count(l in Like where l.post == p.id)}"
`

// An aggregate scans a collection, and collections no longer reach the browser.
// So the page carries the *value* the render computed, addressed by the render
// path it was computed at — and carries no collection at all.
func TestAggregatesShipAsValuesNotCollections(t *testing.T) {
	g, err := compile.String(aggApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"first", "second"} {
		if _, status, _ := srv.runAction(systemSID, srv.byAction["addPost"], []any{body}); status != 200 {
			t.Fatalf("seeding %q failed", body)
		}
	}
	// three likes on post 1, none on post 2
	for i := 0; i < 3; i++ {
		if _, status, _ := srv.runAction(systemSID, srv.byAction["addLike"], []any{1}); status != 200 {
			t.Fatal("seeding a like failed")
		}
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")

	state := pageState(t, html)
	for _, ent := range []string{"Post", "Like"} {
		if _, ok := state[ent]; ok {
			t.Errorf("fa-state still carries the %s collection", ent)
		}
	}
	aggs, _ := state["@aggs"].(map[string]any)
	if len(aggs) == 0 {
		t.Fatal("the page carries no aggregate values, so the client has nothing to render counts from")
	}

	// The addresses are the contract. The page-level `count(Post)` is a binding on
	// the first text node, so it is addressed at that node's render path; the
	// per-row `count(Like)` is evaluated once per row, at a path carrying that
	// row's id — which is what keeps every row of a feed from collapsing onto one
	// answer. The indices come from the one walk both sides perform: the in-region
	// aggregate is reached first (the view tree), the binding after it.
	key, _ := onlyRegion(t, pageRegions(t, html))
	page := childPath(childPath("", 0), 0) // box -> first text node
	if got := aggs[matKey(page, 1)]; toInt(got) != 2 {
		t.Errorf("count(Post) should be 2 at %s, got %v (have %v)", matKey(page, 1), got, aggs)
	}
	row1 := childPath(rowPath(key, record{"id": 1}), 0)
	if got := aggs[matKey(row1, 0)]; toInt(got) != 3 {
		t.Errorf("post 1's count(Like) should be 3 at %s, got %v (have %v)", matKey(row1, 0), got, aggs)
	}
	row2 := childPath(rowPath(key, record{"id": 2}), 0)
	if got := aggs[matKey(row2, 0)]; toInt(got) != 0 {
		t.Errorf("post 2's count(Like) should be 0 at %s, got %v (have %v)", matKey(row2, 0), got, aggs)
	}

	// And the HTML the server painted says the same thing, so a client that reads
	// these values renders what the first paint already showed.
	if !strings.Contains(html, `posts: <span data-fa-bind="b0">2</span>`) ||
		!strings.Contains(html, "first has 3") || !strings.Contains(html, "second has 0") {
		t.Errorf("the first paint disagrees with the shipped values: %s", html)
	}
}

// The server numbers a page's aggregates, and the shipped client numbers them
// again from the same IR. If the two walks ever disagree the client looks up an
// address the server never wrote, silently renders 0 for a count that is not
// zero, and nothing else in the repo notices — so the walks are compared here
// directly, against the client source that actually ships.
func TestClientNumbersAggregatesLikeTheServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}
	g, err := compile.String(aggApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	pg := &g.Pages[0]

	// What the server assigned, as (kind, name) in index order.
	idx := srv.aggIndex(pg)
	want := make([]string, len(idx))
	for e, i := range idx {
		want[i] = e.Kind + ":" + e.Name
	}

	// The IR the client receives is this page's view/bindings plus the app's
	// components — exactly what handlePage ships.
	reqIR := *srv.ir
	reqIR.View = pg.View
	reqIR.Bindings = pg.Bindings
	reqIR.DepGraph = pg.DepGraph
	reqIR.Pages = nil
	irJSON, err := json.Marshal(&reqIR)
	if err != nil {
		t.Fatal(err)
	}

	client, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatal(err)
	}
	// numberAggregates walks a node's interpolated values through segLists, which
	// is itself the mirror of ir.Node.SegLists — so the mirror under test is both
	// functions, taken from the source that ships.
	fn := extractFunction(t, string(client), "segLists") + "\n" +
		extractFunction(t, string(client), "numberAggregates")
	script := "function list(xs){return xs||[];}\n" + fn + "\n" +
		"const g = " + string(irJSON) + ";\n" +
		"numberAggregates(g);\n" +
		"const out = [];\n" +
		"(function walk(e){ if(!e) return; if(e.__faAgg!==undefined) out[e.__faAgg]=e.kind+\":\"+e.name;\n" +
		"  for (const k of ['l','r','x','obj','key','where']) walk(e[k]);\n" +
		"  for (const a of list(e.args)) walk(a); })," +
		"(function nodes(ns){ for (const n of list(ns)) { for (const s of list(n.segs)) walkAll(s.expr);\n" +
		"  walkAll(n.cond); walkAll(n.where); walkAll(n.limit); for (const a of list(n.args)) walkAll(a); nodes(n.children); } })(g.view);\n" +
		"for (const b of list(g.bindings)) walkAll(b.expr);\n" +
		"for (const c of list(g.components)) (function nodes(ns){ for (const n of list(ns)) { for (const s of list(n.segs)) walkAll(s.expr); walkAll(n.cond); walkAll(n.where); walkAll(n.limit); for (const a of list(n.args)) walkAll(a); nodes(n.children); } })(c.view);\n" +
		"console.log(JSON.stringify(out));\n" +
		"function walkAll(e){ if(!e) return; if(e.__faAgg!==undefined) out[e.__faAgg]=e.kind+\":\"+e.name;\n" +
		"  for (const k of ['l','r','x','obj','key','where']) walkAll(e[k]);\n" +
		"  for (const a of list(e.args)) walkAll(a); }\n"

	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("running the client's numbering: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("client output was not a list: %v (%s)", err, out)
	}
	if len(got) != len(want) {
		t.Fatalf("client numbered %d aggregates, server numbered %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("aggregate %d: client calls it %s, server calls it %s — the two walks have diverged", i, got[i], want[i])
		}
	}
}

// extractFunction pulls one top-level function out of the shipped client by
// source, so the mirror under test is the code that ships rather than a copy.
func extractFunction(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "  function "+name+"(")
	if start < 0 {
		t.Fatalf("assets/facet.js has no function %s — the mirror this test checks has moved", name)
	}
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("function %s in assets/facet.js is unterminated", name)
	return ""
}

// ── the live stream announces, it does not push tables ──────────────────────

// A write used to fan the whole changed collection out to every subscriber. Now
// it fans out the entity's *name*: each client re-asks for the regions that read
// it and gets one page of rows, so a fifty-thousand-row table no longer crosses
// the wire on every post. Rows still travel for the collections a client's own
// evaluator reads whole — and gated fields never do, on either path.
func TestLiveStreamAnnouncesRatherThanPushingRows(t *testing.T) {
	g, err := compile.String(leakApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, status, _ := srv.runAction(systemSID, srv.byAction["add"], []any{"Alice", 250000}); status != 200 {
		t.Fatal("seed failed")
	}
	ch := make(chan []byte, 4)
	srv.subsMu.Lock()
	srv.subs[ch] = true
	srv.subsMu.Unlock()

	srv.broadcast(map[string]any{"Person": srv.entities["Person"]})

	var msg struct {
		Deltas  map[string][]map[string]any `json:"deltas"`
		Changed []string                    `json:"changed"`
		Seq     int64                       `json:"seq"`
	}
	select {
	case raw := <-ch:
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("the stream frame is not JSON: %v", err)
		}
	default:
		t.Fatal("nothing was fanned out")
	}
	if len(msg.Changed) != 1 || msg.Changed[0] != "Person" {
		t.Errorf("the frame must name the entity that changed, got %v", msg.Changed)
	}
	if _, pushed := msg.Deltas["Person"]; pushed {
		// Every read of Person on this page is an aggregate, and aggregates ship as
		// values now — so there is no reason to push its rows at anyone.
		t.Errorf("rows were pushed for an entity no client reads whole: %v", msg.Deltas)
	}
	if msg.Seq == 0 {
		t.Error("the frame must carry the authority's write count, so a client can tell its page is stale")
	}
}

// The write count moves on every entity change and is stamped on the page, which
// is what lets a client notice a write that landed between its render and its
// subscription — the race the old whole-database opening snapshot covered.
func TestPageCarriesTheWriteSequence(t *testing.T) {
	g, err := compile.String(leakApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := jarClient(t)

	before, _ := getPage(t, c, ts.URL+"/")
	if _, status, _ := srv.runAction(systemSID, srv.byAction["add"], []any{"Alice", 1}); status != 200 {
		t.Fatal("write failed")
	}
	after, _ := getPage(t, c, ts.URL+"/")

	b := toInt(pageState(t, before)["@seq"])
	a := toInt(pageState(t, after)["@seq"])
	if a <= b {
		t.Errorf("the write count must move on an entity change: %d -> %d", b, a)
	}
}

// ── aggregates resolve through the store, one request per shape ─────────────

const feedApp = `app Feed:
    entity Post:
        id: int
        body: text
    entity Like:
        id: int
        post: Post
    entity Reply:
        id: int
        post: Post
    action addPost(body: text):
        add Post { body: body }
    action addLike(post: int):
        add Like { post: post }
    action addReply(post: int):
        add Reply { post: post }
    view Home at "/":
        box:
            text "total: {count(Post)}"
            for p in Post by id desc limit 20:
                text "{p.body}: {count(l in Like where l.post == p.id)} likes, {count(r in Reply where r.post == p.id)} replies"
`

// A rendered page asks the database for its counts, and asks once per aggregate
// rather than once per row.
//
// The per-row shape is the one that matters: twenty posts with two counts each is
// forty questions, and forty requests would be an N+1 across the network — the
// workaround this whole change exists to avoid. They are forty *pinned values of
// two questions*, so they cost two grouped reads. The page-level count, which
// varies with nothing, is a single read.
func TestRenderedCountsAreHoistedIntoOneRequestPerShape(t *testing.T) {
	g, err := compile.String(feedApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		if _, status, _ := srv.runAction(systemSID, srv.byAction["addPost"], []any{"post"}); status != 200 {
			t.Fatalf("seeding post %d failed", i)
		}
	}
	// The newest post (id 25) gets three likes and one reply; id 24 gets one like.
	for i := 0; i < 3; i++ {
		srv.runAction(systemSID, srv.byAction["addLike"], []any{25})
	}
	srv.runAction(systemSID, srv.byAction["addReply"], []any{25})
	srv.runAction(systemSID, srv.byAction["addLike"], []any{24})

	spy := &spyStore{Store: srv.store}
	srv.store = spy
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")

	// The counts the page renders are the right ones.
	if !strings.Contains(html, "post: 3 likes, 1 replies") {
		t.Errorf("the newest post should show 3 likes and 1 reply: %s", html)
	}
	if !strings.Contains(html, "post: 1 likes, 0 replies") {
		t.Errorf("a post with one like and no replies should say so: %s", html)
	}
	if !strings.Contains(html, "total: <span data-fa-bind=\"b0\">25</span>") {
		t.Errorf("the page-level count(Post) should be 25: %s", html)
	}

	// And the shape of the asking.
	if len(spy.groups) != 2 {
		t.Fatalf("want one grouped read per per-row aggregate (2), got %d: %+v", len(spy.groups), spy.groups)
	}
	seen := map[string]bool{}
	for _, gr := range spy.groups {
		seen[gr.query.Entity] = true
		if gr.groupBy != "post" {
			t.Errorf("%s should be grouped by the column the row pins, got %q", gr.query.Entity, gr.groupBy)
		}
		if len(gr.values) != 20 {
			t.Errorf("%s should pin the 20 rendered rows' ids, got %d values — an unpinned "+
				"grouped read computes every answer in the table to use twenty",
				gr.query.Entity, len(gr.values))
		}
	}
	if !seen["Like"] || !seen["Reply"] {
		t.Errorf("both per-row aggregates should have been hoisted, got %v", seen)
	}
	// One ungrouped count, for the aggregate that does not vary per row. Anything
	// more means a per-row question escaped the batch.
	if len(spy.counts) != 1 {
		t.Fatalf("want exactly one ungrouped count (the page-level one), got %d: %+v", len(spy.counts), spy.counts)
	}
	if spy.counts[0].Entity != "Post" || spy.counts[0].Where != nil {
		t.Errorf("the ungrouped count should be the unfiltered count(Post), got %+v", spy.counts[0])
	}
}

// An aggregate the batch cannot hoist stays on the in-memory working set inside a
// multi-row list. Falling back to one request per row would be precisely the
// fan-out the hoist exists to prevent, so the slower correct answer wins.
func TestUnhoistableRowAggregateDoesNotFanOut(t *testing.T) {
	g, err := compile.String(`app Feed2:
    entity Post:
        id: int
        body: text
    entity Tag:
        id: int
        label: text
    action addPost(body: text):
        add Post { body: body }
    view Home at "/":
        box:
            for p in Post by id desc limit 10:
                text "{p.body}: {count(t in Tag where contains(t.label, p.body))}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		srv.runAction(systemSID, srv.byAction["addPost"], []any{"post"})
	}
	spy := &spyStore{Store: srv.store}
	srv.store = spy
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	getPage(t, jarClient(t), ts.URL+"/")

	if len(spy.counts) != 0 || len(spy.groups) != 0 {
		t.Errorf("a predicate the store cannot push down must not become a request per row: "+
			"%d counts, %d grouped reads", len(spy.counts), len(spy.groups))
	}
}

// The two ways of answering must agree. A count served from the database and the
// same count taken over the working set are the same number, or a page changes
// meaning depending on which path answered it — including for a @softdelete
// entity, whose archived rows the store still holds and the mirror never had.
func TestStoreCountAgreesWithTheWorkingSet(t *testing.T) {
	g, err := compile.String(`app Soft:
    entity Note @softdelete:
        id: int
        owner: text
    action add(owner: text):
        add Note { owner: owner }
    action drop(id: int):
        remove Note(id)
    view Home at "/":
        box:
            text "mine: {count(n in Note where n.owner == \"ada\")}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"ada", "ada", "bob", "ada"} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["add"], []any{owner}); status != 200 {
			t.Fatalf("seed failed: %d %s", status, msg)
		}
	}
	if _, status, msg := srv.runAction(systemSID, srv.byAction["drop"], []any{1}); status != 200 {
		t.Fatalf("archiving failed: %d %s", status, msg)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")

	// Two of ada's three rows survive the archive; the store must not count the
	// archived one back in.
	if !strings.Contains(html, "mine: <span data-fa-bind=\"b0\">2</span>") {
		t.Errorf("an archived row must not be counted: %s", html)
	}
	// The same question asked of the working set directly.
	scope := srv.fullStore("")
	delete(scope, materializerKey) // no collector: this is the mirror's own answer
	pg := &g.Pages[0]
	if got := toInt(eval(pg.Bindings[0].Expr, scope)); got != 2 {
		t.Errorf("the working set answers %d where the store answered 2", got)
	}
}

// A choice list drawn from data is a query, addressed like any other.
//
// The options of a `select`/`radio` over an entity are rows the authority
// resolved, recorded under the option group's render path and shipped with the
// page — and the client asks for that same address when its state moves. If the
// two sides spelled the address differently the endpoint would answer 404, and
// the client ignores a failed ask: the dropdown would paint once and then never
// change again, with nothing anywhere reporting an error. So the address is
// checked from both ends here.
func TestChoiceListRowsAreAddressedLikeAnyOtherRegion(t *testing.T) {
	g, err := compile.String(`app Picker:
    entity Category:
        name: text
        slug: text

    state chosen: text = "" @client

    action add(name: text, slug: text):
        add Category { name: name, slug: slug }

    view Home at "/":
        box:
            select bind chosen:
                option "— any —" -> ""
                for c in Category:
                    option "{c.name}" -> c.slug
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]any{{"Books", "books"}, {"Toys", "toys"}} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["add"], row); status != 200 {
			t.Fatalf("seed failed: %d %s", status, msg)
		}
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := jarClient(t)
	html, csrf := getPage(t, c, ts.URL+"/")

	// The select is the box's first child and the option group its second child,
	// so the group's rows are recorded at /0/0/1 — the address both sides compute
	// from the tree, with no id allocated for it.
	key, rows := onlyRegion(t, pageRegions(t, html))
	if key != "/0/0/1" {
		t.Fatalf("the choice list's rows are recorded at %q, want %q", key, "/0/0/1")
	}
	if len(rows) != 2 {
		t.Fatalf("the choice list shipped %d rows, want the 2 the page rendered", len(rows))
	}

	// The collection is not shipped alongside: a choice list is a query answered
	// once, exactly like a `for`.
	if _, ok := pageState(t, html)["Category"]; ok {
		t.Error("fa-state carries the Category collection; a choice list ships its answer, not its table")
	}

	// And the endpoint answers that address, which is what the client's re-ask
	// depends on.
	out, code := postRegion(t, c, ts.URL, csrf, "/", key, map[string]any{"chosen": ""})
	if code != 200 {
		t.Fatalf("POST /region for the choice list's own address: %d — the client's re-ask would be silently dropped", code)
	}
	got, _ := out["rows"].([]any)
	if len(got) != 2 {
		t.Fatalf("the region endpoint answered %d rows, want 2", len(got))
	}
}
