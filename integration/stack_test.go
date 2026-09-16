// Package integration boots the real stack — a FacetQL server process and an
// in-process fct runtime pointed at it — and asserts the properties that only
// hold when every layer is working together.
//
// It exists because each of the bugs it now guards against was invisible to
// every unit test in the tree and visible immediately to anyone who opened the
// page:
//
//   - a component with no parameters made the client throw during mount, and
//     because mount cleared the root first, the page went blank on every load.
//     Every Go test passed.
//   - a route parameter fed to an `int` action silently became 0, orphaning the
//     row, and the request answered {"ok":true}.
//   - one post longer than an index key bound made the app refuse to start,
//     permanently, because indexes are reconciled at boot.
//
// What they have in common is that they live in the seams: between Go and
// JavaScript, between the runtime and the engine, between compiling and
// booting. So this harness runs the actual binary, serves actual HTTP, and —
// where the property is a client one — executes the shipped client under Node
// against the page the server really sent.
package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/runtime"
)

// facetqlBinary locates the engine. The harness skips rather than fails when it
// is absent: a checkout without a built engine should not report a red suite for
// something it never had.
//
// It does NOT skip a binary that exists but predates the engine source it's
// meant to be proving — that already happened once (AGENT_LOG.md, "Changelog —
// 2026-09-06"): every "integration suite green" claim after 2026-09-04 20:40
// was run against a facetql built before ten `src` files it should have been
// exercising. A stale binary here is a false positive, not a missing one, so
// checkNotStale fails the suite instead.
func facetqlBinary(t *testing.T) string {
	t.Helper()

	if p := os.Getenv("FACETQL_BIN"); p != "" {
		checkNotStale(t, p)
		return p
	}

	for _, p := range []string{
		"../../facetql/target/release/facetql",
		"../../facetql/target/debug/facetql",
	} {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				checkNotStale(t, abs)
				return abs
			}
		}
	}

	t.Skip("no facetql binary; set FACETQL_BIN or build ../facetql")
	return ""
}

// checkNotStale refuses (t.Fatal, not t.Skip) a facetql binary that is older
// than the newest file under its own engine's src/ — the exact condition that
// let a two-day-stale binary pass as "integration green" on 09-04/09-06. It
// looks for src/ next to the binary's own facetql checkout (three directories
// up from target/{release,debug}/facetql, or ../../facetql/src as a fallback
// for a binary named directly by FACETQL_BIN); when neither exists — a binary
// built from a checkout this harness can't see, e.g. a CI artifact — it has no
// source to compare against and does not fail, since it cannot know.
func checkNotStale(t *testing.T, bin string) {
	t.Helper()

	binInfo, err := os.Stat(bin)
	if err != nil {
		return // facetqlBinary's own os.Stat already gated this path existing
	}

	src := filepath.Join(filepath.Dir(bin), "..", "..", "src") // target/{release,debug}/facetql -> facetql/src
	if _, err := os.Stat(src); err != nil {
		if abs, err2 := filepath.Abs("../../facetql/src"); err2 == nil {
			if _, err3 := os.Stat(abs); err3 == nil {
				src = abs
			} else {
				return // no src tree reachable — nothing to compare against
			}
		} else {
			return
		}
	}

	var newest string
	var newestTime time.Time
	_ = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".rs") {
			return nil
		}
		if info.ModTime().After(newestTime) {
			newestTime, newest = info.ModTime(), path
		}
		return nil
	})
	if newest == "" {
		return
	}
	if newestTime.After(binInfo.ModTime()) {
		t.Fatalf("facetql binary %s (built %s) is older than %s (modified %s) — "+
			"rebuild it first: cd facetql && cargo build --release. "+
			"An integration run against a stale engine is a false positive, not a pass.",
			bin, binInfo.ModTime().Format(time.RFC3339),
			newest, newestTime.Format(time.RFC3339))
	}
}

// freePort asks the kernel for a port and immediately releases it. Racy in
// principle, reliable in practice, and far better than a hardcoded port in a
// suite that may run beside a developer's own servers.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port
}

// engine is a running FacetQL with its own data directory.
type engine struct {
	t     *testing.T
	dir   string
	port  int
	token string
	cmd   *exec.Cmd
}

// startEngine boots FacetQL and waits for it to answer. The data directory is
// the test's own, so nothing here can see or corrupt a developer's database.
func startEngine(t *testing.T) *engine {
	t.Helper()

	return startEngineIn(t, t.TempDir())
}

