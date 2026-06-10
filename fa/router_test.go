package fa

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newRoutedApp() *App {
	app := New([]byte(`{}`), WithSigningKey([]byte("0123456789abcdef")))
	app.Route("/", "Home", func(rc RouteCtx) template.HTML {
		return "<h1>home</h1>"
	})
	app.Route("/u/:handle", "Profile", func(rc RouteCtx) template.HTML {
		return template.HTML("<h1>@" + template.HTMLEscapeString(rc.Param("handle")) + "</h1>")
	})
	app.Route("/posts/:id/edit", "Edit", func(rc RouteCtx) template.HTML {
		return template.HTML("<h1>edit " + template.HTMLEscapeString(rc.Param("id")) + "</h1>")
	})
	return app
}

func TestRouteMatchingAndParams(t *testing.T) {
	app := newRoutedApp()
	mux := http.NewServeMux()
	app.MountRouter(mux, ShellOptions{Title: "Base"})

	cases := []struct{ path, want string }{
		{"/", "<h1>home</h1>"},
		{"/u/ada", "<h1>@ada</h1>"},
		{"/posts/42/edit", "<h1>edit 42</h1>"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != 200 {
			t.Errorf("%s: status %d", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: missing %q in:\n%s", tc.path, tc.want, rec.Body.String())
		}
	}
}

func TestFullLoadWrapsShellWithRouteTitle(t *testing.T) {
	app := newRoutedApp()
	mux := http.NewServeMux()
	app.MountRouter(mux, ShellOptions{Title: "Base", Lang: "en"})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/u/ada", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",               // full document
		`<title>Profile</title>`,        // route title overrides base
		`data-facet-id="fa:root"`,       // root mount present
		`<script src="/fa-runtime.js">`, // runtime wired
		"<h1>@ada</h1>",                 // content
	} {
		if !strings.Contains(body, want) {
			t.Errorf("full load missing %q", want)
		}
	}
}

func TestClientNavReturnsFragment(t *testing.T) {
	app := newRoutedApp()
	mux := http.NewServeMux()
	app.MountRouter(mux, ShellOptions{Title: "Base"})

	req := httptest.NewRequest("GET", "/u/ada", nil)
	req.Header.Set("FA-Nav", "1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("nav response should be JSON, got %q", ct)
	}
	var frag struct{ Title, HTML string }
	if err := json.Unmarshal(rec.Body.Bytes(), &frag); err != nil {
		t.Fatalf("nav body not JSON: %v", err)
	}
	if frag.Title != "Profile" {
		t.Errorf("nav title = %q, want Profile", frag.Title)
	}
	if frag.HTML != "<h1>@ada</h1>" {
		t.Errorf("nav html = %q", frag.HTML)
	}
	// Critically: a nav response must NOT be a full document (no reload, SSE survives).
	if strings.Contains(frag.HTML, "<!doctype") || strings.Contains(frag.HTML, "fa-runtime.js") {
		t.Error("nav fragment leaked the document shell — would reload the page")
	}
}

func TestNativeRouteReturnsViewTree(t *testing.T) {
	app := New([]byte(`{}`), WithSigningKey([]byte("0123456789abcdef")))
	app.Route("/tip", "Tip", func(rc RouteCtx) template.HTML {
		return `<button class="fa-tip" data-action="tip.send" data-facet-id="TipButton"><span>Tip 100</span></button>`
	})
	mux := http.NewServeMux()
	app.MountRouter(mux, ShellOptions{})

	req := httptest.NewRequest("GET", "/tip", nil)
	req.Header.Set("FA-Native", "1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("native response should be JSON, got %q", ct)
	}
	var resp struct {
		Title string
		Tree  ViewNode
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("native body not JSON: %v", err)
	}
	if resp.Title != "Tip" {
		t.Errorf("title = %q", resp.Title)
	}
	// A native client builds a button view from this — no HTML, no WebView.
	if resp.Tree.Kind != "button" || resp.Tree.Action != "tip.send" {
		t.Errorf("tree root = kind %q action %q, want button/tip.send", resp.Tree.Kind, resp.Tree.Action)
	}
	if len(resp.Tree.Children) != 1 || resp.Tree.Children[0].Text != "Tip 100" {
		t.Errorf("tree child wrong: %+v", resp.Tree.Children)
	}
}

func TestUnmatchedRouteIs404(t *testing.T) {
	app := newRoutedApp()
	mux := http.NewServeMux()
	app.MountRouter(mux, ShellOptions{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/nope/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unmatched path status = %d, want 404", rec.Code)
	}
}

func TestCustomNotFound(t *testing.T) {
	app := newRoutedApp()
	app.NotFound(func(rc RouteCtx) template.HTML { return "<p>custom 404</p>" })
	mux := http.NewServeMux()
	app.MountRouter(mux, ShellOptions{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "custom 404") {
		t.Error("custom NotFound content not rendered")
	}
}

func TestRouterCoexistsWithMount(t *testing.T) {
	app := newRoutedApp()
	mux := http.NewServeMux()
	app.Mount(mux)                                   // /sse, /events, /manifest.json, /fa-runtime.js
	app.MountRouter(mux, ShellOptions{Title: "App"}) // catch-all GET /

	// The framework endpoints still win over the router catch-all.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/manifest.json", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("/manifest.json hijacked by router: %q", ct)
	}
	// A normal page still routes.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "<h1>home</h1>") {
		t.Error("router not serving / alongside Mount")
	}
}
