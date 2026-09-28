package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A root left by a run that was killed is removed by the next run; the
// root of a run still alive, and a directory of any other shape, are not.
func TestTempRootSweepsOnlyDeadRuns(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	deadRoot := filepath.Join(base, fmt.Sprintf("leaktest-%d-123", dead.Process.Pid))
	liveRoot := filepath.Join(base, fmt.Sprintf("leaktest-%d-456", os.Getppid()))
	oldShape := filepath.Join(base, "leaktest-987654")
	for _, d := range []string{deadRoot, liveRoot, oldShape} {
		if err := os.MkdirAll(filepath.Join(d, "x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := testTempRoot("leaktest")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(root), fmt.Sprintf("leaktest-%d-", os.Getpid())) {
		t.Errorf("new root %s does not name this process", root)
	}
	if _, err := os.Stat(deadRoot); !os.IsNotExist(err) {
		t.Errorf("the dead run's root survived: %v", err)
	}
	for _, d := range []string{liveRoot, oldShape} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s was removed: %v", d, err)
		}
	}
}

// A child started with dieWithParent does not outlive the process that
// started it, however that process ends: here the parent is this test
// binary re-run as a helper that starts `sleep 60` and exits at once.
func TestDieWithParentTakesTheChildDown(t *testing.T) {
	if os.Getenv("DIE_WITH_PARENT_HELPER") == "1" {
		c := exec.Command("sleep", "60")
		dieWithParent(c)
		if err := c.Start(); err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(c.Process.Pid)
		os.Exit(0) // no Wait, no Kill: the parent simply goes
	}
	helper := exec.Command(os.Args[0], "-test.run", "^TestDieWithParentTakesTheChildDown$")
	helper.Env = append(os.Environ(), "DIE_WITH_PARENT_HELPER=1")
	out, err := helper.Output()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
	if err != nil {
		t.Fatalf("helper said %q", out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		// gone, or a zombie awaiting a reaper that is not us
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil && strings.Contains(string(stat), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the child %d outlived its parent", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
