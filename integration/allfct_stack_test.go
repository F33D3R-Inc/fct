package integration

// The all-fct stack, end to end, against the reference stack.
//
// Every process is `facet exec` of a program in this tree: two FacetQL
// servers written in fct (selfhost/fqserver.fct), the Fabric control plane
// written in fct (selfhost/fabricd_lib.fct through testdata/
// fabricd_hotcell_gated.fct) placing one cell on each and serving
// FacetQL's wire on its data port — the fabric front door of
// selfhost/fabric_frontdoor.fct, fed the daemon's live routing — and the
// web runtime written in fct (selfhost/runtime_server.fct) serving the
// facets/ apps against that door. The same scripted session the runtime
// parity harness drives (selfhost/runtime_parity_test.go: sign-up, sign-in,
// posts, a feed read, a live SSE frame; for facets/api/main.fct its
// declared routes) is fired at it and at the reference stack — the Go
// runtime on the Rust facetql — and every answer compared after the same
// masking that harness applies. Midway, once the session has rows, the
// daemon is shown a hot cell (a gate file) and moves the app's data cell
// from one engine to the other while the session continues: no request may
// fail, the stream stays open across the cutover, and afterwards the rows
// are on the new holder and a new write lands there alone.
//
// The door in front is fabricd's own data port, not
// selfhost/fabric_frontdoor_main.fct: both are fabric_frontdoor.fct, but
// the standalone command is configured once from its environment
// (FRONTDOOR_PLACEMENTS) and cannot learn that a cell moved, whereas the
// daemon's port is re-fed the routing table every cycle — which is what a
// migration with no failed request needs.
//
// Two engine properties the daemon depends on are asserted explicitly,
// because a fleet fails on them silently: the daemon's liveness probe
// (GET / within probe_timeout_ms, here 500 ms, every 100 ms) must keep
// answering while the mover's 250-row transactions land on the
// destination and the session's writes land on the source — a probe that
// times out files no heartbeat, the silence budget runs out, the
// destination is declared unreachable mid-flight and the move is rolled
// back. The operator status is sampled throughout and must never show a
// backend other than serviceable, a probe other than "serving", or a
// refused copy; and the destination is probed directly, under the mover's
// load, with the daemon's own timeout.
//
// Three stacks are run, each as a subtest, so a failure names its layer:
//   go-runtime/rust-facetql        the reference; no fabric
//   go-runtime/fct-fabric          the fct daemon and engines, under a
//                                  runtime that persists through them
//   fct-runtime/fct-fabric         everything in fct
// Known gap, asserted rather than hidden (the last stack): the fct
// runtime holds its rows in memory — no runtime_*.fct uses io.net, and
// runtime_server.fct reads no FACET_DATABASE_URL — so nothing it serves
// reaches the engines and the cell it "migrates" is empty. Its answers
// still match the reference byte for byte; the assertion that the engine
// holds the rows fails, naming that.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

const (
	afEngineToken = "fabtok"
	afAdminToken  = "operator-secret"
	afSource      = "us-east-db-0"
	afDestination = "us-west-db-0"
	afSecret      = "allfct-stack-secret-allfct-stack-secret"
)

// afProc is one `facet exec` (or facetql) process with its log.
type afProc struct {
	cmd  *exec.Cmd
	log  *afLog
	done chan struct{}
}

type afLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *afLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *afLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func afStart(t *testing.T, cmd *exec.Cmd) *afProc {
	t.Helper()
	p := &afProc{cmd: cmd, log: &afLog{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.log, p.log
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", cmd.Path, err)
	}
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.done
	})
	return p
}

// afEngine is one FacetQL — the Rust facetql or fqserver.fct — with a
// token that is both owner and admin.
type afEngine struct {
	base string
	proc *afProc
}

func afEngineEnv(dir string, port int) []string {
	return []string{
		"FACETQL_DATA_DIR=" + dir, "FACET_DATA_DIR=" + dir,
		fmt.Sprintf("FACETQL_PORT=%d", port),
		"FACETQL_TOKENS=" + afEngineToken + ":fabric:admin",
		"FACETQL_ENV=development",
		"FACETQL_RATE_READ=off", "FACETQL_RATE_WRITE=off", "FACETQL_RATE_BULK=off", "FACETQL_RATE_ADMIN=off", "FACETQL_RATE_SUBSCRIBE=off",
	}
}

