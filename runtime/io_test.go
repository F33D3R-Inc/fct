package runtime

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
)

// ioFileRunApp mirrors internal/compile/io_test.go's ioFileApp: a proc that
// writes a file and one that reads it back, both declaring `uses io.file`,
// called from an action that round-trips a value through the sandboxed data
// directory — proven live over HTTP against a real filesystem (t.TempDir()),
// not mocked.
const ioFileRunApp = `app A:
    proc save(path: text, content: text) -> bool uses io.file:
        return writeFile(path, content)
    proc load(path: text) -> text uses io.file:
        return readFile(path)
    state result: text = ""
    action run(path: text, content: text):
        let ok = do save(path, content)
        let back = do load(path)
        result = back
    view Home at "/":
        box:
            text "{result}"
`

// TestFileIOReadWriteLive proves readFile/writeFile do real, live filesystem
// I/O under the server's sandboxed data directory (FACET_DATA_DIR, wired here
// to t.TempDir()): a proc writes a real file, a second proc reads it back,
// and the round-tripped content must match exactly, over a real HTTP request
// to a real httptest server — not evaluated in-process against a fake.
func TestFileIOReadWriteLive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACET_DATA_DIR", dir)

	g, err := compile.String(ioFileRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "run", `{"args":["greeting.txt","hello from a proc"]}`)
	if got := fmt.Sprint(deltas["result"]); got != "hello from a proc" {
		t.Fatalf("round-tripped file content = %q, want %q", got, "hello from a proc")
	}

	// The file must be a real file, really sitting under the configured
	// sandbox root — not just an in-memory illusion the action layer faked.
	want := filepath.Join(dir, "greeting.txt")
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected a real file at %s: %v", want, err)
	}
	if string(data) != "hello from a proc" {
		t.Fatalf("file on disk = %q, want %q", string(data), "hello from a proc")
	}

	// A nested path creates its parent directories, as writeFile's own doc
	// promises.
	deltas = postJSON(t, ts, "run", `{"args":["nested/dir/report.txt","nested content"]}`)
	if got := fmt.Sprint(deltas["result"]); got != "nested content" {
		t.Fatalf("nested round-tripped content = %q, want %q", got, "nested content")
	}
}

// ioTraversalRunApp exposes a single proc that only reads a file — enough to
// prove the sandbox rejects both a `..`-climbing relative path and a bare
// absolute path.
const ioTraversalRunApp = `app A:
    proc load(path: text) -> text uses io.file:
        return readFile(path)
    state result: text = ""
    action run(path: text):
        let back = do load(path)
        result = back
    view Home at "/":
        box:
            text "{result}"
`

// TestFileIOPathTraversalRejected proves a proc cannot escape its configured
// sandbox directory — neither via a `../../` relative climb nor via a bare
// absolute path naming a file outside it — even though the path arrives as
// ordinary runtime data (an action argument), not something checkNoIO or any
// other compile-time pass could ever see. Both must fail cleanly (a non-2xx
// response, not a crash), and the server must keep answering normally
// afterward.
func TestFileIOPathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACET_DATA_DIR", dir)

	// A real, sensitive-looking file OUTSIDE the sandbox root, so a successful
	// escape would be observable (not just "some error happened").
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := compile.String(ioTraversalRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cases := []string{
		outside,                        // an absolute path escaping the sandbox entirely
		"../../../../etc/passwd",       // a relative climb out of the sandbox
		"../" + filepath.Base(outside), // a climb that (if resolved naively) reaches the sibling temp dir
	}
	for _, p := range cases {
		body := fmt.Sprintf(`{"args":[%q]}`, p)
		resp, err := postRaw(t, ts, "run", body)
		if err != nil {
			t.Fatal(err)
		}
		respBody := readBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("path %q: want a 4xx/5xx rejection, got %d: %s", p, resp.StatusCode, respBody)
		}
		if strings.Contains(respBody, "top secret") {
			t.Fatalf("path %q: sandbox escape! response leaked the outside file's content: %s", p, respBody)
		}
	}

	// Prove the SAME server is still alive and answering normally after every
	// rejected escape attempt: a real file placed directly inside the sandbox
	// root reads back fine through the very ts instance the escapes were tried
	// against.
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := postJSON(t, ts, "run", `{"args":["ok.txt"]}`)
	if got := fmt.Sprint(after["result"]); got != "fine" {
		t.Fatalf("server did not recover cleanly after rejected traversal attempts: got %q, want %q", got, "fine")
	}
}

// ioNetRunApp mirrors internal/compile/io_test.go's ioNetApp: procs that call
// httpGet/httpPost, both declaring `uses io.net`.
const ioNetRunApp = `app A:
    proc fetch(url: text) -> text uses io.net:
        return httpGet(url)
    proc send(url: text, body: text) -> text uses io.net:
        return httpPost(url, body)
    state result: text = ""
    action get(url: text):
        let r = do fetch(url)
        result = r
    action post(url: text, body: text):
        let r = do send(url, body)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestHTTPClientLive spins up a REAL httptest server standing in for "some
// external service" and has a proc call httpGet/httpPost against its real
// URL — genuinely exercising net/http, not a mock — asserting the
// round-tripped response body is correct both ways.
func TestHTTPClientLive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			fmt.Fprint(w, "hello from upstream")
		case http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "echo: %s", b)
		}
	}))
	defer upstream.Close()

	g, err := compile.String(ioNetRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	getDeltas := postJSON(t, ts, "get", fmt.Sprintf(`{"args":[%q]}`, upstream.URL))
	if got := fmt.Sprint(getDeltas["result"]); got != "hello from upstream" {
		t.Fatalf("httpGet result = %q, want %q", got, "hello from upstream")
	}

	postDeltas := postJSON(t, ts, "post", fmt.Sprintf(`{"args":[%q,"ping"]}`, upstream.URL))
	if got := fmt.Sprint(postDeltas["result"]); got != "echo: ping" {
		t.Fatalf("httpPost result = %q, want %q", got, "echo: ping")
	}
}

// TestHTTPNonTwoXXIsCleanError proves a non-2xx upstream response is a clean
// runtime error (the documented behavior), not silently treated as success
// with the error page's body masquerading as real data.
func TestHTTPNonTwoXXIsCleanError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	g, err := compile.String(ioNetRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := postRaw(t, ts, "get", fmt.Sprintf(`{"args":[%q]}`, upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("non-2xx upstream should fail the request, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "503") {
		t.Errorf("error body should mention the upstream status code, got: %s", body)
	}
}

// TestHTTPNetworkFailureIsCleanError proves httpGet against an unreachable
// host fails with a clean runtime error, not a crash, and that the server
// keeps answering normally afterward — the same "handled error, not a taken-
// down process" proof runtime/array_test.go's TestArrayOutOfBoundsIsCleanError
// gives for an out-of-bounds array read.
func TestHTTPNetworkFailureIsCleanError(t *testing.T) {
	g, err := compile.String(ioNetRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Port 0 on localhost is never a listening service — a reliable, fast
	// connection-refused without depending on external network/DNS.
	resp, err := postRaw(t, ts, "get", `{"args":["http://127.0.0.1:0/nope"]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("unreachable host should fail the request, got %d", resp.StatusCode)
	}

	// The server must still be alive and answering normally afterward.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "still alive")
	}))
	defer upstream.Close()
	deltas := postJSON(t, ts, "get", fmt.Sprintf(`{"args":[%q]}`, upstream.URL))
	if got := fmt.Sprint(deltas["result"]); got != "still alive" {
		t.Fatalf("server did not recover cleanly after a network failure: got %q, want %q", got, "still alive")
	}
}
