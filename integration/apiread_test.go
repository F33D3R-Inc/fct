package integration

import (
	"strings"
	"testing"
)

// An owner-scoped app: a Post is a draft until it is published, and every query
// a stranger can reach carries the row-level `where` that hides someone else's
// draft. The web channel is sound because of it; the question this file asks is
// whether the JSON projection of the SAME app is.
const ownerScopedApp = `app Vault:
    auth
    policy member:
        actor != "guest"
    entity Post:
        id: int
        author: text
        title: text
        body: text
        published: bool
    action write(title: text, body: text):
        requires member
        add Post { author: actor, title: title, body: body, published: false }
    view Home at "/":
        box:
            text "posts: {count(Post)}"
            for p in Post where p.published || p.author == actor by id desc limit 20:
                text "{p.title}"
`

func TestAPIDoesNotServeAnotherActorsDraft(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, ownerScopedApp)

	if code, body := a.action("signup", "ada", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("write", "Nuclear codes", "0000"); code != 200 {
		t.Fatalf("write: %d %s", code, body)
	}

	// The author sees their own draft on the page.
	if code, html := a.get("/"); code != 200 || !strings.Contains(html, "Nuclear codes") {
		t.Fatalf("author's own page: %d, contains draft=%v", code, strings.Contains(html, "Nuclear codes"))
	}

	// A stranger: new cookie jar, never signed in.
	a.newSession()

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("stranger's page: %d", code)
	}
	if strings.Contains(html, "Nuclear codes") {
		t.Errorf("the WEB channel leaked the draft: %s", html)
	}

	code, body := a.get("/api/Post")
	t.Logf("GET /api/Post (unauthenticated) -> %d %s", code, body)
	if strings.Contains(body, "Nuclear codes") || strings.Contains(body, "0000") {
		t.Errorf("the JSON API served an unauthenticated stranger another actor's draft: %d %s", code, body)
	}
}

// Failing closed is only defensible if an app that genuinely wants a public feed
// can still have one. Publishing the entity is one setting, and it changes
// nothing else about the app.
func TestAPIServesAPublishedEntity(t *testing.T) {
	e := startEngine(t)
	t.Setenv("FACET_API_READ", "Post")
	a := startApp(t, e, ownerScopedApp)

	if code, body := a.action("signup", "ada", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("write", "Hello", "world"); code != 200 {
		t.Fatalf("write: %d %s", code, body)
	}

	a.newSession()
	code, body := a.get("/api/Post")
	if code != 200 {
		t.Fatalf("a published entity must serve: %d %s", code, body)
	}
	if !strings.Contains(body, "Hello") {
		t.Errorf("a published entity served no rows: %s", body)
	}
}

// A publication may carry a guard: the rows are served, but only to an actor the
// named zero-argument policy admits. This is the finest read rule the language
// can express about an entity today — per-row is what it still cannot say.
func TestAPIReadGuardIsEnforced(t *testing.T) {
	e := startEngine(t)
	t.Setenv("FACET_API_READ", "Post:member")
	a := startApp(t, e, ownerScopedApp)

	if code, body := a.action("signup", "ada", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("write", "Hello", "world"); code != 200 {
		t.Fatalf("write: %d %s", code, body)
	}

	// The author is a member, so the guard admits them.
	if code, body := a.get("/api/Post"); code != 200 || !strings.Contains(body, "Hello") {
		t.Errorf("a member was refused a guarded collection: %d %s", code, body)
	}

	// A stranger is a guest, and the guard does not admit a guest.
	a.newSession()
	if code, body := a.get("/api/Post"); code != 403 {
		t.Errorf("a guest must not read a member-guarded collection: %d %s", code, body)
	}
}
