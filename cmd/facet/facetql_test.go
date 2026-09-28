package main

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/selfhost"
)

// The embedded set is exactly what `facet facetql` compiles: the server's
// (fqserver.fct) and the operator CLI's (fqcli.fct) import closures, every
// module of which is in selfhost.Engine — so it never needs a file the
// binary does not carry.
func TestFacetQLEmbedCoversEngineImports(t *testing.T) {
	entry, err := unpackEngine()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{entry, filepath.Join(filepath.Dir(entry), selfhost.CLIEntry)} {
		srcs, err := compile.Sources(e)
		if err != nil {
			t.Fatalf("%s does not resolve on its own: %v", filepath.Base(e), err)
		}
		for _, s := range srcs {
			if filepath.Dir(s) != filepath.Dir(e) {
				t.Errorf("%s imports %s, outside the embedded set", filepath.Base(e), s)
			}
			if _, err := selfhost.Engine.ReadFile(filepath.Base(s)); err != nil {
				t.Errorf("%s imports %s, which selfhost/engine_embed.go does not embed", filepath.Base(e), filepath.Base(s))
			}
		}
		if _, err := compile.File(e); err != nil {
			t.Fatalf("the unpacked %s does not compile: %v", filepath.Base(e), err)
		}
	}
	// unpacking again reuses the same content-addressed copy
	again, err := unpackEngine()
	if err != nil || again != entry {
		t.Errorf("a second unpack gave %q (%v), want %q", again, err, entry)
	}
}

var facetqlBannerRE = regexp.MustCompile(`FacetQL Server Running on port (\d+)`)

// lockedOutput is a process's stdout, readable while it still writes.
type lockedOutput struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedOutput) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedOutput) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startEngine runs `<bin> facetql` against dir/data on an ephemeral port,
// configured by the environment, and answers the process, its output and the
// port the banner names.
func startEngine(t *testing.T, bin, dir string) (*exec.Cmd, *lockedOutput, string) {
	t.Helper()
	return startEngineWith(t, bin, dir, []string{"FACETQL_DATA_DIR=" + filepath.Join(dir, "data"), "FACETQL_PORT=0"}, "facetql")
}

// startEngineWith runs `<bin> args...` in dir with the development posture,
// one admin token and a master key, plus env.
func startEngineWith(t *testing.T, bin, dir string, env []string, args ...string) (*exec.Cmd, *lockedOutput, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"),
		"FACETQL_ENV=development", "FACETQL_TOKENS=tok:alice:admin",
		"FACETQL_MASTER_KEY=" + strings.Repeat("ab", 32)}, env...)
	out := &lockedOutput{}
	cmd.Stdout, cmd.Stderr = out, out
	dieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if m := facetqlBannerRE.FindStringSubmatch(out.String()); m != nil {
			return cmd, out, m[1]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the engine never listened:\n%s", out.String())
	return nil, nil, ""
}

// stopEngine sends SIGTERM and insists on a clean stop.
func stopEngine(t *testing.T, cmd *exec.Cmd, out *lockedOutput) {
	t.Helper()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if cmd.ProcessState.ExitCode() != 0 || !strings.Contains(out.String(), "storage checkpointed; stopped cleanly") {
		t.Fatalf("SIGTERM: exit %d\n%s", cmd.ProcessState.ExitCode(), out.String())
	}
}

// facetqlCLI runs `<bin> facetql args...` in dir and answers stdout, stderr
// and the exit status.
func facetqlCLI(t *testing.T, bin, dir string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"facetql"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append([]string{"HOME=" + dir, "PATH=" + os.Getenv("PATH")}, env...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	dieWithParent(cmd)
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), code
}

func engineCall(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("x-api-key", "tok")
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func buildToolchain(t *testing.T, work string) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no Go toolchain to build the facet binary with")
	}
	facet := filepath.Join(work, "facet")
	build := exec.Command(goTool, "build", "-o", facet, "facet/cmd/facet")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the toolchain: %v\n%s", err, out)
	}
	return facet
}

