package fa

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestHistogramBucketsMatchArray(t *testing.T) {
	if len64 != len(histogramBuckets)+1 {
		t.Fatalf("len64 = %d, want len(histogramBuckets)+1 = %d", len64, len(histogramBuckets)+1)
	}
}

func TestHistogramObserve(t *testing.T) {
	var m Metrics
	m.DispatchSeconds.Observe(50 * time.Microsecond)  // < first bound (100µs) → bucket 0
	m.DispatchSeconds.Observe(100 * time.Microsecond) // == first bound: le is inclusive → bucket 0
	m.DispatchSeconds.Observe(2 * time.Millisecond)   // → le=0.0025
	m.DispatchSeconds.Observe(time.Minute)            // beyond all bounds → +Inf only

	rec := httptest.NewRecorder()
	m.servePrometheus(rec, nil)
	body := rec.Body.String()

	for _, want := range []string{
		`fa_dispatch_duration_seconds_bucket{le="0.0001"} 2`,
		`fa_dispatch_duration_seconds_bucket{le="0.0025"} 3`,
		`fa_dispatch_duration_seconds_bucket{le="10"} 3`,
		`fa_dispatch_duration_seconds_bucket{le="+Inf"} 4`,
		`fa_dispatch_duration_seconds_count 4`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestPrometheusEndpoint(t *testing.T) {
	app := New([]byte(`{}`))
	app.On("noop", func(c Ctx) ([]Event, error) { return nil, nil })
	mux := http.NewServeMux()
	app.Mount(mux)
	conn := testConn(app.hub, "")

	postEvent(t, mux, `{"type":"noop","payload":{},"conn":"`+conn.id+`"}`)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want Prometheus exposition format", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE fa_events_in_total counter",
		"fa_events_in_total 1",
		"# TYPE fa_sse_connections_active gauge",
		"fa_sse_connections_active 1",
		"# TYPE fa_dispatch_duration_seconds histogram",
		"fa_dispatch_duration_seconds_count 1",
		"# TYPE fa_fanout_duration_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in /metrics output", want)
		}
	}
}
