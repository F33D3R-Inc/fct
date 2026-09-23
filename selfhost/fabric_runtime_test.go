package selfhost

// fabric_runtime*.fct port fabric-runtime (state.rs, registry.rs,
// placement.rs, mechanism.rs, lib.rs). fabric_runtime_cases.fct replays the
// crate's own #[test]s input for input and digests every asserted value plus
// the state around it; testdata/fabric_runtime_golden.tsv holds the digests
// the real crate prints for the same steps (case name, tab, digest). A
// mismatch in any field is a behavioural divergence from the Rust crate.

import (
	"bufio"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

func fabricRuntimeLoadCases(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File("fabric_runtime_cases.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fabric_runtime_cases.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func fabricRuntimeGolden(t *testing.T, path string) [][2]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][2]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		name, digest, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("%s: malformed line %q", path, line)
		}
		out = append(out, [2]string{name, digest})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// fabricRuntimeFirstDiff names the first '|'-separated step that differs.
func fabricRuntimeFirstDiff(got, want string) string {
	gs, ws := strings.Split(got, "|"), strings.Split(want, "|")
	for i := 0; i < len(gs) && i < len(ws); i++ {
		if gs[i] != ws[i] {
			return "step " + strconv.Itoa(i) + ":\n got: " + gs[i] + "\nwant: " + ws[i]
		}
	}
	return "step count differs: got " + strconv.Itoa(len(gs)) + ", want " + strconv.Itoa(len(ws))
}

func TestFabricRuntime(t *testing.T) {
	ts := fabricRuntimeLoadCases(t)
	cases := fabricRuntimeGolden(t, "testdata/fabric_runtime_golden.tsv")
	if len(cases) == 0 {
		t.Fatal("no golden cases")
	}
	for _, c := range cases {
		name, want := c[0], c[1]
		t.Run(name, func(t *testing.T) {
			d := postJSON(t, ts, "runRuntimeCase", name)
			got, ok := d["runtimeCaseOut"].(string)
			if !ok {
				t.Fatalf("runRuntimeCase(%q) set no output: %v", name, d)
			}
			if got != want {
				t.Fatalf("%s diverges from fabric-runtime — %s", name, fabricRuntimeFirstDiff(got, want))
			}
		})
	}
}

// TestFabricRuntimeGoldenMatchesRust re-derives the golden digests from the
// real crate (testdata/fabric_check, mode runtime-cases) whenever cargo is
// available, so the golden file can never drift from fabric-runtime.
func TestFabricRuntimeGoldenMatchesRust(t *testing.T) {
	live := laRust(t, "runtime-cases", nil)
	golden := fabricRuntimeGolden(t, "testdata/fabric_runtime_golden.tsv")
	if len(live) != len(golden) {
		t.Fatalf("fabric_check runtime-cases printed %d cases, golden has %d", len(live), len(golden))
	}
	for i, line := range live {
		name, digest, _ := strings.Cut(line, "\t")
		if name != golden[i][0] || digest != golden[i][1] {
			t.Fatalf("golden %s is stale against fabric-runtime — %s", golden[i][0], fabricRuntimeFirstDiff(golden[i][1], digest))
		}
	}
}
