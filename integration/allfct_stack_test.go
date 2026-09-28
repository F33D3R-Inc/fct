package integration

// The all-fct stack, end to end, against the reference stack.
//
// Every process is this tree's facet running a program written in fct: two
// FacetQL servers (`facet facetql`, selfhost/fqserver.fct as the toolchain
// ships it), the Fabric control plane
// written in fct (selfhost/fabricd_lib.fct through testdata/
// fabricd_hotcell_gated.fct) placing one cell on each and serving
// FacetQL's wire on its data port — the fabric front door of
// selfhost/fabric_frontdoor.fct, fed the daemon's live routing — and the
// web runtime written in fct (selfhost/runtime_server.fct) serving the
// facets/ apps against that door. The same scripted session the runtime
// parity harness drives (selfhost/runtime_parity_test.go: sign-up, sign-in,
// posts, a feed read, a live SSE frame; for facets/api/main.fct its
// declared routes) is fired at it and at the reference stack — the Go
// runtime on the shipped FacetQL — and every answer compared after the same
// masking that harness applies. Midway, once the session has rows, the
// daemon is shown a hot cell (a gate file) and moves the app's data cell
// from one engine to the other while the session continues: no request may
// fail, the stream stays open across the cutover, and afterwards the rows
// are on the new holder and a new write lands there alone.
//
// The door in front is fabricd's own data port, re-fed the routing table
// every cycle, or — in the standalone-door stacks — a separate process,
// selfhost/fabric_frontdoor_main.fct, following the daemon's published
// routing over its operator port (FRONTDOOR_ROUTING_FEED;
// selfhost/fabric_routing_feed.fct). The daemon then moves authority only
// once that door has confirmed the fenced table, so the same migration
// runs with no failed request through either door.
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
// The reference is the Go runtime on the FacetQL the toolchain ships
// (`facet facetql`, the fct engine embedded in the facet binary), with no
// fabric. The stacks, each a subtest so a failure names its layer:
//   go-runtime/facetql             the reference's own shape (a check of the
//                                  harness)
//   go-runtime/rust-facetql        the Go runtime on the Rust reference
//                                  engine — only in a FACETQL_REFERENCE=rust
//                                  run
//   go-runtime/fct-fabric          the fct daemon and engines, under a
//                                  runtime that persists through them
//   fct-runtime/fct-fabric         everything in fct
//   fct-runtime/fct-fabric/standalone-door
//                                  everything in fct, the app talking to a
//                                  front door that is its own process
//   fct-runtime/fct-fabric/operator-migrate
//                                  everything in fct, the migration asked
//                                  for by an operator with the fct CLI
//                                  (`fabric daemon migrate`) instead of the
//                                  daemon's own decision
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
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

// afProcs is every process a test started, so a failing step can show what
// each of them said.
var (
	afProcsMu sync.Mutex
	afProcs   = map[*testing.T][]*afProc{}
)

// afLogs is what every process the test (or its parent) started has
// written, each under its command line.
func afLogs(t *testing.T) string {
	afProcsMu.Lock()
	defer afProcsMu.Unlock()
	var b strings.Builder
	for _, ps := range afProcs {
		for _, p := range ps {
			fmt.Fprintf(&b, "── %s ──\n%s\n", strings.Join(p.cmd.Args, " "), p.log.String())
		}
	}
	return b.String()
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
	dieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", cmd.Path, err)
	}
	go func() { _ = cmd.Wait(); close(p.done) }()
	afProcsMu.Lock()
	afProcs[t] = append(afProcs[t], p)
	afProcsMu.Unlock()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.done
		afProcsMu.Lock()
		delete(afProcs, t)
		afProcsMu.Unlock()
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

// afStartEngine starts one engine: "fct" — `facet facetql`, the FacetQL the
// toolchain ships (selfhost/fqserver.fct, embedded in this tree's facet
// binary) — or "rust", the Rust reference (`facetql start`), which only a
// FACETQL_REFERENCE=rust run starts.
func afStartEngine(t *testing.T, kind string) *afEngine {
	t.Helper()
	dir := t.TempDir()
	var cmd *exec.Cmd
	if kind == "rust" {
		cmd = exec.Command(facetqlBinary(t), "start")
	} else {
		cmd = exec.Command(facetBinary(t), "facetql")
	}
	cmd.Dir = dir
	// Port 0: the engine binds whatever the kernel gives it and names it in
	// its banner, so no port is chosen here that another process could take
	// first (stack_test.go's bannerPort, read off this process's log).
	cmd.Env = append(append(os.Environ(), afEngineEnv(dir, 0)...), engineWatchedEnv()...)
	p := afStart(t, cmd)
	port := afBanner(t, p, bannerPortRE, 60*time.Second)[0]
	e := &afEngine{base: fmt.Sprintf("http://127.0.0.1:%d", port), proc: p}
	afWaitHTTP(t, e.base+"/", "", 200, 60*time.Second, e.proc)
	engineWatch(t, cmd, e.base, p.log, kind != "rust")
	return e
}

