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
//   - `emit` evaluates its payload and drops it: the declared event
//     streams (streams.go) and webhooks (webhook.go) are the next slice.
//     Ported since the first phase: /upload, /upload/{init,chunk,finish,
//     abort} and /uploads/ (upload_media.fct, with a `fileModTime` builtin
//     added in Go for ServeFile's Last-Modified), the contract routes
//     (contract_doc.fct / contract_history.fct: the OpenAPI document byte
//     for byte, /version, /history, /diff), and the FacetQL store
//     (store_facetql.fct: FACET_DATABASE_URL, index/reference migration,
//     paged load, one transaction per action, entity lists and the API
//     read through the engine — TestRuntimeParityDurable proves the fct
//     runtime on fqserver.fct and the Go runtime on the Rust engine answer
//     alike and leave the same rows in their engines).
//   - Not yet ported: /assets/, /admin, /metrics, signed media
//     (FACET_MEDIA_TTL is honoured on /uploads/ but not minted into
//     rendered URLs), i18n catalogs, HTTP Range on /uploads/, HLS playlist
//     rewriting, multipart (`bytes`) bodies and HTTP Basic (`basicId`) on
//     declared routes, proc spawn/join and file statements, aggregate
//     pushdown to the engine (aggregates are computed over the working
//     set; the values are the same), clustering.
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
	"os"
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
	"../../facets/f33d3r.fct",
	"../../facets/timeline.fct",
	"../../facets/home.fct",
	"../../facets/api/main.fct",
	"../../facets/live.fct",
	"../../facets/messages.fct",
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
	t.Cleanup(ts.Close)
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
	js, err := os.ReadFile("../runtime/assets/facet.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "facet.js"), js, 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewInMemory(rtFctRuntimeGraph(t))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDataDir(dir)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	postExprJSON(t, ts, "rtConfigure", port, "app.ir.json")
	srv.StartJobs()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	// The port parses the whole graph in fct before it listens: a budget
	// per megabyte of IR (facets/api/main.fct is ~4 MB), on top of a floor.
	deadline := time.Now().Add(20*time.Second + time.Duration(len(irJSON)/(1<<20)+1)*15*time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fct runtime never listened on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &rtSide{name: "fct", base: "http://" + addr, client: rtClient(t)}
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
	return s
}

func rtDo(t *testing.T, side *rtSide, st rtStep, csrf string) rtAnswer {
	t.Helper()
	path := strings.ReplaceAll(st.path, "SIDE_UPLOAD_ID", side.uploadID)
	path = strings.ReplaceAll(path, "SIDE_UPLOAD", side.upload)
	body := st.body
	for i := len(side.refs) - 1; i >= 0; i-- {
		n := strconv.Itoa(i)
		path = strings.ReplaceAll(path, "SIDE_SIGNED_"+n, side.signed[i])
		path = strings.ReplaceAll(path, "SIDE_REF_"+n, side.refs[i])
		body = strings.ReplaceAll(body, "SIDE_REF_"+n, side.refs[i])
	}
	req, err := http.NewRequest(st.method, side.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("%s: %s %s: %v", side.name, st.method, st.path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: %s %s: reading body: %v", side.name, st.method, st.path, err)
	}
	if m := rtTokenValue.FindSubmatch(b); m != nil {
		side.token = string(m[1])
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
		cookieAttrs: attrs, body: rtNormalize(string(b)), replay: resp.Header.Get("X-Facet-Idempotent-Replay")}
}

// rtCsrfOf reads the CSRF token a side stamped on the page it last served.
func rtCsrfOf(t *testing.T, side *rtSide) string {
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
	if g.replay != f.replay {
		t.Errorf("%s: idempotent replay header go=%q fct=%q", step.name, g.replay, f.replay)
	}
	if g.cookieAttrs != f.cookieAttrs {
		t.Errorf("%s: set-cookie go=%q fct=%q", step.name, g.cookieAttrs, f.cookieAttrs)
	}
	if g.body != f.body {
		t.Errorf("%s: body differs (%d vs %d bytes)\n%s", step.name, len(g.body), len(f.body), rtDiff(g.body, f.body))
	}
}

// rtExtraSteps are the steps one app's own rules need — run as the
// signed-in member, before the script signs out.
func rtExtraSteps(app string) []rtStep {
	switch filepath.Base(app) {
	case "live.fct":
		// StreamKey carries `read: owner == actor` and a `key @requires(member)`:
		// the owner mints one and sees it on the dashboard; a second member
		// sees no key row at all (the row-read rule) and may not mint one.
		return []rtStep{
			{name: "live: create channel", method: "POST", path: "/api/createChannel", body: `{"args":["Grace live","gaming"]}`},
			{name: "live: mint key", method: "POST", path: "/api/mintKey", body: `{"args":[1]}`},
			{name: "live: dashboard as owner", method: "GET", path: "/dashboard"},
			{name: "live: theater as owner", method: "GET", path: "/live/1"},
			{name: "live: region dashboard", method: "POST", path: "/region", body: `{"path":"/dashboard","key":"","state":{}}`, csrf: true},
			{name: "live: logout owner", method: "POST", path: "/api/logout", body: `{"args":[]}`},
			{name: "live: signup alan", method: "POST", path: "/api/signup", body: `{"args":["alan","turing"]}`},
			{name: "live: dashboard as other member", method: "GET", path: "/dashboard"},
			{name: "live: mint key as other member", method: "POST", path: "/api/mintKey", body: `{"args":[1]}`},
			{name: "live: logout alan", method: "POST", path: "/api/logout", body: `{"args":[]}`},
			{name: "live: login grace again", method: "POST", path: "/api/login", body: `{"args":["grace","hopper"]}`},
		}
	}
	return nil
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
	rtDrive(t, app, g, goSide, fctSide)
}

// rtDrive runs the app's script against both sides, step by step.
func rtDrive(t *testing.T, app string, g *ir.IR, goSide, fctSide *rtSide) {
	t.Helper()
	var goCsrf, fctCsrf string
	script := rtScript(g)
	if len(g.APIs) > 0 {
		script = rtScriptAPI(g)
	} else if extra := rtExtraSteps(app); len(extra) > 0 {
		// Before the generic sign-out: the member the script signed in is
		// the one the app's own steps act as.
		for i, st := range script {
			if st.name == "logout" {
				script = append(append(append([]rtStep{}, script[:i]...), extra...), script[i:]...)
				break
			}
		}
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
		ga := rtDo(t, goSide, goStep, goCsrf)
		fa := rtDo(t, fctSide, fctStep, fctCsrf)
		if os.Getenv("RT_PARITY_TRACE") != "" {
			t.Logf("%-40s go=%d fct=%d %s", st.name, ga.status, fa.status, ga.contentType)
		}
		rtCompare(t, st, ga, fa)
		if t.Failed() && os.Getenv("RT_PARITY_ALL") == "" {
			t.Fatalf("stopping at the first failing step (%s); set RT_PARITY_ALL=1 to run the rest", st.name)
		}
	}
	if len(g.APIs) == 0 {
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
	type stream struct {
		r    *bufio.Reader
		resp *http.Response
	}
	// A stream that never delivers must fail the test, not hang it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	open := func(side *rtSide) *stream {
		req, _ := http.NewRequestWithContext(ctx, "GET", side.base+"/live", nil)
		resp, err := http.DefaultTransport.RoundTrip(rtWithCookies(side, req))
		if err != nil {
			t.Fatalf("%s: /live: %v", side.name, err)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("%s: /live: %s %q", side.name, resp.Status, resp.Header.Get("Content-Type"))
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
				return rtNormalize(b.String())
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
	st := rtStep{name: "change while subscribed (" + name + ")", method: "POST", path: "/api/" + name, body: body}
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

func TestRuntimeParityF33d3r(t *testing.T)   { rtRunApp(t, rtParityApps[0]) }
func TestRuntimeParityTimeline(t *testing.T) { rtRunApp(t, rtParityApps[1]) }
func TestRuntimeParityHome(t *testing.T)     { rtRunApp(t, rtParityApps[2]) }
func TestRuntimeParityApiMain(t *testing.T)  { rtRunApp(t, rtParityApps[3]) }
func TestRuntimeParityLive(t *testing.T)     { rtRunApp(t, rtParityApps[4]) }
func TestRuntimeParityMessages(t *testing.T) { rtRunApp(t, rtParityApps[5]) }

// TestRuntimeParityDurable is the parity harness with a store behind both
// runtimes: the Go runtime on the Rust FacetQL engine, the fct runtime on
// fqserver.fct (the engine written in fct), each reached through
// FACET_DATABASE_URL. The same script runs against both; afterwards the two
// engines must hold the same rows (kind, address and data, the clock
// readings and password hashes masked) — every write the fct runtime made
// reached its engine exactly as the Go runtime's reached its own.
func TestRuntimeParityDurable(t *testing.T) {
	if testing.Short() {
		t.Skip("starts two FacetQL engines per app")
	}
	for _, app := range rtParityApps {
		app := app
		t.Run(filepath.Base(app), func(t *testing.T) { rtRunDurable(t, app) })
	}
}

// rtRunDurable is one app's durable run: a fresh engine pair, the same
// script the in-memory run uses, then the engines compared.
func rtRunDurable(t *testing.T, app string) {
	t.Helper()
	rustPort, fctPort := fqServerPair(t)
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
	t.Setenv("FACET_DATABASE_URL", fmt.Sprintf("facetql://tok@127.0.0.1:%d", rustPort))
	gGo, _ := compile.File(app)
	goSrv, err := runtime.New(gGo)
	if err != nil {
		t.Fatalf("go runtime on the rust engine: %v", err)
	}
	goTS := httptest.NewServer(goSrv.Handler())
	t.Cleanup(goTS.Close)
	goSrv.StartJobs()
	goSide := &rtSide{name: "go", base: goTS.URL, client: rtClient(t)}
	t.Setenv("FACET_DATABASE_URL", fmt.Sprintf("facetql://tok@127.0.0.1:%d", fctPort))
	fctSide := rtStartFct(t, g)
	rtDrive(t, app, g, goSide, fctSide)
	for _, e := range g.Entities {
		rust := rtEngineRows(t, rustPort, e.Name)
		fct := rtEngineRows(t, fctPort, e.Name)
		if rust != fct {
			t.Errorf("%s rows differ between the engines:\n%s", e.Name, rtDiff(rust, fct))
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

// TestRuntimeParitySignedMedia runs facets/timeline.fct with media signing
// on (FACET_MEDIA_TTL): an upload answered with its signed URL, the
// /uploads/ access rules (unsigned, expired, tampered, signed), http.ServeFile's
// Range / If-Modified-Since / HEAD behaviour, an HLS playlist whose segment
// lines come back signed, and a video post whose page renders a signed src
// and whose bootstrap and region answer carry the grant map.
func TestRuntimeParitySignedMedia(t *testing.T) {
	app := rtParityApps[1]
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
	mp := map[string]string{"Content-Type": rtMultipartType}
	steps := []rtStep{
		{name: "signup", method: "POST", path: "/api/signup", body: `{"args":["grace","hopper"]}`},
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
