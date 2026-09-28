package selfhost

// The runtime parity harness: one compiled app served by the Go runtime
// (runtime.Server over httptest) and by the runtime written in fct
// (runtime_server.fct, loaded with the same graph's JSON through
// ir_graph.fct and served over http_server.fct), the same scripted
// requests fired at both — pages, the stylesheet and client script, the
// JSON API schema and refusals, the built-in auth actions, server actions
// over /api and /event, a live SSE subscription receiving the hello frame
// and a change — and every answer compared: status, the headers that
// matter, and the body byte for byte after the bytes neither side can make
// deterministic are masked (session and CSRF tokens, visitor keys, minted
// verify/reset tokens, `now()` timestamps and their `ago` spellings).
//
// The apps are the facets/ ones, smallest first. A step that the fct port
// cannot yet answer identically fails the test; nothing is skipped.
//
// Language and runtime gaps this port found, and what was done about them:
//
//   - A runtime in fct could not sign a cookie or store a password: no
//     HMAC, no base64url, no bcrypt (sha256Hex is hex-over-text, and
//     verifyPassword admits only a @password FIELD as its hash). Added as
//     proc builtins in Go — sha256Bytes, hmacSha256, base64UrlRaw,
//     bcryptHash, bcryptMatches (runtime/cryptobuiltins.go, pinned to
//     secret.go by TestCryptoBuiltinsMatchSecretGo; ToolchainVersion
//     1.39.0). The compiler port's own builtin tables (expr_tree.fct,
//     check.fct, proc_lower.fct — compiler team) are to list them too.
//   - A `do` result cannot be assigned into a field (`x.f = do g()` is
//     "invalid assignment target"); every such site binds a local first
//     (the `dvN` temporaries). To be replaced by the direct form once the
//     compiler admits it.
//   - The proc engine is ported (runtime_eval_ir.fct's rtRunProc): an
//     action's `do <proc>` and a proc named inside an expression both run
//     in fct, with the Go engine's index/field/unknown-proc errors and the
//     500s they become. spawn/join, file statements, `act` and service
//     `call`s inside procs are not (no concurrency or I/O in a request).
//   - Ported and in the scripts: declared streams (auth, policy, rate,
//     Last-Event-ID replay, connect/disconnect hooks, `emit … to … on …`
//     fan-out through per-stream logs capped at 512; a subscriber never
//     receives its own join, because a hook's events fan out before it is
//     registered) and inbound webhooks (HMAC signature, idempotent replay
//     of success and refusal, system-run action) — streams_webhooks.fct;
//     multipart `bytes` bodies, `form` bodies and HTTP Basic on declared
//     routes — api_decl.fct; signed media (mediaSig, minted src/poster,
//     grant maps on uploads, page bootstraps, region answers and live
//     frames), http.ServeFile's Range / multi-range / 416 /
//     If-Modified-Since, and HLS playlist signing — upload_media.fct,
//     TestRuntimeParitySignedMedia; runReserved's claim on the tenancy and
//     billing action names (501 while disabled).
//   - The Go runtime has no OUTBOUND webhook delivery (webhook.go is the
//     inbound endpoint only), so there is no retry/signature behaviour of
//     delivery to port or to compare against.
//   - Not yet ported: enabled multi-tenancy and billing (FACET_MULTI_TENANT
//     / FACET_BILLING: their actions answer 501 here), /assets/, /admin,
//     /metrics, i18n catalogs, HLS segment sniffing for unknown extensions
//     (DetectContentType), If-Range, proc spawn/join and file statements,
//     aggregate pushdown to the engine (values are the same either way),
//     clustering.
//
import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

var rtParityApps = []string{
	"../../facets/layered_demo.fct",
	"../../facets/f33d3r_com.fct",
	"../../facets/api/main.fct",
}

// rtSide is one running runtime: where to send requests, and the cookie
// jar its browser-like client keeps between them.
type rtSide struct {
	name   string
	base   string
	client *http.Client
	// What this side minted for itself, replayed into later steps: its own
	// bearer token (the last `"token":"…"` it answered) and the ETag of its
	// last conditional GET. The two sides never share these — each is
	// asked with what it issued.
	token string
	etag  string
	// the last stored upload's reference and the last resumable session id
	upload   string
	uploadID string
	// the last page cursor this side answered (opaque: an engine's own)
	next string
	// signed media: each upload's reference and the signed URL this side
	// minted for it, in upload order (SIDE_REF_n / SIDE_SIGNED_n)
	refs   []string
	signed []string
	// ticking: the app declares an `every Ns` job, so what that job's
	// ticks advance — the page's write sequence, the job counters — is a
	// clock reading, compared masked (rtTicks)
	ticking bool
	// the CSRF token last read off a page, and the session it belongs to
	csrf, csrfSid string
	// tickActions: the actions a tick runs (the job's, and those it runs)
	tickActions map[string]bool
}

// rtMultipartType and rtMultipart build one-file multipart bodies with a
// fixed boundary, so both sides receive the same bytes.
const rtMultipartType = "multipart/form-data; boundary=facetparity"

// rtMultipartParts builds a multipart body of several parts, each
// {field, filename ("" for a plain value), content}.
func rtMultipartParts(parts ...[3]string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("--facetparity\r\nContent-Disposition: form-data; name=\"" + p[0] + "\"")
		if p[1] != "" {
			b.WriteString("; filename=\"" + p[1] + "\"\r\nContent-Type: application/octet-stream")
		}
		b.WriteString("\r\n\r\n" + p[2] + "\r\n")
	}
	b.WriteString("--facetparity--\r\n")
	return b.String()
}

func rtMultipart(field, filename, content string) string {
	return "--facetparity\r\nContent-Disposition: form-data; name=\"" + field + "\"; filename=\"" + filename + "\"\r\nContent-Type: text/plain\r\n\r\n" + content + "\r\n--facetparity--\r\n"
}

var (
	rtFctGraphOnce sync.Once
	rtFctGraph     *ir.IR
	rtFctGraphErr  error
)

// rtFctRuntimeGraph compiles runtime_server.fct once per test process.
func rtFctRuntimeGraph(t *testing.T) *ir.IR {
	t.Helper()
	rtFctGraphOnce.Do(func() {
		rtFctGraph, rtFctGraphErr = compile.File("runtime_server.fct")
	})
	if rtFctGraphErr != nil {
		t.Fatalf("compile selfhost/runtime_server.fct: %v", rtFctGraphErr)
	}
	return rtFctGraph
}

// rtStartGo serves the app with the Go runtime.
func rtStartGo(t *testing.T, g *ir.IR) *rtSide {
	t.Helper()
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Shutdown() })
	// As `facet serve` does: the `on start` jobs and the daemons run.
	srv.StartJobs()
	return &rtSide{name: "go", base: ts.URL, client: rtClient(t)}
}