// afBanner waits for a process to print the line re matches and answers
// its captured numbers — the ports a process bound on port 0 and named.
func afBanner(t *testing.T, p *afProc, re *regexp.Regexp, budget time.Duration) []int {
	t.Helper()
	for end := time.Now().Add(budget); ; time.Sleep(20 * time.Millisecond) {
		if m := re.FindStringSubmatch(p.log.String()); m != nil {
			var out []int
			for _, g := range m[1:] {
				var n int
				fmt.Sscanf(g, "%d", &n)
				out = append(out, n)
			}
			return out
		}
		select {
		case <-p.done:
			t.Fatalf("%s exited before saying which port it bound:\n%s", strings.Join(p.cmd.Args, " "), p.log.String())
		default:
		}
		if time.Now().After(end) {
			t.Fatalf("%s never said which port it bound:\n%s", strings.Join(p.cmd.Args, " "), p.log.String())
		}
	}
}

var (
	afFabricdBannerRE = regexp.MustCompile(`fabricd: serving FacetQL's wire on 127\.0\.0\.1:(\d+) and the operator surface on 127\.0\.0\.1:(\d+)`)
	afDoorBannerRE    = regexp.MustCompile(`fabric front door serving on port (\d+)`)
)

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
	// byCLI: the daemon is plain selfhost/fabricd.fct (FacetQL's own /stats
	// as its telemetry, no gate) and the migration is asked for by an
	// operator, with the fct CLI: `fabric daemon migrate` over the operator
	// port.
	byCLI bool
}

// afStartFabric runs testdata/fabricd_hotcell_gated.fct — fabricd with the
// gated HotCell source — placing shard 1's cell (0,0), the app's whole
// keyspace (every kind falls back to it), on the source engine and shard
// 2's on the destination. The cadence is fast and the probe budget tight:
// a probe every 100 ms, answered within 500 ms, five seconds of silence
// before a backend is out.
func afStartFabric(t *testing.T, source, destination *afEngine) *afFabric {
	t.Helper()
	return afStartFabricWith(t, source, destination, false)
}

func afStartFabricWith(t *testing.T, source, destination *afEngine, byCLI bool) *afFabric {
	t.Helper()
	dir := t.TempDir()
	// Port 0 on both listeners: fabricd names the ports it bound in its
	// banner (daemon.rs's local_addr), and those are read back.
	config := fmt.Sprintf(`{
		"data_listen": "127.0.0.1:0", "admin_listen": "127.0.0.1:0",
		"backends": [
			{"id": %q, "url": %q, "region": "us-east", "token_env": "FABRIC_DB_TOKEN", "placements": [{"shard": 1, "x": 0, "y": 0}]},
			{"id": %q, "url": %q, "region": "us-west", "token_env": "FABRIC_DB_TOKEN", "placements": [{"shard": 2, "x": 0, "y": 0}]}
		],
		"keyspace": {"rules": [], "fallback": {"shard": 1, "x": 0, "y": 0}},
		"cadence": {"liveness_probe_ms": 100, "telemetry_poll_ms": 100, "control_cycle_ms": 50},
		"silence_budget_ms": 5000, "probe_timeout_ms": 500,
		"policy": {"measurement_settle_ms": 0, "phase_timeout_ms": 120000},
		"drain_ms": 5000
	}`, afSource, source.base, afDestination, destination.base)
	if err := os.WriteFile(filepath.Join(dir, "fabric.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	daemonProgram := "testdata/fabricd_hotcell_gated.fct"
	if byCLI {
		daemonProgram = "../selfhost/fabricd.fct"
	}
	program, err := filepath.Abs(daemonProgram)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(facetBinary(t), "exec", program, "--config", "fabric.json")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "FACET_DATA_DIR=" + dir,
		"FABRIC_ADMIN_TOKEN=" + afAdminToken, "FABRIC_DB_TOKEN=" + afEngineToken}
	p := afStart(t, cmd)
	ports := afBanner(t, p, afFabricdBannerRE, 60*time.Second)
	f := &afFabric{data: fmt.Sprintf("http://127.0.0.1:%d", ports[0]), admin: fmt.Sprintf("http://127.0.0.1:%d", ports[1]),
		dir: dir, proc: p, source: source, destination: destination, byCLI: byCLI}
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
	// As `facet serve` and `facet exec` do (and the fct runtime does before
	// it listens): the `on start` jobs run — api/main.fct seeds its feed
	// surfaces in one — and the daemons start.
	srv.StartJobs()
	// A listener this process bound, handed over (no port chosen and then
	// released), and the runtime stopped with the test: its jobs and daemons
	// would otherwise go on running against engines the test has killed,
	// for as long as the test binary does.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { _ = srv.ServeOn(ln); close(served) }()
	t.Cleanup(func() {
		ln.Close()
		<-served
		srv.Shutdown()
	})
	side := &afSide{name: "go", base: "http://" + ln.Addr().String(), client: afClient(t)}
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
	program, err := filepath.Abs("../selfhost/runtime_server.fct")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(facetBinary(t), "exec", program)
	cmd.Dir = dir
	// RT_PORT=0: the runtime binds any free port and announces it, so no port
	// is chosen here first and raced for.
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "FACET_DATA_DIR=" + dir,
		"RT_PORT=0", "RT_IR=app.ir.json"}, afRuntimeEnv(dsn, apiRead)...)
	p := afStart(t, cmd)
	port := afRuntimeBannerPort(t, p)
	side := &afSide{name: "fct", base: fmt.Sprintf("http://127.0.0.1:%d", port), client: afClient(t)}
	afWaitSide(t, side, p)
	return side
}

