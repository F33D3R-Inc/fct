package compile

import (
	"path/filepath"
	"strings"
	"testing"
)

// A private proc called inline in an expression (not through `do`) resolves
// to its own file's declaration after mangling, and two files may each keep
// a private helper of the same name.
func TestPrivateProcInlineCalls(t *testing.T) {
	g, err := File(filepath.Join("testdata", "private_inline", "main.fct"))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	twice := 0
	for _, p := range g.Procs {
		if strings.HasPrefix(p.Name, "twice~") {
			twice++
		}
		if p.Name == "twice" {
			t.Fatalf("private proc kept its bare name: %+v", p.Name)
		}
	}
	if twice != 2 {
		t.Fatalf("want two distinct mangled `twice` procs, got %d", twice)
	}
}
