package runtime

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// sessionCartApp models the storefront "anonymous cart" gap: an Item is keyed
// by `session` — the caller's session id — rather than by `actor`, so a
// never-logged-in visitor can accumulate state keyed to "this browser" before
// there is any `actor` to key it to. `session`, like `actor`/`role`/`tenant`,
// is a runtime-provided identity builtin usable anywhere those are: here it is
// written into an entity field from an action and read back in a view's
// `where` clause.
const sessionCartApp = `app Cart:
    entity Item:
        id: int
        session: text
        qty: int
    action addItem(qty: int):
        add Item { session: session, qty: qty }
    view Home at "/":
        box:
            for i in Item where i.session == session:
                text "qty {i.qty}"
`

// TestSessionBuiltinResolves is a compile-only check that `session` resolves as
// a valid identity reference the same way `actor`/`role`/`tenant` do, instead of
// failing with `unknown reference "session"` (the bug this change fixes).
func TestSessionBuiltinResolves(t *testing.T) {
	if _, err := compile.String(sessionCartApp); err != nil {
		t.Fatalf("`session` should resolve as a builtin identity reference like `actor`, got: %v", err)
	}
}

// TestUnknownSessionReferenceWasTheBug pins the pre-fix symptom so a future
// regression that removes `session` from the allowlist is caught here rather
// than only downstream: a genuinely unknown name must still be refused.
func TestUnknownSessionReferenceWasTheBug(t *testing.T) {
	_, err := compile.String(`app A:
    entity Item:
        id: int
    view Home at "/":
        box:
            text "{notARealBuiltin}"
`)
	if err == nil || !strings.Contains(err.Error(), `unknown reference "notARealBuiltin"`) {
		t.Fatalf("expected an unknown-reference error for a genuinely unknown name, got: %v", err)
	}
}

// TestAnonymousSessionScopesState is the live, HTTP-level proof of the whole
// feature: an anonymous, never-logged-in visitor gets a stable session id
// across requests (so repeated actions from the same browser accumulate into
// the same row set), and two different anonymous visitors — no shared cookies —
// get different, isolated session-scoped state.
func TestAnonymousSessionScopesState(t *testing.T) {
	g, err := compile.String(sessionCartApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	addItem := func(client *http.Client, qty string) {
		t.Helper()
		resp, err := client.Post(ts.URL+"/api/addItem", "application/json", strings.NewReader(`{"args":[`+qty+`]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("addItem returned %d: %s", resp.StatusCode, body)
		}
	}

	// Client A: a cookie-preserving anonymous browser, never logged in, that
	// visits twice.
	jarA, _ := cookiejar.New(nil)
	clientA := &http.Client{Jar: jarA}
	addItem(clientA, "1")
	addItem(clientA, "2")

	// Client B: a second anonymous browser that shares no cookies with A.
	jarB, _ := cookiejar.New(nil)
	clientB := &http.Client{Jar: jarB}
	addItem(clientB, "5")

	srv.mu.Lock()
	rows := append([]any{}, srv.entities["Item"]...)
	srv.mu.Unlock()
	if len(rows) != 3 {
		t.Fatalf("expected 3 Item rows total, got %d", len(rows))
	}
	sessionOf := func(i int) string {
		s, _ := rows[i].(map[string]any)["session"].(string)
		return s
	}
	sessA0, sessA1, sessB0 := sessionOf(0), sessionOf(1), sessionOf(2)

	if sessA0 == "" || sessB0 == "" {
		t.Fatalf("an anonymous, never-logged-in visitor must still get a non-empty session id: got %q and %q", sessA0, sessB0)
	}
	if sessA0 != sessA1 {
		t.Errorf("the same cookie-jar client should reuse the same session id across requests, got %q then %q", sessA0, sessA1)
	}
	if sessA0 == sessB0 {
		t.Error("two anonymous clients with no shared cookies must not resolve to the same session id")
	}

	// And from each anonymous visitor's own vantage point (the page each
	// renders): A's page shows only A's two items, B's page only B's one item —
	// neither ever having signed in or presented any identity but the cookie.
	get := func(client *http.Client) string {
		t.Helper()
		resp, err := client.Get(ts.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	// Count only rendered `fa-text` spans, not the raw literal "qty " that also
	// appears once, unrendered, inside the embedded IR template JSON.
	const rendered = `class="fa-text">qty `
	htmlA := get(clientA)
	if n := strings.Count(htmlA, rendered); n != 2 {
		t.Errorf("client A's page should render exactly its own 2 items, rendered %d: %s", n, htmlA)
	}
	htmlB := get(clientB)
	if n := strings.Count(htmlB, rendered); n != 1 {
		t.Errorf("client B's page should render exactly its own 1 item, rendered %d: %s", n, htmlB)
	}
}
