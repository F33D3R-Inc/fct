package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"facet/internal/compile"
)

// A fragment's expectation binds to the host's real declaration at runtime:
// the button its component draws invokes the host's action body, over the
// host's rows. Standalone, the fragment runs against its stand-ins.
func TestExpectBindsToHostAtRuntime(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("card.fct", `app Host:
    expect entity Stream:
        id: int
        title: text
        viewers: int
    expect action tip(stream: int, cents: money)
    component StreamCard(s: Stream):
        box:
            text "{s.title} {s.viewers}"
            button "Tip" -> tip(s.id, 500)
`)
	write("main.fct", `import "card.fct"

app Host:
    entity Stream:
        id: int
        owner: text
        title: text
        viewers: int
    entity Tip:
        id: int
        stream: Stream
        cents: money
    action tip(stream: int, cents: money):
        add Tip { stream: stream, cents: cents }
        set Stream(stream).viewers = Stream(stream).viewers + 1
    view Home at "/":
        for s in Stream:
            use StreamCard(s)
`)
	g, err := compile.File(filepath.Join(dir, "main.fct"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.AddRow("Stream", map[string]any{"owner": "ada", "title": "Ada live", "viewers": 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Run("bob", "member", true, "tip", []any{1, 500}); err != nil {
		t.Fatalf("tip through the host: %v", err)
	}
	if n := len(srv.EntityRows("Tip")); n != 1 {
		t.Errorf("the host's tip body must have run: Tip rows = %d, want 1", n)
	}
	if v := srv.EntityRows("Stream")[0].(record)["viewers"]; toInt(v) != 1 {
		t.Errorf("viewers = %v, want 1", v)
	}

	// Standalone: the fragment alone serves its stand-in entity and action.
	g2, err := compile.File(filepath.Join(dir, "card.fct"))
	if err != nil {
		t.Fatalf("fragment standalone: %v", err)
	}
	srv2, err := NewInMemory(g2)
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Shutdown()
	if _, err := srv2.AddRow("Stream", map[string]any{"title": "x", "viewers": 3}); err != nil {
		t.Fatalf("the stand-in entity must accept rows: %v", err)
	}
	if _, err := srv2.Run("bob", "member", true, "tip", []any{1, 500}); err != nil {
		t.Fatalf("the stand-in action must run (as a no-op): %v", err)
	}
	if n := len(srv2.EntityRows("Stream")); n != 1 {
		t.Errorf("Stream rows = %d, want 1", n)
	}
}
