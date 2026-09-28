package selfhost

// fabric_cli.fct ports fabric-cli's args.rs, session.rs and main.rs (render.rs
// is fabric_cli_render.fct) and runs as a command through `facet exec`
// (runtime.RunMain + the io.console builtins). Each case below is one
// invocation — argv, stdin, the session files beside it — and the port must
// reproduce the real `fabric` binary's stdout, stderr and exit code byte for
// byte. testdata/fabric_cli_golden.json holds what the real binary printed;
// when cargo is available the binary is built (without touching fabric/) and
// the golden file is checked against it live.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// fabricCliFiles are the session files every case can name, written into the
// directory both the real binary and the port run in.
var fabricCliFiles = map[string]string{
	"empty.json":     "[]",
	"malformed.json": "{ not valid json ",
	"unknown.json":   `[ { "Nonsense": { "node_id": "x" } } ]`,
	"trailing.json":  `[] x`,
	"offgrid.json": `[
  {"Topology": {"node_id": "n1", "timestamp_ms": 5, "placements": [
    {"coordinate": {"x": 20, "y": 1}, "dbms_id": "n1", "region": "r"},
    {"coordinate": {"x": 3, "y": 14}, "dbms_id": "n1", "region": "r"},
    {"coordinate": {"x": 1, "y": 1}, "dbms_id": "n1", "region": "r"}]}},
  {"Telemetry": {"timestamp_ms": 9, "node_id": "n1", "shard": {"id": 4, "workload_domain": "d"},
    "samples": [{"coordinate": {"x": 20, "y": 1}, "operations_per_second": 5.0, "read_ratio": 0.5, "write_ratio": 0.5,
      "read_latency_us": 1.0, "write_latency_us": 2.0, "cpu_utilization": 0.9, "memory_utilization": 0.9, "queue_depth": 20000}]}}
]`,
}

