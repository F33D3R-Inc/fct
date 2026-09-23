package runtime

import (
	"bytes"
	"strings"
	"testing"

	"facet/internal/compile"
)

func TestNestedFieldAndIndexWrites(t *testing.T) {
	g, err := compile.File("testdata/nestedwrite.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	srv.SetStdio(strings.NewReader(""), &out, &out)
	if code, err := srv.RunMain(nil); err != nil || code != 0 {
		t.Fatalf("main: %d, %v", code, err)
	}
	if got := out.String(); got != "42 7 60 1 1 6\n" {
		t.Fatalf("stdout %q", got)
	}
}

func TestNestedWriteNeedsMutableRoot(t *testing.T) {
	_, err := compile.File("testdata/nestedwrite_immutable.fct")
	if err == nil || !strings.Contains(err.Error(), "not mutable") {
		t.Fatalf("want a `let mut` error, got %v", err)
	}
}
