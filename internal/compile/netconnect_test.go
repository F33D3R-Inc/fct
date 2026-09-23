package compile

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestConnectCompilesInAnOrdinaryProc: connect/setTimeoutMs/connError and
// the connection I/O builtins are legal in a plain proc declaring io.net,
// and the connection builtins stay legal in an io.net.listen daemon.
func TestConnectCompilesInAnOrdinaryProc(t *testing.T) {
	g, err := File(filepath.Join("testdata", "netconnect", "ok.fct"))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 1 || len(g.Daemons) != 1 {
		t.Fatalf("want 1 proc and 1 daemon, got %d procs, %d daemons", len(g.Procs), len(g.Daemons))
	}
}

// TestConnectGates: each misuse is refused at compile time with a message
// naming what is wrong.
func TestConnectGates(t *testing.T) {
	cases := []struct {
		file string
		want []string
	}{
		{"connect_no_uses.fct", []string{`proc "dial" calls connect(...)`, `requires capability "io.net"`}},
		{"connect_file_cap.fct", []string{`proc "dial" calls connect(...)`, `requires capability "io.net"`}},
		{"read_no_uses.fct", []string{`proc "peek" calls readBytes(...)`, `"io.net" (or "io.net.listen" for an accepted connection)`}},
		{"connect_in_action.fct", []string{"connect(...) is only available inside a proc that declares `uses io.net`"}},
		{"listen_in_proc.fct", []string{"listen(...) is only available inside a daemon body"}},
		{"connect_arity.fct", []string{"connect(...) takes exactly two arguments"}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			_, err := File(filepath.Join("testdata", "netconnect", c.file))
			if err == nil {
				t.Fatal("want a compile error, got none")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q should contain %q", err.Error(), w)
				}
			}
		})
	}
}
