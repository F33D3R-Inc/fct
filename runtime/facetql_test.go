package runtime

// A real FacetQL for the server integration tests in server_test.go. Each
// test boots its own engine — its own port, token and data directory under
// t.TempDir() — and points FACET_DATABASE_URL at it, so New(graph) opens the
// same store a deployment would and nothing here can see or corrupt a
// developer's database. This is the runtime-package twin of
// integration/stack_test.go's harness: the binary lookup, the staleness
// refusal and the boot environment are the same, restated here because a
// test file cannot import another package's _test.go.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// facetqlBinary locates the engine: FACETQL_BIN, else the sibling checkout's
// release then debug build. Absent, the test skips — a checkout without a
// built engine should not report a red suite for something it never had. A
// binary that is older than the engine's own sources fails instead of
// skipping (see checkNotStale): a green run against a stale engine is a false
// positive, not a pass.
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

// checkNotStale refuses a facetql binary older than the newest .rs under its
// engine's src/ (target/{release,debug}/facetql -> facetql/src, or the
// sibling checkout's src for a binary named by FACETQL_BIN). No reachable
// source tree means nothing to compare against, and no verdict.
func checkNotStale(t *testing.T, bin string) {
	t.Helper()
	binInfo, err := os.Stat(bin)
	if err != nil {
		return
	}
	src := filepath.Join(filepath.Dir(bin), "..", "..", "src")
	if _, err := os.Stat(src); err != nil {
		abs, err2 := filepath.Abs("../../facetql/src")
		if err2 != nil {
			return
		}
		if _, err3 := os.Stat(abs); err3 != nil {
			return
		}
		src = abs
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
	if newest != "" && newestTime.After(binInfo.ModTime()) {
		t.Fatalf("facetql binary %s (built %s) is older than %s (modified %s) — "+
			"rebuild it first: cd facetql && cargo build --release. "+
			"A run against a stale engine is a false positive, not a pass.",
			bin, binInfo.ModTime().Format(time.RFC3339),
			newest, newestTime.Format(time.RFC3339))
	}
}

// freePort asks the kernel for a port and releases it.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// requireFacetQL boots a FacetQL for this test and points the runtime at it:
// FACET_DATABASE_URL=facetql://tok@127.0.0.1:<port>, restored when the test
// ends, plus a fixed FACET_SECRET so signed cookies and @secret columns have
// a key. The engine is killed and its data directory discarded with the test.
func requireFacetQL(t *testing.T) {
	t.Helper()
	bin := facetqlBinary(t)
	port := freePort(t)
	dir := t.TempDir()
	const token = "tok"

	log, err := os.Create(filepath.Join(dir, "facetql.log"))
	if err != nil {
		t.Fatalf("engine log: %v", err)
	}
	cmd := exec.Command(bin, "start")
	cmd.Env = append(os.Environ(),
		"FACETQL_DATA_DIR="+dir,
		fmt.Sprintf("FACETQL_PORT=%d", port),
		"FACETQL_TOKENS="+token+":root:admin",
		// Development posture: the engine refuses an all-zero at-rest key and
		// plaintext HTTP unless told this is not production. It is not.
		"FACETQL_ENV=development",
		"FACETQL_ALLOW_PLAINTEXT=1",
		// One identity doing in a second what a deployment spreads over
		// minutes: the per-identity limiter is not the property under test.
		"FACETQL_RATE_READ=off",
		"FACETQL_RATE_WRITE=off",
		"FACETQL_RATE_BULK=off",
		"FACETQL_RATE_ADMIN=off",
		"FACETQL_RATE_SUBSCRIBE=off",
	)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting facetql: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Close()
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, base+"/stats", nil)
		req.Header.Set("x-api-key", token)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			body, _ := os.ReadFile(filepath.Join(dir, "facetql.log"))
			t.Fatalf("facetql never became ready; log:\n%s", body)
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Setenv("FACET_DATABASE_URL", fmt.Sprintf("facetql://%s@127.0.0.1:%d", token, port))
	t.Setenv("FACET_SECRET", "server-test-secret-server-test-secret")
	if os.Getenv("FACET_LOG_LEVEL") == "" {
		t.Setenv("FACET_LOG_LEVEL", "error")
	}
}