var afRuntimeBanner = regexp.MustCompile(`Facet runtime serving \S+ on port ([0-9]+)`)

// afRuntimeBannerPort waits for the fct runtime's banner and returns the
// port it bound. The runtime parses the whole graph before it listens
// (facets/api/main.fct is ~4 MB of IR), hence the generous budget.
func afRuntimeBannerPort(t *testing.T, p *afProc) int {
	t.Helper()
	for end := time.Now().Add(180 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if m := afRuntimeBanner.FindStringSubmatch(p.log.String()); m != nil {
			port, _ := strconv.Atoi(m[1])
			return port
		}
		select {
		case <-p.done:
			t.Fatalf("the fct runtime exited before it listened: %s", p.log.String())
		default:
		}
		if time.Now().After(end) {
			t.Fatalf("the fct runtime never announced its port:\n%s", p.log.String())
		}
	}
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
	// reset starts a new visitor before the step: a fresh cookie jar (and
	// no bearer token), as site_test.go's a.newSession() does.
	reset bool
	// What the reference must answer (0 / "": not checked), so a step both
	// stacks fail alike cannot pass as agreement.
	status   int
	contains string
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
	// the same instants rendered into a page's text ("Joined 2026-…Z"):
	// wall-clock values the two sides wrote a moment apart
	afISOText    = regexp.MustCompile(`\b[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?Z\b`)
	afEpochKeys  = regexp.MustCompile(`"([A-Za-z_]+)":[0-9]{9,}`)
	afRandKey    = regexp.MustCompile(`"key":"([a-z_]+)[0-9]+"`)
	afCsrfOnPage = regexp.MustCompile(`<meta name="fa-csrf" content="([^"]*)">`)
	// The runtime's change counter — "@seq" in a page's state, and the
	// "seq" closing a change-stream frame — counts every broadcast, and a
	// periodic job broadcasts on each runtime's own clock (the site's
	// `job pushOutbox every 10s` rewrites PushCursor on every tick), so two
	// processes started at different instants hold different counts. It is
	// a clock reading, masked as those are; what the changes were is
	// compared through the rows themselves.
	afPageSeq  = regexp.MustCompile(`"@seq":[0-9]+`)
	afFrameSeq = regexp.MustCompile(`(?m)"seq":[0-9]+}$`)
	// `ago(t)` spells how long ago t was on the runtime's clock ("now",
	// "3m", "2h"), so the same row reads "now" on the faster stack and "1m"
	// on the slower one. Masked where the site renders it — the post card's
	// time link, a stream's live badge, the relative-time atom — and in the
	// spellings a session can produce; the row's timestamp itself is masked
	// as an epoch value.
	afAgo = regexp.MustCompile(`(class="[^"]*\b(?:x-post-when|x-media-views|x-relago)\b[^"]*"[^>]*>)(· )?(?:now|[0-9]+[mh])<`)
)

func afNormalize(s string) string {
	s = afCsrfMeta.ReplaceAllString(s, `<meta name="fa-csrf" content="CSRF">`)
	s = afSessionKey.ReplaceAllString(s, `"session":"VISITOR"`)
	s = afCreated.ReplaceAllString(s, `"created":0`)
	s = afTokens.ReplaceAllString(s, `"$1":"TOKEN"`)
	s = afISOTime.ReplaceAllString(s, `"ISO"`)
	s = afISOText.ReplaceAllString(s, `ISO`)
	s = afEpochKeys.ReplaceAllString(s, `"$1":0`)
	s = afRandKey.ReplaceAllString(s, `"key":"${1}RAND"`)
	s = afPageSeq.ReplaceAllString(s, `"@seq":0`)
	s = afFrameSeq.ReplaceAllString(s, `"seq":0}`)
	s = afAgo.ReplaceAllString(s, `${1}${2}AGO<`)
	return s
}

func afDo(t *testing.T, side *afSide, st afStep) afAnswer {
	t.Helper()
	if st.reset {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		side.client.Jar = jar
		side.token, side.etag = "", ""
	}
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
		t.Fatalf("%s: %s %s: %v; the stack's processes said:\n%s", side.name, st.method, st.path, err, afLogs(t))
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
	live                  bool   // open the change stream before the migration and read a change after it
	liveChange            afStep // the change whose frame the stream must carry
	rowKind               string // the kind whose rows must reach the engine
	afterWrite            afStep // one more row of rowKind, written after the cutover
}

