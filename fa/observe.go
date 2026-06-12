package fa

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

// Metrics holds process-wide counters/gauges for an FA app. Exposed as JSON at
// GET /debug/metrics and in Prometheus exposition format at GET /metrics (both
// mounted by App.Mount). All fields are safe for concurrent use.
type Metrics struct {
	EventsIn    atomic.Int64 // client actions received at /events
	EventsOut   atomic.Int64 // events published to the broker
	ConnsActive atomic.Int64 // currently-open SSE connections (gauge)
	ConnsTotal  atomic.Int64 // SSE connections ever opened
	RateLimited atomic.Int64 // /events requests rejected (429)
	Forbidden   atomic.Int64 // /events requests rejected by guard/authz/CSRF (403)

	// DispatchSeconds observes the full server-side cost of one client action:
	// guard + handler + emit (sign and publish to the broker).
	DispatchSeconds Histogram
	// FanoutSeconds observes one broker message being applied to this instance's
	// matching local connections (render-for-native + per-connection delivery).
	FanoutSeconds Histogram
}

// histogramBuckets are the upper bounds (seconds) of the latency histograms,
// 100µs–10s. Chosen around the measured baseline (~18µs/dispatch in-process)
// so real deployments resolve both the healthy range and the tail.
var histogramBuckets = []float64{
	.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10,
}

// Histogram is a fixed-bucket latency histogram safe for concurrent use, with
// Prometheus histogram semantics (cumulative buckets + _sum + _count). The
// zero value is ready to use.
type Histogram struct {
	counts [len64]atomic.Int64 // one per bucket, +1 for +Inf
	sum    atomic.Uint64       // float64 bits of the running sum (CAS-updated)
}

// len64 sizes Histogram.counts: one slot per declared bucket plus +Inf.
const len64 = 17

// Observe records one duration.
func (h *Histogram) Observe(d time.Duration) {
	s := d.Seconds()
	i := sort.SearchFloat64s(histogramBuckets, s) // first bucket with bound >= s
	h.counts[i].Add(1)
	for {
		old := h.sum.Load()
		next := math.Float64bits(math.Float64frombits(old) + s)
		if h.sum.CompareAndSwap(old, next) {
			return
		}
	}
}

// write emits the histogram in Prometheus exposition format.
func (h *Histogram) write(w *promWriter, name, help string) {
	w.header(name, help, "histogram")
	var cum int64
	for i, bound := range histogramBuckets {
		cum += h.counts[i].Load()
		fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, formatBound(bound), cum)
	}
	cum += h.counts[len(histogramBuckets)].Load()
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, cum)
	fmt.Fprintf(w, "%s_sum %g\n", name, math.Float64frombits(h.sum.Load()))
	fmt.Fprintf(w, "%s_count %d\n", name, cum)
}

func formatBound(b float64) string { return strconv.FormatFloat(b, 'g', -1, 64) }

// promWriter accumulates Prometheus text-format output.
type promWriter struct{ http.ResponseWriter }

func (w *promWriter) header(name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (w *promWriter) counter(name, help string, v int64) {
	w.header(name, help, "counter")
	fmt.Fprintf(w, "%s %d\n", name, v)
}

func (w *promWriter) gauge(name, help string, v int64) {
	w.header(name, help, "gauge")
	fmt.Fprintf(w, "%s %d\n", name, v)
}

// servePrometheus writes every metric in Prometheus exposition format
// (text/plain; version=0.0.4) — scrapeable without any client library.
func (m *Metrics) servePrometheus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	p := &promWriter{w}
	p.counter("fa_events_in_total", "Client actions received at /events.", m.EventsIn.Load())
	p.counter("fa_events_out_total", "Events published to the broker.", m.EventsOut.Load())
	p.gauge("fa_sse_connections_active", "Currently-open SSE connections.", m.ConnsActive.Load())
	p.counter("fa_sse_connections_total", "SSE connections ever opened.", m.ConnsTotal.Load())
	p.counter("fa_events_rate_limited_total", "Requests to /events rejected by the per-IP rate limit (429).", m.RateLimited.Load())
	p.counter("fa_events_forbidden_total", "Requests to /events rejected by guard/authz/CSRF (403).", m.Forbidden.Load())
	m.DispatchSeconds.write(p, "fa_dispatch_duration_seconds", "Server-side cost of one client action: guard + handler + emit.")
	m.FanoutSeconds.write(p, "fa_fanout_duration_seconds", "One broker message applied to this instance's local connections.")
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

// mountObservability registers /healthz (liveness), /readyz (readiness),
// /debug/metrics (JSON), and /metrics (Prometheus exposition format). Health is
// always 200 while serving; readiness is 503 after the
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
	mux.HandleFunc("GET /metrics", a.metrics.servePrometheus)
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
