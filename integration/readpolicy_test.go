package integration

import (
	"strings"
	"testing"
)

// readPolicyApp is apiread_test.go's ownerScopedApp scenario, rewritten to
// prove the feature this file tests rather than the hand-written `where` that
// predates it: Post declares `read: published || author == actor`, and the
// view's `for` — and its `count(Post)` — carry NO row-level clause of their
// own at all. If this feature works, the entity's own rule is what protects
// the draft on both the page and the JSON API, with nothing written at either
// call site to remember (and nothing to forget, which is what the leak
// apiread_test.go guards against actually was).
const readPolicyApp = `app Vault:
    auth
    policy member:
        actor != "guest"
    entity Post:
        id: int
        author: text
        title: text
        body: text
        published: bool
        read: published || author == actor
    action write(title: text, body: text):
        requires member
        add Post { author: actor, title: title, body: body, published: false }
    view Home at "/":
        box:
            text "posts: {count(Post)}"
            for p in Post by id desc limit 20:
                text "{p.title}"
`

func TestReadPolicyProtectsAPageWithNoWhereClause(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, readPolicyApp)

	if code, body := a.action("signup", "ada", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("write", "Nuclear codes", "0000"); code != 200 {
		t.Fatalf("write: %d %s", code, body)
	}

	// The author's own page: the draft renders, and count(Post) — which the
	// view never filters itself — counts it. read: admits an actor their own
	// unpublished row. The count is a reactive bind (`data-fa-bind="b0"`), not
	// inline text, so it is matched as the runtime actually renders it.
	if code, html := a.get("/"); code != 200 || !strings.Contains(html, "Nuclear codes") || !strings.Contains(html, `data-fa-bind="b0">1<`) {
		t.Fatalf("author's own page: %d %s", code, html)
	}

	// A stranger: new cookie jar, never signed in. Same app, same query, no
	// where clause anywhere in the view — read: is the only thing standing
	// between them and the draft.
	a.newSession()
	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("stranger's page: %d", code)
	}
	if strings.Contains(html, "Nuclear codes") {
		t.Errorf("read: did not protect the `for` region from a stranger: %s", html)
	}
	if !strings.Contains(html, `data-fa-bind="b0">0<`) {
		t.Errorf("read: did not protect count(Post) from a stranger: %s", html)
	}
}

// The same entity, published over the JSON API with NO guard — the exact
// setting TestAPIServesAPublishedEntity (apiread_test.go) proves is
// unconditional for an entity with no read: clause. Here Post has one, so
// publishing it is no longer that same all-or-nothing decision: a stranger
// sees nothing of the draft, the author still sees their own.
func TestReadPolicyFiltersTheJSONAPI(t *testing.T) {
	e := startEngine(t)
	t.Setenv("FACET_API_READ", "Post")
	a := startApp(t, e, readPolicyApp)

	if code, body := a.action("signup", "ada", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("write", "Nuclear codes", "0000"); code != 200 {
		t.Fatalf("write: %d %s", code, body)
	}

	// The author, over the API: their own draft is there.
	if code, body := a.get("/api/Post"); code != 200 || !strings.Contains(body, "Nuclear codes") {
		t.Fatalf("author's own /api/Post: %d %s", code, body)
	}

	// A stranger, over the API: FACET_API_READ=Post publishes the collection
	// (apiMayRead admits the request), but read: still withholds this row.
	a.newSession()
	code, body := a.get("/api/Post")
	if code != 200 {
		t.Fatalf("a published entity must still answer 200 for a stranger: %d %s", code, body)
	}
	if strings.Contains(body, "Nuclear codes") || strings.Contains(body, "0000") {
		t.Errorf("FACET_API_READ=Post with no guard served a read:-withheld draft to a stranger: %d %s", code, body)
	}
}
