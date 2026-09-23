package runtime

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

// detachSharedServer compiles testdata/detach_shared.fct and serves its HTTP
// API; the daemon is not started until the caller asks.
func detachSharedServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	g, err := compile.File("testdata/detach_shared.fct")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// detachSharedFreePort finds a loopback port nothing is listening on.
func detachSharedFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// detachSharedDial connects to the daemon, waiting for it to start listening.
func detachSharedDial(t *testing.T, port int) net.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			t.Cleanup(func() { c.Close() })
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never listened on %d: %v", port, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func detachSharedAsk(t *testing.T, c net.Conn, r *bufio.Reader, line string) string {
	t.Helper()
	if _, err := io.WriteString(c, line+"\n"); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("no answer to %q: %v", line, err)
	}
	return strings.TrimSuffix(got, "\n")
}

// TestDetachServesConnectionsConcurrently: a daemon accept loop that detaches
// each connection's handler serves a second client while the first is still
// open and silent — with one-at-a-time serving the second would wait for the
// first to close. And a `shared` cell published by an action (through a proc)
// is what every handler reads, live: the already-open first connection sees
// the new value.
func TestDetachServesConnectionsConcurrently(t *testing.T) {
	srv, ts := detachSharedServer(t)
	port := detachSharedFreePort(t)
	postJSON(t, ts, "setup", fmt.Sprintf(`{"args":[%d,"hello"]}`, port))
	srv.StartJobs()

	first := detachSharedDial(t, port)
	firstR := bufio.NewReader(first)
	second := detachSharedDial(t, port)
	secondR := bufio.NewReader(second)

	if got := detachSharedAsk(t, second, secondR, "two"); got != "hello two" {
		t.Fatalf("second connection = %q, want %q", got, "hello two")
	}

	postJSON(t, ts, "publish", `{"args":["hi"]}`)
	if got := detachSharedAsk(t, first, firstR, "one"); got != "hi one" {
		t.Fatalf("first connection = %q, want %q", got, "hi one")
	}
	deltas := postJSON(t, ts, "peek", `{"args":[]}`)
	if got := fmt.Sprint(deltas["result"]); got != "hi" {
		t.Fatalf("peek = %q, want %q", got, "hi")
	}
}

// TestSharedReadBeforeAssignIsAnError: a shared cell has no default — reading
// one nothing has assigned fails the action, naming the cell.
func TestSharedReadBeforeAssignIsAnError(t *testing.T) {
	_, ts := detachSharedServer(t)
	resp, err := http.Post(ts.URL+"/api/peek", "application/json", strings.NewReader(`{"args":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK || !strings.Contains(string(body), `shared cell "greeting" was read before anything assigned it`) {
		t.Fatalf("peek before setup = %d %s", resp.StatusCode, body)
	}
}

// TestShutdownConnWakesAParkedReader: a spawned task parked in readBytes on a
// quiet connection is woken by shutdownConn from its parent — a clean EOF,
// no transport error — and the handle is still valid for the parent to close.
func TestShutdownConnWakesAParkedReader(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	port := netConnectListen(t, func(c net.Conn) {
		defer c.Close()
		io.WriteString(c, "abc")
		<-hold
	})
	_, ts := detachSharedServer(t)
	deltas := postJSON(t, ts, "stop", fmt.Sprintf(`{"args":["127.0.0.1",%d]}`, port))
	if got, want := fmt.Sprint(deltas["result"]), "true|3||true"; got != want {
		t.Fatalf("result = %q, want %q", got, want)
	}
}

// TestSleepMsIsMeasuredOnMonoMs: sleepMs pauses at least as long as asked,
// as the monotonic clock sees it.
func TestSleepMsIsMeasuredOnMonoMs(t *testing.T) {
	_, ts := detachSharedServer(t)
	deltas := postJSON(t, ts, "clock", `{"args":[30]}`)
	if got, want := fmt.Sprint(deltas["result"]), "true|true"; got != want {
		t.Fatalf("result = %q, want %q", got, want)
	}
}

// TestDetachAndSharedAreChecked: detach outside a daemon, a local reusing a
// shared cell's name, a mistyped assignment, an unknown cell type, and a
// shared cell named from an action are compile errors.
func TestDetachAndSharedAreChecked(t *testing.T) {
	for file, want := range map[string]string{
		"detach_in_proc_bad.fct":   "detach is only valid inside a daemon body",
		"shared_shadow_bad.fct":    `"total" is a shared cell — a parameter or local may not reuse its name`,
		"shared_type_bad.fct":      `cannot assign text to shared cell "total", whose type is int`,
		"shared_unknown_bad.fct":   `shared cell "fleet" has unknown type "Fleet"`,
		"shared_in_action_bad.fct": `unknown reference "total"`,
	} {
		_, err := compile.File("testdata/" + file)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to mention %q", file, err, want)
		}
	}
}
