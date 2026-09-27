package main

import (
	"bytes"
	"strings"
	"testing"

	"facet/internal/compile"
)

// shadowedApp declares a view at /admin — the exact defect the storefront app
// hit: the runtime's generated admin console is registered at that path
// unconditionally (runtime/server.go), so this view is compiled successfully
// but never dispatched to. It also declares a view under /admin/ (prefix
// shadowing) and one at "/", which must NOT be flagged.
const shadowedApp = `app Shadow:
    view Home at "/":
        box:
            text "hi"

    view Admin at "/admin":
        box:
            text "custom admin, never served"

    view AdminSettings at "/admin/settings":
        box:
            text "also never served"
`

// TestRouteShadowing proves `facet routes` marks a view whose path the
// runtime's own fixed endpoints already own, loudly and by name, instead of
// reporting it as an ordinary served route.
func TestRouteShadowing(t *testing.T) {
	g, err := compile.String(shadowedApp)
	if err != nil {
		t.Fatalf("shadowedApp must compile: %v", err)
	}
	routes := buildRoutes(g, false)

	byPath := map[string]RouteEntry{}
	for _, r := range routes {
		byPath[r.Path] = r
	}

	for _, path := range []string{"/admin", "/admin/settings"} {
		r, ok := byPath[path]
		if !ok {
			t.Fatalf("expected a route for %s", path)
		}
		if r.Shadowed == "" {
			t.Errorf("%s: expected Shadowed to name the built-in that wins, got empty", path)
		}
		if !strings.Contains(r.Shadowed, "/admin") {
			t.Errorf("%s: Shadowed = %q, want it to name the /admin built-in", path, r.Shadowed)
		}
	}

	home, ok := byPath["/"]
	if !ok {
		t.Fatal("expected a route for /")
	}
	if home.Shadowed != "" {
		t.Errorf("/ must not be shadowed, got %q", home.Shadowed)
	}

	// The human table must say so loudly, not bury it in a note.
	var buf bytes.Buffer
	writeRoutesText(&buf, RouteReport{App: g.App, Routes: routes})
	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("routes table does not warn about the shadowed route:\n%s", out)
	}
}

// TestCheckRouteShadowing proves the same defect fails `facet doctor` — a dead
// route is a real defect, not merely something `routes` mentions in passing.
func TestCheckRouteShadowing(t *testing.T) {
	g, err := compile.String(shadowedApp)
	if err != nil {
		t.Fatalf("shadowedApp must compile: %v", err)
	}
	checks := checkRouteShadowing(g)
	if len(checks) != 2 {
		t.Fatalf("expected 2 shadowing findings (one per shadowed view), got %d: %+v", len(checks), checks)
	}
	for _, c := range checks {
		if c.State != statusFail {
			t.Errorf("a dead route must be statusFail, got %v (%s)", c.State, c.Detail)
		}
	}

	// A graph with no shadowed views must report nothing at all.
	clean, err := compile.String(sampleApp)
	if err != nil {
		t.Fatalf("sampleApp must compile: %v", err)
	}
	if got := checkRouteShadowing(clean); len(got) != 0 {
		t.Errorf("sampleApp declares no shadowed routes, got %d findings: %+v", len(got), got)
	}
}

// contractApp declares typed HTTP operations with `api` and publishes them
// with `contract`: the surface a native client is written against.
const contractApp = `app Contract:
    entity Account:
        id: int
        handle: text
    type SessionDTO:
        token: text
    policy member:
        actor != "guest"
    action signup(handle: text) -> SessionDTO:
        add Account { handle: handle }
        return SessionDTO{token: handle}
    action me() -> SessionDTO:
        requires member
        return SessionDTO{token: actor}
    action revokeSession(id: int):
        requires member
        remove Account(id)
    api POST "/api/v2/accounts" -> signup status 201:
        summary "Create an account."
    api GET "/api/v2/me" -> me:
        summary "The signed-in account."
    api DELETE "/api/v2/sessions/{id}" -> revokeSession status 204
    stream "/api/v2/events" requires member:
        ping: SessionDTO "A ping."
    contract "/api/v2/contract":
        title "Contract"
    view Home at "/":
        box:
            text "hi"
`

// TestRoutesListDeclaredContract proves `facet routes` lists every declared
// `api` operation and the published contract document. Before this, the
// table showed the generic /api/<Action> projection and the pages, and not
// one of the routes the app's own contract promised — the F33D3R API facet
// declares over a hundred and `facet routes` reported none of them.
func TestRoutesListDeclaredContract(t *testing.T) {
	g, err := compile.String(contractApp)
	if err != nil {
		t.Fatalf("contractApp must compile: %v", err)
	}
	routes := buildRoutes(g, false)

	type key struct{ method, path string }
	byKey := map[key]RouteEntry{}
	for _, r := range routes {
		if r.Kind == "contract" {
			byKey[key{r.Method, r.Path}] = r
		}
	}
	want := []struct {
		method, path, name, requires, note string
		params                             []string
	}{
		{"POST", "/api/v2/accounts", "signup", "", "→ 201", nil},
		{"GET", "/api/v2/me", "me", "member", "me()", nil},
		{"DELETE", "/api/v2/sessions/{id}", "revokeSession", "member", "→ 204", []string{"id"}},
		{"GET", "/api/v2/contract", "contract", "", "document", nil},
		{"GET", "/api/v2/contract/version", "contract", "", "version", nil},
		{"GET", "/api/v2/contract/history", "contract", "", "every version", nil},
		{"GET", "/api/v2/contract/diff", "contract", "", "changed", nil},
		{"GET", "/api/v2/events", "stream", "member", "server-sent events: ping", nil},
	}
	for _, w := range want {
		r, ok := byKey[key{w.method, w.path}]
		if !ok {
			t.Fatalf("expected a contract route %s %s, got %v", w.method, w.path, routes)
		}
		if r.Name != w.name {
			t.Errorf("%s %s: Name = %q, want %q", w.method, w.path, r.Name, w.name)
		}
		if r.Requires != w.requires {
			t.Errorf("%s %s: Requires = %q, want %q", w.method, w.path, r.Requires, w.requires)
		}
		if !strings.Contains(r.Note, w.note) {
			t.Errorf("%s %s: Note = %q, want it to mention %q", w.method, w.path, r.Note, w.note)
		}
		if strings.Join(r.Params, ",") != strings.Join(w.params, ",") {
			t.Errorf("%s %s: Params = %v, want %v", w.method, w.path, r.Params, w.params)
		}
	}

	// The contract section sits between the generic API projection and the
	// webhooks, and the human table names it.
	var buf bytes.Buffer
	writeRoutesText(&buf, RouteReport{App: g.App, Routes: routes})
	out := buf.String()
	api, contract := strings.Index(out, "\nAPI "), strings.Index(out, "\nCONTRACT ")
	if contract < 0 {
		t.Fatalf("the text table must have a CONTRACT section:\n%s", out)
	}
	if api < 0 || api > contract {
		t.Errorf("CONTRACT must follow the API section:\n%s", out)
	}
	if !strings.Contains(out, "POST   /api/v2/accounts") && !strings.Contains(out, "POST /api/v2/accounts") {
		t.Errorf("the table must list the declared operation:\n%s", out)
	}
}