// afStartEngine starts one engine: "rust" (`facetql start`) or "fct"
// (`facet exec selfhost/fqserver.fct`).
func afStartEngine(t *testing.T, kind string) *afEngine {
	t.Helper()
	dir := t.TempDir()
	port := freePort(t)
	var cmd *exec.Cmd
	if kind == "rust" {
		cmd = exec.Command(facetqlBinary(t), "start")
	} else {
		server, err := filepath.Abs("../selfhost/fqserver.fct")
		if err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(facetBinary(t), "exec", server)
	}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), afEngineEnv(dir, port)...)
	e := &afEngine{base: fmt.Sprintf("http://127.0.0.1:%d", port), proc: afStart(t, cmd)}
	afWaitHTTP(t, e.base+"/", "", 200, 60*time.Second, e.proc)
	return e
}

// afWaitHTTP polls url until it answers status, or fails with the process
// log. An empty token sends no credential.
func afWaitHTTP(t *testing.T, url, token string, status int, budget time.Duration, p *afProc) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	for end := time.Now().Add(budget); ; time.Sleep(50 * time.Millisecond) {
		req, _ := http.NewRequest("GET", url, nil)
		if token != "" {
			req.Header.Set("x-api-key", token)
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == status {
				return
			}
		}
		select {
		case <-p.done:
			t.Fatalf("%s %s exited before answering (%v):\n%s", p.cmd.Path, strings.Join(p.cmd.Args[1:], " "), p.cmd.ProcessState, p.log.String())
		default:
		}
		if time.Now().After(end) {
			t.Fatalf("%s never answered %s:\n%s", p.cmd.Path, url, p.log.String())
		}
	}
}

// afFabric is the fct control plane over two engines: its data port (the
// door the runtime talks to), its operator port, and the directory whose
// `hot` file starts the migration.
type afFabric struct {
	data, admin string
	dir         string
	proc        *afProc
	source      *afEngine
	destination *afEngine
}

