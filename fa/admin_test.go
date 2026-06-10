package fa

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testAdmin(authed bool) (*http.ServeMux, *Admin) {
	adm := NewAdmin("Acme").
		Authorize(func(r *http.Request) bool { return authed }).
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
	adm.Mount(mux, "/admin")
	return mux, adm
}

func get(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestAdminDeniesByDefault(t *testing.T) {
	// No Authorize set → every request denied (fail-safe).
	mux := http.NewServeMux()
	NewAdmin("X").Resource(AdminResource{Name: "x"}).Mount(mux, "/admin")
	if rec := get(mux, "/admin"); rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated admin should be 403, got %d", rec.Code)
	}
}

func TestAdminAuthGate(t *testing.T) {
	mux, _ := testAdmin(false)
	if rec := get(mux, "/admin"); rec.Code != http.StatusForbidden {
		t.Errorf("denied authorize should 403, got %d", rec.Code)
	}
}

func TestAdminDashboardListDetail(t *testing.T) {
	mux, _ := testAdmin(true)

	dash := get(mux, "/admin")
	if dash.Code != 200 || !strings.Contains(dash.Body.String(), "Dashboard") || !strings.Contains(dash.Body.String(), "Users") {
		t.Errorf("dashboard missing nav/resources:\n%s", dash.Body.String())
	}

	list := get(mux, "/admin/r/users")
	body := list.Body.String()
	for _, want := range []string{"adm-table", "Handle", "ada", "Ada Lovelace", `/admin/r/users/1`} {
		if !strings.Contains(body, want) {
			t.Errorf("list view missing %q", want)
		}
	}

	detail := get(mux, "/admin/r/users/1")
	db := detail.Body.String()
	if !strings.Contains(db, "ada") || !strings.Contains(db, "Users · 1") {
		t.Errorf("detail view wrong:\n%s", db)
	}
}

func TestAdminMetricsDashboard(t *testing.T) {
	m := &Metrics{}
	m.EventsIn.Add(7)
	adm := NewAdmin("Acme").Authorize(func(r *http.Request) bool { return true }).WithMetrics(m)
	mux := http.NewServeMux()
	adm.Mount(mux, "/admin")
	body := get(mux, "/admin").Body.String()
	if !strings.Contains(body, "events_in") || !strings.Contains(body, ">7<") {
		t.Errorf("metrics dashboard missing live counter:\n%s", body)
	}
}