// afSiteSession is f33d3r.com's session (facets/f33d3r_com.fct: the whole
// site at one origin — the product's entities, /api/v2 routes and contract
// from api/surface.fct, and the web pages over the same rows, one session
// model), built from site_test.go TestSiteIsOneApp's flows: a web sign-up
// whose cookie session is the API's, a web post the native API reads, an
// account made by the API signing in on the web, a like, a message, going
// live, and every page rendering what it shows. It also carries what the
// earlier targets exercised, so they need not run: api/main.fct is
// api/surface.fct plus one page — its declared routes (a conditional GET,
// the sessions list, the tagged-message dispatch, a refused login) are
// steps here — and home.fct's runtime paths (an /event action with its
// CSRF token, the generic /api/<Entity> list, the /live stream's hello and
// change frames, sign-out and a wrong password) are steps here too.
//
// Feed reads are newest-first by id, not by a clock value: the two
// runtimes read two clocks, so rows made within one second may tie on one
// side and not the other.
func afSiteSession(g *ir.IR) afSession {
	bearer := map[string]string{"Authorization": "Bearer SIDE_TOKEN"}
	// TestSiteIsOneApp's pages, with what each must show; listed in order so
	// the session is the same every run.
	pages := [][2]string{
		// "/" and ada's profile show the newest posts, her seed rows, on top;
		// post 1 and #garden are her first web post.
		{"/", "seed row"}, {"/profile/ada", "seed row 39"}, {"/post/1", "hello from the web"},
		{"/tag/garden", "hello from the web"}, {"/search", "Search"}, {"/notifications", "Notifications"},
		{"/bookmarks", "Bookmarks"}, {"/messages", "@ada"}, {"/dm/1", "hi ada"}, {"/live", "late show"},
		{"/browse/general", ""}, {"/live/1", "late show"}, {"/studio", "You're live"}, {"/wallet", "Wallet"},
		{"/people", "@ada"}, {"/login", "Sign in"},
	}
	var pageSteps []afStep
	for _, p := range pages {
		pageSteps = append(pageSteps, afStep{name: "page " + p[0], method: "GET", path: p[0], status: 200, contains: p[1]})
	}
	list := "/api/" + g.Entities[0].Name
	after := []afStep{
		// bob, back on the web: a like, a message, a stream.
		{name: "like as bob", method: "POST", path: "/api/like", body: `{"args":[1]}`, status: 200},
		{name: "message ada", method: "POST", path: "/api/webMessage", body: `{"args":["ada","hi ada"]}`, status: 200},
		{name: "go live", method: "POST", path: "/api/webGoLive", body: `{"args":["late show"]}`, status: 200},
		{name: "go live with no title", method: "POST", path: "/api/webGoLive", body: `{"args":["  "]}`},
	}
	after = append(after, pageSteps...)
	after = append(after,
		// The native client's own routes, on bob's bearer session.
		afStep{name: "api: sign in bob (bearer)", method: "POST", path: "/api/v2/sessions", body: `{"handle":"bob","password":"battery-staple","device_name":"laptop"}`},
		afStep{name: "api: me", method: "GET", path: "/api/v2/me", headers: bearer},
		afStep{name: "api: me, conditional", method: "GET", path: "/api/v2/me", headers: map[string]string{"Authorization": "Bearer SIDE_TOKEN", "If-None-Match": "SIDE_ETAG"}},
		afStep{name: "api: sessions", method: "GET", path: "/api/v2/sessions", headers: bearer},
		afStep{name: "api: notifications", method: "GET", path: "/api/v2/notifications", headers: bearer},
		afStep{name: "dispatch: unknown event", method: "POST", path: "/events", headers: bearer, body: `{"event_type":"no_such_event"}`},
		afStep{name: "dispatch: block", method: "POST", path: "/events", headers: bearer, body: `{"event_type":"block","target_handle":"alan"}`},
		afStep{name: "api: wrong password", method: "POST", path: "/api/v2/sessions", body: `{"handle":"bob","password":"nope"}`},
		// Sign out on the web, a refused sign-in, ada back.
		afStep{name: "web: sign out", method: "POST", path: "/api/webLogout", body: `{"args":[]}`},
		afStep{name: "home signed out", method: "GET", path: "/"},
		afStep{name: "web: wrong password", method: "POST", path: "/api/webLogin", body: `{"args":["ada","nope"]}`},
		afStep{name: "web: sign in ada", method: "POST", path: "/api/webLogin", body: `{"args":["ada","correct-horse"]}`, status: 200},
		afStep{name: "home as ada again", method: "GET", path: "/"},
		afStep{name: "api list after the cutover", method: "GET", path: list + "?by=id&desc=1&limit=5"},
		// Last: the product's published contract.
		afStep{name: "the contract", method: "GET", path: "/api/v2/contract", status: 200, contains: "F33D3R native API"},
	)
	return afSession{
		before: []afStep{
			{name: "healthz", method: "GET", path: "/healthz"},
			{name: "home as a guest", method: "GET", path: "/"},
			{name: "sign-in page", method: "GET", path: "/login"},
			{name: "post as a guest", method: "POST", path: "/api/post", body: `{"args":["hello from a guest"]}`},
			{name: "api: me, signed out", method: "GET", path: "/api/v2/me"},
			// The web: sign up through the page's form action, on a cookie
			// session — which is the API's session too.
			{name: "web: sign up ada", method: "POST", path: "/api/webSignup", body: `{"args":["ada","correct-horse"]}`, status: 200},
			{name: "web: sign up ada again", method: "POST", path: "/api/webSignup", body: `{"args":["ada","other-horse"]}`},
			{name: "home signed in", method: "GET", path: "/", status: 200, contains: "signed in as @ada"},
			{name: "web: post", method: "POST", path: "/api/post", body: `{"args":["hello from the web #garden"]}`, status: 200},
			{name: "api: me on the web session", method: "GET", path: "/api/v2/me", status: 200, contains: `"handle":"ada"`},
			{name: "like via /event", method: "POST", path: "/event", body: `{"action":"like","args":[1]}`, csrf: true, status: 200},
			{name: "/event without its CSRF token", method: "POST", path: "/event", body: `{"action":"like","args":[1]}`},
			{name: "api list", method: "GET", path: list + "?by=id&desc=1&limit=5"},
		},
		seed: 40,
		during: []afStep{
			// The native API, a new visitor: bob signs up with a bearer token
			// and reads ada's web post.
			{name: "api: sign up bob", method: "POST", path: "/api/v2/accounts", reset: true,
				body: `{"handle":"bob","password":"battery-staple","terms_accepted":true}`, status: 201},
			{name: "api: me as bob", method: "GET", path: "/api/v2/me", headers: bearer, status: 200, contains: `"handle":"bob"`},
			// ada's web posts, as the native client reads her profile (newest
			// first: the last seed row she posted on the web is on top).
			{name: "api: ada's works", method: "GET", path: "/api/v2/users/ada/works", headers: bearer, status: 200, contains: "seed row 39"},
			{name: "api: feed", method: "GET", path: "/api/v2/feed", headers: bearer, status: 200},
			// bob, made by the API, signs in on the web.
			{name: "web: sign in bob", method: "POST", path: "/api/webLogin", body: `{"args":["bob","battery-staple"]}`, status: 200},
			{name: "home as bob", method: "GET", path: "/", status: 200, contains: "signed in as @bob"},
			{name: "post during the copy", method: "POST", path: "/api/post", body: `{"args":["written while the cell is being copied"]}`},
		},
		after:      after,
		live:       true,
		liveChange: afStep{name: "change while subscribed", method: "POST", path: "/api/post", body: `{"args":["a change the stream carries"]}`},
		rowKind:    "Work",
		afterWrite: afStep{name: "write after the migration", method: "POST", path: "/api/post", body: `{"args":["landed on the new holder"]}`},
	}
}

