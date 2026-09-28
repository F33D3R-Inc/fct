package selfhost

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// testTempRoot makes "<prefix>-<pid>-<random>" in the system temp
// directory, after removing every earlier "<prefix>-<pid>-…" whose process
// no longer exists — the roots of runs that were killed before their
// TestMain could remove them.
func testTempRoot(prefix string) (string, error) {
	base := os.TempDir()
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			rest, ok := strings.CutPrefix(e.Name(), prefix+"-")
			if !ok || !e.IsDir() {
				continue
			}
			// only "<pid>-<random>": a name of another shape is not one
			// this function made, and is never touched
			pidText, _, twoParts := strings.Cut(rest, "-")
			if !twoParts {
				continue
			}
			pid, err := strconv.Atoi(pidText)
			if err != nil || pid == os.Getpid() || processAlive(pid) {
				continue
			}
			os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
	return os.MkdirTemp(base, fmt.Sprintf("%s-%d-", prefix, os.Getpid()))
}

// processAlive: signal 0 reaches it (EPERM means it exists, owned by
// someone else).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