// afStartFabric runs testdata/fabricd_hotcell_gated.fct — fabricd with the
// gated HotCell source — placing shard 1's cell (0,0), the app's whole
// keyspace (every kind falls back to it), on the source engine and shard
// 2's on the destination. The cadence is fast and the probe budget tight:
// a probe every 100 ms, answered within 500 ms, five seconds of silence
// before a backend is out.
func afStartFabric(t *testing.T, source, destination *afEngine) *afFabric {
	t.Helper()
	dir := t.TempDir()
	dataPort, adminPort := freePort(t), freePort(t)
	config := fmt.Sprintf(`{
		"data_listen": "127.0.0.1:%d", "admin_listen": "127.0.0.1:%d",
		"backends": [
			{"id": %q, "url": %q, "region": "us-east", "token_env": "FABRIC_DB_TOKEN", "placements": [{"shard": 1, "x": 0, "y": 0}]},
			{"id": %q, "url": %q, "region": "us-west", "token_env": "FABRIC_DB_TOKEN", "placements": [{"shard": 2, "x": 0, "y": 0}]}
		],
		"keyspace": {"rules": [], "fallback": {"shard": 1, "x": 0, "y": 0}},
		"cadence": {"liveness_probe_ms": 100, "telemetry_poll_ms": 100, "control_cycle_ms": 50},
		"silence_budget_ms": 5000, "probe_timeout_ms": 500,
		"policy": {"measurement_settle_ms": 0, "phase_timeout_ms": 120000},
		"drain_ms": 5000
	}`, dataPort, adminPort, afSource, source.base, afDestination, destination.base)
	if err := os.WriteFile(filepath.Join(dir, "fabric.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	program, err := filepath.Abs("testdata/fabricd_hotcell_gated.fct")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(facetBinary(t), "exec", program, "--config", "fabric.json")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "FACET_DATA_DIR=" + dir,
		"FABRIC_ADMIN_TOKEN=" + afAdminToken, "FABRIC_DB_TOKEN=" + afEngineToken}
	f := &afFabric{data: fmt.Sprintf("http://127.0.0.1:%d", dataPort), admin: fmt.Sprintf("http://127.0.0.1:%d", adminPort),
		dir: dir, proc: afStart(t, cmd), source: source, destination: destination}
	for end := time.Now().Add(30 * time.Second); !strings.Contains(f.proc.log.String(), "serving FacetQL's wire"); time.Sleep(20 * time.Millisecond) {
		select {
		case <-f.proc.done:
			t.Fatalf("fabricd exited: %s", f.proc.log.String())
		default:
		}
		if time.Now().After(end) {
			t.Fatalf("fabricd never served: %s", f.proc.log.String())
		}
	}
	afWaitHTTP(t, f.admin+"/status", afAdminToken, 200, 10*time.Second, f.proc)
	return f
}

// adminJSON reads one operator route, or nil when it does not answer.
func (f *afFabric) adminJSON(path string) any {
	req, _ := http.NewRequest("GET", f.admin+path, nil)
	req.Header.Set("x-api-key", afAdminToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var v any
	json.NewDecoder(resp.Body).Decode(&v)
	return v
}

func (f *afFabric) holder() string {
	routing, _ := f.adminJSON("/routing").(map[string]any)
	placements, _ := routing["placements"].([]any)
	if len(placements) == 0 {
		return ""
	}
	h, _ := placements[0].(map[string]any)["holder"].(string)
	return h
}

// afLiveness watches the operator status while a scenario runs and keeps
// everything that would mean the daemon lost sight of an engine.
type afLiveness struct {
	mu      sync.Mutex
	samples int
	bad     []string
	stop    chan struct{}
	done    chan struct{}
}

func (f *afFabric) watchLiveness() *afLiveness {
	w := &afLiveness{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-w.stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			st, _ := f.adminJSON("/status").(map[string]any)
			if st == nil {
				continue
			}
			w.mu.Lock()
			w.samples++
			backends, _ := st["backends"].([]any)
			for _, b := range backends {
				m := b.(map[string]any)
				probe, _ := m["last_probe"].(string)
				if m["availability"] != "serviceable" || !strings.HasPrefix(probe, "serving") {
					w.bad = append(w.bad, fmt.Sprintf("%v: availability=%v health=%v last_probe=%q", m["id"], m["availability"], m["health"], probe))
				}
			}
			if movers, ok := st["movers"].(map[string]any); ok {
				if refused, _ := movers["refused"].([]any); len(refused) > 0 {
					w.bad = append(w.bad, fmt.Sprintf("refused copies: %v", refused))
				}
			}
			w.mu.Unlock()
		}
	}()
	return w
}

func (w *afLiveness) report(t *testing.T) {
	t.Helper()
	close(w.stop)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.samples == 0 {
		t.Errorf("the operator status was never sampled")
	}
	seen := map[string]bool{}
	for _, b := range w.bad {
		if !seen[b] {
			seen[b] = true
			t.Errorf("liveness: %s", b)
		}
	}
}

// afEngineNodes: every node of a kind an engine holds, by address (the
// keyset walk the mover uses).
func afEngineNodes(t *testing.T, e *afEngine, kind string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	after := ""
	for {
		body := map[string]any{"limit": 500, "after": after}
		if kind != "" {
			body["kind"] = kind
		}
		code, text := afPost(t, e.base+"/nodes/query", afEngineToken, body)
		if code != 200 {
			t.Fatalf("query on %s: %d %s", e.base, code, text)
		}
		var page struct {
			Nodes []map[string]any `json:"nodes"`
			Next  string           `json:"next"`
		}
		if err := json.Unmarshal([]byte(text), &page); err != nil {
			t.Fatal(err)
		}
		for _, n := range page.Nodes {
			out[n["address"].(string)] = n
		}
		if page.Next == "" {
			return out
		}
		after = page.Next
	}
}

func afPost(t *testing.T, url, token string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(b)))
	req.Header.Set("x-api-key", token)
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// afSide is one running web runtime and the browser-like client that
// keeps its cookies, its bearer token and its last ETag between steps.
type afSide struct {
	name   string
	base   string
	client *http.Client
	token  string
	etag   string
}

func afClient(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// afRuntimeEnv is the environment both runtimes read: the store, one
// secret, the published entity.
func afRuntimeEnv(dsn, apiRead string) []string {
	return []string{"FACET_DATABASE_URL=" + dsn, "FACET_SECRET=" + afSecret, "FACET_API_READ=" + apiRead, "FACET_LOG_LEVEL=error"}
}

// afStartGoRuntime serves the app with the Go runtime, in process, over
// the store dsn names.
func afStartGoRuntime(t *testing.T, g *ir.IR, dsn, apiRead string) *afSide {
	t.Helper()
	for _, kv := range afRuntimeEnv(dsn, apiRead) {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	srv, err := runtime.New(g)
	if err != nil {
		t.Fatalf("starting the Go runtime: %v", err)
	}
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	go func() { _ = srv.Serve(addr) }()
	side := &afSide{name: "go", base: "http://" + addr, client: afClient(t)}
	afWaitSide(t, side, nil)
	return side
}

// afStartFctRuntime serves the app with the fct runtime as a process:
// `facet exec selfhost/runtime_server.fct`, the graph's JSON and the client
// script in its data directory, RT_PORT / RT_IR from the environment.
func afStartFctRuntime(t *testing.T, g *ir.IR, dsn, apiRead string) *afSide {
	t.Helper()
	dir := t.TempDir()
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
	program, err := filepath.Abs("../selfhost/runtime_server.fct")
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	cmd := exec.Command(facetBinary(t), "exec", program)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "FACET_DATA_DIR=" + dir,
		fmt.Sprintf("RT_PORT=%d", port), "RT_IR=app.ir.json"}, afRuntimeEnv(dsn, apiRead)...)
	p := afStart(t, cmd)
	side := &afSide{name: "fct", base: fmt.Sprintf("http://127.0.0.1:%d", port), client: afClient(t)}
	afWaitSide(t, side, p)
	return side
}

