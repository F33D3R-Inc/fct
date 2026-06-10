package fa

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Metrics holds process-wide counters/gauges for an FA app. Exposed as JSON at
// GET /debug/metrics (mounted by App.Mount). All fields are safe for concurrent
// use.
type Metrics struct {
	EventsIn    atomic.Int64 // client actions received at /events
	EventsOut   atomic.Int64 // events published to the broker
	ConnsActive atomic.Int64 // currently-open SSE connections (gauge)
	ConnsTotal  atomic.Int64 // SSE connections ever opened
	RateLimited atomic.Int64 // /events requests rejected (429)
	Forbidden   atomic.Int64 // /events requests rejected by guard/authz/CSRF (403)
}

func (m *Metrics) snapshot() map[string]int64 {
	return map[string]int64{
		"events_in":    m.EventsIn.Load(),
		"events_out":   m.EventsOut.Load(),
		"conns_active": m.ConnsActive.Load(),
		"conns_total":  m.ConnsTotal.Load(),
		"rate_limited": m.RateLimited.Load(),
		"forbidden":    m.Forbidden.Load(),
	}
}

// Metrics returns the app's live counters (read or expose them yourself).
func (a *App) Metrics() *Metrics { return a.metrics }

// mountObservability registers /healthz (liveness), /readyz (readiness), and
// /debug/metrics. Health is always 200 while serving; readiness is 503 after the
// app starts shutting down, so a load balancer drains it before SSE goroutines
// are cut.
func (a *App) mountObservability(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if a.draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ready"))
	})
	mux.HandleFunc("GET /debug/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.metrics.snapshot())
	})
}

// LogRequests wraps a handler with structured slog request logging (method, path,
// status, duration). Apps opt in by wrapping their mux. SSE/streaming paths are
// logged at connect; their long duration is expected.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		slog.Info("fa.request",
			"method", r.Method, "path", r.URL.Path,
			"status", sw.status, "dur_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r))
	})
}

// statusWriter captures the response status and supports SSE (http.Flusher).
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