// startEngineIn boots FacetQL against an existing data directory — how a restart
// is spelled, and the only way to test that anything was actually written down.
func startEngineIn(t *testing.T, dir string) *engine {
	t.Helper()

	bin := facetqlBinary(t)
	port := freePort(t)

	e := &engine{t: t, dir: dir, port: port, token: "integration-token"}

	log, err := os.Create(filepath.Join(dir, "facetql.log"))
	if err != nil {
		t.Fatalf("engine log: %v", err)
	}

	e.cmd = exec.Command(bin, "start")
	e.cmd.Env = append(os.Environ(),
		"ENOCHIAN_DATA_DIR="+dir,
		fmt.Sprintf("ENOCHIAN_PORT=%d", port),
		"ENOCHIAN_TOKENS="+e.token+":integration:admin",

		// FacetQL refuses to start with development defaults — an all-zero
		// at-rest key, no TLS — unless it is told this is not production. That
		// refusal is correct and this harness is not going to work around it by
		// supplying half-real credentials: a test that boots the engine the way
		// production boots it would need a real key and a real certificate, and
		// a test that pretends to have them teaches the wrong lesson. It says
		// what it is instead.
		"FACETQL_ENV=development",

		// Rate limiting off, and only here. The engine limits per identity,
		// and a test harness is one identity doing in two seconds what a
		// real deployment spreads over minutes — a suite that seeds 500 rows
		// would trip the `bulk` bucket and fail for a reason that is not the
		// property under test. Turning it off is stated explicitly (the
		// engine refuses to infer "unlimited" from a malformed value, so
		// `off` is the only spelling that means this) rather than by
		// choosing numbers large enough to hide the limiter, which would
		// silently stop testing anything the day a suite got bigger.
		"FACETQL_RATE_READ=off",
		"FACETQL_RATE_WRITE=off",
		"FACETQL_RATE_BULK=off",
		"FACETQL_RATE_ADMIN=off",
		"FACETQL_RATE_SUBSCRIBE=off",
	)
	e.cmd.Stdout = log
	e.cmd.Stderr = log

	if err := e.cmd.Start(); err != nil {
		t.Fatalf("starting facetql: %v", err)
	}

	t.Cleanup(e.stop)
	e.waitReady()

	return e
}

func (e *engine) dsn() string {
	return fmt.Sprintf("facetql://%s@127.0.0.1:%d", e.token, e.port)
}

func (e *engine) waitReady() {
	e.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := e.get("/stats"); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	body, _ := os.ReadFile(filepath.Join(e.dir, "facetql.log"))
	e.t.Fatalf("facetql never became ready; log:\n%s", body)
}

func (e *engine) get(path string) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d%s", e.port, path), nil)
	req.Header.Set("x-api-key", e.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, body)
	}

	return body, nil
}

// stop ends the engine. Killed rather than signalled politely: the test owns
// this process and its data directory outlives nothing.
func (e *engine) stop() {
	if e.cmd != nil && e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
		_ = e.cmd.Wait()
	}
}

func (e *engine) stats(t *testing.T) map[string]any {
	t.Helper()

	body, err := e.get("/stats")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding stats: %v", err)
	}

	return out
}

// app is the fct runtime serving a compiled .fct against an engine, in this
// process. In-process on purpose: a subprocess would hide a panic behind an exit
// code, and the point of an integration test is to see the failure.
type app struct {
	t    *testing.T
	port int
	jar  http.CookieJar
	cl   *http.Client
}

func startApp(t *testing.T, e *engine, source string) *app {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "app.fct")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("writing the app: %v", err)
	}

	return startAppFile(t, e, path)
}

func startAppFile(t *testing.T, e *engine, path string) *app {
	t.Helper()

	// The runtime reads its backend and secret from the environment, and
	// t.Setenv restores them when the test ends.
	t.Setenv("FACET_DATABASE_URL", e.dsn())
	t.Setenv("FACET_SECRET", "integration-secret-integration-secret")

	// One structured log line per request would bury the assertion that
	// actually failed under a few hundred lines of successful ones. Set
	// FACET_LOG_LEVEL=info when a test's behaviour is what you are debugging.
	if os.Getenv("FACET_LOG_LEVEL") == "" {
		t.Setenv("FACET_LOG_LEVEL", "error")
	}

	graph, err := compile.File(path)
	if err != nil {
		t.Fatalf("compiling %s: %v", path, err)
	}

	srv, err := runtime.New(graph)
	if err != nil {
		t.Fatalf("starting the runtime: %v", err)
	}

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	go func() { _ = srv.Serve(addr) }()

	a := &app{t: t, port: port, cl: &http.Client{Timeout: 30 * time.Second}}
	a.newSession()
	a.waitReady()

	return a
}

func newJar() http.CookieJar {
	jar, _ := cookiejar.New(nil)
	return jar
}

// newSession gives the client a fresh cookie jar, which is how the harness gets
// a *different actor*: sessions are cookie-bound, so a new jar is a new visitor.
func (a *app) newSession() {
	jar := newJar()
	a.jar = jar
	a.cl.Jar = jar
}

func (a *app) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", a.port, path)
}

func (a *app) waitReady() {
	a.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := a.cl.Get(a.url("/healthz"))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	a.t.Fatal("the app never became ready")
}

// get returns the status and body of a page request.
func (a *app) get(path string) (int, string) {
	a.t.Helper()

	resp, err := a.cl.Get(a.url(path))
	if err != nil {
		a.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// action calls one of the app's actions through the JSON API. Arguments are
// positional, matching the wire the client uses.
func (a *app) action(name string, args ...any) (int, string) {
	a.t.Helper()

	body, _ := json.Marshal(map[string]any{"args": args})

	resp, err := a.cl.Post(a.url("/api/"+name), "application/json", strings.NewReader(string(body)))
	if err != nil {
		a.t.Fatalf("POST /api/%s: %v", name, err)
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

var stateScript = regexp.MustCompile(`(?s)<script[^>]*id="fa-state"[^>]*>(.*?)</script>`)

// pageState decodes the state payload the server embedded in a page — the
// snapshot the client renders its first frame from, and therefore exactly what
// the server handed to this visitor's browser.
func pageState(t *testing.T, html string) map[string]any {
	t.Helper()

	m := stateScript.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("the page carries no fa-state payload")
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(m[1]), &out); err != nil {
		t.Fatalf("decoding fa-state: %v", err)
	}

	return out
}
