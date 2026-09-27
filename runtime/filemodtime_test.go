package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"facet/internal/compile"
)

// fileModTime(path) answers a stored file's modification time in unix
// seconds — 0 for a file that does not exist — inside the io.file sandbox.
func TestFileModTimeBuiltin(t *testing.T) {
	g, err := compile.String(`app M:
    proc when(path: text) -> int uses io.file:
        return fileModTime(path)
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	srv.SetDataDir(dir)
	stamp := time.Unix(1_700_000_000, 0)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	got, err := srv.runProcLocked(srv.byProc["when"], []any{"a.txt"})
	if err != nil || toInt(got) != 1_700_000_000 {
		t.Fatalf("fileModTime(a.txt) = %v, %v; want 1700000000", got, err)
	}
	if got, err := srv.runProcLocked(srv.byProc["when"], []any{"missing.txt"}); err != nil || toInt(got) != 0 {
		t.Fatalf("fileModTime(missing) = %v, %v; want 0", got, err)
	}
	if _, err := srv.runProcLocked(srv.byProc["when"], []any{"../escape.txt"}); err == nil {
		t.Fatal("a path escaping the sandbox must be refused")
	}
}