// afStream is an open subscription to the live change stream (/api/_live,
// the runtime's own namespace; /live is its former path).
type afStream struct {
	r    *bufio.Reader
	resp *http.Response
}

func afOpenLive(t *testing.T, ctx context.Context, side *afSide) *afStream {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", side.base+"/api/_live", nil)
	for _, c := range side.client.Jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s: /api/_live: %v", side.name, err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%s: /api/_live: %s %q", side.name, resp.Status, resp.Header.Get("Content-Type"))
	}
	return &afStream{r: bufio.NewReader(resp.Body), resp: resp}
}

// afFrameBudget: how long one frame may take to arrive. A stream stays open
// for the whole session, however long that runs; what must not happen is a
// frame the runtime owes never arriving.
const afFrameBudget = 30 * time.Second

func (s *afStream) frame(t *testing.T) string {
	t.Helper()
	type read struct {
		frame string
		err   error
	}
	got := make(chan read, 1)
	go func() {
		var b strings.Builder
		for {
			line, err := s.r.ReadString('\n')
			if err != nil {
				got <- read{b.String(), err}
				return
			}
			if line == "\n" {
				got <- read{b.String(), nil}
				return
			}
			if !strings.HasPrefix(line, ":") {
				b.WriteString(line)
			}
		}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("reading an SSE frame: %v (got %q)", r.err, r.frame)
		}
		return afNormalize(r.frame)
	case <-time.After(afFrameBudget):
		s.resp.Body.Close() // ends the read above
		t.Fatalf("no SSE frame arrived within %v", afFrameBudget)
		return ""
	}
}

