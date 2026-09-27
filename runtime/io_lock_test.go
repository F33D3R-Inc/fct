package runtime

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"facet/internal/compile"
)

// ioLockApp: lockFile, the exclusive advisory lock a single-writer data
// directory is claimed with.
const ioLockApp = `app L:
    proc claim(path: text) -> text uses io.file:
        let first = lockFile(path)
        let again = lockFile(path)
        return "" + first + "|" + again + "|" + fileExists(path)
    state result: text = ""
    action go(path: text):
        let out = do claim(path)
        result = out
    view Home at "/":
        text "{result}"
`

func TestLockFileBuiltin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACET_DATA_DIR", dir)
	g, err := compile.String(ioLockApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDataDir(dir)

	// Another process holds it: flock is per open file description, so a
	// second descriptor in this process stands in for another process.
	held := filepath.Join(dir, "taken.lock")
	other, err := os.OpenFile(held, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if got, err := srv.ioLockFile("taken.lock"); err != nil || got != false {
		t.Fatalf("a lock another descriptor holds: got %v, %v; want false", got, err)
	}
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	other.Close()

	// Free: taken, created if absent, and idempotent for this process.
	if got, err := srv.ioLockFile("taken.lock"); err != nil || got != true {
		t.Fatalf("a free lock: got %v, %v; want true", got, err)
	}
	if got, err := srv.ioLockFile("fresh.lock"); err != nil || got != true {
		t.Fatalf("a fresh lock file: got %v, %v; want true", got, err)
	}
	if got, err := srv.ioLockFile("fresh.lock"); err != nil || got != true {
		t.Fatalf("the same lock again in this process: got %v, %v; want true", got, err)
	}
	// Held: another descriptor cannot take it now.
	probe, err := os.OpenFile(filepath.Join(dir, "fresh.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != syscall.EWOULDBLOCK {
		t.Fatalf("the held lock from another descriptor: %v; want EWOULDBLOCK", err)
	}
	// Outside the sandbox: refused, not locked.
	if _, err := srv.ioLockFile("../escape.lock"); err == nil {
		t.Fatal("a path outside the data directory was locked")
	}

	// Through the language: the proc's answer.
	if got, err := srv.callProcBuiltin("lockFile", []any{"proc.lock"}); err != nil || got != true {
		t.Fatalf("lockFile via the builtin table: got %v, %v", got, err)
	}
}