func afWaitSide(t *testing.T, side *afSide, p *afProc) {
	t.Helper()
	// The fct runtime parses the whole graph before it listens (facets/api/
	// main.fct is ~4 MB of IR).
	for end := time.Now().Add(180 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(side.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if p != nil {
			select {
			case <-p.done:
				t.Fatalf("the %s runtime exited: %s", side.name, p.log.String())
			default:
			}
		}
		if time.Now().After(end) {
			log := ""
			if p != nil {
				log = p.log.String()
			}
			t.Fatalf("the %s runtime never answered /healthz: %v\n%s", side.name, err, log)
		}
	}
}

// ── the scripted session (selfhost/runtime_parity_test.go's, and its masks) ──

type afStep struct {
	name    string
	method  string
	path    string
	body    string
	headers map[string]string
	csrf    bool
}

type afAnswer struct {
	status      int
	contentType string
	cookieAttrs string
	body        string
}

var (
	afCsrfMeta   = regexp.MustCompile(`<meta name="fa-csrf" content="[^"]*">`)
	afSessionKey = regexp.MustCompile(`"session":"[A-Za-z0-9_-]*"`)
	afCreated    = regexp.MustCompile(`"created":[0-9]+`)
	afTokens     = regexp.MustCompile(`"(token|verifyToken|resetToken)":"[^"]*"`)
	afCookieVal  = regexp.MustCompile(`fa_sid=[^;]*`)
	afCookieExp  = regexp.MustCompile(`Expires=[^;]*`)
	afTokenValue = regexp.MustCompile(`"token":"([^"]*)"`)
	afISOTime    = regexp.MustCompile(`"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z?"`)
	afEpochKeys  = regexp.MustCompile(`"([A-Za-z_]+)":[0-9]{9,}`)
	afRandKey    = regexp.MustCompile(`"key":"([a-z_]+)[0-9]+"`)
	afCsrfOnPage = regexp.MustCompile(`<meta name="fa-csrf" content="([^"]*)">`)
)

func afNormalize(s string) string {
	s = afCsrfMeta.ReplaceAllString(s, `<meta name="fa-csrf" content="CSRF">`)
	s = afSessionKey.ReplaceAllString(s, `"session":"VISITOR"`)
	s = afCreated.ReplaceAllString(s, `"created":0`)
	s = afTokens.ReplaceAllString(s, `"$1":"TOKEN"`)
	s = afISOTime.ReplaceAllString(s, `"ISO"`)
	s = afEpochKeys.ReplaceAllString(s, `"$1":0`)
	s = afRandKey.ReplaceAllString(s, `"key":"${1}RAND"`)
	return s
}

func afDo(t *testing.T, side *afSide, st afStep) afAnswer {
	t.Helper()
	csrf := ""
	if st.csrf {
		resp, err := side.client.Get(side.base + "/")
		if err != nil {
			t.Fatalf("%s: reading the page for its CSRF token: %v", side.name, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if m := afCsrfOnPage.FindSubmatch(b); m != nil {
			csrf = string(m[1])
		}
	}
	req, err := http.NewRequest(st.method, side.base+st.path, strings.NewReader(st.body))
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
	resp, err := side.client.Do(req)
	if err != nil {
		t.Fatalf("%s: %s %s: %v", side.name, st.method, st.path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: %s %s: reading body: %v", side.name, st.method, st.path, err)
	}
	if m := afTokenValue.FindSubmatch(b); m != nil {
		side.token = string(m[1])
	}
	if e := resp.Header.Get("ETag"); e != "" {
		side.etag = e
	}
	attrs := ""
	for _, c := range resp.Header.Values("Set-Cookie") {
		c = afCookieVal.ReplaceAllString(c, "fa_sid=SID")
		c = afCookieExp.ReplaceAllString(c, "Expires=EXP")
		attrs += c + "\n"
	}
	return afAnswer{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), cookieAttrs: attrs, body: afNormalize(string(b))}
}

func afDiff(a, b string) string {
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
	return fmt.Sprintf("first difference at byte %d:\n  reference: …%s…\n  stack:     …%s…", i, a[lo:hiA], b[lo:hiB])
}

func afCompare(t *testing.T, step afStep, ref, got afAnswer) {
	t.Helper()
	if ref.status != got.status {
		t.Errorf("%s: status reference=%d stack=%d\n  reference body: %.300s\n  stack body: %.300s", step.name, ref.status, got.status, ref.body, got.body)
		return
	}
	if ref.contentType != got.contentType {
		t.Errorf("%s: content-type reference=%q stack=%q", step.name, ref.contentType, got.contentType)
	}
	if ref.cookieAttrs != got.cookieAttrs {
		t.Errorf("%s: set-cookie reference=%q stack=%q", step.name, ref.cookieAttrs, got.cookieAttrs)
	}
	if ref.body != got.body {
		t.Errorf("%s: body differs (%d vs %d bytes)\n%s", step.name, len(ref.body), len(got.body), afDiff(ref.body, got.body))
	}
}

// afSession is the session, in three parts: before the migration, during
// it (the cell has rows now), and after the cutover. `seed` posts enough
// rows for the copy to take real time.
type afSession struct {
	before, during, after []afStep
	seed                  int
	live                  bool   // open /live before the migration and read a change after it
	liveChange            afStep // the change whose frame the stream must carry
	rowKind               string // the kind whose rows must reach the engine
}

func afHomeSession() afSession {
	return afSession{
		before: []afStep{
			{name: "healthz", method: "GET", path: "/healthz"},
			{name: "home as guest", method: "GET", path: "/"},
			{name: "post as guest", method: "POST", path: "/api/post", body: `{"args":["hello from a guest"]}`},
			{name: "login before signup", method: "POST", path: "/api/login", body: `{"args":["grace","hopper"]}`},
			{name: "signup grace", method: "POST", path: "/api/signup", body: `{"args":["grace","hopper"]}`},
			{name: "post as grace", method: "POST", path: "/api/post", body: `{"args":["hello, world"]}`},
			{name: "post second", method: "POST", path: "/api/post", body: `{"args":["second post with #facet and @alan"]}`},
			{name: "like via event", method: "POST", path: "/event", body: `{"action":"like","args":[1]}`, csrf: true},
			{name: "feed as grace", method: "GET", path: "/"},
			{name: "api feed", method: "GET", path: "/api/Tweet?by=created&desc=1&limit=5"},
		},
		seed: 40,
		during: []afStep{
			{name: "post during the copy", method: "POST", path: "/api/post", body: `{"args":["written while the cell is being copied"]}`},
			{name: "feed during the copy", method: "GET", path: "/"},
			{name: "api feed during the copy", method: "GET", path: "/api/Tweet?by=created&desc=1&limit=5"},
			{name: "follow during the copy", method: "POST", path: "/api/follow", body: `{"args":["alan"]}`},
		},
		after: []afStep{
			{name: "post after the cutover", method: "POST", path: "/api/post", body: `{"args":["written after the cell moved"]}`},
			{name: "feed after the cutover", method: "GET", path: "/"},
			{name: "api feed after the cutover", method: "GET", path: "/api/Tweet?by=created&desc=1&limit=5"},
			{name: "api feed filtered", method: "GET", path: "/api/Tweet?author=grace&by=id&limit=3"},
			{name: "logout", method: "POST", path: "/api/logout", body: `{"args":[]}`},
			{name: "login wrong password", method: "POST", path: "/api/login", body: `{"args":["grace","nope"]}`},
			{name: "login grace", method: "POST", path: "/api/login", body: `{"args":["grace","hopper"]}`},
			{name: "home logged in again", method: "GET", path: "/"},
		},
		live:       true,
		liveChange: afStep{name: "change while subscribed", method: "POST", path: "/api/post", body: `{"args":["a change the stream carries"]}`},
		rowKind:    "Tweet",
	}
}

func afAPISession() afSession {
	bearer := map[string]string{"Authorization": "Bearer SIDE_TOKEN"}
	return afSession{
		before: []afStep{
			{name: "page Home", method: "GET", path: "/"},
			{name: "declared: missing parameter", method: "POST", path: "/api/v2/accounts", body: `{}`},
			{name: "declared: me unauthenticated", method: "GET", path: "/api/v2/me"},
			{name: "declared: signup", method: "POST", path: "/api/v2/accounts",
				body: `{"handle":"grace","password":"hopper-1906","display_name":"Grace","terms_accepted":true,"device_name":"phone"}`},
			{name: "declared: signup taken", method: "POST", path: "/api/v2/accounts",
				body: `{"handle":"grace","password":"hopper-1906","terms_accepted":true}`},
			{name: "declared: me", method: "GET", path: "/api/v2/me", headers: bearer},
			{name: "declared: feed", method: "GET", path: "/api/v2/feed", headers: bearer},
		},
		during: []afStep{
			{name: "declared: sessions during the copy", method: "GET", path: "/api/v2/sessions", headers: bearer},
			{name: "declared: login during the copy", method: "POST", path: "/api/v2/sessions",
				body: `{"handle":"grace","password":"hopper-1906","device_name":"laptop"}`},
			{name: "declared: feed during the copy", method: "GET", path: "/api/v2/feed", headers: bearer},
		},
		after: []afStep{
			{name: "declared: me after the cutover", method: "GET", path: "/api/v2/me", headers: bearer},
			{name: "declared: me conditional", method: "GET", path: "/api/v2/me", headers: map[string]string{"Authorization": "Bearer SIDE_TOKEN", "If-None-Match": "SIDE_ETAG"}},
			{name: "declared: sessions after the cutover", method: "GET", path: "/api/v2/sessions", headers: bearer},
			{name: "dispatch: block", method: "POST", path: "/events", headers: bearer, body: `{"event_type":"block","target_handle":"alan"}`},
			{name: "declared: login wrong password", method: "POST", path: "/api/v2/sessions", body: `{"handle":"grace","password":"nope"}`},
			{name: "declared: notifications", method: "GET", path: "/api/v2/notifications", headers: bearer},
		},
		rowKind: "Account",
	}
}

// afStream is an open /live subscription.
type afStream struct {
	r    *bufio.Reader
	resp *http.Response
}

func afOpenLive(t *testing.T, ctx context.Context, side *afSide) *afStream {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", side.base+"/live", nil)
	for _, c := range side.client.Jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s: /live: %v", side.name, err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%s: /live: %s %q", side.name, resp.Status, resp.Header.Get("Content-Type"))
	}
	return &afStream{r: bufio.NewReader(resp.Body), resp: resp}
}

func (s *afStream) frame(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading an SSE frame: %v (got %q)", err, b.String())
		}
		if line == "\n" {
			return afNormalize(b.String())
		}
		if !strings.HasPrefix(line, ":") {
			b.WriteString(line)
		}
	}
}

