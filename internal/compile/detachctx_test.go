package compile

import (
	"strings"
	"testing"
)

// A proc only ever detached from a daemon-context body is one itself: it
// may accept, listen and detach (internal/ir/detachctx.go).
func TestDetachedProcIsDaemonContext(t *testing.T) {
	if _, err := File("testdata/detachctx/nested.fct"); err != nil {
		t.Fatalf("a chain of detaches from a daemon: %v", err)
	}
}

// One ordinary reference makes it an ordinary proc again, and the
// diagnostic names that reference.
func TestDetachedProcAlsoCalledIsNot(t *testing.T) {
	_, err := File("testdata/detachctx/also_called.fct")
	if err == nil {
		t.Fatal("a proc also run by `do` from an action compiled with accept in it")
	}
	for _, want := range []string{"accept(...) is only available inside a daemon body", `proc "acceptLoop" is started by ` + "`detach`", "`do acceptLoop(...)` at line", "the app's Actions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic %q lacks %q", err, want)
		}
	}
}

// grantRead is refused anywhere but `proc main` (runtime/grants.go).
func TestGrantReadOnlyInMain(t *testing.T) {
	_, err := File("testdata/detachctx/grant_outside_main.fct")
	if err == nil || !strings.Contains(err.Error(), "grantRead(...) is only available in `proc main") {
		t.Fatalf("want grantRead refused outside main, got %v", err)
	}
}