type fabricCliCase struct {
	Args   []string `json:"args"`
	Stdin  string   `json:"stdin,omitempty"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
	Code   int      `json:"code"`
}

func fabricCliCases() []fabricCliCase {
	var cs []fabricCliCase
	add := func(args ...string) { cs = append(cs, fabricCliCase{Args: args}) }
	add()
	add("--help")
	add("help")
	add("-V")
	add("version")
	add("bogus")
	add("status", "--bogus")
	add("status", "extra")
	add("nodes", "--now")
	add("nodes", "--now=abc")
	add("nodes", "--deadline", "x")
	add("nodes", "--deadline=+7", "--input", "session.json")
	add("status", "-i")
	for _, sub := range []string{"status", "topology", "workload", "metrics", "placement", "predict", "validate", "nodes"} {
		add(sub, "--help")
		add(sub)
		add(sub, "--input", "session.json")
		add(sub, "--input=session.json", "--json")
		add(sub, "-i", "offgrid.json")
		add(sub, "-i", "offgrid.json", "--json")
	}
	add("nodes", "-i", "session.json", "--now", "5000", "--deadline", "1000")
	add("nodes", "-i", "session.json", "--now=999999")
	add("status", "--input", "missing.json")
	add("status", "--input", "empty.json")
	add("status", "--input", "malformed.json")
	add("status", "--input", "unknown.json")
	add("status", "--input", "trailing.json")
	cs = append(cs, fabricCliCase{Args: []string{"topology", "--input", "-"}, Stdin: "session.json"})
	cs = append(cs, fabricCliCase{Args: []string{"status", "--input", "-"}, Stdin: "malformed.json"})
	return cs
}

// fabricCliDir writes the session files (plus the crate's own example
// session) into a fresh directory.
func fabricCliDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	example, err := os.ReadFile("../../fabric/crates/fabric-cli/examples/session.json")
	if err != nil {
		t.Fatalf("the fabric-cli example session is needed beside fct: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), example, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range fabricCliFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fabricCliStdin resolves a case's stdin: a file name from the directory.
func fabricCliStdin(t *testing.T, dir, name string) string {
	t.Helper()
	if name == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fabricCliPort(t *testing.T, dir string, c fabricCliCase) fabricCliCase {
	t.Helper()
	g, err := compile.File("fabric_cli.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fabric_cli.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	srv.SetStdio(strings.NewReader(fabricCliStdin(t, dir, c.Stdin)), &out, &errOut)
	srv.SetDataDir(dir)
	code, err := srv.RunMain(c.Args)
	if err != nil {
		t.Errorf("fabric %q: the port failed: %v", c.Args, err)
		return fabricCliCase{Args: c.Args, Stdin: c.Stdin, Stdout: out.String(), Stderr: "(port error) " + err.Error(), Code: -1}
	}
	return fabricCliCase{Args: c.Args, Stdin: c.Stdin, Stdout: out.String(), Stderr: errOut.String(), Code: code}
}

// fabricCliRealBinary: fabric-cli's `fabric` (fabricWorkspaceBinary).
func fabricCliRealBinary(t *testing.T) string {
	t.Helper()
	return fabricWorkspaceBinary(t, "fabric")
}

func fabricCliReal(t *testing.T, bin, dir string, c fabricCliCase) fabricCliCase {
	t.Helper()
	cmd := exec.Command(bin, c.Args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(fabricCliStdin(t, dir, c.Stdin))
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("fabric %v: %v", c.Args, err)
		}
		code = exit.ExitCode()
	}
	return fabricCliCase{Args: c.Args, Stdin: c.Stdin, Stdout: out.String(), Stderr: errOut.String(), Code: code}
}

func fabricCliGolden(t *testing.T) []fabricCliCase {
	t.Helper()
	b, err := os.ReadFile("testdata/fabric_cli_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs []fabricCliCase
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

func fabricCliSame(t *testing.T, what string, got, want fabricCliCase) {
	t.Helper()
	if got.Code != want.Code || got.Stdout != want.Stdout || got.Stderr != want.Stderr {
		t.Errorf("fabric %q (%s):\n got code %d stdout %q stderr %q\nwant code %d stdout %q stderr %q",
			got.Args, what, got.Code, got.Stdout, got.Stderr, want.Code, want.Stdout, want.Stderr)
	}
}

func TestFabricCli(t *testing.T) {
	dir := fabricCliDir(t)
	golden := fabricCliGolden(t)
	cases := fabricCliCases()
	if len(golden) != len(cases) {
		t.Fatalf("golden has %d cases, the test %d — regenerate with FCT_REGEN_FABRIC_CLI_GOLDEN=1", len(golden), len(cases))
	}
	for i, c := range cases {
		if strings.Join(golden[i].Args, " ") != strings.Join(c.Args, " ") || golden[i].Stdin != c.Stdin {
			t.Fatalf("golden case %d is %q, the test's is %q", i, golden[i].Args, c.Args)
		}
		fabricCliSame(t, "port vs golden", fabricCliPort(t, dir, c), golden[i])
	}
}

// TestFabricCliGoldenMatchesRealBinary checks the golden file against the
// real binary (and rewrites it under FCT_REGEN_FABRIC_CLI_GOLDEN=1).
func TestFabricCliGoldenMatchesRealBinary(t *testing.T) {
	bin := fabricCliRealBinary(t)
	dir := fabricCliDir(t)
	var live []fabricCliCase
	for _, c := range fabricCliCases() {
		live = append(live, fabricCliReal(t, bin, dir, c))
	}
	if os.Getenv("FCT_REGEN_FABRIC_CLI_GOLDEN") == "1" {
		b, _ := json.MarshalIndent(live, "", "  ")
		if err := os.WriteFile("testdata/fabric_cli_golden.json", append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden := fabricCliGolden(t)
	if len(golden) != len(live) {
		t.Fatalf("golden has %d cases, the real binary ran %d", len(golden), len(live))
	}
	for i := range live {
		fabricCliSame(t, "golden vs real binary", golden[i], live[i])
	}
}

// ── fabric daemon ... (daemon.rs / fabric_cli_daemon.fct) ─────────────────

// fabricCliDaemonCase is one `fabric daemon` invocation: its argv and
// environment ($ADMIN and $BAD name the two stand-in operator ports), what
// it printed and exited with, and the requests it sent.
type fabricCliDaemonCase struct {
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	Stdout   string            `json:"stdout"`
	Stderr   string            `json:"stderr"`
	Code     int               `json:"code"`
	Requests []string          `json:"requests"`
}

func fabricCliDaemonCases() []fabricCliDaemonCase {
	token := map[string]string{"FABRIC_ADMIN_TOKEN": "operator-secret"}
	both := map[string]string{"FABRIC_ADMIN_TOKEN": "operator-secret", "FABRIC_ADMIN_URL": "$ADMIN"}
	c := func(env map[string]string, args ...string) fabricCliDaemonCase {
		return fabricCliDaemonCase{Args: append([]string{"daemon"}, args...), Env: env}
	}
	return []fabricCliDaemonCase{
		c(nil), c(nil, "--help"), c(nil, "status", "--help"), c(nil, "bogus"), c(nil, "status", "extra"),
		c(nil, "migrate", "1", "0"), c(nil, "migrate", "1", "0", "300", "d"), c(nil, "migrate", "x", "0", "0", "d"),
		c(nil, "migrate", "1", "0", "0", "d", "e"), c(nil, "status", "--token", "t"), c(nil, "status", "--admin"),
		c(token, "status"),
		c(both, "status"),
		c(token, "status", "--admin", "$ADMIN", "--json"),
		c(token, "placements", "--admin=$ADMIN"),
		c(token, "placements", "--admin", "$ADMIN", "--json"),
		c(token, "status", "--admin", "https://127.0.0.1:7071"),
		c(token, "status", "--admin", "http://127.0.0.1"),
		c(token, "status", "--admin", "http://127.0.0.1:7071/status"),
		c(both, "status", "--admin="),
		c(nil, "status", "--admin", "$ADMIN"),
		c(map[string]string{"FABRIC_ADMIN_TOKEN": ""}, "status", "--admin", "$ADMIN"),
		c(map[string]string{"FABRIC_ADMIN_TOKEN": "operator-secrets"}, "status", "--admin", "$ADMIN"),
		c(token, "status", "--admin", "http://127.0.0.1:1"),
		c(both, "migrate", "1", "0", "0", "us-west-db-0"),
		c(both, "migrate", "1", "0", "0", "busy"),
		c(both, "migrate", "1", "0", "0", "stopped"),
		c(both, "migrate", "18446744073709551615", "255", "+0", "us-west-db-0"),
		c(token, "status", "--admin", "$BAD"),
		c(token, "placements", "--admin", "$BAD"),
		c(token, "migrate", "1", "0", "0", "us-west-db-0", "--admin", "$BAD"),
	}
}

// fabricCliOperatorPorts starts the two stand-ins: $ADMIN answers as
// fabricd's operator port does (the /status and /placements bodies were
// taken from the crate's fabricd with an operator's move in flight), $BAD
// with what a client must not mistake for an answer. Each records what it
// was asked.
func fabricCliOperatorPorts(t *testing.T) (admin, bad string, record func() []string) {
	t.Helper()
	status, err := os.ReadFile("testdata/fabric_cli_daemon/status.json")
	if err != nil {
		t.Fatal(err)
	}
	placements, err := os.ReadFile("testdata/fabric_cli_daemon/placements.json")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	rec := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s %s key=%q type=%q body=%q", r.Method, r.URL.RequestURI(), r.Header.Get("x-api-key"), r.Header.Get("content-type"), b))
		mu.Unlock()
	}
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec(r)
		text := func(code int, s string) {
			w.Header().Set("content-type", "text/plain; charset=utf-8")
			w.WriteHeader(code)
			io.WriteString(w, s)
		}
		if r.Header.Get("x-api-key") != "operator-secret" {
			text(401, "fabricd: the operator surface requires x-api-key\n")
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/status":
			w.Header().Set("content-type", "application/json")
			w.Write(status)
		case r.Method == "GET" && r.URL.Path == "/placements":
			w.Header().Set("content-type", "application/json")
			w.Write(placements)
		case r.Method == "POST" && r.URL.Path == "/actions":
			switch {
			case strings.Contains(string(body), `"busy"`):
				text(409, "fabricd: shard 1 (0,0) already has action-1 (move) in flight\n")
			case strings.Contains(string(body), `"stopped"`):
				text(503, "fabricd: the control loop has stopped\n")
			default:
				text(200, "action-1: admitted, move shard 1 (0,0) to 'us-west-db-0'\n")
			}
		default:
			text(404, "fabricd: no such admin route\n")
		}
	}))
	t.Cleanup(good.Close)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		switch r.URL.Path {
		case "/status":
			io.WriteString(w, "not json")
		case "/placements":
			w.WriteHeader(500)
			io.WriteString(w, "boom\n\n")
		default:
			w.WriteHeader(400)
			io.WriteString(w, "fabricd: missing field `destination`\n")
		}
	}))
	t.Cleanup(broken.Close)
	return good.URL, broken.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := seen
		seen = nil
		return out
	}
}

func fabricCliDaemonSubst(s, admin, bad string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "$ADMIN", admin), "$BAD", bad)
}

// fabricCliDaemonRun runs one case, through the port (bin "") or the real
// binary, against the stand-ins; what it asked is recorded with the ports'
// addresses written back as $ADMIN / $BAD.
func fabricCliDaemonRun(t *testing.T, bin string, c fabricCliDaemonCase, admin, bad string, record func() []string) fabricCliDaemonCase {
	t.Helper()
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = fabricCliDaemonSubst(a, admin, bad)
	}
	env := map[string]string{}
	for k, v := range c.Env {
		env[k] = fabricCliDaemonSubst(v, admin, bad)
	}
	record()
	var out, errOut bytes.Buffer
	code := 0
	if bin == "" {
		for _, k := range []string{"FABRIC_ADMIN_URL", "FABRIC_ADMIN_TOKEN"} {
			if v, ok := env[k]; ok {
				t.Setenv(k, v)
			} else {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
		}
		g, err := compile.File("fabric_cli.fct")
		if err != nil {
			t.Fatalf("compile selfhost/fabric_cli.fct: %v", err)
		}
		srv, err := runtime.NewInMemory(g)
		if err != nil {
			t.Fatal(err)
		}
		srv.SetStdio(strings.NewReader(""), &out, &errOut)
		srv.SetDataDir(t.TempDir())
		code, err = srv.RunMain(args)
		if err != nil {
			t.Fatalf("fabric %q: the port failed: %v", args, err)
		}
	} else {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("fabric %v: %v", args, err)
			}
			code = exit.ExitCode()
		}
	}
	var asked []string
	for _, r := range record() {
		asked = append(asked, strings.ReplaceAll(strings.ReplaceAll(r, strings.TrimPrefix(admin, "http://"), "$ADMIN"), strings.TrimPrefix(bad, "http://"), "$BAD"))
	}
	return fabricCliDaemonCase{Args: c.Args, Env: c.Env, Stdout: out.String(), Stderr: errOut.String(), Code: code, Requests: asked}
}

func fabricCliDaemonSame(t *testing.T, what string, got, want fabricCliDaemonCase) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("fabric %q env %v (%s):\n got %s\nwant %s", got.Args, got.Env, what, g, w)
	}
}

const fabricCliDaemonGoldenPath = "testdata/fabric_cli_daemon/golden.json"

// TestFabricCliDaemon: the port's `fabric daemon ...` against the golden
// the crate's binary printed — output, exit status and every request sent.
func TestFabricCliDaemon(t *testing.T) {
	admin, bad, record := fabricCliOperatorPorts(t)
	b, err := os.ReadFile(fabricCliDaemonGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var golden []fabricCliDaemonCase
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	cases := fabricCliDaemonCases()
	if len(golden) != len(cases) {
		t.Fatalf("golden has %d cases, the test %d — regenerate with FCT_REGEN_FABRIC_CLI_GOLDEN=1", len(golden), len(cases))
	}
	for i, c := range cases {
		fabricCliDaemonSame(t, "port vs golden", fabricCliDaemonRun(t, "", c, admin, bad, record), golden[i])
	}
}

// TestFabricCliDaemonGoldenMatchesRealBinary checks the golden against the
// crate's binary (and rewrites it under FCT_REGEN_FABRIC_CLI_GOLDEN=1).
func TestFabricCliDaemonGoldenMatchesRealBinary(t *testing.T) {
	bin := fabricCliRealBinary(t)
	admin, bad, record := fabricCliOperatorPorts(t)
	var live []fabricCliDaemonCase
	for _, c := range fabricCliDaemonCases() {
		live = append(live, fabricCliDaemonRun(t, bin, c, admin, bad, record))
	}
	if os.Getenv("FCT_REGEN_FABRIC_CLI_GOLDEN") == "1" {
		b, _ := json.MarshalIndent(live, "", "  ")
		if err := os.WriteFile(fabricCliDaemonGoldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(fabricCliDaemonGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var golden []fabricCliDaemonCase
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) != len(live) {
		t.Fatalf("golden has %d cases, the real binary ran %d", len(golden), len(live))
	}
	for i := range live {
		fabricCliDaemonSame(t, "golden vs real binary", golden[i], live[i])
	}
}
