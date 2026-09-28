package integration

import (
	"os/exec"
	"syscall"
)

// dieWithParent makes cmd's process end when the test process that started
// it does: the kernel sends it SIGKILL when its parent goes (Pdeathsig). A
// test's Cleanup kills its children on every path the test itself returns
// by — but not when the test binary is killed (a -timeout panic, SIGKILL,
// an interrupted run), which left engines running for hours, holding their
// data directories and the CPU. Call before cmd.Start.
func dieWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
