package runtime

// Observability — the operator's window into a running server. Three signals,
// one middleware:
//
//   - Structured logs (log/slog, JSON) — every request and every notable runtime
//     event is one machine-parseable line carrying the trace id, so logs join up
//     with traces in a log aggregator.
//   - Tracing (W3C Trace Context) — each request is a span with a 16-byte trace
//     id and an 8-byte span id; an inbound `traceparent` header is honored so a
//     Facet server slots into a distributed trace, and the id is propagated on the
//     request context to everything the request touches. Spans are emitted as
//     structured logs and, if FACET_OTLP_LOG is set, in an OTLP-shaped record.
//   - Metrics (Prometheus) — see metrics.go; the middleware feeds it.
//
// Plus the two endpoints every orchestrator probes: /healthz (liveness — the
// process is up) and /readyz (readiness — the database answers, so this instance
// can take traffic). A load balancer pulls an instance out of rotation the moment
// /readyz fails, which is what makes a rolling deploy safe.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// obs bundles the observability services threaded through every request.
type obs struct {
	log     *slog.Logger
	metrics *metrics
	level   slog.Level
}

// newObs builds the observability stack. FACET_LOG_LEVEL (debug|info|warn|error)
// sets verbosity; logs are JSON on stderr so a collector can parse them.
func newObs() *obs {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("FACET_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return &obs{log: slog.New(h), metrics: newMetrics(), level: level}
}

// OpsDescription summarizes the active operational posture for the startup
// banner: clustering, the durable job queue, the read cache, and the always-on
// observability endpoints.
func OpsDescription() string {
	parts := []string{"/healthz", "/readyz", "/metrics", "durable jobs"}
	if clusterEnabled() {
		parts = append([]string{"clustered (pub/sub + shared sessions)"}, parts...)
	} else {
		parts = append([]string{"single-process"}, parts...)
	}
	if os.Getenv("FACET_API_CACHE_TTL") != "" {
		parts = append(parts, "api cache")
	}
	return strings.Join(parts, " · ")
}

// ── tracing (W3C Trace Context) ───────────────────────────────────────────────

type ctxKey int

const spanKey ctxKey = 0

// span is one unit of traced work: a request, a job run, an action.
type span struct {
	traceID  string // 16 bytes, hex (32 chars)
	spanID   string // 8 bytes, hex (16 chars)
	parentID string // inbound parent span id, "" if root
	name     string
	start    time.Time
}

// startSpan opens a span, continuing the inbound trace if `traceparent` is
// present (version-traceid-spanid-flags) and starting a new trace otherwise.
func startSpan(ctx context.Context, name, traceparent string) (context.Context, *span) {
	sp := &span{name: name, spanID: randHex(8), start: time.Now()}
	if tp := parseTraceparent(traceparent); tp != nil {
		sp.traceID, sp.parentID = tp.traceID, tp.spanID
	} else {
		sp.traceID = randHex(16)
	}
	return context.WithValue(ctx, spanKey, sp), sp
}

// spanFrom returns the span carried on a context, or nil.
func spanFrom(ctx context.Context) *span {
	if sp, ok := ctx.Value(spanKey).(*span); ok {
		return sp
	}
	return nil
}

// traceparent serializes this span for an outbound request (so a call this
// server makes joins the same trace).
func (sp *span) traceparent() string {
	if sp == nil {
		return ""
	}
	return "00-" + sp.traceID + "-" + sp.spanID + "-01"
}

type traceContext struct{ traceID, spanID string }

// parseTraceparent reads a W3C `traceparent` header, returning nil if malformed.
func parseTraceparent(h string) *traceContext {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 {
		return nil
	}
	if !isHex(parts[1]) || !isHex(parts[2]) || parts[1] == strings.Repeat("0", 32) {
		return nil
	}
	return &traceContext{traceID: parts[1], spanID: parts[2]}
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// slog_err is the conventional error attribute, kept here so call sites that do
// not import log/slog (e.g. server.go) can still annotate a log line.
func slog_err(err error) slog.Attr { return slog.Any("error", err) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// A trace id that is not unique is still useful; fall back to the clock.
		t := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(t >> (8 * (i % 8)))
		}
	}
	return hex.EncodeToString(b)
}

// ── request middleware ────────────────────────────────────────────────────────

// statusRecorder captures the response status so the middleware can log and
// meter it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush forwards to the underlying writer so SSE streaming still works through
// the middleware wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// observe wraps a handler with the request span, structured access log, and
// Prometheus metrics. It is the outermost layer of the served handler.
func (o *obs) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, sp := startSpan(r.Context(), "http "+r.Method+" "+r.URL.Path, r.Header.Get("traceparent"))
		rec := &statusRecorder{ResponseWriter: w, status: 0}
		// Expose the trace id on the response so a caller (or a log line) can
		// correlate a request end-to-end.
		w.Header().Set("X-Trace-Id", sp.traceID)

		next.ServeHTTP(rec, r.WithContext(ctx))

		dur := time.Since(sp.start)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		route := routeLabel(r.URL.Path)
		o.metrics.observeRequest(r.Method, route, status, dur)
		o.log.LogAttrs(ctx, slog.LevelInfo, "http_request",
			slog.String("trace_id", sp.traceID),
			slog.String("span_id", sp.spanID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("duration", dur),
			slog.String("ip", clientIP(r)),
		)
		o.emitSpan(ctx, sp, map[string]any{
			"http.method":      r.Method,
			"http.target":      r.URL.Path,
			"http.status_code": status,
		})
	})
}

// routeLabel collapses a request path to a low-cardinality metric label so the
// metrics series count stays bounded (an unbounded label set melts Prometheus).
func routeLabel(path string) string {
	switch {
	case path == "/":
		return "/"
	case strings.HasPrefix(path, "/api/"):
		return "/api/*"
	case strings.HasPrefix(path, "/auth/"):
		return "/auth/*"
	default:
		return path
	}
}

// ── health / readiness ────────────────────────────────────────────────────────

// handleHealthz is the liveness probe: if the process can answer, it is alive.
// It never touches the database, so a slow database does not trigger a restart
// (that is readiness' job).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok", "app": s.ir.App})
}

// handleReadyz is the readiness probe: this instance is ready only if the
// database answers a ping. A load balancer routes traffic away from an instance
// whose /readyz fails, so a rolling deploy or a database hiccup sheds load
// gracefully instead of serving errors.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"status": "unavailable", "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"status": "ready"})
}

// otlpRecord renders a span in an OTLP-log-shaped JSON object. Emitted only when
// FACET_OTLP_LOG=1, for pipelines that ingest spans as logs.
func (o *obs) emitSpan(ctx context.Context, sp *span, attrs map[string]any) {
	if sp == nil || os.Getenv("FACET_OTLP_LOG") != "1" {
		return
	}
	rec := map[string]any{
		"traceId":           sp.traceID,
		"spanId":            sp.spanID,
		"parentSpanId":      sp.parentID,
		"name":              sp.name,
		"startTimeUnixNano": sp.start.UnixNano(),
		"endTimeUnixNano":   time.Now().UnixNano(),
		"attributes":        attrs,
	}
	if b, err := json.Marshal(rec); err == nil {
		o.log.LogAttrs(ctx, slog.LevelDebug, "span", slog.String("otlp", string(b)))
	}
}