// rtStartFct serves the app with the fct runtime: the graph's JSON and the
// client script are put in the port's data directory, the port is
// configured through the port's own /api, and its daemon listens.
func rtStartFct(t *testing.T, g *ir.IR) *rtSide {
	t.Helper()
	dir := os.Getenv("FACET_DATA_DIR")
	irJSON, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.ir.json"), irJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	// The client the Go runtime serves, byte for byte: the build's embedded
	// copy, not the file on disk, which may be mid-edit.
	js := runtime.ClientJS()
	if err := os.WriteFile(filepath.Join(dir, "facet.js"), js, 0o644); err != nil {
		t.Fatal(err)
	}
	// The base stylesheet too: the Go build's own copy, which the fct
	// runtime serves ahead of the app's theme and CSS (runtime.BaseCSS).
	if err := os.WriteFile(filepath.Join(dir, "facet-base.css"), []byte(runtime.BaseCSS()), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewProgram(rtFctRuntimeGraph(t))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDataDir(dir)
	// RT_PARITY_PROCS=1 with -cpuprofile: every sample carries the fct proc
	// it was taken in (`go tool pprof -tags`), a profile of the runtime
	// rather than of the evaluator.
	if os.Getenv("RT_PARITY_PROCS") != "" {
		srv.SetProfiling(true)
	}
	// The runtime binds port 0 and says which port it got: the harness
	// reads that off its stdout rather than picking a port and dialing
	// until something answers.
	banner := &rtBanner{port: make(chan int, 1)}
	srv.SetStdio(strings.NewReader(""), banner, os.Stderr)
	ts := httptest.NewServer(srv.Handler())
	// The program ends with the test: its daemon, its tickers and its
	// sockets (Shutdown halts them), not only the host's HTTP front.
	t.Cleanup(func() { ts.Close(); srv.Shutdown() })
	postExprJSON(t, ts, "rtConfigure", 0, "app.ir.json")
	srv.StartJobs()
	// The port parses the whole graph in fct before it listens: a budget
	// per megabyte of IR (facets/api/main.fct is ~4 MB), on top of a floor.
	budget := 20*time.Second + time.Duration(len(irJSON)/(1<<20)+1)*15*time.Second
	var addr string
	select {
	case port := <-banner.port:
		addr = fmt.Sprintf("127.0.0.1:%d", port)
	case <-time.After(budget):
		t.Fatalf("the fct runtime never listened (no banner in %v)", budget)
	}
	return &rtSide{name: "fct", base: "http://" + addr, client: rtClient(t)}
}

// rtBanner is the fct runtime's stdout: passed through, and the port its
// banner ("Facet runtime serving <app> on port <n>") names handed over once.
type rtBanner struct {
	mu   sync.Mutex
	buf  strings.Builder
	port chan int
	sent bool
}

var rtBannerRE = regexp.MustCompile(`Facet runtime serving \S+ on port ([0-9]+)`)

func (b *rtBanner) Write(p []byte) (int, error) {
	os.Stdout.Write(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.sent {
		b.buf.Write(p)
		if m := rtBannerRE.FindStringSubmatch(b.buf.String()); m != nil {
			n, _ := strconv.Atoi(m[1])
			b.port <- n
			b.sent = true
		}
	}
	return len(p), nil
}

func rtClient(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// rtStep is one scripted request. `csrf` asks the harness to attach the
// side's own CSRF token (read off its last page) as X-Facet-CSRF.
type rtStep struct {
	name    string
	method  string
	path    string
	body    string
	headers map[string]string
	csrf    bool
	// sign, when set, is the webhook key the body is signed with
	// (X-Facet-Signature: hex HMAC-SHA256 of the body).
	sign string
}

// rtAnswer is what one side answered, reduced to what is compared.
type rtAnswer struct {
	status      int
	contentType string
	cookieAttrs string
	body        string
	replay      string // X-Facet-Idempotent-Replay
	lang        string // Content-Language (the negotiated locale)
}

var (
	rtCsrfMeta   = regexp.MustCompile(`<meta name="fa-csrf" content="[^"]*">`)
	rtSessionKey = regexp.MustCompile(`"session":"[A-Za-z0-9_-]*"`)
	rtCreated    = regexp.MustCompile(`"created":[0-9]+`)
	rtTokens     = regexp.MustCompile(`"(token|verifyToken|resetToken)":"[^"]*"`)
	rtCookieVal  = regexp.MustCompile(`fa_sid=[^;]*`)
	rtCookieExp  = regexp.MustCompile(`Expires=[^;]*`)
	rtNextCursor = regexp.MustCompile(`"next":"([^"]*)"`)
	rtUploadURL  = regexp.MustCompile(`"url":"(/uploads/[0-9a-f]{32}[^"]*)"`)
	// a stored upload's reference wherever a reply carries it (a track's
	// audio, a cover): the name is 16 random bytes on each side
	rtUploadRef = regexp.MustCompile(`/uploads/[0-9a-f]{32}`)
	// a signed media link's expiry and signature (a clock reading and its
	// HMAC), wherever it is spelled: raw, HTML-escaped or JSON-escaped
	rtSignedPart = regexp.MustCompile(`exp=[0-9]+(&amp;|&|\\u0026)sig=[0-9a-f]{64}`)
	rtGrant      = regexp.MustCompile(`"(/uploads/[0-9a-f]{32}[^"?]*)\?exp=([0-9]+)\\u0026sig=([0-9a-f]{64})"`)
	// a multipart/byteranges boundary: 30 random bytes, hex
	rtBoundary   = regexp.MustCompile(`[0-9a-f]{60}`)
	rtUploadID   = regexp.MustCompile(`"id":"([0-9a-f]{32})"`)
	rtTokenValue = regexp.MustCompile(`"token":"([^"]*)"`)
	rtISOTime    = regexp.MustCompile(`"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z?"`)
	// Any epoch-sized number is a clock reading (`now()` in a row, a state
	// cell's default, a session's expiry): the two runtimes read their
	// clocks at different instants, so the value is masked and the key kept.
	rtEpochKeys = regexp.MustCompile(`"([A-Za-z_]+)":[0-9]{9,}`)
	// A value an app mints with rand() (a stream key) cannot agree across
	// two processes; its shape is what both sides must share.
	rtRandKey = regexp.MustCompile(`"key":"([a-z_]+)[0-9]+"`)
)

// rtNormalizePage is rtNormalize for a whole response, leaving a page's
// bootstrap IR (the `fa-ir` script: the page's slice of the compiled graph,
// runtime/pageir.go — compiled output, the same bytes on both sides for the
// same page, holding nothing a mask is for) compared as it is.
func rtNormalizePage(s string) string {
	const open = `<script type="application/json" id="fa-ir">`
	i := strings.Index(s, open)
	if i < 0 {
		return rtNormalize(s)
	}
	j := strings.Index(s[i:], "</script>")
	if j < 0 {
		return rtNormalize(s)
	}
	j += i
	return rtNormalize(s[:i+len(open)]) + s[i+len(open):j] + rtNormalize(s[j:])
}

func rtNormalize(s string) string {
	s = rtCsrfMeta.ReplaceAllString(s, `<meta name="fa-csrf" content="CSRF">`)
	s = rtSessionKey.ReplaceAllString(s, `"session":"VISITOR"`)
	s = rtCreated.ReplaceAllString(s, `"created":0`)
	s = rtTokens.ReplaceAllString(s, `"$1":"TOKEN"`)
	s = rtISOTime.ReplaceAllString(s, `"ISO"`)
	s = rtEpochKeys.ReplaceAllString(s, `"$1":0`)
	s = rtRandKey.ReplaceAllString(s, `"key":"${1}RAND"`)
	// A page cursor encodes the last row's order value, a clock reading
	// when the order is `created`; it is captured for replay, not compared.
	s = rtNextCursor.ReplaceAllString(s, `"next":"NEXT"`)
	s = rtUploadURL.ReplaceAllString(s, `"url":"/uploads/NAME"`)
	s = rtUploadID.ReplaceAllString(s, `"id":"ID"`)
	s = rtUploadRef.ReplaceAllString(s, "/uploads/NAME")
	s = rtSignedPart.ReplaceAllString(s, "exp=EXP${1}sig=SIG")
	s = rtBoundary.ReplaceAllString(s, "BOUNDARY")
	s = rtTimings.ReplaceAllString(s, "$1 T")
	s = rtFormCsrf.ReplaceAllString(s, `name="_csrf" value="CSRF"`)
	s = rtTombstone.ReplaceAllString(s, "erased-TOMB")
	s = rtAgo.ReplaceAllString(s, "${1}AGO")
	return s
}

// rtSeq, rtJobCounts: what an `every Ns` job's ticks advance.
var (
	rtSeq        = regexp.MustCompile(`"@seq":[0-9]+`)
	rtQueueDepth = regexp.MustCompile(`(?m)^(facet_job_queue_depth) [0-9]+$`)
	rtJobLine    = regexp.MustCompile(`(?m)^facet_jobs_total\{[^}]*\} [0-9]+\n`)
	rtActCount   = regexp.MustCompile(`(?m)^(facet_actions_total\{action="([^"]*)",outcome="[^"]*"\}) [0-9]+\n`)
)

// rtTickMask masks, for an app with periodic jobs, what depends on how many
// ticks a side has seen by the time it answers.
func rtTickMask(side *rtSide, body string) string {
	if !side.ticking {
		return body
	}
	body = rtSeq.ReplaceAllString(body, `"@seq":SEQ`)
	body = rtQueueDepth.ReplaceAllString(body, "$1 N")
	// A periodic job's series exist only once its first tick has run, and
	// each runtime's ticks are timed from its own start (the fct runtime
	// boots after the Go one, and Go's tick waits on its queue's worker
	// poll): whether the line is there at all is a clock reading, not only
	// its count. They are left out, as the rows a tick writes are left out
	// of the engine comparison (rtTickWritten).
	body = rtJobLine.ReplaceAllString(body, "")
	return rtActCount.ReplaceAllStringFunc(body, func(line string) string {
		m := rtActCount.FindStringSubmatch(line)
		if side.tickActions[m[2]] {
			return ""
		}
		return line
	})
}

// rtLiveSeq masks a live frame's "seq" for an app whose ticks advance it.
func rtLiveSeq(side *rtSide, frame string) string {
	if !side.ticking {
		return frame
	}
	return rtFrameSeq.ReplaceAllString(frame, `"seq":SEQ`)
}

var rtFrameSeq = regexp.MustCompile(`"seq":[0-9]+`)

// rtHasPeriodicJobs reports whether the app declares a scheduled job.
func rtHasPeriodicJobs(g *ir.IR) bool {
	for _, j := range g.Jobs {
		if j.Every > 0 || j.OnStart {
			return true
		}
	}
	return false
}

// rtTickWritten: the entities an `every Ns` job's action writes (through
// the actions it runs too) — their rows depend on the tick count.
func rtTickWritten(g *ir.IR) map[string]bool {
	written, _ := rtTickReach(g)
	return written
}

// rtTickReach: what a tick writes, and which actions it runs.
func rtTickReach(g *ir.IR) (map[string]bool, map[string]bool) {
	byName := map[string]*ir.Action{}
	for i := range g.Actions {
		byName[g.Actions[i].Name] = &g.Actions[i]
	}
	out := map[string]bool{}
	seen := map[string]bool{}
	var walk func(body []ir.Stmt)
	walk = func(body []ir.Stmt) {
		for _, st := range body {
			if st.Entity != "" {
				out[st.Entity] = true
			}
			if st.Op == "run" && !seen[st.Service] && byName[st.Service] != nil {
				seen[st.Service] = true
				walk(byName[st.Service].Body)
			}
			walk(st.Body)
			walk(st.Else)
		}
	}
	for _, j := range g.Jobs {
		// every scheduled action — an `every Ns` tick's and an `on start`
		// job's alike: what the scheduler runs, how often and when, is not a
		// step of the script (a boot count, a clock), so neither its counts
		// nor its rows are compared
		if (j.Every > 0 || j.OnStart) && byName[j.Action] != nil && !seen[j.Action] {
			seen[j.Action] = true
			walk(byName[j.Action].Body)
		}
	}
	return out, seen
}

// rtMarkTicking tells both sides what the app's periodic jobs advance.
func rtMarkTicking(g *ir.IR, sides ...*rtSide) {
	_, acts := rtTickReach(g)
	for _, sd := range sides {
		sd.ticking = rtHasPeriodicJobs(g)
		sd.tickActions = acts
	}
}

// rtAgo: an `ago(ts)` a page renders after a middle dot ("· now", "· 3m",
// "· 2h") — a clock reading: the same row rendered a moment later by the
// other side can cross a minute.
var rtAgo = regexp.MustCompile(`(· )(now|[0-9]{1,2}[mh])\b`)

// rtTombstone: the random identifier a GDPR erasure writes in place of
// the subject's.
var rtTombstone = regexp.MustCompile(`erased-[0-9a-f]{12}`)

// rtFormCsrf: an admin form's CSRF field, its own session's token.
var rtFormCsrf = regexp.MustCompile(`name="_csrf" value="[^"]*"`)

// rtTimings: the /metrics samples that are clock readings (uptime, request
// latency buckets and sum), not counts.
var rtTimings = regexp.MustCompile(`(?m)^(facet_uptime_seconds|facet_http_request_duration_seconds_sum|facet_http_request_duration_seconds_bucket\{le="[0-9.]+"\}) .*$`)

func rtDo(t *testing.T, side *rtSide, st rtStep, csrf string) rtAnswer {
	t.Helper()
	a, err := rtDoErr(side, st, csrf)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// rtDoPair sends one step to both sides at once — they are separate
// servers, and a step's two requests share nothing but the script — and
// reports how long each took.
func rtDoPair(t *testing.T, goSide, fctSide *rtSide, goStep, fctStep rtStep, goCsrf, fctCsrf string) (rtAnswer, rtAnswer, time.Duration, time.Duration) {
	t.Helper()
	var ga rtAnswer
	var gerr error
	var gd time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		t0 := time.Now()
		ga, gerr = rtDoErr(goSide, goStep, goCsrf)
		gd = time.Since(t0)
	}()
	t1 := time.Now()
	fa, ferr := rtDoErr(fctSide, fctStep, fctCsrf)
	fd := time.Since(t1)
	<-done
	if gerr != nil {
		t.Fatal(gerr)
	}
	if ferr != nil {
		t.Fatal(ferr)
	}
	return ga, fa, gd, fd
}

// rtDoErr is rtDo for any goroutine: a transport failure is returned.
func rtDoErr(side *rtSide, st rtStep, csrf string) (rtAnswer, error) {
	path := strings.ReplaceAll(st.path, "SIDE_UPLOAD_ID", side.uploadID)
	path = strings.ReplaceAll(path, "SIDE_UPLOAD", side.upload)
	body := strings.ReplaceAll(st.body, "SIDE_CSRF", csrf)
	for i := len(side.refs) - 1; i >= 0; i-- {
		n := strconv.Itoa(i)
		path = strings.ReplaceAll(path, "SIDE_SIGNED_"+n, side.signed[i])
		path = strings.ReplaceAll(path, "SIDE_REF_"+n, side.refs[i])
		body = strings.ReplaceAll(body, "SIDE_REF_"+n, side.refs[i])
	}
	req, err := http.NewRequest(st.method, side.base+path, strings.NewReader(body))
	if err != nil {
		return rtAnswer{}, err
	}
	for k, v := range st.headers {
		v = strings.ReplaceAll(v, "SIDE_TOKEN", side.token)
		v = strings.ReplaceAll(v, "SIDE_ETAG", side.etag)
		req.Header.Set(k, v)
	}
	if st.body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if st.csrf {
		req.Header.Set("X-Facet-CSRF", csrf)
	}
	if st.sign != "" {
		mac := hmac.New(sha256.New, []byte(st.sign))
		mac.Write([]byte(st.body))
		req.Header.Set("X-Facet-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := side.client.Do(req)
	if err != nil {
		return rtAnswer{}, fmt.Errorf("%s: %s %s: %v", side.name, st.method, st.path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return rtAnswer{}, fmt.Errorf("%s: %s %s: reading body: %v", side.name, st.method, st.path, err)
	}
	if m := rtTokenValue.FindSubmatch(b); m != nil {
		side.token = string(m[1])
	}
	// adoptSession hands a re-keyed session's signed id back as
	// X-Session-Token, the bearer a native client keeps.
	if tok := resp.Header.Get("X-Session-Token"); tok != "" {
		side.token = tok
	}
	if m := rtNextCursor.FindSubmatch(b); m != nil {
		side.next = string(m[1])
	}
	if e := resp.Header.Get("ETag"); e != "" {
		side.etag = e
	}
	if m := rtUploadURL.FindSubmatch(b); m != nil {
		side.upload = string(m[1])
	}
	if st.path == "/upload" {
		if m := rtGrant.FindSubmatch(b); m != nil {
			side.refs = append(side.refs, string(m[1]))
			side.signed = append(side.signed, string(m[1])+"?exp="+string(m[2])+"&sig="+string(m[3]))
		}
	}
	if m := rtUploadID.FindSubmatch(b); m != nil {
		side.uploadID = string(m[1])
	}
	attrs := ""
	for _, c := range resp.Header.Values("Set-Cookie") {
		c = rtCookieVal.ReplaceAllString(c, "fa_sid=SID")
		c = rtCookieExp.ReplaceAllString(c, "Expires=EXP")
		attrs += c + "\n"
	}
	return rtAnswer{status: resp.StatusCode, contentType: rtBoundary.ReplaceAllString(resp.Header.Get("Content-Type"), "BOUNDARY"),
		cookieAttrs: attrs, body: rtTickMask(side, rtNormalizePage(string(b))), replay: resp.Header.Get("X-Facet-Idempotent-Replay"),
		lang: resp.Header.Get("Content-Language")}, nil
}

// rtCsrfOf reads the CSRF token a side stamped on the page it last served.
func rtCsrfOf(t *testing.T, side *rtSide) string {
	t.Helper()
	// The token is a function of the session alone: read it off a page
	// once per session, not once per step.
	sid := ""
	if u, err := url.Parse(side.base + "/"); err == nil {
		for _, c := range side.client.Jar.Cookies(u) {
			if c.Name == "fa_sid" {
				sid = c.Value
			}
		}
	}
	if sid != "" && sid == side.csrfSid {
		return side.csrf
	}
	tok := rtCsrfFetch(t, side)
	if sid == "" {
		// the page just minted the session: remember it by its cookie
		if u, err := url.Parse(side.base + "/"); err == nil {
			for _, c := range side.client.Jar.Cookies(u) {
				if c.Name == "fa_sid" {
					sid = c.Value
				}
			}
		}
	}
	side.csrfSid, side.csrf = sid, tok
	return tok
}

func rtCsrfFetch(t *testing.T, side *rtSide) string {
	t.Helper()
	resp, err := side.client.Get(side.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	m := regexp.MustCompile(`<meta name="fa-csrf" content="([^"]*)">`).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// rtDiff reports the first difference between two texts with context.
func rtDiff(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	lo := i - 120
	if lo < 0 {
		lo = 0
	}
	hiA, hiB := i+200, i+200
	if hiA > len(a) {
		hiA = len(a)
	}
	if hiB > len(b) {
		hiB = len(b)
	}
	line := 1 + bytes.Count([]byte(a[:i]), []byte("\n"))
	return fmt.Sprintf("first difference at byte %d (line %d):\n  go : …%s…\n  fct: …%s…", i, line, a[lo:hiA], b[lo:hiB])
}

func rtCompare(t *testing.T, step rtStep, g, f rtAnswer) {
	t.Helper()
	if g.status != f.status {
		t.Errorf("%s: status go=%d fct=%d\n  go body: %.300s\n  fct body: %.300s", step.name, g.status, f.status, g.body, f.body)
		return
	}
	if g.contentType != f.contentType {
		t.Errorf("%s: content-type go=%q fct=%q", step.name, g.contentType, f.contentType)
	}
	if g.lang != f.lang {
		t.Errorf("%s: content-language go=%q fct=%q", step.name, g.lang, f.lang)
	}
	if g.replay != f.replay {
		t.Errorf("%s: idempotent replay header go=%q fct=%q", step.name, g.replay, f.replay)
	}
	if g.cookieAttrs != f.cookieAttrs {
		t.Errorf("%s: set-cookie go=%q fct=%q", step.name, g.cookieAttrs, f.cookieAttrs)
	}
	if dir := os.Getenv("RT_PARITY_DUMP_ALL"); dir != "" {
		name := strings.NewReplacer("/", "_", " ", "_").Replace(step.name)
		os.WriteFile(filepath.Join(dir, name+".go"), []byte(g.body), 0o644)
		os.WriteFile(filepath.Join(dir, name+".fct"), []byte(f.body), 0o644)
	}
	if g.body != f.body {
		t.Errorf("%s: body differs (%d vs %d bytes)\n%s", step.name, len(g.body), len(f.body), rtDiff(g.body, f.body))
		// RT_PARITY_DUMP=<dir>: both bodies written out, for a diff tool.
		if dir := os.Getenv("RT_PARITY_DUMP"); dir != "" {
			name := strings.NewReplacer("/", "_", " ", "_").Replace(step.name)
			os.WriteFile(filepath.Join(dir, name+".go"), []byte(g.body), 0o644)
			os.WriteFile(filepath.Join(dir, name+".fct"), []byte(f.body), 0o644)
		}
	}
}

// rtScript is the request sequence every app is driven through. Paths a
// given app does not serve still count: both sides must refuse alike.
func rtScript(g *ir.IR) []rtStep {
	steps := []rtStep{
		{name: "css", method: "GET", path: "/facet.css"},
		{name: "js", method: "GET", path: "/facet.js"},
		{name: "healthz", method: "GET", path: "/healthz"},
		{name: "api schema", method: "GET", path: "/api"},
		{name: "api entity unpublished", method: "GET", path: "/api/" + g.Entities[1].Name},
		{name: "api entity published, empty", method: "GET", path: "/api/" + g.Entities[0].Name},
		{name: "api unknown order field", method: "GET", path: "/api/" + g.Entities[0].Name + "?by=nope"},
		{name: "api bad limit", method: "GET", path: "/api/" + g.Entities[0].Name + "?limit=abc"},
		{name: "api unknown filter", method: "GET", path: "/api/" + g.Entities[0].Name + "?nope=1"},
		{name: "region all", method: "POST", path: "/region", body: `{"path":"/","key":"","state":{}}`, csrf: true},
		{name: "region unknown page", method: "POST", path: "/region", body: `{"path":"/nope","key":"","state":{}}`, csrf: true},
		{name: "region unknown key", method: "POST", path: "/region", body: `{"path":"/","key":"/9/9","state":{}}`, csrf: true},
		{name: "region get", method: "GET", path: "/region"},
		{name: "upload get", method: "GET", path: "/upload"},
		{name: "upload without csrf", method: "POST", path: "/upload", body: rtMultipart("file", "note.txt", "hello upload\n"), headers: map[string]string{"Content-Type": rtMultipartType}},
		{name: "uploads missing", method: "GET", path: "/uploads/nope.txt"},
		{name: "uploads escaping", method: "GET", path: "/uploads/..%2Fnope"},
		{name: "upload chunk unknown session", method: "POST", path: "/upload/chunk?id=nope", body: "x", csrf: true},
		{name: "upload abort unknown session", method: "POST", path: "/upload/abort?id=nope", csrf: true},
		{name: "upload other op", method: "POST", path: "/upload/other", csrf: true},
		{name: "api unknown entity", method: "GET", path: "/api/Nope"},
		{name: "unknown page", method: "GET", path: "/no/such/page"},
	}
	for _, pg := range g.Pages {
		path := pg.Path
		path = strings.ReplaceAll(path, ":id", "1")
		path = strings.ReplaceAll(path, ":handle", "grace")
		path = strings.ReplaceAll(path, ":slug", "meadow")
		path = strings.ReplaceAll(path, ":tag", "facet")
		path = strings.ReplaceAll(path, ":category", "gaming")
		steps = append(steps, rtStep{name: "page " + pg.Name + " as guest", method: "GET", path: path})
	}
	if g.Auth {
		steps = append(steps,
			rtStep{name: "post as guest", method: "POST", path: "/api/post", body: `{"args":["hello from a guest"]}`},
			rtStep{name: "login before signup", method: "POST", path: "/api/login", body: `{"args":["grace","hopper"]}`},
			rtStep{name: "signup grace", method: "POST", path: "/api/signup", body: `{"args":["grace","hopper"]}`},
			rtStep{name: "signup grace again", method: "POST", path: "/api/signup", body: `{"args":["grace","other"]}`},
			rtStep{name: "post as grace", method: "POST", path: "/api/post", body: `{"args":["hello, world"]}`},
			rtStep{name: "post second", method: "POST", path: "/api/post", body: `{"args":["second post with #facet and @alan"]}`},
			rtStep{name: "like via event", method: "POST", path: "/event", body: `{"action":"like","args":[1]}`, csrf: true},
			rtStep{name: "event without csrf", method: "POST", path: "/event", body: `{"action":"like","args":[2]}`},
			rtStep{name: "event get", method: "GET", path: "/event"},
			rtStep{name: "follow alan", method: "POST", path: "/api/follow", body: `{"args":["alan"]}`},
			rtStep{name: "unknown action", method: "POST", path: "/api/nope", body: `{"args":[]}`},
			rtStep{name: "bad arg type", method: "POST", path: "/api/like", body: `{"args":["not-an-id"]}`},
		)
		for _, pg := range g.Pages {
			path := pg.Path
			path = strings.ReplaceAll(path, ":id", "1")
			path = strings.ReplaceAll(path, ":handle", "grace")
			path = strings.ReplaceAll(path, ":slug", "meadow")
			path = strings.ReplaceAll(path, ":tag", "facet")
			path = strings.ReplaceAll(path, ":category", "gaming")
			steps = append(steps, rtStep{name: "page " + pg.Name + " as grace", method: "GET", path: path})
		}
		steps = append(steps,
			rtStep{name: "upload a file", method: "POST", path: "/upload", body: rtMultipart("file", "note.txt", "hello upload\n"), headers: map[string]string{"Content-Type": rtMultipartType}, csrf: true},
			rtStep{name: "upload missing field", method: "POST", path: "/upload", body: rtMultipart("other", "note.txt", "x"), headers: map[string]string{"Content-Type": rtMultipartType}, csrf: true},
			rtStep{name: "upload not multipart", method: "POST", path: "/upload", body: `{"file":1}`, csrf: true},
			rtStep{name: "uploads fetch", method: "GET", path: "SIDE_UPLOAD"},
			rtStep{name: "upload init", method: "POST", path: "/upload/init", body: `{"filename":"clip.txt"}`, csrf: true},
			rtStep{name: "upload chunk 1", method: "POST", path: "/upload/chunk?id=SIDE_UPLOAD_ID", body: "part one, ", headers: map[string]string{"Content-Type": "application/octet-stream"}, csrf: true},
			rtStep{name: "upload chunk 2", method: "POST", path: "/upload/chunk?id=SIDE_UPLOAD_ID", body: "part two", headers: map[string]string{"Content-Type": "application/octet-stream"}, csrf: true},
			rtStep{name: "upload finish", method: "POST", path: "/upload/finish?id=SIDE_UPLOAD_ID", csrf: true},
			rtStep{name: "uploads fetch assembled", method: "GET", path: "SIDE_UPLOAD"},
			rtStep{name: "upload finish again", method: "POST", path: "/upload/finish?id=SIDE_UPLOAD_ID", csrf: true},
			rtStep{name: "api list desc limit 1", method: "GET", path: "/api/" + g.Entities[0].Name + "?by=created&desc=1&limit=1"},
			rtStep{name: "api list next page", method: "GET", path: "/api/" + g.Entities[0].Name + "?by=created&desc=1&limit=1&after=NEXT"},
			rtStep{name: "api list filtered", method: "GET", path: "/api/" + g.Entities[0].Name + "?author=grace&by=id"},
			rtStep{name: "api list filtered none", method: "GET", path: "/api/" + g.Entities[0].Name + "?author=nobody"},
			rtStep{name: "region all as grace", method: "POST", path: "/region", body: `{"path":"/","key":"","state":{"shown":1}}`, csrf: true},
			rtStep{name: "logout", method: "POST", path: "/api/logout", body: `{"args":[]}`},
			rtStep{name: "home after logout", method: "GET", path: "/"},
			rtStep{name: "login wrong password", method: "POST", path: "/api/login", body: `{"args":["grace","nope"]}`},
			rtStep{name: "login grace", method: "POST", path: "/api/login", body: `{"args":["grace","hopper"]}`},
			rtStep{name: "home logged in again", method: "GET", path: "/"},
		)
	}
	return steps
}

// rtPagePath is a routed page's path with its parameters filled in.
func rtPagePath(path string) string {
	for _, kv := range [][2]string{{":id", "1"}, {":handle", "grace"}, {":slug", "meadow"}, {":tag", "facet"}, {":category", "gaming"}, {":lane", "gaming"}} {
		path = strings.ReplaceAll(path, kv[0], kv[1])
	}
	return path
}

// rtScriptSite drives f33d3r.com (facets/f33d3r_com.fct): one app whose
// web pages and native /api/v2 routes share one session. The web signs up
// and in through the product's own signup/createSession actions, so the
// cookie session answers /api/v2/me; a web login's re-keyed id comes back
// as X-Session-Token and works as a bearer; and the site's posting,
// liking, messaging and going live run through the same actions, with
// every page rendered before and after.
func rtScriptSite(g *ir.IR) []rtStep {
	bearer := map[string]string{"Authorization": "Bearer SIDE_TOKEN"}
	// A view whose path a built-in endpoint claims (the app's `/live`, the
	// runtime's change stream) is never served; its page is not a page.
	shadowed := map[string]bool{}
	for _, sh := range runtime.ShadowedRoutes(g) {
		shadowed[sh.Route] = true
	}
	var pages []ir.Page
	for _, pg := range g.Pages {
		if !shadowed[pg.Path] {
			pages = append(pages, pg)
		}
	}
	// The runtime's own surface, as every app gets it (rtScript's steps
	// short of pages and built-in auth, which this app does not use): the
	// stylesheet and client, the API projection's refusals, regions and
	// uploads.
	var steps []rtStep
	for _, st := range rtScript(g) {
		if !strings.HasPrefix(st.name, "page ") {
			steps = append(steps, st)
		}
	}
	steps = append(steps,
		rtStep{name: "me with no session", method: "GET", path: "/api/v2/me"},
	)
	for _, pg := range pages {
		steps = append(steps, rtStep{name: "page " + pg.Name + " as guest", method: "GET", path: rtPagePath(pg.Path)})
	}
	steps = append(steps,
		rtStep{name: "native signup alan", method: "POST", path: "/api/v2/accounts", body: `{"handle":"alan","password":"turing-1912","display_name":"Alan","terms_accepted":true,"device_name":"phone"}`},
		rtStep{name: "native me as alan", method: "GET", path: "/api/v2/me", headers: bearer},
		rtStep{name: "web signup refused (weak password)", method: "POST", path: "/event", body: `{"action":"webSignup","args":["grace","short"]}`, csrf: true},
		rtStep{name: "web signup", method: "POST", path: "/event", body: `{"action":"webSignup","args":["grace","hopper-1906"]}`, csrf: true},
		rtStep{name: "cookie session answers me", method: "GET", path: "/api/v2/me"},
		rtStep{name: "web signup taken", method: "POST", path: "/event", body: `{"action":"webSignup","args":["alan","turing-1912"]}`, csrf: true},
		rtStep{name: "post from the compose box", method: "POST", path: "/event", body: `{"action":"post","args":["hello from the web #facet"]}`, csrf: true},
		rtStep{name: "post with every option", method: "POST", path: "/event", body: `{"action":"post","args":["second post",false,"",false,false,"everyone","","",[],"",[],false]}`, csrf: true},
		rtStep{name: "like", method: "POST", path: "/event", body: `{"action":"like","args":[1]}`, csrf: true},
		rtStep{name: "unlike", method: "POST", path: "/event", body: `{"action":"like","args":[1]}`, csrf: true},
		rtStep{name: "like again", method: "POST", path: "/event", body: `{"action":"like","args":[1]}`, csrf: true},
		rtStep{name: "repost", method: "POST", path: "/event", body: `{"action":"repost","args":[1]}`, csrf: true},
		rtStep{name: "bookmark", method: "POST", path: "/event", body: `{"action":"bookmark","args":[2]}`, csrf: true},
		rtStep{name: "reply", method: "POST", path: "/event", body: `{"action":"reply","args":[1,"a reply"]}`, csrf: true},
		rtStep{name: "message alan", method: "POST", path: "/event", body: `{"action":"webMessage","args":["alan","hi alan"]}`, csrf: true},
		rtStep{name: "message nobody", method: "POST", path: "/event", body: `{"action":"webMessage","args":["nobody","hi"]}`, csrf: true},
		rtStep{name: "go live without a title", method: "POST", path: "/event", body: `{"action":"webGoLive","args":["  "]}`, csrf: true},
		rtStep{name: "go live", method: "POST", path: "/event", body: `{"action":"webGoLive","args":["building f33d3r live"]}`, csrf: true},
		rtStep{name: "me after acting", method: "GET", path: "/api/v2/me"},
		rtStep{name: "feed over the api", method: "GET", path: "/api/v2/feed"},
	)
	for _, pg := range pages {
		steps = append(steps, rtStep{name: "page " + pg.Name + " as grace", method: "GET", path: rtPagePath(pg.Path)})
	}
	steps = append(steps,
		rtStep{name: "web logout", method: "POST", path: "/event", body: `{"action":"webLogout","args":[]}`, csrf: true},
		rtStep{name: "me after logout", method: "GET", path: "/api/v2/me"},
		rtStep{name: "web login wrong password", method: "POST", path: "/api/webLogin", body: `{"args":["grace","nope"]}`},
		rtStep{name: "web login", method: "POST", path: "/api/webLogin", body: `{"args":["grace","hopper-1906"]}`},
		rtStep{name: "the web login's token as a bearer", method: "GET", path: "/api/v2/me", headers: bearer},
		rtStep{name: "sessions as that bearer", method: "GET", path: "/api/v2/sessions", headers: bearer},
		rtStep{name: "home after the web login", method: "GET", path: "/"},
		rtStep{name: "metrics", method: "GET", path: "/metrics"},
	)
	return steps
}

// rtHookKey is the webhook secret both runtimes are given
// (MARKETS_INGEST_KEY, the variable facets/api's webhooks name).
const rtHookKey = "parity-hook-key"

// rtScriptAPI drives an app whose contract is its declared `api` routes
// (facets/api/main.fct): the page, an account created through the
// contract, the refusals the router makes on its own, and bearer-
// authenticated reads including a conditional GET and the tagged-message
// dispatch route.
func rtScriptAPI(g *ir.IR) []rtStep {
	bearer := map[string]string{"Authorization": "Bearer SIDE_TOKEN"}
	return []rtStep{
		{name: "css", method: "GET", path: "/facet.css"},
		{name: "page Home", method: "GET", path: "/"},
		{name: "api schema", method: "GET", path: "/api"},
		{name: "declared: wrong method", method: "PUT", path: "/api/v2/accounts", body: `{}`},
		{name: "declared: missing parameter", method: "POST", path: "/api/v2/accounts", body: `{}`},
		{name: "declared: not a JSON object", method: "POST", path: "/api/v2/accounts", body: `[1]`},
		{name: "declared: sessions unauthenticated", method: "GET", path: "/api/v2/sessions"},
		{name: "declared: me unauthenticated", method: "GET", path: "/api/v2/me"},
		{name: "declared: signup refused (weak password)", method: "POST", path: "/api/v2/accounts",
			body: `{"handle":"grace","password":"short","terms_accepted":true}`},
		{name: "declared: signup", method: "POST", path: "/api/v2/accounts",
			body: `{"handle":"grace","password":"hopper-1906","display_name":"Grace","terms_accepted":true,"device_name":"phone"}`},
		{name: "declared: signup taken", method: "POST", path: "/api/v2/accounts",
			body: `{"handle":"grace","password":"hopper-1906","terms_accepted":true}`},
		{name: "declared: me", method: "GET", path: "/api/v2/me", headers: bearer},
		{name: "declared: me conditional", method: "GET", path: "/api/v2/me", headers: map[string]string{"Authorization": "Bearer SIDE_TOKEN", "If-None-Match": "SIDE_ETAG"}},
		{name: "declared: sessions", method: "GET", path: "/api/v2/sessions", headers: bearer},
		{name: "declared: feed", method: "GET", path: "/api/v2/feed", headers: bearer},
		{name: "declared: notifications", method: "GET", path: "/api/v2/notifications", headers: bearer},
		{name: "declared: create session (login)", method: "POST", path: "/api/v2/sessions",
			body: `{"handle":"grace","password":"hopper-1906","device_name":"laptop"}`},
		{name: "declared: login wrong password", method: "POST", path: "/api/v2/sessions",
			body: `{"handle":"grace","password":"nope"}`},
		{name: "dispatch: unknown event", method: "POST", path: "/events", headers: bearer, body: `{"event_type":"no_such_event"}`},
		{name: "dispatch: missing field", method: "POST", path: "/events", headers: bearer, body: `{"event_type":"block"}`},
		{name: "dispatch: unauthenticated", method: "POST", path: "/events", body: `{"event_type":"block","target_handle":"alan"}`},
		{name: "dispatch: block", method: "POST", path: "/events", headers: bearer, body: `{"event_type":"block","target_handle":"alan"}`},
		{name: "declared: sessions after login", method: "GET", path: "/api/v2/sessions", headers: bearer},
		{name: "unknown route", method: "GET", path: "/api/v2/no/such/route"},
		{name: "webhook get", method: "GET", path: "/hooks/markets/source"},
		{name: "webhook unsigned", method: "POST", path: "/hooks/markets/source", body: `{"kind":"quote","provider":"tiingo","enabled":true}`},
		{name: "webhook wrong key", method: "POST", path: "/hooks/markets/source", body: `{"kind":"quote","provider":"tiingo","enabled":true}`, sign: "not-the-key"},
		{name: "webhook delivery", method: "POST", path: "/hooks/markets/source", body: `{"kind":"quotes","provider":"tiingo","enabled":true}`, sign: rtHookKey},
		{name: "webhook redelivery (replayed success)", method: "POST", path: "/hooks/markets/source", body: `{"kind":"quotes","provider":"tiingo","enabled":true}`, sign: rtHookKey},
		{name: "webhook refused delivery", method: "POST", path: "/hooks/markets/source", body: `{"kind":"quote","provider":"tiingo","enabled":true}`, sign: rtHookKey},
		{name: "webhook refused redelivery (replayed refusal)", method: "POST", path: "/hooks/markets/source", body: `{"kind":"quote","provider":"tiingo","enabled":true}`, sign: rtHookKey},
		{name: "webhook keyed delivery", method: "POST", path: "/hooks/markets/source", body: `{"kind":"gifs","provider":"giphy","enabled":false}`, sign: rtHookKey, headers: map[string]string{"Idempotency-Key": "delivery-7"}},
		{name: "webhook keyed redelivery", method: "POST", path: "/hooks/markets/source", body: `{"kind":"sports","provider":"x","enabled":true}`, sign: rtHookKey, headers: map[string]string{"Idempotency-Key": "delivery-7"}},
		{name: "webhook refused by the action", method: "POST", path: "/hooks/markets/close", body: `{"symbol":"","ts":0,"close":0}`, sign: rtHookKey},
		{name: "webhook malformed", method: "POST", path: "/hooks/markets/source", body: `not json`, sign: rtHookKey},
		{name: "stream post", method: "POST", path: "/api/v2/events", headers: bearer},
		{name: "dev token without credentials", method: "POST", path: "/api/v2/dev/oauth/token", body: "grant_type=authorization_code&code=x", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
		{name: "dev token malformed basic", method: "POST", path: "/api/v2/dev/oauth/token", body: "grant_type=authorization_code&code=x", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic not*base64"}},
		{name: "dev token empty secret", method: "POST", path: "/api/v2/dev/oauth/token", body: "grant_type=authorization_code&code=x", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:"))}},
		{name: "dev token wrong grant", method: "POST", path: "/api/v2/dev/oauth/token", body: "grant_type=password&code=x", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:csecret"))}},
		{name: "dev token unknown code", method: "POST", path: "/api/v2/dev/oauth/token", body: "grant_type=authorization_code&code=nope", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:csecret"))}},
		{name: "dev token as JSON", method: "POST", path: "/api/v2/dev/oauth/token", body: `{"grant_type":"authorization_code","code":"nope"}`, headers: map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:csecret"))}},
		{name: "track upload not multipart", method: "POST", path: "/api/v2/music/tracks", body: `{"title":"x"}`, headers: bearer},
		{name: "track upload without the file", method: "POST", path: "/api/v2/music/tracks", body: rtMultipartParts([3]string{"title", "", "Song"}), headers: map[string]string{"Authorization": "Bearer SIDE_TOKEN", "Content-Type": rtMultipartType}},
		{name: "track upload unsupported file", method: "POST", path: "/api/v2/music/tracks", body: rtMultipartParts([3]string{"audio", "notes.txt", "not audio"}, [3]string{"title", "", "Song"}), headers: map[string]string{"Authorization": "Bearer SIDE_TOKEN", "Content-Type": rtMultipartType}},
		{name: "track upload", method: "POST", path: "/api/v2/music/tracks", body: rtMultipartParts([3]string{"audio", "song.mp3", "ID3\x03\x00\x00\x00\x00\x00\x00frame"}, [3]string{"title", "", "Song"}, [3]string{"genre", "", "ambient"}, [3]string{"duration_secs", "", "187"}), headers: map[string]string{"Authorization": "Bearer SIDE_TOKEN", "Content-Type": rtMultipartType}},
		{name: "contract document", method: "GET", path: "/api/v2/contract"},
		{name: "contract conditional", method: "GET", path: "/api/v2/contract", headers: map[string]string{"If-None-Match": "SIDE_ETAG"}},
		{name: "contract post", method: "POST", path: "/api/v2/contract", body: "{}"},
		{name: "contract version", method: "GET", path: "/api/v2/contract/version"},
		{name: "contract history", method: "GET", path: "/api/v2/contract/history"},
		{name: "contract diff current", method: "GET", path: "/api/v2/contract/diff?from=current"},
		{name: "contract diff missing from", method: "GET", path: "/api/v2/contract/diff"},
		{name: "contract diff unknown", method: "GET", path: "/api/v2/contract/diff?from=nope"},
	}
}

func rtRunApp(t *testing.T, app string) {
	t.Helper()
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	// One secret for both runtimes, so signatures are computed the same way
	// (the values still differ: session ids are random on both sides).
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	t.Setenv("MARKETS_INGEST_KEY", rtHookKey)
	// The script signs up, logs in and asks for dev tokens more often than
	// the `auth` class's default burst (10) allows; both sides get the same
	// wider budget so every step reaches its action (the limiter itself is
	// compared by the contract document's x-rate-limit and the 429 texts).
	t.Setenv("FACET_RATE_LIMIT_AUTH", "6000")
	// Uploads land in a directory both sides share the name of; the fct
	// side's sandbox is that same temp directory (see rtStartFct).
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	// The first entity is published over the JSON API on both sides (the Go
	// runtime reads the variable at construction, the fct one at boot).
	t.Setenv("FACET_API_READ", g.Entities[0].Name)
	// The graph is served by both; the Go runtime mutates its copy (enterprise
	// entities are injected at construction), so each side gets its own.
	gGo, _ := compile.File(app)
	goSide := rtStartGo(t, gGo)
	fctSide := rtStartFct(t, g)
	rtMarkTicking(g, goSide, fctSide)
	rtDrive(t, app, g, goSide, fctSide)
}

// rtDrive runs the app's script against both sides, step by step.
func rtDrive(t *testing.T, app string, g *ir.IR, goSide, fctSide *rtSide) {
	t.Helper()
	var goCsrf, fctCsrf string
	script := rtScript(g)
	if filepath.Base(app) == "f33d3r_com.fct" {
		script = rtScriptSite(g)
	} else if len(g.APIs) > 0 {
		script = rtScriptAPI(g)
	}

	for _, st := range script {
		if st.csrf {
			goCsrf, fctCsrf = rtCsrfOf(t, goSide), rtCsrfOf(t, fctSide)
		}
		// A cursor page asks for what the previous page answered: each side
		// replays the cursor it minted (an engine's cursor is its own).
		goStep, fctStep := st, st
		goStep.path = strings.ReplaceAll(st.path, "after=NEXT", "after="+goSide.next)
		fctStep.path = strings.ReplaceAll(st.path, "after=NEXT", "after="+fctSide.next)
		ga, fa, gd, fd := rtDoPair(t, goSide, fctSide, goStep, fctStep, goCsrf, fctCsrf)
		if os.Getenv("RT_PARITY_TRACE") != "" {
			t.Logf("%-40s go=%d %v fct=%d %v %s", st.name, ga.status, gd.Round(time.Millisecond), fa.status, fd.Round(time.Millisecond), ga.contentType)
		}
		rtCompare(t, st, ga, fa)
		if t.Failed() && os.Getenv("RT_PARITY_ALL") == "" {
			t.Fatalf("stopping at the first failing step (%s); set RT_PARITY_ALL=1 to run the rest", st.name)
		}
	}
	if len(g.APIs) == 0 || filepath.Base(app) == "f33d3r_com.fct" {
		rtLiveParity(t, g, goSide, fctSide)
	}
	if len(g.Streams) > 0 {
		rtStreamParity(t, g, goSide, fctSide)
	}
}

var rtHelloID = regexp.MustCompile(`"session_id":"[0-9a-f]{24}"`)

// rtStreamParity subscribes both sides to the app's first declared stream
// (a `{param}` filled with 1) with the bearer token each side issued, and
// compares, frame by frame: the refusal of a request carrying no credential,
// the opening frames (the connected comment, hello, the connect hooks'
// frames), then an emitted frame reaching that subscriber — for an app with
// a `notif_read_all` mutation the event it emits to the actor; for a stream
// with connect hooks a second subscriber's join and then, when it leaves,
// its disconnect hook's event.
func rtStreamParity(t *testing.T, g *ir.IR, goSide, fctSide *rtSide) {
	t.Helper()
	st := g.Streams[0]
	path := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(st.Path, "1")
	opening := 2 // ": connected" and hello
	if st.Hello == "" {
		opening = 1
	}
	for _, h := range st.Connects {
		if h.Event != "" {
			opening++
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	type stream struct {
		r      *bufio.Reader
		resp   *http.Response
		cancel context.CancelFunc
	}
	open := func(side *rtSide) *stream {
		sctx, scancel := context.WithCancel(ctx)
		req, _ := http.NewRequestWithContext(sctx, "GET", side.base+path, nil)
		req.Header.Set("Authorization", "Bearer "+side.token)
		for _, c := range side.client.Jar.Cookies(req.URL) {
			req.AddCookie(c)
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			scancel()
			t.Fatalf("%s: %s: %v", side.name, path, err)
		}
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			scancel()
			t.Fatalf("%s: %s: %s %s", side.name, path, resp.Status, b)
		}
		return &stream{r: bufio.NewReader(resp.Body), resp: resp, cancel: scancel}
	}
	frame := func(s *stream, who string) string {
		var b strings.Builder
		for {
			line, err := s.r.ReadString('\n')
			if err != nil {
				t.Fatalf("%s: reading a stream frame: %v (got %q)", who, err, b.String())
			}
			if line == "\n" {
				return rtHelloID.ReplaceAllString(rtNormalize(b.String()), `"session_id":"CONN"`)
			}
			b.WriteString(line)
		}
	}
	both := func(what string, gs, fs *stream) string {
		gf, ff := frame(gs, "go"), frame(fs, "fct")
		if gf != ff {
			t.Errorf("%s differs:\n%s", what, rtDiff(gf, ff))
		}
		return gf
	}
	// No credential at all (no bearer, no cookie): refused before any head.
	anon := func(side *rtSide) rtAnswer {
		req, _ := http.NewRequestWithContext(ctx, "GET", side.base+path, nil)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s: anonymous %s: %v", side.name, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return rtAnswer{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(b)}
	}
	anonStep := rtStep{name: "stream without a credential", method: "GET", path: path}
	rtCompare(t, anonStep, anon(goSide), anon(fctSide))
	gs, fs := open(goSide), open(fctSide)
	defer gs.cancel()
	defer fs.cancel()
	if a, b := gs.resp.Header.Get("Content-Type"), fs.resp.Header.Get("Content-Type"); a != b {
		t.Errorf("stream content-type go=%q fct=%q", a, b)
	}
	for i := 0; i < opening; i++ {
		both(fmt.Sprintf("stream opening frame %d", i), gs, fs)
	}
	emitted := ""
	hasNotif := false
	for _, m := range g.Messages {
		for _, v := range m.Variants {
			if v.WireName() == "notif_read_all" {
				hasNotif = true
			}
		}
	}
	switch {
	case hasNotif:
		bearer := map[string]string{"Authorization": "Bearer SIDE_TOKEN"}
		emit := rtStep{name: "dispatch notif_read_all while subscribed", method: "POST", path: "/events", headers: bearer, body: `{"event_type":"notif_read_all"}`}
		ga, fa := rtDo(t, goSide, emit, ""), rtDo(t, fctSide, emit, "")
		rtCompare(t, emit, ga, fa)
		emitted = both("the emitted frame", gs, fs)
	case len(st.Connects) > 0:
		// A connect hook announces a *new* member of the room: the second
		// subscriber is a second account, signed up on its own client.
		viewer := func(side *rtSide) *rtSide {
			v := &rtSide{name: side.name, base: side.base, client: rtClient(t)}
			su := rtStep{name: "second subscriber signs up", method: "POST", path: "/api/signup", body: `{"args":["viewer2","watching-2"]}`}
			if a := rtDo(t, v, su, ""); a.status != 200 {
				t.Fatalf("%s: %s: %d %s", side.name, su.name, a.status, a.body)
			}
			return v
		}
		g2, f2 := open(viewer(goSide)), open(viewer(fctSide))
		for i := 0; i < opening; i++ {
			both(fmt.Sprintf("second subscriber's opening frame %d", i), g2, f2)
		}
		emitted = both("the second subscriber's join, as the first sees it", gs, fs)
		g2.cancel()
		f2.cancel()
		if st.Disconnect != "" {
			emitted += both("the second subscriber's leave, as the first sees it", gs, fs)
		}
	}
	if os.Getenv("RT_PARITY_TRACE") != "" {
		t.Logf("emitted: %q", emitted)
	}
}

// rtLiveParity opens /live on both sides, reads the hello frame, makes one
// change through an action, and reads the frame it fans out.
func rtLiveParity(t *testing.T, g *ir.IR, goSide, fctSide *rtSide) {
	t.Helper()
	rtLiveParityAt(t, g, goSide, fctSide, "/api/_live")
}

// rtLiveParityAt is rtLiveParity at one of the stream's paths.
func rtLiveParityAt(t *testing.T, g *ir.IR, goSide, fctSide *rtSide, path string) {
	t.Helper()
	type stream struct {
		r    *bufio.Reader
		resp *http.Response
	}
	// A stream that never delivers must fail the test, not hang it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	open := func(side *rtSide) *stream {
		req, _ := http.NewRequestWithContext(ctx, "GET", side.base+path, nil)
		resp, err := http.DefaultTransport.RoundTrip(rtWithCookies(side, req))
		if err != nil {
			t.Fatalf("%s: %s: %v", side.name, path, err)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("%s: %s: %s %q", side.name, path, resp.Status, resp.Header.Get("Content-Type"))
		}
		return &stream{r: bufio.NewReader(resp.Body), resp: resp}
	}
	frame := func(s *stream) string {
		var b strings.Builder
		for {
			line, err := s.r.ReadString('\n')
			if err != nil {
				t.Fatalf("reading an SSE frame: %v (got %q)", err, b.String())
			}
			if line == "\n" {
				// a frame's write sequence counts a periodic job's ticks too
				return rtTickMask(goSide, rtLiveSeq(goSide, rtNormalize(b.String())))
			}
			b.WriteString(line)
		}
	}
	gs, fs := open(goSide), open(fctSide)
	defer gs.resp.Body.Close()
	defer fs.resp.Body.Close()
	gh, fh := frame(gs), frame(fs)
	if gh != fh {
		t.Errorf("live hello frame differs:\n%s", rtDiff(gh, fh))
	}
	if !g.Auth {
		return
	}
	// One change, made through whichever of the app's own actions adds a
	// row (the signed-in member the script left behind may run it); the
	// frame it fans out must match. An app with no such action, or one
	// whose action refuses these arguments on both sides alike, has
	// nothing further to stream and the check ends with the hello frame.
	name, body := rtRowAddingAction(g)
	if name == "" {
		return
	}
	st := rtStep{name: "change while subscribed at " + path + " (" + name + ")", method: "POST", path: "/api/" + name, body: body}
	ga, fa := rtDo(t, goSide, st, ""), rtDo(t, fctSide, st, "")
	rtCompare(t, st, ga, fa)
	if ga.status != 200 {
		return
	}
	gf, ff := frame(gs), frame(fs)
	if gf != ff {
		t.Errorf("live change frame differs:\n%s", rtDiff(gf, ff))
	}
}

// rtRowAddingAction picks the first server action whose body's first
// statement adds a row and whose policies take no arguments, with one
// plausible argument per parameter.
func rtRowAddingAction(g *ir.IR) (string, string) {
	for _, a := range g.Actions {
		if a.Placement != ir.Server || len(a.Body) == 0 || a.Body[0].Op != "add" {
			continue
		}
		ok := true
		for _, r := range a.Requires {
			if len(r.Args) > 0 {
				ok = false
			}
		}
		if !ok {
			continue
		}
		var args []string
		for _, p := range a.Params {
			switch {
			case p.List:
				args = append(args, `[]`)
			case p.Type == "int" || p.Type == "money" || p.Type == "date":
				args = append(args, `1`)
			case p.Type == "bool":
				args = append(args, `true`)
			default:
				args = append(args, `"a change the stream carries"`)
			}
		}
		return a.Name, `{"args":[` + strings.Join(args, ",") + `]}`
	}
	return "", ""
}

// rtWithCookies attaches the side's jar cookies to a request sent outside
// the client (a stream read raw off RoundTrip).
func rtWithCookies(side *rtSide, req *http.Request) *http.Request {
	for _, c := range side.client.Jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	return req
}

func TestRuntimeParityF33d3r(t *testing.T)    { rtRunApp(t, rtParityApps[0]) }
func TestRuntimeParityF33d3rCom(t *testing.T) { rtRunApp(t, rtParityApps[1]) }
func TestRuntimeParityApiMain(t *testing.T)   { rtRunApp(t, rtParityApps[2]) }

// TestRuntimeParityDurable is the parity harness with a store behind both
// runtimes, each reached through FACET_DATABASE_URL: by default the Go
// runtime and the fct runtime each on its own `facet facetql` (the engine
// this toolchain ships, written in fct); with FACETQL_REFERENCE=rust, also
// the Go runtime on the Rust reference engine against the fct runtime on
// fqserver.fct. The same script runs against both; afterwards the two
// engines must hold the same rows (kind, address and data, the clock
// readings and password hashes masked) — every write the fct runtime made
// reached its engine exactly as the Go runtime's reached its own.
func TestRuntimeParityDurable(t *testing.T) {
	if testing.Short() {
		t.Skip("starts two FacetQL engines per app")
	}
	for _, app := range rtParityApps {
		app := app
		t.Run(filepath.Base(app), func(t *testing.T) { rtRunDurable(t, app, rtShippedEnginePair) })
	}
	if os.Getenv(fqReferenceEnv) == "rust" {
		for _, app := range rtParityApps {
			app := app
			t.Run("rust/"+filepath.Base(app), func(t *testing.T) { rtRunDurable(t, app, fqServerPair) })
		}
	}
}

// rtUnmetered turns off every FacetQL rate class.
var rtUnmetered = []string{"FACETQL_RATE_READ=off", "FACETQL_RATE_WRITE=off", "FACETQL_RATE_BULK=off", "FACETQL_RATE_ADMIN=off", "FACETQL_RATE_SUBSCRIBE=off"}

// rtEnginePair starts the Go side's engine and the fct side's, with the
// same extra environment, and returns their ports.
type rtEnginePair func(t *testing.T, env ...string) (goPort, fctPort int)

// rtShippedEngine starts one `facet facetql` in its own data directory on
// port 0 and returns the port its banner names once it answers.
func rtShippedEngine(t *testing.T, name string, env ...string) int {
	t.Helper()
	facet := fqServerFacet(t)
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := &fqLockedBuf{}
	cmd := exec.Command(facet, "facetql")
	cmd.Env = append(append(os.Environ(), "FACETQL_TOKENS=tok:alice:admin,utok:bob", "FACETQL_ENV=development", "FACETQL_MASTER_KEY="+facetqlCheckKey), env...)
	cmd.Env = append(cmd.Env, "FACETQL_DATA_DIR="+dir, "FACETQL_PORT=0")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting facet facetql (%s): %v", name, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if port := fqBannerPort(out.String()); port != 0 {
			if c, err := fqDial(port); err == nil {
				c.Close()
				return port
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("facet facetql (%s) never listened:\n%s", name, out.String())
	return 0
}

// rtShippedEnginePair starts two `facet facetql` engines — the one this
// toolchain ships — each in its own data directory on port 0, and returns
// the ports their banners name once both answer.
func rtShippedEnginePair(t *testing.T, env ...string) (int, int) {
	t.Helper()
	return rtShippedEngine(t, "go", env...), rtShippedEngine(t, "fct", env...)
}

// rtRunDurable is one app's durable run: a fresh engine pair, the same
// script the in-memory run uses, then the engines compared.
func rtRunDurable(t *testing.T, app string, engines rtEnginePair) {
	t.Helper()
	// The script drives each runtime far past any identity's production
	// budget, so the engines meter no one here: every 429 would cost both
	// sides a Retry-After second of waiting and compare nothing. Pacing
	// under a limiter is TestRuntimeParityRateLimited's subject.
	goPort, fctPort := engines(t, rtUnmetered...)
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	t.Setenv("MARKETS_INGEST_KEY", rtHookKey)
	// The script signs up, logs in and asks for dev tokens more often than
	// the `auth` class's default burst (10) allows; both sides get the same
	// wider budget so every step reaches its action (the limiter itself is
	// compared by the contract document's x-rate-limit and the 429 texts).
	t.Setenv("FACET_RATE_LIMIT_AUTH", "6000")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	t.Setenv("FACET_API_READ", g.Entities[0].Name)
	// The Go runtime reads FACET_DATABASE_URL when it is constructed, the fct
	// runtime when its daemon boots: each is started under its own engine's URL.
	t.Setenv("FACET_DATABASE_URL", fmt.Sprintf("facetql://tok@127.0.0.1:%d", goPort))
	gGo, _ := compile.File(app)
	goSrv, err := runtime.New(gGo)
	if err != nil {
		t.Fatalf("go runtime on its engine: %v", err)
	}
	goTS := httptest.NewServer(goSrv.Handler())
	t.Cleanup(func() { goTS.Close(); goSrv.Shutdown() })
	goSrv.StartJobs()
	goSide := &rtSide{name: "go", base: goTS.URL, client: rtClient(t)}
	t.Setenv("FACET_DATABASE_URL", fmt.Sprintf("facetql://tok@127.0.0.1:%d", fctPort))
	fctSide := rtStartFct(t, g)
	rtMarkTicking(g, goSide, fctSide)
	rtDrive(t, app, g, goSide, fctSide)
	// A stream subscriber's disconnect hook (a viewer count going back down)
	// runs when each server learns the connection closed, which is after the
	// script's last step returns: the engines are compared once both have
	// settled, and must then agree.
	deadline := time.Now().Add(10 * time.Second)
	ticked := rtTickWritten(g)
	for _, e := range g.Entities {
		if ticked[e.Name] {
			continue // written by a periodic job: as many rows as ticks
		}
		for {
			goRows := rtEngineRows(t, goPort, e.Name)
			fct := rtEngineRows(t, fctPort, e.Name)
			if goRows == fct {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("%s rows differ between the engines:\n%s", e.Name, rtDiff(goRows, fct))
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

var (
	rtBcryptHash = regexp.MustCompile(`\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}`)
	rtSealed     = regexp.MustCompile(`fctenc:[A-Za-z0-9_-]+`)
	// a session's visitor key (`session`), stored in a row by an app that
	// keys state to it: 24 random bytes, minted independently per process
	rtVisitorKey = regexp.MustCompile(`"sid":"[A-Za-z0-9_-]{32}"`)
)

// rtEngineRows lists one kind's nodes on an engine as "address data" lines,
// sorted, with what no two processes mint alike masked.
func rtEngineRows(t *testing.T, port int, kind string) string {
	t.Helper()
	var lines []string
	after := ""
	for {
		body, _ := json.Marshal(map[string]any{"kind": kind, "item_var": "item", "order": "id", "limit": 500, "after": after})
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/nodes/query", port), strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			// A rate-limited engine: wait the interval it names, then ask again.
			secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			time.Sleep(time.Duration(max(secs, 1)) * time.Second)
			continue
		}
		if resp.StatusCode != 200 {
			t.Fatalf("engine %d query %s: %d %s", port, kind, resp.StatusCode, raw)
		}
		var page struct {
			Nodes []struct {
				Address string `json:"address"`
				Data    string `json:"data"`
			} `json:"nodes"`
			Next string `json:"next"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		for _, n := range page.Nodes {
			d := rtNormalize(n.Data)
			d = rtBcryptHash.ReplaceAllString(d, "BCRYPT")
			d = rtSealed.ReplaceAllString(d, "SEALED")
			d = rtVisitorKey.ReplaceAllString(d, `"sid":"VISITOR"`)
			lines = append(lines, n.Address+" "+d)
		}
		if page.Next == "" || len(page.Nodes) == 0 {
			break
		}
		after = page.Next
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestRuntimeParitySignedMedia runs facets/f33d3r_com.fct with media signing
// on (FACET_MEDIA_TTL): an upload answered with its signed URL, the
// /uploads/ access rules (unsigned, expired, tampered, signed), http.ServeFile's
// Range / If-Modified-Since / HEAD behaviour, an HLS playlist whose segment
// lines come back signed, and a video post whose page renders a signed src
// and whose bootstrap and region answer carry the grant map.
func TestRuntimeParitySignedMedia(t *testing.T) {
	app := "../../facets/f33d3r_com.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	t.Setenv("FACET_MEDIA_TTL", "3600")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	t.Setenv("FACET_API_READ", g.Entities[0].Name)
	gGo, _ := compile.File(app)
	goSide := rtStartGo(t, gGo)
	fctSide := rtStartFct(t, g)
	rtMarkTicking(g, goSide, fctSide)
	mp := map[string]string{"Content-Type": rtMultipartType}
	steps := []rtStep{
		{name: "signup", method: "POST", path: "/api/webSignup", body: `{"args":["grace","hopper-1906"]}`},
		{name: "upload a clip", method: "POST", path: "/upload", body: rtMultipart("file", "clip.mp4", "0123456789abcdef"), headers: mp, csrf: true},
		{name: "upload a playlist", method: "POST", path: "/upload", body: rtMultipart("file", "list.m3u8", "#EXTM3U\n#EXTINF:4,\nseg1.ts\n\n#EXTINF:4,\n  seg2.ts\n#EXT-X-ENDLIST"), headers: mp, csrf: true},
		{name: "unsigned link", method: "GET", path: "SIDE_REF_0"},
		{name: "expired link", method: "GET", path: "SIDE_REF_0?exp=1&sig=00"},
		{name: "tampered link", method: "GET", path: "SIDE_REF_0?exp=9999999999&sig=" + strings.Repeat("0", 64)},
		{name: "non-numeric expiry", method: "GET", path: "SIDE_REF_0?exp=1e9&sig=00"},
		{name: "signed link", method: "GET", path: "SIDE_SIGNED_0"},
		{name: "signed HEAD", method: "HEAD", path: "SIDE_SIGNED_0"},
		{name: "range 2-5", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"Range": "bytes=2-5"}},
		{name: "range open-ended", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"Range": "bytes=10-"}},
		{name: "range suffix", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"Range": "bytes=-4"}},
		{name: "range clamped", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"Range": "bytes=12-100"}},
		{name: "range multi", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"Range": "bytes=0-1, 4-5"}},
		{name: "range past the end", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"Range": "bytes=100-200"}},
		{name: "range malformed", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"Range": "bytes=abc"}},
		{name: "range wider than the file", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"Range": "bytes=0-10,5-15"}},
		{name: "if-modified-since future", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"If-Modified-Since": "Fri, 01 Jan 2100 00:00:00 GMT"}},
		{name: "if-modified-since past", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"If-Modified-Since": "Mon, 01 Jan 2001 00:00:00 GMT"}},
		{name: "if-modified-since with if-none-match", method: "GET", path: "SIDE_SIGNED_0", headers: map[string]string{"If-Modified-Since": "Fri, 01 Jan 2100 00:00:00 GMT", "If-None-Match": `"x"`}},
		{name: "signed playlist", method: "GET", path: "SIDE_SIGNED_1"},
		{name: "unsigned playlist", method: "GET", path: "SIDE_REF_1"},
		{name: "post a video", method: "POST", path: "/api/postVideo", body: `{"args":["a clip","SIDE_REF_0"]}`},
		{name: "home with a signed video", method: "GET", path: "/"},
		{name: "region with a signed video", method: "POST", path: "/region", body: `{"path":"/","key":"","state":{}}`, csrf: true},
	}
	var goCsrf, fctCsrf string
	for _, st := range steps {
		if st.csrf {
			goCsrf, fctCsrf = rtCsrfOf(t, goSide), rtCsrfOf(t, fctSide)
		}
		ga, fa := rtDo(t, goSide, st, goCsrf), rtDo(t, fctSide, st, fctCsrf)
		if os.Getenv("RT_PARITY_TRACE") != "" {
			t.Logf("%-40s go=%d fct=%d %s", st.name, ga.status, fa.status, ga.contentType)
		}
		rtCompare(t, st, ga, fa)
		if t.Failed() && os.Getenv("RT_PARITY_ALL") == "" {
			t.Fatalf("stopping at the first failing step (%s)", st.name)
		}
		if st.name == "upload a playlist" && (len(goSide.refs) != 2 || len(fctSide.refs) != 2) {
			t.Fatalf("an upload's signed URL was not captured (go %d, fct %d)", len(goSide.refs), len(fctSide.refs))
		}
	}
}

// TestRuntimeParityRun drives the `run` statement and @internal actions
// through both runtimes: an @internal action is absent from GET /api and
// answers 404 on /api and /event; a run binds its callee's return and joins
// the caller's transaction — a failed check in the callee (with its code),
// a refusal after it in the caller, or a callee `requires` that does not
// hold rolls back the caller's own writes too; recursion stops at the
// 16-deep backstop and rolls everything back.
func TestRuntimeParityRun(t *testing.T) {
	app := "testdata/runtime_parity/run.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	t.Setenv("FACET_API_READ", "Acct,Log")
	gGo, _ := compile.File(app)
	goSide := rtStartGo(t, gGo)
	fctSide := rtStartFct(t, g)
	steps := []rtStep{
		{name: "schema hides @internal", method: "GET", path: "/api"},
		{name: "become ada", method: "GET", path: "/?as=ada"},
		{name: "internal over /api", method: "POST", path: "/api/credit", body: `{"args":["bob",5]}`},
		{name: "internal over /event", method: "POST", path: "/event", body: `{"action":"credit","args":["bob",5]}`, csrf: true},
		{name: "pay", method: "POST", path: "/api/pay", body: `{"args":["bob",5]}`},
		{name: "pay again", method: "POST", path: "/api/pay", body: `{"args":["bob",2]}`},
		{name: "pay a bad amount", method: "POST", path: "/api/pay", body: `{"args":["bob",0]}`},
		{name: "pay with a bad argument", method: "POST", path: "/api/pay", body: `{"args":["bob","lots"]}`},
		{name: "pay then fail", method: "POST", path: "/api/pay", body: `{"args":["bob",7]}`},
		{name: "pay then the caller refuses", method: "POST", path: "/api/payThenFail", body: `{"args":["bob",7]}`},
		{name: "a callee gate refuses", method: "POST", path: "/api/payAudited", body: `{"args":["carol",3]}`},
		{name: "a chain within the backstop", method: "POST", path: "/api/shallow", body: `{"args":[]}`},
		{name: "a chain past the backstop", method: "POST", path: "/api/dive", body: `{"args":[]}`},
		{name: "logs", method: "GET", path: "/api/Log"},
		{name: "accounts", method: "GET", path: "/api/Acct"},
		{name: "home", method: "GET", path: "/"},
		{name: "a bundled asset", method: "GET", path: "/assets/" + rtAssetName(t, g)},
		{name: "an asset never bundled", method: "GET", path: "/assets/0000.png"},
		{name: "the assets root", method: "GET", path: "/assets/"},
		{name: "an asset path with a slash", method: "GET", path: "/assets/a/b.png"},
		{name: "metrics", method: "GET", path: "/metrics"},
	}
	var goCsrf, fctCsrf string
	for _, st := range steps {
		if st.csrf {
			goCsrf, fctCsrf = rtCsrfOf(t, goSide), rtCsrfOf(t, fctSide)
		}
		ga, fa := rtDo(t, goSide, st, goCsrf), rtDo(t, fctSide, st, fctCsrf)
		if os.Getenv("RT_PARITY_TRACE") != "" {
			t.Logf("%-40s go=%d fct=%d %.120s", st.name, ga.status, fa.status, ga.body)
		}
		rtCompare(t, st, ga, fa)
		if t.Failed() && os.Getenv("RT_PARITY_ALL") == "" {
			t.Fatalf("stopping at the first failing step (%s)", st.name)
		}
	}
}

// TestRuntimeParityRateLimited runs both runtimes against engines whose bulk
// budget is one request (refilled at four a second): booting alone loads more
// entities than that, so the engine answers 429 and each runtime must pace
// itself — wait and send again, as Go's fqClient.do does — instead of failing
// to start or dropping a read.
func TestRuntimeParityRateLimited(t *testing.T) {
	if testing.Short() {
		t.Skip("starts two FacetQL engines")
	}
	app := "testdata/runtime_parity/run.fct"
	engines := rtEnginePair(rtShippedEnginePair)
	if os.Getenv(fqReferenceEnv) == "rust" {
		engines = fqServerPair
	}
	goPort, fctPort := engines(t, "FACETQL_RATE_BULK=1:4")
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	t.Setenv("FACET_API_READ", "Acct,Log")
	t.Setenv("FACET_DATABASE_URL", fmt.Sprintf("facetql://tok@127.0.0.1:%d", goPort))
	gGo, _ := compile.File(app)
	goSrv, err := runtime.New(gGo)
	if err != nil {
		t.Fatalf("go runtime on the rate-limited engine: %v", err)
	}
	goTS := httptest.NewServer(goSrv.Handler())
	t.Cleanup(func() { goTS.Close(); goSrv.Shutdown() })
	goSrv.StartJobs()
	goSide := &rtSide{name: "go", base: goTS.URL, client: rtClient(t)}
	t.Setenv("FACET_DATABASE_URL", fmt.Sprintf("facetql://tok@127.0.0.1:%d", fctPort))
	fctSide := rtStartFct(t, g)
	steps := []rtStep{
		{name: "become ada", method: "GET", path: "/?as=ada"},
		{name: "pay", method: "POST", path: "/api/pay", body: `{"args":["bob",5]}`},
		{name: "logs", method: "GET", path: "/api/Log"},
		{name: "accounts", method: "GET", path: "/api/Acct"},
		{name: "accounts again", method: "GET", path: "/api/Acct"},
		{name: "logs again", method: "GET", path: "/api/Log"},
	}
	for _, st := range steps {
		ga, fa := rtDo(t, goSide, st, ""), rtDo(t, fctSide, st, "")
		rtCompare(t, st, ga, fa)
		if ga.status >= 500 {
			t.Errorf("%s: %d %s", st.name, ga.status, ga.body)
		}
	}
	for _, e := range g.Entities {
		if goRows, fct := rtEngineRows(t, goPort, e.Name), rtEngineRows(t, fctPort, e.Name); goRows != fct {
			t.Errorf("%s rows differ between the engines:\n%s", e.Name, rtDiff(goRows, fct))
		}
	}
}

// rtAssetName is the content-addressed name the app's one bundled asset is
// served under.
func rtAssetName(t *testing.T, g *ir.IR) string {
	t.Helper()
	for name := range g.Assets {
		return name
	}
	t.Fatal("the app bundles no asset")
	return ""
}

// TestRuntimeParityAdmin drives the generated admin console (admin.go) and
// the stored form of @password fields through both runtimes: the console
// refuses anyone but an admin, lists, forms, creates, edits (a blank
// password leaves the hash), deletes with a cascade, and checks its CSRF
// token; a `set` of another field never re-hashes a stored password, an
// empty password stores empty, and one over 72 bytes is refused.
func TestRuntimeParityAdmin(t *testing.T) {
	app := "testdata/runtime_parity/admin.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	gGo, _ := compile.File(app)
	goSide := rtStartGo(t, gGo)
	fctSide := rtStartFct(t, g)
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	long := strings.Repeat("x", 73)
	steps := []rtStep{
		{name: "admin with no session", method: "GET", path: "/admin"},
		{name: "home", method: "GET", path: "/"},
		{name: "join ada", method: "POST", path: "/api/join", body: `{"args":["ada","correct horse"]}`},
		{name: "join with a password bcrypt cannot take", method: "POST", path: "/api/join", body: `{"args":["bob","` + long + `"]}`},
		{name: "join with no password", method: "POST", path: "/api/join", body: `{"args":["cy",""]}`},
		{name: "write", method: "POST", path: "/api/write", body: `{"args":["ada","hello"]}`},
		{name: "rescore leaves the password", method: "POST", path: "/api/rescore", body: `{"args":["ada",2.5]}`},
		{name: "sign in after the rescore", method: "POST", path: "/api/signIn", body: `{"args":["ada","correct horse"]}`},
		{name: "an empty password never matches", method: "POST", path: "/api/signIn", body: `{"args":["cy",""]}`},
		{name: "repassword too long", method: "POST", path: "/api/repassword", body: `{"args":[1,"` + long + `"]}`},
		{name: "become a member", method: "POST", path: "/api/becomeMember", body: `{"args":[]}`},
		{name: "admin as a member", method: "GET", path: "/admin"},
		{name: "become admin", method: "POST", path: "/api/becomeAdmin", body: `{"args":[]}`},
		{name: "who, as admin", method: "POST", path: "/api/whoAmI", body: `{"args":[]}`},
		{name: "admin index", method: "GET", path: "/admin"},
		{name: "admin index, trailing slash", method: "GET", path: "/admin/"},
		{name: "authors", method: "GET", path: "/admin/Author"},
		{name: "posts", method: "GET", path: "/admin/Post/"},
		{name: "new author form", method: "GET", path: "/admin/Author/new"},
		{name: "edit author 1", method: "GET", path: "/admin/Author/1"},
		{name: "edit a missing author", method: "GET", path: "/admin/Author/99"},
		{name: "an unknown entity", method: "GET", path: "/admin/Nope"},
		{name: "save needs POST", method: "GET", path: "/admin/_save"},
		{name: "save with a bad token", method: "POST", path: "/admin/_save", headers: form, body: "_csrf=nope&_entity=Author&_id=0"},
		{name: "save an unknown entity", method: "POST", path: "/admin/_save", headers: form, body: "_csrf=SIDE_CSRF&_entity=Nope&_id=0", csrf: true},
		{name: "create an author", method: "POST", path: "/admin/_save", headers: form, body: "_csrf=SIDE_CSRF&_entity=Author&_id=0&handle=dee+d%C3%A9&password=pw&token=tk&active=1&score=3.25&tier=pro&joined=5", csrf: true},
		{name: "create with a password too long", method: "POST", path: "/admin/_save", headers: form, body: "_csrf=SIDE_CSRF&_entity=Author&_id=0&handle=eve&password=" + long, csrf: true},
		{name: "edit ada, password blank", method: "POST", path: "/admin/_save", headers: form, body: "_csrf=SIDE_CSRF&_entity=Author&_id=1&handle=ada2&password=&score=9&tier=pro&joined=7", csrf: true},
		{name: "ada's password survived the edit", method: "POST", path: "/api/signIn", body: `{"args":["ada2","correct horse"]}`},
		{name: "authors after the edits", method: "GET", path: "/admin/Author"},
		{name: "delete needs POST", method: "GET", path: "/admin/_delete"},
		{name: "delete with a bad token", method: "POST", path: "/admin/_delete", headers: form, body: "_csrf=nope&_entity=Author&_id=1"},
		{name: "delete ada", method: "POST", path: "/admin/_delete", headers: form, body: "_csrf=SIDE_CSRF&_entity=Author&_id=1", csrf: true},
		{name: "delete a missing row", method: "POST", path: "/admin/_delete", headers: form, body: "_csrf=SIDE_CSRF&_entity=Author&_id=42", csrf: true},
		{name: "posts after the cascade", method: "GET", path: "/admin/Post"},
		{name: "index after the delete", method: "GET", path: "/admin"},
		{name: "the audit feed", method: "GET", path: "/api/_audit"},
		{name: "the audit feed, three", method: "GET", path: "/api/_audit?limit=3"},
		{name: "the audit feed, a bad limit", method: "GET", path: "/api/_audit?limit=-2"},
		{name: "the audit feed over POST", method: "POST", path: "/api/_audit", body: `{}`},
		{name: "export my own data", method: "GET", path: "/api/_export"},
		{name: "export another's data as admin", method: "GET", path: "/api/_export?user=cy"},
		{name: "erase needs POST", method: "GET", path: "/api/_erase"},
		{name: "erase another as admin", method: "POST", path: "/api/_erase?user=cy", body: `{}`},
		{name: "erase by form value", method: "POST", path: "/api/_erase", headers: form, body: "user=dee+d%C3%A9"},
		{name: "authors after the erasures", method: "GET", path: "/admin/Author"},
		{name: "the audit feed after the erasures", method: "GET", path: "/api/_audit?limit=4"},
		{name: "become a member again", method: "POST", path: "/api/becomeMember", body: `{"args":[]}`},
		{name: "the audit feed as a member", method: "GET", path: "/api/_audit"},
		{name: "export another's data as a member", method: "GET", path: "/api/_export?user=root"},
		{name: "export my own data as a member", method: "GET", path: "/api/_export"},
		{name: "erase myself", method: "POST", path: "/api/_erase", body: `{}`},
		{name: "who, after erasing myself", method: "POST", path: "/api/whoAmI", body: `{"args":[]}`},
		{name: "metrics", method: "GET", path: "/metrics"},
	}
	var goCsrf, fctCsrf string
	for _, st := range steps {
		if st.csrf {
			goCsrf, fctCsrf = rtCsrfOf(t, goSide), rtCsrfOf(t, fctSide)
		}
		ga, fa := rtDo(t, goSide, st, goCsrf), rtDo(t, fctSide, st, fctCsrf)
		if os.Getenv("RT_PARITY_TRACE") != "" {
			t.Logf("%-40s go=%d fct=%d %.120s", st.name, ga.status, fa.status, ga.body)
		}
		rtCompare(t, st, ga, fa)
		if t.Failed() && os.Getenv("RT_PARITY_ALL") == "" {
			t.Fatalf("stopping at the first failing step (%s)", st.name)
		}
	}
}

// TestRuntimeParityWire drives wire-typed action parameters (argFor: a
// [Type] or Type parameter shaped field by field, absent fields zero, nested
// types shaped, anything else refused; a list is never the whole body) and
// float ordering (lessVal: a float on either side compares as floats)
// through both runtimes.
func TestRuntimeParityWire(t *testing.T) {
	app := "testdata/runtime_parity/wire.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	t.Setenv("FACET_API_READ", "Line,R")
	gGo, _ := compile.File(app)
	goSide := rtStartGo(t, gGo)
	fctSide := rtStartFct(t, g)
	steps := []rtStep{
		{name: "home", method: "GET", path: "/"},
		{name: "a list of items", method: "POST", path: "/api/items", body: `{"items":[{"name":"tea","qty":"2","price":3},{"name":"cake","qty":1,"price":4.5,"note":"birthday","tag":{"label":"sweet","weight":"0.5"},"tags":[{"label":"a"},{"label":"b","weight":2}]}]}`},
		{name: "elements that are not objects", method: "POST", path: "/api/items", body: `{"items":[1,2]}`},
		{name: "a field of the wrong type", method: "POST", path: "/api/items", body: `{"items":[{"name":"x","qty":"lots"}]}`},
		{name: "an object where a list belongs", method: "POST", path: "/api/items", body: `{"items":{"name":"x"}}`},
		{name: "a nested field of the wrong type", method: "POST", path: "/api/items", body: `{"items":[{"name":"x","qty":1,"tags":[{"weight":"heavy"}]}]}`},
		{name: "a list as the whole body", method: "POST", path: "/api/items", body: `[{"name":"x","qty":1}]`},
		{name: "no items at all", method: "POST", path: "/api/items", body: `{}`},
		{name: "a failing check rolls back", method: "POST", path: "/api/items", body: `{"items":[{"name":"ok","qty":1},{"name":"zero","qty":0}]}`},
		{name: "a wire type as the whole body", method: "POST", path: "/api/orders", body: `{"who":"ada","rush":true,"first":{"name":"tea","qty":"3"}}`},
		{name: "a whole body missing its nested type", method: "POST", path: "/api/orders", body: `{"who":"bo"}`},
		{name: "a nested type that is not an object", method: "POST", path: "/api/orders", body: `{"who":"cy","first":5}`},
		{name: "a list beside a path parameter", method: "POST", path: "/api/batches/ann", body: `{"items":[{"name":"a","qty":1},{"name":"b"}]}`},
		{name: "a list over the generic api", method: "POST", path: "/api/addItems", body: `{"args":[[{"name":"pie","qty":4,"price":"1.25"}]]}`},
		{name: "a bad list over the generic api", method: "POST", path: "/api/addItems", body: `{"args":[["pie"]]}`},
		{name: "lines", method: "GET", path: "/api/Line"},
		{name: "seed floats", method: "POST", path: "/api/seedRows", body: `{"args":[]}`},
		{name: "top by float", method: "POST", path: "/api/top", body: `{"args":[]}`},
		{name: "bottom by float", method: "POST", path: "/api/bottom", body: `{"args":[]}`},
		{name: "rows by float", method: "GET", path: "/api/R?by=sim&desc=1"},
		{name: "home after", method: "GET", path: "/"},
		{name: "metrics", method: "GET", path: "/metrics"},
	}
	for _, st := range steps {
		ga, fa := rtDo(t, goSide, st, ""), rtDo(t, fctSide, st, "")
		if os.Getenv("RT_PARITY_TRACE") != "" {
			t.Logf("%-40s go=%d fct=%d %.160s", st.name, ga.status, fa.status, ga.body)
		}
		rtCompare(t, st, ga, fa)
		if t.Failed() && os.Getenv("RT_PARITY_ALL") == "" {
			t.Fatalf("stopping at the first failing step (%s)", st.name)
		}
	}
}

// TestRuntimeParityI18n drives the message catalogs (compliance.go) through
// both runtimes: FACET_I18N_DIR's <locale>.json files that are objects of
// strings, the locale negotiated from ?lang= then Accept-Language (a tag or
// its base subtag) then the default, stamped on a page as Content-Language
// and served at /api/_i18n; and handleAPI's other own names, _contract and
// _billing.
func TestRuntimeParityI18n(t *testing.T) {
	app := "testdata/runtime_parity/wire.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	dir := filepath.Join(shared, "i18n")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"fr.json":   `{"hello":"bonjour","bye":"au revoir","amp":"<a & b>"}`,
		"de.json":   `{"hello":1}`,
		"es.json":   `["hola"]`,
		"notes.txt": `{"hello":"x"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FACET_I18N_DIR", dir)
	gGo, _ := compile.File(app)
	goSide := rtStartGo(t, gGo)
	fctSide := rtStartFct(t, g)
	al := func(v string) map[string]string { return map[string]string{"Accept-Language": v} }
	steps := []rtStep{
		{name: "page, no preference", method: "GET", path: "/"},
		{name: "page, ?lang=fr", method: "GET", path: "/?lang=fr"},
		{name: "page, ?lang= unknown", method: "GET", path: "/?lang=xx"},
		{name: "page, a base subtag", method: "GET", path: "/", headers: al("fr-CA,en;q=0.8")},
		{name: "page, a catalog that is not strings", method: "GET", path: "/", headers: al("de")},
		{name: "page, a catalog that is not an object", method: "GET", path: "/", headers: al("es, fr")},
		{name: "page, a wildcard", method: "GET", path: "/", headers: al("*")},
		{name: "catalog, default", method: "GET", path: "/api/_i18n"},
		{name: "catalog, fr", method: "GET", path: "/api/_i18n?lang=fr"},
		{name: "catalog, negotiated", method: "GET", path: "/api/_i18n", headers: al("fr-FR;q=0.9")},
		{name: "catalog over POST", method: "POST", path: "/api/_i18n?lang=fr", body: `{}`},
		{name: "contract", method: "GET", path: "/api/_contract"},
		{name: "contract again", method: "GET", path: "/api/_contract"},
		{name: "contract over POST", method: "POST", path: "/api/_contract", body: `{}`},
		{name: "billing, off", method: "GET", path: "/api/_billing"},
	}
	for _, st := range steps {
		ga, fa := rtDo(t, goSide, st, ""), rtDo(t, fctSide, st, "")
		if os.Getenv("RT_PARITY_TRACE") != "" {
			t.Logf("%-40s go=%d fct=%d %s %.160s", st.name, ga.status, fa.status, ga.lang, ga.body)
		}
		rtCompare(t, st, ga, fa)
		if t.Failed() && os.Getenv("RT_PARITY_ALL") == "" {
			t.Fatalf("stopping at the first failing step (%s)", st.name)
		}
	}
}

// TestRuntimeParityProcs drives spawn/join in app procs through both
// runtimes: two children joined in turn, a child's failure surfacing at its
// join, and a spawn joined inside a loop.
func TestRuntimeParityProcs(t *testing.T) {
	app := "testdata/runtime_parity/procs.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	gGo, _ := compile.File(app)
	goSide := rtStartGo(t, gGo)
	fctSide := rtStartFct(t, g)
	steps := []rtStep{
		{name: "two children", method: "POST", path: "/api/run", body: `{"args":[500,333]}`},
		{name: "a child fails at its join", method: "POST", path: "/api/failing", body: `{"args":[3]}`},
		{name: "a child that succeeds", method: "POST", path: "/api/failing", body: `{"args":[0]}`},
		{name: "a spawn per iteration", method: "POST", path: "/api/many", body: `{"args":[]}`},
		{name: "write then read a text file", method: "POST", path: "/api/note", body: `{"args":["first line"]}`},
		{name: "overwrite it", method: "POST", path: "/api/note", body: `{"args":["second\nline"]}`},
		{name: "write then read a byte file", method: "POST", path: "/api/blobs", body: `{"args":[7]}`},
		{name: "a byte out of range", method: "POST", path: "/api/blobs", body: `{"args":[300]}`},
		{name: "a path out of the sandbox", method: "POST", path: "/api/escapes", body: `{"args":[]}`},
		{name: "a file never written", method: "POST", path: "/api/missings", body: `{"args":[]}`},
		{name: "home", method: "GET", path: "/"},
		{name: "metrics", method: "GET", path: "/metrics"},
	}
	for _, st := range steps {
		ga, fa := rtDo(t, goSide, st, ""), rtDo(t, fctSide, st, "")
		if os.Getenv("RT_PARITY_TRACE") != "" {
			t.Logf("%-40s go=%d fct=%d %.160s", st.name, ga.status, fa.status, ga.body)
		}
		rtCompare(t, st, ga, fa)
		if t.Failed() && os.Getenv("RT_PARITY_ALL") == "" {
			t.Fatalf("stopping at the first failing step (%s)", st.name)
		}
	}
}

// TestRuntimeCluster runs two instances over one FacetQL with
// FACET_CLUSTER=1 (cluster.go) — two fct runtimes, and a Go runtime beside
// an fct one, which must read each other's sessions and announcements —
// and checks what clustersession_test.go checks of the Go runtime: a
// session minted on one instance works on the other; one re-keyed or
// revoked on one stops working on the other, including one only the other
// ever held; and an entity written on one reaches the other's pages.
func TestRuntimeCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a FacetQL engine and two runtimes")
	}
	t.Run("fct-fct", func(t *testing.T) { rtRunCluster(t, false) })
	t.Run("go-fct", func(t *testing.T) { rtRunCluster(t, true) })
}

func rtRunCluster(t *testing.T, aGo bool) {
	// Unmetered, as the durable runs are: pacing under the engine's limiter
	// is TestRuntimeParityRateLimited's subject, not this one's.
	port := rtShippedEngine(t, "engine", rtUnmetered...)
	app := "testdata/runtime_parity/cluster.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	// The Go runtime reads FACET_SECRET once per process (its keyring):
	// every test in this package signs with the same one.
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	t.Setenv("FACET_RATE_LIMIT_AUTH", "6000")
	t.Setenv("FACET_CLUSTER", "1")
	t.Setenv("FACET_DATABASE_URL", fmt.Sprintf("facetql://tok@127.0.0.1:%d", port))
	var a string
	if aGo {
		gGo, _ := compile.File(app)
		srv, err := runtime.New(gGo)
		if err != nil {
			t.Fatalf("go runtime: %v", err)
		}
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(func() { ts.Close(); srv.Shutdown() })
		a = ts.URL
	} else {
		a = rtStartFct(t, g).base
	}
	g2, _ := compile.File(app)
	b := rtStartFct(t, g2).base

	call := func(base, method, path, token, body string) (int, map[string]any, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		json.Unmarshal(raw, &out)
		return resp.StatusCode, out, string(raw)
	}
	eventually := func(base, token string, want int, what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			code, _, _ := call(base, "GET", "/api/me", token, "")
			if code == want {
				return
			}
			if time.Now().After(deadline) {
				_, _, body := call(base, "GET", "/api/me", token, "")
				t.Fatalf("%s: GET /api/me = %d, want %d (%s); shared sessions:\n%s", what, code, want, body, rtEngineRows(t, port, "__session"))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	_, body, raw := call(a, "POST", "/api/accounts", "", `{"handle":"ada","password":"correct horse"}`)
	t1, _ := body["token"].(string)
	if t1 == "" {
		t.Fatalf("signup on A: %s", raw)
	}
	eventually(b, t1, http.StatusOK, "a session minted on A, used on B")

	_, body, _ = call(a, "POST", "/api/sessions", t1, `{"handle":"ada","password":"correct horse"}`)
	t2, _ := body["token"].(string)
	if t2 == "" || t2 == t1 {
		t.Fatalf("login did not re-key (t1 %q, t2 %q)", t1, t2)
	}
	eventually(b, t1, http.StatusUnauthorized, "the pre-login id on the peer")
	eventually(b, t2, http.StatusOK, "the re-keyed id on the peer")

	_, body, _ = call(b, "POST", "/api/sessions", "", `{"handle":"ada","password":"correct horse"}`)
	t3, _ := body["token"].(string)
	_, key, _ := call(b, "GET", "/api/me/session", t3, "")
	if code, _, raw := call(a, "DELETE", "/api/sessions", t2, `{"key":"`+fmt.Sprint(key["token"])+`"}`); code != http.StatusNoContent {
		t.Fatalf("revoke on A = %d %s", code, raw)
	}
	eventually(b, t3, http.StatusUnauthorized, "a peer-only session revoked from A")
	eventually(a, t3, http.StatusUnauthorized, "the same session on A (rehydration finds nothing)")

	if code, _, raw := call(a, "POST", "/api/write", t2, `{"args":["from A"]}`); code != http.StatusOK {
		t.Fatalf("write on A = %d %s", code, raw)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, _, page := call(b, "GET", "/", "", "")
		if rtNoteCount.MatchString(page) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B never showed A's note:\n%s", rtRootOf(page))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// rtNoteCount: the cluster app's home page once it counts one note (the
// count renders inside its binding's element).
var rtNoteCount = regexp.MustCompile(`>1</[a-z]+> notes|>1 notes<`)

// rtRootOf: a page's rendered root, for a failure message.
func rtRootOf(page string) string {
	if i := strings.Index(page, `<div id="fa-root"`); i >= 0 {
		page = page[i:]
		if j := strings.Index(page, "</div>"); j >= 0 {
			return page[:j+6]
		}
	}
	return page
}

// TestRuntimeStreamsKeepRequestsFlat: open change streams must not slow the
// fct runtime's other requests. On the site (157 entities, 57 pages), with
// streams open whose clients never read — every change still fans out to
// them — a write costs what it costs with none: the loop neither walks the
// whole graph per broadcast nor waits on a subscriber that cannot keep up
// (a full outbox is skipped, as Go's select … default does).
func TestRuntimeStreamsKeepRequestsFlat(t *testing.T) {
	app := "../../facets/f33d3r_com.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	t.Setenv("FACET_RATE_LIMIT_AUTH", "6000")
	t.Setenv("FACET_API_READ", "Account,Work")
	side := rtStartFct(t, g)
	n := 0
	signup := func() time.Duration {
		n++
		st := rtStep{name: "signup", method: "POST", path: "/api/v2/accounts", body: fmt.Sprintf(`{"handle":"user%d","password":"battery-staple-%d","terms_accepted":true}`, n, n)}
		t0 := time.Now()
		if a := rtDo(t, side, st, ""); a.status != 201 {
			t.Fatalf("signup = %d %s", a.status, a.body)
		}
		return time.Since(t0)
	}
	var base time.Duration
	for i := 0; i < 3; i++ {
		if d := signup(); d > base {
			base = d
		}
	}
	// Streams whose clients read the opening frame and then nothing more.
	for i := 0; i < 4; i++ {
		c, err := net.Dial("tcp", strings.TrimPrefix(side.base, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		fmt.Fprintf(c, "GET /api/_live HTTP/1.1\r\nHost: x\r\nAccept: text/event-stream\r\n\r\n")
		buf := make([]byte, 512)
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		if _, err := c.Read(buf); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
	limit := 3*base + time.Second
	for i := 0; i < 8; i++ {
		if d := signup(); d > limit {
			t.Fatalf("write %d with 4 idle streams open took %v; with none the slowest took %v", i, d, base)
		}
	}
}

// TestRuntimeParityRowIndex drives runtime/rowindex_test.go's app through
// both runtimes: 400 works and 300 likes (enough for the indexes to be
// built), then every aggregate, lookup, `in` probe, capped list and exists
// (with their early stops) and the writes between reads that must drop the
// indexes — the fct runtime's indexed answers against the Go runtime's.
func TestRuntimeParityRowIndex(t *testing.T) {
	app := "testdata/runtime_parity/rowindex.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	t.Setenv("FACET_RATE_LIMIT", "100000")
	gGo, _ := compile.File(app)
	goSide := rtStartGo(t, gGo)
	fctSide := rtStartFct(t, g)
	var steps []rtStep
	// Seeded as rowindex_test.go seeds them: an int field holding ints,
	// nothing, text that reads as the number, a float and a bool; a text
	// field holding a number every 13th row — the mixed values go through
	// the app's untyped path (fromJson), whose value lands as it is.
	for i := 1; i <= 400; i++ {
		quoted := fmt.Sprint(i % 37)
		switch i % 11 {
		case 0:
			quoted = "null"
		case 1:
			quoted = fmt.Sprintf("%q", fmt.Sprint(i%37))
		case 2:
			quoted = fmt.Sprint(i % 37) // a JSON number: a float, integral or not
		case 3:
			quoted = fmt.Sprint(i%2 == 0)
		}
		tag := fmt.Sprintf("%q", fmt.Sprintf("t%d", i%9))
		if i%13 == 0 {
			tag = fmt.Sprint(i % 9)
		}
		if i%7 == 0 && i%11 > 3 {
			quoted = fmt.Sprintf("%d.5", i%37) // a float no int equals
		}
		if i%11 <= 3 || i%13 == 0 || i%7 == 0 {
			steps = append(steps, rtStep{name: "seed work (untyped)", method: "POST", path: "/api/addRawWork", body: fmt.Sprintf(`{"args":[%d,%q,%q,%d]}`, 1+i%17, quoted, tag, i%5)})
		} else {
			steps = append(steps, rtStep{name: "seed work", method: "POST", path: "/api/addWork", body: fmt.Sprintf(`{"args":[%d,%d,"t%d",%d]}`, 1+i%17, i%37, i%9, i%5)})
		}
	}
	for i := 1; i <= 300; i++ {
		steps = append(steps, rtStep{name: "seed like", method: "POST", path: "/api/addLike", body: fmt.Sprintf(`{"args":[%d,%d]}`, 1+i%97, 1+i%7)})
	}
	for x := 0; x <= 40; x++ {
		for _, tg := range []string{"t3", "3", "", "t8"} {
			steps = append(steps, rtStep{name: fmt.Sprintf("report(%d,%q)", x, tg), method: "POST", path: "/api/report", body: fmt.Sprintf(`{"args":[%d,%q,%d]}`, x, tg, x%7)})
		}
		steps = append(steps, rtStep{name: fmt.Sprintf("probes(%d)", x), method: "POST", path: "/api/probes", body: fmt.Sprintf(`{"args":[%d]}`, x)})
		if x <= 20 {
			steps = append(steps, rtStep{name: fmt.Sprintf("lists(%d)", x), method: "POST", path: "/api/lists", body: fmt.Sprintf(`{"args":[%d]}`, x)})
		}
	}
	for _, x := range []int{0, 1, 5, 36, 99} {
		steps = append(steps, rtStep{name: fmt.Sprintf("mutate(%d)", x), method: "POST", path: "/api/mutate", body: fmt.Sprintf(`{"args":[%d]}`, x)})
	}
	for _, st := range steps {
		ga, fa, _, _ := rtDoPair(t, goSide, fctSide, st, st, "", "")
		rtCompare(t, st, ga, fa)
		if t.Failed() && os.Getenv("RT_PARITY_ALL") == "" {
			t.Fatalf("stopping at the first failing step (%s)", st.name)
		}
	}
}

// TestRuntimeParityColumns: a column holds its declared type from the first
// write (runtime/store.go's columnValue), so the same writes read the same
// on both runtimes, in memory and over a store — and over a store, the same
// again after a restart (a second pair of runtimes loading what the first
// wrote).
func TestRuntimeParityColumns(t *testing.T) {
	app := "testdata/runtime_parity/columns.fct"
	steps := []rtStep{
		{name: "nothing everywhere", method: "POST", path: "/api/put", body: `{"args":["null","null","null","null","null"]}`},
		{name: "numbers", method: "POST", path: "/api/put", body: `{"args":["3.7","2","1","12","5"]}`},
		{name: "text", method: "POST", path: "/api/put", body: `{"args":["\"12\"","\"2.5\"","\"yes\"","true","\"9\""]}`},
		{name: "bools", method: "POST", path: "/api/put", body: `{"args":["true","false","0","4.5","null"]}`},
		{name: "set a fraction", method: "POST", path: "/api/reset", body: `{"args":[2,"9.9"]}`},
	}
	read := rtStep{name: "rows", method: "GET", path: "/api/Row?by=id"}
	boot := func(t *testing.T, dsnGo, dsnFct string) (*rtSide, *rtSide) {
		g, err := compile.File(app)
		if err != nil {
			t.Fatalf("compile %s: %v", app, err)
		}
		t.Setenv("FACET_SECRET", "runtime-parity-secret")
		shared := t.TempDir()
		t.Setenv("FACET_DATA_DIR", shared)
		t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
		t.Setenv("FACET_API_READ", "Row")
		t.Setenv("FACET_DATABASE_URL", dsnGo)
		gGo, _ := compile.File(app)
		var goSide *rtSide
		if dsnGo == "" {
			goSide = rtStartGo(t, gGo)
		} else {
			goSrv, err := runtime.New(gGo)
			if err != nil {
				t.Fatalf("go runtime: %v", err)
			}
			goTS := httptest.NewServer(goSrv.Handler())
			t.Cleanup(func() { goTS.Close(); goSrv.Shutdown() })
			goSide = &rtSide{name: "go", base: goTS.URL, client: rtClient(t)}
		}
		t.Setenv("FACET_DATABASE_URL", dsnFct)
		return goSide, rtStartFct(t, g)
	}
	compare := func(t *testing.T, goSide, fctSide *rtSide) string {
		for _, st := range steps {
			ga, fa, _, _ := rtDoPair(t, goSide, fctSide, st, st, "", "")
			rtCompare(t, st, ga, fa)
		}
		ga, fa, _, _ := rtDoPair(t, goSide, fctSide, read, read, "", "")
		rtCompare(t, read, ga, fa)
		if os.Getenv("RT_PARITY_TRACE") != "" {
			t.Logf("rows: %s", ga.body)
		}
		return ga.body
	}
	t.Run("memory", func(t *testing.T) {
		goSide, fctSide := boot(t, "", "")
		compare(t, goSide, fctSide)
	})
	t.Run("store", func(t *testing.T) {
		goPort, fctPort := rtShippedEnginePair(t, rtUnmetered...)
		dsnGo := fmt.Sprintf("facetql://tok@127.0.0.1:%d", goPort)
		dsnFct := fmt.Sprintf("facetql://tok@127.0.0.1:%d", fctPort)
		var before string
		t.Run("written", func(t *testing.T) {
			goSide, fctSide := boot(t, dsnGo, dsnFct)
			before = compare(t, goSide, fctSide)
		})
		t.Run("after a restart", func(t *testing.T) {
			goSide, fctSide := boot(t, dsnGo, dsnFct)
			ga, fa, _, _ := rtDoPair(t, goSide, fctSide, read, read, "", "")
			rtCompare(t, read, ga, fa)
			if ga.body != before {
				t.Errorf("rows after a restart differ from before it:\n%s", rtDiff(before, ga.body))
			}
		})
	})
}

// TestRuntimeParityMetricsAcrossTicks forces the race the site's /metrics
// step can lose: both runtimes' `every Ns` jobs tick on their own clocks
// (the fct runtime starts later), so for the next 25 seconds /metrics is
// read on both every 200 ms, across several ticks on each side, and every
// read must agree — the requests each side counted, and nothing of a tick.
func TestRuntimeParityMetricsAcrossTicks(t *testing.T) {
	app := "../../facets/f33d3r_com.fct"
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "runtime-parity-secret")
	shared := t.TempDir()
	t.Setenv("FACET_DATA_DIR", shared)
	t.Setenv("FACET_UPLOAD_DIR", filepath.Join(shared, "facet-uploads"))
	gGo, _ := compile.File(app)
	goSide := rtStartGo(t, gGo)
	fctSide := rtStartFct(t, g)
	rtMarkTicking(g, goSide, fctSide)
	st := rtStep{name: "metrics", method: "GET", path: "/metrics"}
	deadline := time.Now().Add(25 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		ga, fa, _, _ := rtDoPair(t, goSide, fctSide, st, st, "", "")
		st.name = fmt.Sprintf("metrics read %d", i)
		rtCompare(t, st, ga, fa)
		if t.Failed() {
			t.FailNow()
		}
		time.Sleep(200 * time.Millisecond)
	}
}