// `facet facetql` is a database: it serves, stores durably, stops cleanly on
// SIGTERM, and starts again on the same data — with nothing installed but the
// facet binary (no source tree: it runs from an empty directory).
func TestFacetQLCommandServesAndPersists(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the toolchain and runs the engine")
	}
	work := t.TempDir()
	facet := buildToolchain(t, work)
	dir := t.TempDir()
	cmd, out, port := startEngine(t, facet, dir)
	base := "http://127.0.0.1:" + port
	if code, body := engineCall(t, "GET", base+"/", ""); code != 200 || body != "FacetQL Online" {
		t.Fatalf("GET / = %d %q", code, body)
	}
	if code, body := engineCall(t, "POST", base+"/node", `{"address":"a:1","kind":"A","x":0,"y":0,"z":0,"q":0,"data":"{\"v\":1}"}`); code != 201 {
		t.Fatalf("POST /node = %d %s", code, body)
	}
	stopEngine(t, cmd, out)
	_, _, port2 := startEngine(t, facet, dir)
	if code, body := engineCall(t, "GET", "http://127.0.0.1:"+port2+"/node/a:1", ""); code != 200 || !strings.Contains(body, `"address":"a:1"`) {
		t.Fatalf("after a restart GET /node/a:1 = %d %s", code, body)
	}
	// It is `facetql`'s command line: start's flags configure it as its
	// environment does (a relative --data-dir is the working directory's) …
	_, _, port3 := startEngineWith(t, facet, dir, nil, "facetql", "start", "--port", "0", "--data-dir", "data2")
	if code, _ := engineCall(t, "GET", "http://127.0.0.1:"+port3+"/", ""); code != 200 {
		t.Fatalf("start --port 0 --data-dir data2: GET / = %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "data2", "facetql.lock")); err != nil {
		t.Errorf("start --data-dir data2 did not open dir/data2: %v", err)
	}
	// … and a command line clap would refuse is refused as clap refuses it
	_, stderr, code := facetqlCLI(t, facet, dir, nil, "--port", "1")
	if code != 2 || !strings.HasPrefix(stderr, "error: unexpected argument '--port' found") {
		t.Errorf("facet facetql --port 1: exit %d\n%s", code, stderr)
	}
}

// Every operator command runs from the facet binary against the engine it
// serves: identities, writes and reads, indexes, stats and the route matrix
// over the API; then, with the server stopped, a backup restored into an
// empty directory is a database that serves the same data.
func TestFacetQLOperatorCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the toolchain and runs the engine")
	}
	facet := buildToolchain(t, t.TempDir())
	dir := t.TempDir()
	cmd, out, port := startEngine(t, facet, dir)
	env := []string{"FACETQL_URL=http://127.0.0.1:" + port, "FACETQL_TOKEN=tok"}
	run := func(want int, args ...string) string {
		t.Helper()
		stdout, stderr, code := facetqlCLI(t, facet, dir, env, args...)
		if code != want {
			t.Fatalf("facet facetql %q: exit %d, want %d\n%s%s", args, code, want, stdout, stderr)
		}
		return stdout + stderr
	}
	created := run(0, "user", "create", "bob")
	m := regexp.MustCompile(`token: (\S+)`).FindStringSubmatch(created)
	if m == nil || !strings.Contains(created, `Created identity "bob" (role User).`) {
		t.Fatalf("user create:\n%s", created)
	}
	if o := run(0, "put", "Note:1", "--kind", "Note", "--data", `{"n":1}`, "--token", m[1]); !strings.Contains(o, `Wrote node "Note:1" (kind "Note").`) {
		t.Fatalf("put:\n%s", o)
	}
	if o := run(0, "get", "Note:1"); !strings.Contains(o, "owner:      bob") || !strings.Contains(o, `data:       {"n":1}`) {
		t.Fatalf("get:\n%s", o)
	}
	if o := run(0, "index", "create", "note_n", "--kind", "Note", "--field", "n"); !strings.Contains(o, `Declared index "note_n" on Note.n.`) {
		t.Fatalf("index create:\n%s", o)
	}
	if o := run(0, "query", "--kind", "Note", "--order", "n", "--json"); !strings.Contains(o, `"address": "Note:1"`) {
		t.Fatalf("query:\n%s", o)
	}
	if o := run(0, "stats"); !strings.Contains(o, "1 node(s) across 1 kind(s)") {
		t.Fatalf("stats:\n%s", o)
	}
	if o := run(0, "routes"); !strings.Contains(o, "34 route(s)") {
		t.Fatalf("routes:\n%s", o)
	}
	if o := run(1, "get", "Nope:1"); !strings.Contains(o, "error: server returned HTTP 404") {
		t.Fatalf("get of a missing node:\n%s", o)
	}
	if o := run(3, "delete", "Note:1"); !strings.Contains(o, "error: aborted") {
		t.Fatalf("delete without confirmation:\n%s", o)
	}
	stopEngine(t, cmd, out)
	data := filepath.Join(dir, "data")
	if o := run(0, "--data-dir", data, "backup", "bk"); !strings.Contains(o, "Backed up ") {
		t.Fatalf("backup:\n%s", o)
	}
	if o := run(0, "--data-dir", "restored", "restore", "bk"); !strings.Contains(o, "Restored ") {
		t.Fatalf("restore:\n%s", o)
	}
	if o := run(1, "--data-dir", "restored", "restore", "bk"); !strings.Contains(o, "refusing to restore") {
		t.Fatalf("a second restore:\n%s", o)
	}
	_, _, port2 := startEngineWith(t, facet, dir, []string{"FACETQL_DATA_DIR=restored", "FACETQL_PORT=0"}, "facetql")
	if code, body := engineCall(t, "GET", "http://127.0.0.1:"+port2+"/node/Note:1", ""); code != 200 || !strings.Contains(body, `"owner":"bob"`) {
		t.Fatalf("the restored database: GET /node/Note:1 = %d %s", code, body)
	}
}

