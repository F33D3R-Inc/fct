package fa

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAdminApp(authed bool) (*http.ServeMux, *App, *Admin) {
	app := New([]byte(`{}`), WithSigningKey([]byte("0123456789abcdef")))
	adm := NewAdmin("Acme").
		Authorize(func(r *http.Request) bool { return authed }).
		WithMetrics(app.Metrics()).
		Resource(AdminResource{
			Name: "users", Label: "Users", Columns: []string{"Handle", "Name"},
			List: func(ctx context.Context) ([]AdminRow, error) {
				return []AdminRow{{ID: "1", Cells: []string{"ada", "Ada Lovelace"}}}, nil
			},
			Get: func(ctx context.Context, id string) ([]AdminField, error) {
				return []AdminField{{Label: "Handle", Value: "ada"}, {Label: "ID", Value: id}}, nil
			},
		})
	mux := http.NewServeMux()
	app.Mount(mux)
	adm.Mount(app, mux, "/admin")
	return mux, app, adm
}

func adminGet(mux *http.ServeMux, path string, headers ...string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	mux.ServeHTTP(rec, r)
	return rec
}

func TestAdminDeniesByDefault(t *testing.T) {
	app := New([]byte(`{}`), WithSigningKey([]byte("0123456789abcdef")))
	mux := http.NewServeMux()
	NewAdmin("X").Resource(AdminResource{Name: "x"}).Mount(app, mux, "/admin")
	if rec := adminGet(mux, "/admin"); rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated admin should be 403, got %d", rec.Code)
	}
}

func TestAdminAuthGate(t *testing.T) {
	mux, _, _ := testAdminApp(false)
	if rec := adminGet(mux, "/admin"); rec.Code != http.StatusForbidden {
		t.Errorf("denied authorize should 403, got %d", rec.Code)
	}
}

// The admin is rendered from FACETS and served through the Playground shell — so
// the page carries the runtime, the live-subscribe hook, and facet-ids.
func TestAdminIsFacetGrade(t *testing.T) {
	mux, _, _ := testAdminApp(true)
	body := adminGet(mux, "/admin").Body.String()
	for _, want := range []string{
		"<!doctype html>",               // full Playground shell
		`<script src="/fa-runtime.js">`, // the runtime is present (live-capable)
		`data-fa-subscribe="fa.admin"`,  // dashboard subscribes to live metrics
		`data-facet-id="AdminLayout"`,   // rendered from facets
		`data-facet-id="AdminMetrics"`,  // the live metrics block has a target id
		"events_in", "conns_active",     // live system metrics
		"Users", // resource nav
	} {
		if !strings.Contains(body, want) {
			t.Errorf("FA-grade admin missing %q", want)
		}
	}
}

func TestAdminListAndDetail(t *testing.T) {
	mux, _, _ := testAdminApp(true)
	list := adminGet(mux, "/admin/r/users").Body.String()
	for _, want := range []string{"adm-table", "Handle", "ada", "Ada Lovelace", `/admin/r/users/1`, `data-facet-id="AdminTable"`} {
		if !strings.Contains(list, want) {
			t.Errorf("list view missing %q", want)
		}
	}
	detail := adminGet(mux, "/admin/r/users/1").Body.String()
	if !strings.Contains(detail, "ada") || !strings.Contains(detail, "Users · 1") {
		t.Errorf("detail view wrong:\n%s", detail)
	}
}

// The admin answers FA-Native with a neutral tree, so it renders in the iOS /
// Android apps too — not just the browser.
func TestAdminNativeAndNav(t *testing.T) {
	mux, _, _ := testAdminApp(true)
	native := adminGet(mux, "/admin", "FA-Native", "1")
	if ct := native.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("FA-Native admin should be JSON tree, got %q", ct)
	}
	if !strings.Contains(native.Body.String(), `"tree"`) {
		t.Error("FA-Native admin missing tree")
	}
	nav := adminGet(mux, "/admin/r/users", "FA-Nav", "1")
	if !strings.Contains(nav.Body.String(), `"html"`) {
		t.Error("FA-Nav admin should return an html fragment")
	}
}

// StartLiveMetrics pushes a metrics replace over the scoped admin channel.
func TestAdminLiveMetricsPush(t *testing.T) {
	app := New([]byte(`{}`), WithSigningKey([]byte("0123456789abcdef")))
	app.ChannelAuth(func(identity, channel string) bool { return channel == AdminChannel })
	adm := NewAdmin("Acme").WithMetrics(app.Metrics())

	c := &sseClient{id: "c", channels: map[string]bool{}, send: make(chan []byte, 4)}
	app.hub.register(c)
	app.hub.subscribe("c", AdminChannel)

	app.Metrics().EventsIn.Add(5)
	adm.StartLiveMetrics(app, 20*time.Millisecond)

	select {
	case frame := <-c.send:
		e := decodeFrame(t, frame)
		if e.Op != "replace" || e.FacetID != adminMetricsID {
			t.Errorf("expected replace of AdminMetrics, got op=%s id=%s", e.Op, e.FacetID)
		}
		if !strings.Contains(e.Fragment, "events_in") {
			t.Errorf("live metrics fragment missing data: %s", e.Fragment)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no live metrics push received on the admin channel")
	}
}
