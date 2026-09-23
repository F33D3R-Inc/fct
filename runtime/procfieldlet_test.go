package runtime

import (
	"strings"
	"testing"

	"facet/internal/compile"
)

// A `let mut` local bound from a struct FIELD read (`let mut inner =
// outer.inner`, including a chain through a proc parameter) carries that
// field's declared struct type, so it can be written field by field and
// then stored back — the one-level-deep field-write rule then composes into
// nested updates. The copy must not alias the value it was read from.
// testdata/procfieldlet.fct holds the app.
func TestProcFieldReadLocalsAreWritableStructs(t *testing.T) {
	g, err := compile.File("testdata/procfieldlet.fct")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.Run("ada", "member", true, "go", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := srv.StateValue("got"), "2t 12t! 99"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The field type is enforced on the new local too: a text field read into a
// local cannot then be field-written as if it were a struct
// (testdata/procfieldlet_bad.fct is the same app with that one write added).
func TestProcFieldReadLocalKeepsFieldType(t *testing.T) {
	_, err := compile.File("testdata/procfieldlet_bad.fct")
	if err == nil || !strings.Contains(err.Error(), `"s" is not a struct (its type is text)`) {
		t.Fatalf("want a not-a-struct error naming text, got %v", err)
	}
}