// A release binary carries the engine too: `<app> facetql` serves from an
// empty directory with no facet binary anywhere — the deployment's app image
// is its database image — and `<app> healthcheck --path /` probes it.
func TestReleaseBinaryCarriesFacetQL(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a release artifact and runs its engine")
	}
	work := t.TempDir()
	facet := buildToolchain(t, work)
	project := filepath.Join(work, "blog")
	tmpl, _ := findTemplate("app")
	if err := scaffold(tmpl, project, "", false); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	artifact := filepath.Join(work, "dist", "blog")
	if o, err := runIn(project, facet, "build", "--release", tmpl.Entry, "-o", artifact); err != nil {
		t.Fatalf("facet build --release: %v\n%s", err, o)
	}
	os.Remove(facet)
	empty := t.TempDir()
	_, _, port := startEngine(t, artifact, empty)
	if code, body := engineCall(t, "GET", "http://127.0.0.1:"+port+"/", ""); code != 200 || body != "FacetQL Online" {
		t.Fatalf("the artifact's engine: GET / = %d %q", code, body)
	}
	hc, err := runIn(empty, artifact, "healthcheck", "--port", port, "--path", "/")
	if err != nil || !strings.Contains(hc, "healthy") {
		t.Errorf("healthcheck --path / against the engine: %v %s", err, hc)
	}
}

// The generated deployments run the embedded engine, never an external
// FacetQL image: the dev compose file runs `facet facetql` from the toolchain
// image, the production compose file runs the release binary as `facetql`
// (healthchecked, and the migration waits for it), and the systemd set gains
// the engine's unit.
func TestDeploymentsRunTheEmbeddedEngine(t *testing.T) {
	d := deployment{App: "Blog", Binary: "blog", Entry: "app.fct"}
	for name, text := range map[string]string{"docker-compose.yml": dockerCompose, "deploy/docker-compose.yml": prodCompose(d), "deploy/blog-facetql.service": prodSystemdFacetQL(d)} {
		if strings.Contains(text, "ghcr.io/f33d3r-inc/facetql") || strings.Contains(text, "image: ") && strings.Contains(text, "facetql:latest") {
			t.Errorf("%s still names an external FacetQL image", name)
		}
	}
	if !strings.Contains(dockerCompose, `entrypoint: ["facet", "facetql"]`) {
		t.Errorf("the dev compose file does not run facet facetql:\n%s", dockerCompose)
	}
	prod := prodCompose(d)
	for _, want := range []string{`command: ["facetql"]`, `"healthcheck", "--port", "8080", "--path", "/"`, "condition: service_healthy", "FACETQL_DATA_DIR: /data"} {
		if !strings.Contains(prod, want) {
			t.Errorf("the production compose file lacks %q", want)
		}
	}
	unit := prodSystemdFacetQL(d)
	for _, want := range []string{"ExecStart=/usr/local/bin/blog facetql", "Environment=FACETQL_DATA_DIR=/var/lib/blog-facetql", "StateDirectory=blog-facetql", "Before=blog.service"} {
		if !strings.Contains(unit, want) {
			t.Errorf("the engine's systemd unit lacks %q", want)
		}
	}
}
