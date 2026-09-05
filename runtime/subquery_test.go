package runtime

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A correlated sub-query in a `for`'s `where` is evaluated for every row.
//
// It used to be evaluated for the first candidate row and then not at all. An
// aggregate is addressed by (render path, position in the page's IR) so its
// value can be shipped to a client that has no collection to scan — and a
// `where` is answered once per candidate row while that path stands still, so
// the memo behind that address handed row one's answer to every row after it.
// `exists(...)` became "whatever it was for the first row" and `count(...)`
// likewise: a feed filtered on a sub-query silently showed the whole table.
//
// The tagged posts here are the first and the last, so a memo of the first row's
// answer is indistinguishable from "true for everybody" unless the middle row is
// checked — which is exactly what the shipped bug did.
const subqueryApp = `app Corr:
    entity Post:
        id: int
        body: text
        pub: bool
    entity Tag:
        id: int
        post: Post
        name: text
    action addPost(body: text, pub: bool):
        add Post { body: body, pub: pub }
    action addTag(post: Post, name: text):
        add Tag { post: post, name: name }
    view Home at "/":
        box:
            for p in Post where exists(t in Tag where t.post == p.id && t.name == "x"):
                text "E:{p.body}"
        box:
            for p in Post where count(t in Tag where t.post == p.id) > 0:
                text "C:{p.body}"
        box:
            for t in Tag where t.name == "x" && Post(t.post).pub:
                text "L:{t.name}"
`

// subqueryServer seeds three posts (only the first and third are tagged "x")
// and the two tags, then returns the running server.
func subqueryServer(t *testing.T) *Server {
	t.Helper()
	g, err := compile.String(subqueryApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	// Post 3 is unpublished, so the entity-lookup list must drop its tag.
	for _, row := range [][]any{{"one", true}, {"two", true}, {"three", false}} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["addPost"], row); status != 200 {
			t.Fatalf("seeding a post: %d %s", status, msg)
		}
	}
	for _, row := range [][]any{{1, "x"}, {3, "x"}} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["addTag"], row); status != 200 {
			t.Fatalf("seeding a tag: %d %s", status, msg)
		}
	}
	return srv
}

func TestCorrelatedSubqueryInAWhereFiltersEveryRow(t *testing.T) {
	srv := subqueryServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")

	for _, c := range []struct {
		marker string
		want   bool
		why    string
	}{
		{"E:one", true, "post one is tagged x"},
		{"E:two", false, "post two has no tag — a correlated exists(...) must be evaluated for it, not inherited from post one"},
		{"E:three", true, "post three is tagged x"},
		{"C:one", true, "post one has a tag"},
		{"C:two", false, "post two has none — a correlated count(...) must be evaluated for it"},
		{"C:three", true, "post three has a tag"},
	} {
		if got := strings.Contains(html, c.marker); got != c.want {
			t.Errorf("%s rendered=%v, want %v — %s", c.marker, got, c.want, c.why)
		}
	}

	// `Post(t.post).pub` is the same failure in the entity-lookup form: both tags
	// are named x, and only the one whose post is published may render.
	if got := strings.Count(html, "L:x"); got != 1 {
		t.Errorf("`Post(t.post).pub` kept %d tags, want 1 — the lookup must be evaluated per row", got)
	}
}

// The per-row questions of a correlated residual are asked as one grouped read
// per page, not one read per row. Correct-but-N+1 would still beat wrong, but
// the batch the render already uses for a list's per-row counts answers exactly
// these queries, so a residual gets it too.
func TestCorrelatedResidualIsHoistedIntoOneGroupedRead(t *testing.T) {
	srv := subqueryServer(t)
	spy := &spyStore{Store: srv.store}
	srv.store = spy
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	getPage(t, jarClient(t), ts.URL+"/")

	if len(spy.counts) != 0 {
		t.Errorf("a correlated residual issued %d single-row counts; the page's questions must be hoisted into one grouped read", len(spy.counts))
	}
	if len(spy.groups) != 2 {
		t.Fatalf("want one grouped read per correlated aggregate (exists + count) = 2, got %d: %+v", len(spy.groups), spy.groups)
	}
	for _, g := range spy.groups {
		if g.query.Entity != "Tag" || g.groupBy != "post" {
			t.Errorf("the batch must group Tag by the column the predicate pins per row, got %s by %q", g.query.Entity, g.groupBy)
		}
		if len(g.values) != 3 {
			t.Errorf("the batch must pin every candidate row's value in one request, got %v", g.values)
		}
	}
}

