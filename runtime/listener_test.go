package runtime

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"facet/internal/compile"
)

func freeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// A failed bind is a failed listener handle, not an abort; closeListener
// releases the port and ends a parked accept with a failed connection.
func TestListenerLifecycle(t *testing.T) {
	g, err := compile.File("testdata/listener_lifecycle.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuf
	srv.SetStdio(strings.NewReader(""), &out, io.Discard)
	code, err := srv.RunMain([]string{freeTCPPort(t)})
	if err != nil || code != 0 {
		t.Fatalf("main: %d %v", code, err)
	}
	want := "first=\n" +
		"second=Address already in use (os error 98)\n" +
		"closeFailed=false\n" +
		"close=true\n" +
		"accepted:accept: the listener is closed\n" +
		"closeAgain=false\n" +
		"later:accept: the listener is closed\n" +
		"rebind=\n"
	if got := out.String(); got != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", got, want)
	}
}

func TestAcceptOnUnboundListenerAborts(t *testing.T) {
	g, err := compile.File("testdata/listener_unbound_accept.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	_, err = srv.RunMain([]string{freeTCPPort(t)})
	if err == nil || !strings.Contains(err.Error(), "never bound: Address already in use (os error 98)") {
		t.Fatalf("want an abort naming the failed bind, got %v", err)
	}
}

// Read grants: only a path the operator named (an argument or an
// environment variable's value) is granted, and only then readable outside
// the sandbox.
func TestGrantRead(t *testing.T) {
	outside := t.TempDir()
	named := filepath.Join(outside, "named.json")
	envNamed := filepath.Join(outside, "env.json")
	secret := filepath.Join(outside, "secret.json")
	for _, f := range []string{named, envNamed, secret} {
		if err := os.WriteFile(f, []byte("contents of "+filepath.Base(f)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GRANT_READ_TEST", envNamed)
	g, err := compile.File("testdata/grant_read.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDataDir(t.TempDir())
	var out syncBuf
	srv.SetStdio(strings.NewReader(""), &out, io.Discard)
	_, err = srv.RunMain([]string{named, secret})
	want := "computed=true\n" + // args[0] + "" is still exactly the argument
		"other=false\n" +
		"arg=true\n" +
		"read=contents of named.json\n" +
		"env=true\n" +
		"envread=true\n" +
		"empty=false\n"
	if got := out.String(); got != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", got, want)
	}
	// An argument that was never granted stays outside the sandbox.
	if err == nil || !strings.Contains(err.Error(), "escapes the sandboxed data directory") {
		t.Fatalf("an ungranted read outside the sandbox: %v", err)
	}
}