// frameNaming: the next frame whose `changed` names kind — the one the
// session's change caused — and the frames passed over on the way. Other
// writers share the stream: the app's own periodic jobs (webpush's
// `pushOutbox every 10s` writes PushCursor) and the bookkeeping a request
// does besides its change (a session's touch of Account, Session, ...)
// arrive in whatever order the clock and the scheduler give them, on the
// reference and the stack alike, so they are not the change under test.
func (s *afStream) frameNaming(t *testing.T, kind string) (string, []string) {
	t.Helper()
	var skipped []string
	for {
		f := s.frame(t)
		for _, line := range strings.Split(f, "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var body struct {
				Changed []string `json:"changed"`
			}
			if json.Unmarshal([]byte(data), &body) == nil && slices.Contains(body.Changed, kind) {
				return f, skipped
			}
		}
		skipped = append(skipped, f)
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
func afRun(t *testing.T, app string, session func(*ir.IR) afSession, boot func(t *testing.T, g *ir.IR, apiRead string) afStack) {
	t.Helper()
	tm := afTiming{began: time.Now()}
	defer tm.report(t)
	g, err := compile.File(app)
	if err != nil {
		t.Fatalf("compile %s: %v", app, err)
	}
	tm.mark("compile")
	s := session(g)
	apiRead := g.Entities[0].Name
	// The reference: the Go runtime on the FacetQL the toolchain ships
	// (`facet facetql`), no fabric.
	refEngine := afStartEngine(t, "fct")
	gRef, _ := compile.File(app)
	ref := &afSide{name: "reference"}
	*ref = *afStartGoRuntime(t, gRef, "facetql://"+afEngineToken+"@"+strings.TrimPrefix(refEngine.base, "http://"), apiRead)
	ref.name = "reference"
	tm.mark("reference boot")
	gStack, _ := compile.File(app)
	stack := boot(t, gStack, apiRead)
	tm.mark("stack boot")

	run := func(steps []afStep) {
		for _, st := range steps {
			at := time.Now()
			ra := afDo(t, ref, st)
			mid := time.Now()
			sa := afDo(t, stack.side, st)
			tm.request(st.name, mid.Sub(at), time.Since(mid))
			if (st.status != 0 && ra.status != st.status) || !strings.Contains(ra.body, st.contains) {
				t.Errorf("%s: the reference answered %d, want %d containing %q:\n%.400s", st.name, ra.status, st.status, st.contains, ra.body)
			}
			afCompare(t, st, ra, sa)
			if t.Failed() {
				t.Fatalf("stopping at the first failing step (%s); the stack's processes said:\n%s", st.name, afLogs(t))
			}
		}
	}
	run(s.before)
	tm.mark("session before the move")
	for i := 0; i < s.seed; i++ {
		run([]afStep{{name: fmt.Sprintf("seed post %d", i), method: "POST", path: "/api/post", body: fmt.Sprintf(`{"args":["seed row %d"]}`, i)}})
	}
	tm.mark("seeding")
	// The subscriptions live as long as the session; each frame has its own
	// budget (afFrameBudget).
	ctx, cancel := context.WithCancel(context.Background())
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
		tm.mark("migration (with the session's during steps)")
	} else {
		run(s.during)
		tm.mark("session during")
	}
	run(s.after)
	tm.mark("session after the move")
	if s.live {
		ra, sa := afDo(t, ref, s.liveChange), afDo(t, stack.side, s.liveChange)
		afCompare(t, s.liveChange, ra, sa)
		rf, rSkipped := refLive.frameNaming(t, s.rowKind)
		sf, sSkipped := stackLive.frameNaming(t, s.rowKind)
		if rf != sf {
			t.Errorf("live change frame differs:\n%s", afDiff(rf, sf))
		}
		if len(rSkipped)+len(sSkipped) > 0 {
			t.Logf("live: frames of other writers passed over before the change's: reference %q, stack %q", rSkipped, sSkipped)
		}
	}
	tm.mark("live change")
	if stack.fabric != nil {
		afAfterMigration(t, stack, s)
		tm.mark("after-migration checks")
	}
}

// afTiming: where a stack run's wall time went — each phase, and every
// session request split between the reference and the stack — logged when
// the run ends, pass or fail.
type afTiming struct {
	began, last time.Time
	phases      []string
	refTotal    time.Duration
	stackTotal  time.Duration
	requests    int
	slowest     []afSlow
}

type afSlow struct {
	name       string
	ref, stack time.Duration
}

func (tm *afTiming) mark(phase string) {
	now := time.Now()
	from := tm.last
	if from.IsZero() {
		from = tm.began
	}
	tm.phases = append(tm.phases, fmt.Sprintf("%s %.1fs", phase, now.Sub(from).Seconds()))
	tm.last = now
}

func (tm *afTiming) request(name string, ref, stack time.Duration) {
	tm.requests++
	tm.refTotal += ref
	tm.stackTotal += stack
	tm.slowest = append(tm.slowest, afSlow{name, ref, stack})
	sort.Slice(tm.slowest, func(i, j int) bool { return tm.slowest[i].stack > tm.slowest[j].stack })
	if len(tm.slowest) > 5 {
		tm.slowest = tm.slowest[:5]
	}
}