// A nested aggregate — one inside another's filter — is the same shape one level
// down: the inner `exists(...)` is answered once per row of the outer scan while
// the render path stands still.
const nestedAggApp = `app Nested:
    entity Post:
        id: int
        body: text
    entity Tag:
        id: int
        post: Post
    action addPost(body: text):
        add Post { body: body }
    action addTag(post: Post):
        add Tag { post: post }
    view Home at "/":
        box:
            text "n={count(p in Post where exists(t in Tag where t.post == p.id))}"
`

func TestNestedAggregateIsEvaluatedPerRow(t *testing.T) {
	g, err := compile.String(nestedAggApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	for _, body := range []string{"one", "two", "three"} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["addPost"], []any{body}); status != 200 {
			t.Fatalf("seeding a post: %d %s", status, msg)
		}
	}
	for _, id := range []any{1, 3} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["addTag"], []any{id}); status != 200 {
			t.Fatalf("seeding a tag: %d %s", status, msg)
		}
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")
	if got := regionSnippet(html); got != "2" {
		t.Errorf("the outer count kept %q rows; want 2 (posts one and three are tagged) — the inner exists(...) must be evaluated for each outer row", got)
	}
}

// regionSnippet pulls the one interpolated value the nested-aggregate page
// renders out of the span the renderer wraps it in.
func regionSnippet(html string) string {
	i := strings.Index(html, "n=")
	if i < 0 {
		return html
	}
	rest := html[i:]
	open := strings.Index(rest, ">")
	close := strings.Index(rest, "</span>")
	if open < 0 || close < open {
		return rest
	}
	return rest[open+1 : close]
}

// The rule about what an aggregate's address means is implemented twice — once
// in Go for the first paint, once in JavaScript for everything after it — and a
// page whose two halves disagree flickers from a filtered list to an unfiltered
// one. This pins the halves at the source, the way link_test.go, classmerge_test.go
// and attrtext_test.go pin theirs; integration/control_test.go boots the real
// client over a real render.
func TestBothEvaluatorsSuspendTheAggregateMemoForARowPredicate(t *testing.T) {
	client, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	for _, want := range []string{
		// the counter, the gate on it, and both places a predicate runs per row.
		`let unaddressed = 0;`,
		`if (unaddressed > 0) return undefined;`,
		`function evRow(e, sc) {`,
		`if (truthy(evRow(node.where, child))) out.push(r);`,
		`rows = rows.filter((r) => { sc[e.var] = r; return truthy(evRow(e.where, sc)); });`,
	} {
		if !strings.Contains(string(client), want) {
			t.Errorf("runtime/assets/facet.js no longer suspends the aggregate memo for a per-row predicate: missing %q.\n"+
				"The server does (runtime/region.go, (*materializer).perRow); a client that does not will re-render row one's exists(...) for every row.", want)
		}
	}
	for _, f := range []string{"region.go", "eval.go", "server.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		var want string
		switch f {
		case "region.go":
			want = `func (m *materializer) perRow(f func() any) any {`
		case "eval.go":
			want = `if evalRowPredicate(e.Where, scope) {`
		case "server.go":
			want = `if evalRowPredicate(n.Where, child) {`
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("runtime/%s no longer routes its per-row predicate through evalRowPredicate: missing %q", f, want)
		}
	}
}

