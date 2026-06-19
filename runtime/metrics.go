package runtime

// Metrics — a dependency-free Prometheus collector. The runtime increments
// counters and observes histograms as it works; /metrics renders them in the
// Prometheus text exposition format that any Prometheus, Grafana Agent, or
// OpenTelemetry Collector scrapes. Keeping this in-tree (rather than pulling the
// client library) keeps the toolchain a single static binary, which is the whole
// packaging story.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metrics holds every series the server exports, guarded by one mutex (the write
// rate is modest — one update per request/action/job — so a single lock is fine).
type metrics struct {
	mu sync.Mutex

	// HTTP
	reqTotal    map[string]int64 // "method|route|status" -> count
	reqDuration *histogram       // request latency seconds
	// actions
	actionTotal map[string]int64 // "action|outcome" -> count (outcome: ok|denied|error)
	// jobs
	jobTotal      map[string]int64 // "outcome" -> count (done|retry|dead)
	jobQueueDepth int64            // pending durable jobs (set by the worker)
	// live
	sseActive int64 // current SSE connections
	sessions  int64 // current sessions held in memory
	// startup
	start time.Time
}

func newMetrics() *metrics {
	return &metrics{
		reqTotal:    map[string]int64{},
		reqDuration: newHistogram([]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}),
		actionTotal: map[string]int64{},
		jobTotal:    map[string]int64{},
		start:       time.Now(),
	}
}

func (m *metrics) observeRequest(method, route string, status int, d time.Duration) {
	m.mu.Lock()
	m.reqTotal[method+"|"+route+"|"+strconv.Itoa(status)]++
	m.reqDuration.observe(d.Seconds())
	m.mu.Unlock()
}

func (m *metrics) observeAction(name, outcome string) {
	m.mu.Lock()
	m.actionTotal[name+"|"+outcome]++
	m.mu.Unlock()
}

func (m *metrics) observeJob(outcome string) {
	m.mu.Lock()
	m.jobTotal[outcome]++
	m.mu.Unlock()
}

func (m *metrics) setJobQueueDepth(n int64) { m.mu.Lock(); m.jobQueueDepth = n; m.mu.Unlock() }
func (m *metrics) addSSE(delta int64)       { m.mu.Lock(); m.sseActive += delta; m.mu.Unlock() }
func (m *metrics) setSessions(n int64)      { m.mu.Lock(); m.sessions = n; m.mu.Unlock() }

// histogram is a cumulative-bucket Prometheus histogram.
type histogram struct {
	bounds []float64
	counts []int64 // one per bound (cumulative rendered at write time)
	sum    float64
	count  int64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]int64, len(bounds))}
}

func (h *histogram) observe(v float64) {
	h.sum += v
	h.count++
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
}

// handleMetrics renders the Prometheus exposition format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := s.obs.metrics
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	writeHelp(&b, "facet_uptime_seconds", "gauge", "Seconds since the server started.")
	fmt.Fprintf(&b, "facet_uptime_seconds %g\n", time.Since(m.start).Seconds())

	writeHelp(&b, "facet_http_requests_total", "counter", "Total HTTP requests by method, route, and status.")
	for _, k := range sortedKeys(m.reqTotal) {
		p := strings.SplitN(k, "|", 3)
		fmt.Fprintf(&b, "facet_http_requests_total{method=%q,route=%q,status=%q} %d\n", p[0], p[1], p[2], m.reqTotal[k])
	}

	writeHelp(&b, "facet_http_request_duration_seconds", "histogram", "HTTP request latency.")
	var cumulative int64
	for i, bound := range m.reqDuration.bounds {
		cumulative = m.reqDuration.counts[i]
		fmt.Fprintf(&b, "facet_http_request_duration_seconds_bucket{le=%q} %d\n", trimFloat(bound), cumulative)
	}
	fmt.Fprintf(&b, "facet_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", m.reqDuration.count)
	fmt.Fprintf(&b, "facet_http_request_duration_seconds_sum %g\n", m.reqDuration.sum)
	fmt.Fprintf(&b, "facet_http_request_duration_seconds_count %d\n", m.reqDuration.count)

	writeHelp(&b, "facet_actions_total", "counter", "Server actions executed by name and outcome.")
	for _, k := range sortedKeys(m.actionTotal) {
		p := strings.SplitN(k, "|", 2)
		fmt.Fprintf(&b, "facet_actions_total{action=%q,outcome=%q} %d\n", p[0], p[1], m.actionTotal[k])
	}

	writeHelp(&b, "facet_jobs_total", "counter", "Durable job runs by outcome.")
	for _, k := range sortedKeys(m.jobTotal) {
		fmt.Fprintf(&b, "facet_jobs_total{outcome=%q} %d\n", k, m.jobTotal[k])
	}

	writeHelp(&b, "facet_job_queue_depth", "gauge", "Pending durable jobs waiting to run.")
	fmt.Fprintf(&b, "facet_job_queue_depth %d\n", m.jobQueueDepth)

	writeHelp(&b, "facet_sse_connections", "gauge", "Active server-sent-event connections on this instance.")
	fmt.Fprintf(&b, "facet_sse_connections %d\n", m.sseActive)

	writeHelp(&b, "facet_sessions", "gauge", "Sessions held in this instance's memory.")
	fmt.Fprintf(&b, "facet_sessions %d\n", m.sessions)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}

func writeHelp(b *strings.Builder, name, typ, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
