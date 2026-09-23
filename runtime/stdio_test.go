package runtime

import (
	"bytes"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"facet/internal/compile"
)

func TestRunMainWithStdio(t *testing.T) {
	g, err := compile.File("testdata/stdio_main.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	srv.SetStdio(strings.NewReader("piped"), &out, &errOut)
	code, err := srv.RunMain([]string{"a", "b c"})
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := out.String(); got != "arg 0=a\narg 1=b c\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := errOut.String(); got != "stdin:piped\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestStdioNeedsConsoleCapability(t *testing.T) {
	_, err := compile.File("testdata/stdio_uncapable.fct")
	if err == nil || !strings.Contains(err.Error(), "io.console") {
		t.Fatalf("want a missing io.console capability error, got %v", err)
	}
}

func TestRunMainWithoutMain(t *testing.T) {
	g, err := compile.File("testdata/netconnect.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RunMain(nil); err == nil || !strings.Contains(err.Error(), "proc main") {
		t.Fatalf("want a missing-main error, got %v", err)
	}
}

// TestRunMainServes: `main` is a daemon-context body — it may listen, accept
// and detach — so a command can be a server.
func TestRunMainServes(t *testing.T) {
	g, err := compile.File("testdata/stdio_server.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	var out stdioSyncBuffer
	srv.SetStdio(strings.NewReader(""), &out, io.Discard)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := srv.RunMain([]string{strconv.Itoa(port)})
		done <- result{code, err}
	}()
	var conns []net.Conn
	for i := 0; i < 2; i++ {
		var c net.Conn
		deadline := time.Now().Add(5 * time.Second)
		for {
			c, err = net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	// Both are served concurrently: the second answers before the first writes.
	for i := len(conns) - 1; i >= 0; i-- {
		msg := "hello " + strconv.Itoa(i)
		if _, err := conns[i].Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		conns[i].SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conns[i].Read(buf)
		if err != nil || string(buf[:n]) != msg {
			t.Fatalf("echo %d = %q, %v", i, buf[:n], err)
		}
		conns[i].Close()
	}
	select {
	case r := <-done:
		if r.err != nil || r.code != 0 {
			t.Fatalf("main returned %d, %v", r.code, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("main never returned")
	}
	if got := out.String(); got != "listening\nserved served\n" {
		t.Fatalf("stdout = %q", got)
	}
}

// stdioSyncBuffer is a bytes.Buffer safe for the concurrent writes above.
type stdioSyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *stdioSyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *stdioSyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRunMainSignalsAndWallClock: signals() delivers SIGTERM to the program
// instead of ending the process, and nowMs() is the wall clock in ms.
func TestRunMainSignalsAndWallClock(t *testing.T) {
	g, err := compile.File("testdata/stdio_signals.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	var out stdioSyncBuffer
	srv.SetStdio(strings.NewReader(""), &out, io.Discard)
	start := time.Now().UnixMilli()
	done := make(chan int, 1)
	go func() {
		code, err := srv.RunMain(nil)
		if err != nil {
			t.Error(err)
		}
		done <- code
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "ready") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d, stdout %q", code, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the signal never arrived")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	parts := strings.Fields(lines[len(lines)-1])
	if len(parts) != 2 || parts[0] != "SIGTERM" {
		t.Fatalf("stdout %q", out.String())
	}
	ms, _ := strconv.Atoi(parts[1])
	if ms < int(start)-1000 || ms > int(time.Now().UnixMilli())+1000 {
		t.Fatalf("nowMs %d is not the wall clock (%d)", ms, start)
	}
}