// A `for` over a `[T]` state cell is filtered on the client, per row, by the
// client's own evaluator — so a correlated aggregate in its `where` is a read
// the client performs. The render cannot answer it for the client (a per-row
// predicate has no render address, so there is no materialized value), which
// means the rows it scans have to cross in the bootstrap. They did not: the
// aggregate's source was deliberately withheld on the grounds that "the render
// ships its answer instead", which was true only while the answer was one
// memoized value for the whole list.
const cellListApp = `app Cell:
    state picks: [int] = [] @client
    entity Tag:
        id: int
        post: int
    action addTag(post: int):
        add Tag { post: post }
    view Home at "/":
        box:
            for n in picks where exists(t in Tag where t.post == n):
                text "P:{n}"
`

func TestStateCellListShipsTheCollectionItsWhereScans(t *testing.T) {
	g, err := compile.String(cellListApp)
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
	html, _ := getPage(t, jarClient(t), ts.URL+"/")

	if _, ok := pageState(t, html)["Tag"]; !ok {
		t.Error("the bootstrap withheld Tag, but the client filters `for n in picks` itself and its `where` scans Tag — " +
			"without those rows every exists(...) on the client is false")
	}
}

// The storefront shape, and the artifact that made the mechanism visible: the
// SAME `Category(p.category)` lookup, once in `where` position and once in a
// value beside it.
//
// The value-position one was addressed per row (`/0/0#1/0|1`, `/0/0#3/0|1`) and
// the `where`-position one at the region (`/0/0|0` = "light", the FIRST row's
// category, reused for every row) — one expression keyed two ways, because a
// `where` is answered once per candidate row while the render path stands still.
// The filter was therefore true for every row or none, and the tile beside it
// showed the right category name, which is what hid it.
//
// A `where`-position lookup now has no address at all: the client never
// evaluates an entity list's `where` (its rows arrive as a region), so there is
// nothing to ship, and the value it would have shipped is precisely the wrong
// one.
const storefrontApp = `app Store:
    entity Category:
        id: int
        slug: text
        name: text
    entity Product:
        id: int
        title: text
        category: Category
    action addCat(slug: text, name: text):
        add Category { slug: slug, name: name }
    action addProd(title: text, category: Category):
        add Product { title: title, category: category }
    view Home at "/":
        box:
            for p in Product where Category(p.category).slug == "light":
                text "{p.title} in {Category(p.category).name}"
`

func TestAWhereLookupIsNotRecordedAtTheRegion(t *testing.T) {
	g, err := compile.String(storefrontApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	for _, c := range [][]any{{"light", "Lighting"}, {"table", "Table"}} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["addCat"], c); status != 200 {
			t.Fatalf("seeding a category: %d %s", status, msg)
		}
	}
	// lamp and sconce are lighting; desk is not, and it sits between them so a
	// first-row answer reused for the rest cannot pass by accident.
	for _, p := range [][]any{{"lamp", 1}, {"desk", 2}, {"sconce", 1}} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["addProd"], p); status != 200 {
			t.Fatalf("seeding a product: %d %s", status, msg)
		}
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")
	state := pageState(t, html)

	regions, _ := state["@regions"].(map[string]any)
	rows, _ := regions["/0/0"].([]any)
	if len(rows) != 2 {
		t.Errorf("the category filter kept %d products, want 2 — `Category(p.category).slug` must be resolved for each row", len(rows))
	}

	// Every aggregate value the render shipped must be addressed at a row
	// (`…#<id>/…`), never at the list itself: a key with no row in it is the
	// first row's answer standing in for the whole region.
	aggs, _ := state["@aggs"].(map[string]any)
	for k, v := range aggs {
		if !strings.Contains(k, "#") {
			t.Errorf("the render shipped %q = %#v, addressed at the region rather than at a row — "+
				"that is the `where`-position lookup being evaluated once and reused for every row", k, v)
		}
	}
	if len(aggs) != 2 {
		t.Errorf("want the two per-row value-position lookups, got %d: %#v", len(aggs), aggs)
	}
}