func (tm *afTiming) report(t *testing.T) {
	var slow []string
	for _, s := range tm.slowest {
		slow = append(slow, fmt.Sprintf("%q stack %dms / reference %dms", s.name, s.stack.Milliseconds(), s.ref.Milliseconds()))
	}
	t.Logf("timing: %.1fs in all: %s; %d session requests took %.1fs on the reference and %.1fs on the stack; slowest on the stack: %s",
		time.Since(tm.began).Seconds(), strings.Join(tm.phases, ", "), tm.requests, tm.refTotal.Seconds(), tm.stackTotal.Seconds(), strings.Join(slow, "; "))
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
	if f.byCLI {
		afOperatorMigrate(t, f)
	} else if err := os.WriteFile(filepath.Join(f.dir, "hot"), []byte("hot\n"), 0o644); err != nil {
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
	began := time.Now()
	until("the control loop to admit the move", 20*time.Second, func() bool { return action() != nil })
	admitted := time.Now()
	if a := action(); a["source"] != afSource || a["destination"] != afDestination {
		t.Fatalf("the admitted action: %v", a)
	}
	until("the daemon's mover to start", 20*time.Second, func() bool {
		st, _ := f.adminJSON("/status").(map[string]any)
		movers, _ := st["movers"].(map[string]any)
		copying, _ := movers["copying"].([]any)
		return len(copying) > 0
	})
	moving := time.Now()
	// A read load on the app and the daemon's own probe on the destination,
	// under the mover's transactions, until authority moves — and the
	// migration's phases as routing publishes them, sampled from this
	// instant: the `during` steps below take real time on a real runtime,
	// and a small cell can be copied and cut over before they return.
	stop := make(chan struct{})
	var load sync.WaitGroup
	var loadMu sync.Mutex
	var loadFailures []string
	loadReads, probes := 0, 0
	phases := map[string]bool{}
	holderNow := func() (string, string) {
		routing, _ := f.adminJSON("/routing").(map[string]any)
		placements, _ := routing["placements"].([]any)
		if len(placements) == 0 {
			return "", ""
		}
		p := placements[0].(map[string]any)
		phase := ""
		if m, ok := p["migration"].(map[string]any); ok {
			phase, _ = m["phase"].(string)
		}
		holder, _ := p["holder"].(string)
		return holder, phase
	}
	load.Add(3)
	go func() {
		defer load.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, phase := holderNow(); phase != "" {
				loadMu.Lock()
				phases[phase] = true
				loadMu.Unlock()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
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
	stepped := time.Now()
	until("the cell to move", 120*time.Second, func() bool {
		holder, _ := holderNow()
		return holder == afDestination
	})
	moved := time.Now()
	close(stop)
	load.Wait()
	loadMu.Lock()
	if !phases["copying"] {
		t.Errorf("routing never published the copying phase; saw %v", phases)
	}
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
	t.Logf("migration timing: admitted %.1fs after the ask, mover copying %.1fs later, the during steps %.1fs, the cutover %.1fs after them, measured %.1fs after the cutover",
		admitted.Sub(began).Seconds(), moving.Sub(admitted).Seconds(), stepped.Sub(moving).Seconds(), moved.Sub(stepped).Seconds(), time.Since(moved).Seconds())
	history := f.adminJSON("/actions/history").([]any)
	o := history[0].(map[string]any)
	if o["verdict"] == "Failed" || o["verdict"] == "RolledBack" || o["verdict"] == "NotMeasured" || o["failure"] != nil {
		t.Errorf("the measured outcome of the move: %v", o)
	}
	watch.report(t)
}

// afOperatorMigrate asks for the move as an operator does: the fct CLI,
// `facet exec selfhost/fabric_cli.fct daemon migrate 1 0 0 us-west-db-0
// --admin <operator port>`, the token in FABRIC_ADMIN_TOKEN and nowhere
// else. The daemon admits it through its controller and answers so.
func afOperatorMigrate(t *testing.T, f *afFabric) {
	t.Helper()
	cli, err := filepath.Abs("../selfhost/fabric_cli.fct")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(facetBinary(t), "exec", cli, "daemon", "migrate", "1", "0", "0", afDestination, "--admin", f.admin)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "FACET_DATA_DIR=" + cmd.Dir, "FABRIC_ADMIN_TOKEN=" + afAdminToken}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("fabric daemon migrate: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
	}
	if want := "action-1: admitted, move shard 1 (0,0) to '" + afDestination + "'\n"; out.String() != want || errOut.Len() != 0 {
		t.Fatalf("fabric daemon migrate answered %q (stderr %q), want %q", out.String(), errOut.String(), want)
	}
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
	st := s.afterWrite
	if a := afDo(t, stack.side, st); a.status != 200 && a.status != 201 {
		t.Fatalf("%s: %d %s", st.name, a.status, a.body)
	}
	after := afEngineNodes(t, f.destination, s.rowKind)
	if len(after) != before+1 {
		t.Errorf("the write after the migration: destination held %d %s rows, now %d", before, s.rowKind, len(after))
	}
	srcAfter := afEngineNodes(t, f.source, s.rowKind)
	t.Logf("after the migration (%s runtime): the new holder %s held %d %s rows through the runtime's own store, %d after one more write; the old holder %d",
		stack.side.name, afDestination, before, s.rowKind, len(after), len(srcAfter))
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

// afStartStandaloneDoor runs selfhost/fabric_frontdoor_main.fct in front of
// the fabric's engines, following its routing, and waits until the door
// holds a table (before its first feed answer it refuses everything, 503).
func afStartStandaloneDoor(t *testing.T, f *afFabric) string {
	t.Helper()
	program, err := filepath.Abs("../selfhost/fabric_frontdoor_main.fct")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(facetBinary(t), "exec", program)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "FACET_DATA_DIR=" + dir,
		"FRONTDOOR_PORT=0",
		"FRONTDOOR_BACKENDS=" + afSource + "=" + f.source.base + "," + afDestination + "=" + f.destination.base,
		"FRONTDOOR_FALLBACK_SHARD=1",
		"FRONTDOOR_ROUTING_FEED=" + f.admin,
		"FRONTDOOR_ROUTING_TOKEN=" + afAdminToken,
		"FRONTDOOR_DOOR_ID=allfct-door",
		"FRONTDOOR_FEED_INTERVAL_MS=50",
	}
	p := afStart(t, cmd)
	base := fmt.Sprintf("http://127.0.0.1:%d", afBanner(t, p, afDoorBannerRE, 60*time.Second)[0])
	afWaitHTTP(t, base+"/", afEngineToken, 200, 60*time.Second, p)
	return base
}

func afBootFctOnStandaloneDoor(t *testing.T, g *ir.IR, apiRead string) afStack {
	f := afStartFabric(t, afStartEngine(t, "fct"), afStartEngine(t, "fct"))
	door := afStartStandaloneDoor(t, f)
	side := afStartFctRuntime(t, g, "facetql://"+afEngineToken+"@"+strings.TrimPrefix(door, "http://"), apiRead)
	return afStack{name: "fct-runtime/fct-fabric/standalone-door", side: side, fabric: f}
}

func afBootFctByOperator(t *testing.T, g *ir.IR, apiRead string) afStack {
	f := afStartFabricWith(t, afStartEngine(t, "fct"), afStartEngine(t, "fct"), true)
	side := afStartFctRuntime(t, g, "facetql://"+afEngineToken+"@"+strings.TrimPrefix(f.data, "http://"), apiRead)
	return afStack{name: "fct-runtime/fct-fabric/operator-migrate", side: side, fabric: f}
}

// afBootGoOn: the Go runtime on one engine, no fabric — the reference's own
// shape, on the shipped engine (a check of the harness itself: the stack
// must answer as the reference does) or on the Rust reference engine.
func afBootGoOn(engine string) func(t *testing.T, g *ir.IR, apiRead string) afStack {
	return func(t *testing.T, g *ir.IR, apiRead string) afStack {
		e := afStartEngine(t, engine)
		side := afStartGoRuntime(t, g, "facetql://"+afEngineToken+"@"+strings.TrimPrefix(e.base, "http://"), apiRead)
		return afStack{name: "go-runtime/" + engine + "-facetql", side: side}
	}
}

func afRequire(t *testing.T) {
	t.Helper()
	if os.Getenv(frontdoorEnv) != "" {
		t.Skip("the stacks here are fixed; not re-run under the suite's engine-swap modes")
	}
}

// afStacks: every stack a test runs against the reference — all of them by
// default, on the FacetQL the toolchain ships; and, in a
// FACETQL_REFERENCE=rust run, the Go runtime on the Rust engine as well.
func afStacks(t *testing.T, app string, session func(*ir.IR) afSession) {
	t.Helper()
	t.Run("go-runtime/facetql", func(t *testing.T) { afRun(t, app, session, afBootGoOn("fct")) })
	if rustReference() {
		t.Run("go-runtime/rust-facetql", func(t *testing.T) { afRun(t, app, session, afBootGoOn("rust")) })
	}
	t.Run("go-runtime/fct-fabric", func(t *testing.T) { afRun(t, app, session, afBootGoOnFctFabric) })
	t.Run("fct-runtime/fct-fabric", func(t *testing.T) { afRun(t, app, session, afBootFctOnFctFabric) })
	t.Run("fct-runtime/fct-fabric/standalone-door", func(t *testing.T) { afRun(t, app, session, afBootFctOnStandaloneDoor) })
	t.Run("fct-runtime/fct-fabric/operator-migrate", func(t *testing.T) { afRun(t, app, session, afBootFctByOperator) })
}

// f33d3r.com (facets/f33d3r_com.fct) on every stack. It is the only target:
// the site contains api/main.fct's whole product and its session carries
// what home.fct's did (afSiteSession).
func TestAllFctStackSite(t *testing.T) {
	afRequire(t)
	afStacks(t, "../../facets/f33d3r_com.fct", afSiteSession)
}
