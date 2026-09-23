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

var (
	fabricCliBinOnce sync.Once
	fabricCliBin     string
	fabricCliBinErr  string
)

// fabricCliRealBinary builds fabric-cli's `fabric` binary into a temp target
// dir (--locked, so fabric/Cargo.lock is never rewritten), or skips.
func fabricCliRealBinary(t *testing.T) string {
	t.Helper()
	fabricCliBinOnce.Do(func() {
		cargo, err := exec.LookPath("cargo")
		if err != nil {
			fabricCliBinErr = "cargo is not installed"
			return
		}
		target := filepath.Join(os.TempDir(), "fct-fabric-cli")
		cmd := exec.Command(cargo, "build", "--locked", "--quiet", "--manifest-path", "../../fabric/crates/fabric-cli/Cargo.toml")
		cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
		if out, err := cmd.CombinedOutput(); err != nil {
			fabricCliBinErr = "cargo build failed: " + err.Error() + "\n" + string(out)
			return
		}
		fabricCliBin = filepath.Join(target, "debug", "fabric")
	})
	if fabricCliBin == "" {
		if strings.HasPrefix(fabricCliBinErr, "cargo build failed") {
			t.Fatal(fabricCliBinErr)
		}
		t.Skip("real fabric binary unavailable: " + fabricCliBinErr)
	}
	return fabricCliBin
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
