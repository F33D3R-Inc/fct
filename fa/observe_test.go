package fa

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthAndReadiness(t *testing.T) {
	app := New([]byte(`{}`))
	mux := http.NewServeMux()
	app.Mount(mux)

	get := func(path string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code
	}

	if get("/healthz") != 200 {
		t.Error("/healthz should be 200")
	}
	if get("/readyz") != 200 {
		t.Error("/readyz should be 200 before shutdown")
	}
	app.Shutdown() // begin draining
	if get("/readyz") != http.StatusServiceUnavailable {
		t.Error("/readyz should be 503 while draining")
	}
	if get("/healthz") != 200 {
		t.Error("/healthz should stay 200 while draining")
	}
}

func TestMetricsCounters(t *testing.T) {
	app := New([]byte(`{}`))
	app.On("noop", func(c Ctx) ([]Event, error) { return nil, nil })
	mux := http.NewServeMux()
	app.Mount(mux)
	conn := testConn(app.hub, "")

	// one good action, one rate-limited, one cross-origin
	postEvent(t, mux, `{"type":"noop","payload":{},"conn":"`+conn.id+`"}`)
	app.limiter = newRateLimiter(0, 0) // exhausted
	postEvent(t, mux, `{"type":"noop","payload":{},"conn":"`+conn.id+`"}`)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/debug/metrics", nil))
	if !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Error("metrics should be JSON")
	}
	var m map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("metrics not JSON: %v", err)
	}
	if m["events_in"] < 2 {
		t.Errorf("events_in = %d, want >= 2", m["events_in"])
	}
	if m["rate_limited"] < 1 {
		t.Errorf("rate_limited = %d, want >= 1", m["rate_limited"])
	}
	if m["conns_active"] != 1 {
		t.Errorf("conns_active = %d, want 1", m["conns_active"])
	}
}