// afStack is one full stack under test.
type afStack struct {
	name   string
	side   *afSide
	fabric *afFabric // nil for the reference
}

// afRun drives the session through the reference and the stack, migrating
// the stack's cell between the `before` and `after` parts.
func afRun(t *testing.T, app string, s afSession, boot func(t *testing.T, g *ir.IR, apiRead string) afStack) {
	t.Helper()
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	apiRead := g.Entities[0].Name
	// The reference: the Go runtime on the Rust facetql, no fabric.
	refEngine := afStartEngine(t, "rust")
	gRef, _ := compile.File(app)
	ref := &afSide{name: "reference"}
	*ref = *afStartGoRuntime(t, gRef, "facetql://"+afEngineToken+"@"+strings.TrimPrefix(refEngine.base, "http://"), apiRead)
	ref.name = "reference"
	gStack, _ := compile.File(app)
	stack := boot(t, gStack, apiRead)

	run := func(steps []afStep) {
		for _, st := range steps {
			ra := afDo(t, ref, st)
			sa := afDo(t, stack.side, st)
			afCompare(t, st, ra, sa)
			if t.Failed() {
				t.Fatalf("stopping at the first failing step (%s)", st.name)
			}
		}
	}
	run(s.before)
	for i := 0; i < s.seed; i++ {
		run([]afStep{{name: fmt.Sprintf("seed post %d", i), method: "POST", path: "/api/post", body: fmt.Sprintf(`{"args":["seed row %d"]}`, i)}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var refLive, stackLive *afStream
	if s.live {
		refLive, stackLive = afOpenLive(t, ctx, ref), afOpenLive(t, ctx, stack.side)
		defer refLive.resp.Body.Close()
		defer stackLive.resp.Body.Close()
		if rh, sh := refLive.frame(t), stackLive.frame(t); rh != sh {
			t.Errorf("live hello frame differs:\n%s", afDiff(rh, sh))
		}
	}
	if stack.fabric != nil {
		afMigrate(t, stack, s, run)
	} else {
		run(s.during)
	}
	run(s.after)
	if s.live {
		ra, sa := afDo(t, ref, s.liveChange), afDo(t, stack.side, s.liveChange)
		afCompare(t, s.liveChange, ra, sa)
		if rf, sf := refLive.frame(t), stackLive.frame(t); rf != sf {
			t.Errorf("live change frame differs:\n%s", afDiff(rf, sf))
		}
	}
	if stack.fabric != nil {
		afAfterMigration(t, stack, s)
	}
}

// afMigrate: the gate opens, the daemon decides and its mover copies the
// cell; the `during` steps run while the copy is in flight, a read load and
// direct probes of the destination run until the cutover, and the cutover
// is awaited.
func afMigrate(t *testing.T, stack afStack, s afSession, run func([]afStep)) {
	t.Helper()
	f := stack.fabric
	watch := f.watchLiveness()
	if f.holder() != afSource {
		t.Fatalf("before the migration the holder is %q", f.holder())
	}
	if err := os.WriteFile(filepath.Join(f.dir, "hot"), []byte("hot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	until := func(what string, budget time.Duration, cond func() bool) {
		t.Helper()
		for end := time.Now().Add(budget); !cond(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(end) {
				st, _ := json.Marshal(f.adminJSON("/status"))
				t.Fatalf("timed out waiting for %s; fabricd: %s; status: %s", what, f.proc.log.String(), st)
			}
		}
	}
	action := func() map[string]any {
		actions, _ := f.adminJSON("/actions").([]any)
		if len(actions) == 0 {
			return nil
		}
		return actions[0].(map[string]any)
	}
	until("the control loop to admit the move", 20*time.Second, func() bool { return action() != nil })
	if a := action(); a["source"] != afSource || a["destination"] != afDestination {
		t.Fatalf("the admitted action: %v", a)
	}
	until("the daemon's mover to start", 20*time.Second, func() bool {
		st, _ := f.adminJSON("/status").(map[string]any)
		movers, _ := st["movers"].(map[string]any)
		copying, _ := movers["copying"].([]any)
		return len(copying) > 0
	})
	// A read load on the app and the daemon's own probe on the destination,
	// under the mover's transactions, until authority moves.
	stop := make(chan struct{})
	var load sync.WaitGroup
	var loadMu sync.Mutex
	var loadFailures []string
	loadReads, probes := 0, 0
	load.Add(2)
	go func() {
		defer load.Done()
		client := &http.Client{Timeout: 30 * time.Second, Jar: stack.side.client.Jar}
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := client.Get(stack.side.base + "/")
			loadMu.Lock()
			loadReads++
			if err != nil {
				loadFailures = append(loadFailures, "GET /: "+err.Error())
			} else {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					loadFailures = append(loadFailures, fmt.Sprintf("GET /: %d", resp.StatusCode))
				}
			}
			loadMu.Unlock()
			time.Sleep(25 * time.Millisecond)
		}
	}()
	go func() {
		defer load.Done()
		// The daemon's liveness probe, made directly: GET / with no
		// credential, answered within probe_timeout_ms.
		client := &http.Client{Timeout: 500 * time.Millisecond}
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := client.Get(f.destination.base + "/")
			loadMu.Lock()
			probes++
			if err != nil {
				loadFailures = append(loadFailures, "destination probe: "+err.Error())
			} else {
				resp.Body.Close()
				if resp.StatusCode != 200 {
					loadFailures = append(loadFailures, fmt.Sprintf("destination probe: %d", resp.StatusCode))
				}
			}
			loadMu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
	}()
	run(s.during)
	phases := map[string]bool{}
	until("the cell to move", 120*time.Second, func() bool {
		routing, _ := f.adminJSON("/routing").(map[string]any)
		placements, _ := routing["placements"].([]any)
		if len(placements) == 0 {
			return false
		}
		p := placements[0].(map[string]any)
		if m, ok := p["migration"].(map[string]any); ok {
			if phase, _ := m["phase"].(string); phase != "" {
				phases[phase] = true
			}
		}
		return p["holder"] == afDestination
	})
	close(stop)
	load.Wait()
	if !phases["copying"] {
		t.Errorf("routing never published the copying phase; saw %v", phases)
	}
	loadMu.Lock()
	for _, fl := range loadFailures {
		t.Errorf("a request failed during the migration: %s", fl)
	}
	t.Logf("during the migration: %d reads of the app answered 200, %d direct probes of the destination answered within the daemon's 500 ms budget", loadReads, probes)
	loadMu.Unlock()
	// The action concludes and is measured; nothing was rolled back.
	until("the action to conclude and be measured", 30*time.Second, func() bool {
		history, _ := f.adminJSON("/actions/history").([]any)
		return len(history) > 0
	})
	history := f.adminJSON("/actions/history").([]any)
	o := history[0].(map[string]any)
	if o["verdict"] == "Failed" || o["verdict"] == "RolledBack" || o["verdict"] == "NotMeasured" || o["failure"] != nil {
		t.Errorf("the measured outcome of the move: %v", o)
	}
	watch.report(t)
}

// afAfterMigration: the rows are on the new holder, and a fresh write
// lands there alone.
func afAfterMigration(t *testing.T, stack afStack, s afSession) {
	t.Helper()
	f := stack.fabric
	if h := f.holder(); h != afDestination {
		t.Fatalf("after the migration the holder is %q", h)
	}
	onDestination := afEngineNodes(t, f.destination, s.rowKind)
	if len(onDestination) == 0 {
		onSource := afEngineNodes(t, f.source, s.rowKind)
		t.Fatalf("no %s row reached the engines through the %s runtime (destination holds %d, source %d): the %s runtime does not persist its rows through FACET_DATABASE_URL",
			s.rowKind, stack.side.name, len(onDestination), len(onSource), stack.side.name)
	}
	before := len(onDestination)
	// One more write through the runtime: it must reach the destination
	// and not the source.
	st := afStep{name: "write after the migration", method: "POST", path: "/api/post", body: `{"args":["landed on the new holder"]}`}
	if s.rowKind != "Tweet" {
		st = afStep{name: "write after the migration", method: "POST", path: "/api/v2/accounts",
			body: `{"handle":"alan","password":"turing-1912","terms_accepted":true}`}
	}
	if a := afDo(t, stack.side, st); a.status != 200 && a.status != 201 {
		t.Fatalf("%s: %d %s", st.name, a.status, a.body)
	}
	after := afEngineNodes(t, f.destination, s.rowKind)
	if len(after) != before+1 {
		t.Errorf("the write after the migration: destination held %d %s rows, now %d", before, s.rowKind, len(after))
	}
	srcAfter := afEngineNodes(t, f.source, s.rowKind)
	for addr := range after {
		if _, ok := onDestination[addr]; ok {
			continue
		}
		if _, ok := srcAfter[addr]; ok {
			t.Errorf("the write after the migration (%s) also reached the old holder", addr)
		}
	}
}

// ── the stacks ──────────────────────────────────────────────────────────

func afBootGoOnFctFabric(t *testing.T, g *ir.IR, apiRead string) afStack {
	f := afStartFabric(t, afStartEngine(t, "fct"), afStartEngine(t, "fct"))
	side := afStartGoRuntime(t, g, "facetql://"+afEngineToken+"@"+strings.TrimPrefix(f.data, "http://"), apiRead)
	return afStack{name: "go-runtime/fct-fabric", side: side, fabric: f}
}

func afBootFctOnFctFabric(t *testing.T, g *ir.IR, apiRead string) afStack {
	f := afStartFabric(t, afStartEngine(t, "fct"), afStartEngine(t, "fct"))
	side := afStartFctRuntime(t, g, "facetql://"+afEngineToken+"@"+strings.TrimPrefix(f.data, "http://"), apiRead)
	return afStack{name: "fct-runtime/fct-fabric", side: side, fabric: f}
}

func afBootGoOnRust(t *testing.T, g *ir.IR, apiRead string) afStack {
	e := afStartEngine(t, "rust")
	side := afStartGoRuntime(t, g, "facetql://"+afEngineToken+"@"+strings.TrimPrefix(e.base, "http://"), apiRead)
	return afStack{name: "go-runtime/rust-facetql", side: side}
}

func afRequire(t *testing.T) {
	t.Helper()
	if os.Getenv(selfhostEnv) != "" || os.Getenv(frontdoorEnv) != "" {
		t.Skip("the stacks here are fixed; not re-run under the suite's engine-swap modes")
	}
	facetqlBinary(t)
}

func TestAllFctStackHome(t *testing.T) {
	afRequire(t)
	app := "../../facets/home.fct"
	t.Run("go-runtime/rust-facetql", func(t *testing.T) { afRun(t, app, afHomeSession(), afBootGoOnRust) })
	t.Run("go-runtime/fct-fabric", func(t *testing.T) { afRun(t, app, afHomeSession(), afBootGoOnFctFabric) })
	t.Run("fct-runtime/fct-fabric", func(t *testing.T) { afRun(t, app, afHomeSession(), afBootFctOnFctFabric) })
}

func TestAllFctStackAPI(t *testing.T) {
	afRequire(t)
	app := "../../facets/api/main.fct"
	t.Run("go-runtime/rust-facetql", func(t *testing.T) { afRun(t, app, afAPISession(), afBootGoOnRust) })
	t.Run("go-runtime/fct-fabric", func(t *testing.T) { afRun(t, app, afAPISession(), afBootGoOnFctFabric) })
	t.Run("fct-runtime/fct-fabric", func(t *testing.T) { afRun(t, app, afAPISession(), afBootFctOnFctFabric) })
}
