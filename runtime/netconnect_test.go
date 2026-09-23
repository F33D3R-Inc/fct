package runtime

import (
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

// netConnectServer compiles testdata/netconnect.fct and serves it.
func netConnectServer(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File("testdata/netconnect.fct")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// netConnectListen starts a real TCP listener on loopback whose every
// connection is handled by serve, and returns its port.
func netConnectListen(t *testing.T, serve func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestConnectRoundTripFromAnOrdinaryProc: a proc reached from an action
// dials a real listener with connect(), writes, and reads to EOF across
// several readBytes calls (the proc reads 7 bytes at a time) — and a clean
// close by the peer leaves connError empty.
func TestConnectRoundTripFromAnOrdinaryProc(t *testing.T) {
	port := netConnectListen(t, func(c net.Conn) {
		defer c.Close()
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		fmt.Fprintf(c, "you said %q, goodbye", buf[:n])
	})
	ts := netConnectServer(t)
	deltas := postJSON(t, ts, "echo", fmt.Sprintf(`{"args":["127.0.0.1",%d,"hello"]}`, port))
	want := `you said "hello", goodbye|true||true`
	if got := fmt.Sprint(deltas["result"]); got != want {
		t.Fatalf("result = %q, want %q", got, want)
	}
}

// TestConnectReadDeadlineIsAValue: a peer that accepts and then says
// nothing cannot hold an ordinary proc (or the action's store lock above
// it): setTimeoutMs bounds the read, readBytes answers [], and connError
// names the timeout — the action completes normally.
func TestConnectReadDeadlineIsAValue(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	port := netConnectListen(t, func(c net.Conn) {
		defer c.Close()
		<-hold
	})
	ts := netConnectServer(t)
	start := time.Now()
	deltas := postJSON(t, ts, "wait", fmt.Sprintf(`{"args":["127.0.0.1",%d,150]}`, port))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the read deadline did not bound the proc: took %v", elapsed)
	}
	want := "0|true|readBytes: timed out after 150ms"
	if got := fmt.Sprint(deltas["result"]); got != want {
		t.Fatalf("result = %q, want %q", got, want)
	}
}

// TestConnectRefusedIsAValue: a dial nobody answers still mints a handle;
// writeBytes answers false, readBytes answers [], and connError says the
// connect failed — no abort, so a client can report "unreachable" as data.
func TestConnectRefusedIsAValue(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens here now
	ts := netConnectServer(t)
	deltas := postJSON(t, ts, "dialDead", fmt.Sprintf(`{"args":["127.0.0.1",%d]}`, port))
	got := fmt.Sprint(deltas["result"])
	prefix := fmt.Sprintf("false|0|connect 127.0.0.1:%d: ", port)
	if !strings.HasPrefix(got, prefix) || !strings.Contains(got, "refused") {
		t.Fatalf("result = %q, want prefix %q naming a refused connection", got, prefix)
	}
}

// TestConnectBadPortAborts: a port out of range is a programming error, not
// a transport condition, and fails the request cleanly.
func TestConnectBadPortAborts(t *testing.T) {
	ts := netConnectServer(t)
	resp, err := postRaw(t, ts, "dialDead", `{"args":["127.0.0.1",70000]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 400 || !strings.Contains(string(b), "invalid port 70000") {
		t.Fatalf("status %d body %s: want a clean failure naming the port", resp.StatusCode, b)
	}
}

// TestPollBytesInterleavesWithoutFailing: pollBytes on a connection whose
// peer answers late returns [] without recording a failure, collects the
// answer as it arrives, and connOpen turns false when the peer closes.
func TestPollBytesInterleavesWithoutFailing(t *testing.T) {
	port := netConnectListen(t, func(c net.Conn) {
		defer c.Close()
		buf := make([]byte, 2)
		io.ReadFull(c, buf)
		time.Sleep(150 * time.Millisecond)
		c.Write([]byte("late "))
		time.Sleep(50 * time.Millisecond)
		c.Write([]byte("answer"))
	})
	ts := netConnectServer(t)
	deltas := postJSON(t, ts, "poll", fmt.Sprintf(`{"args":["127.0.0.1",%d]}`, port))
	want := "0||late answer|true|"
	if got := fmt.Sprint(deltas["result"]); got != want {
		t.Fatalf("result = %q, want %q", got, want)
	}
}
