package runtime

import (
	"fmt"
	"time"
)

// sleepMs(ms) and monoMs() — the two primitives a bounded retry loop in a
// proc needs (wait a little; measure how long it has been waiting). Both are
// proc-only (internal/ir/build.go's concurrencyBuiltins): a pause belongs
// to a task, never to a view or an action expression.

// monoEpoch anchors monoMs(): milliseconds on the monotonic clock since this
// process started — the clock a retry budget or a timeout is measured on,
// which (unlike now()) never steps when the wall clock is adjusted.
var monoEpoch = time.Now()

// maxSleepMs bounds sleepMs(ms): ten minutes, the same ceiling setTimeoutMs
// has, so a proc that sleeps is still bounded.
const maxSleepMs = 600_000

func ioMonoMs() (any, error) {
	return int(time.Since(monoEpoch).Milliseconds()), nil
}

func (s *Server) ioSleepMs(ms int) (any, error) {
	if ms < 0 || ms > maxSleepMs {
		return nil, fmt.Errorf("sleepMs: %d ms is out of range (must be 0-%d)", ms, maxSleepMs)
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return true, nil
	case <-s.halted:
		return nil, errProgramHalted
	}
}

// errProgramHalted ends a task that sleeps once its server has shut down.
var errProgramHalted = fmt.Errorf("the program has been shut down")

// haltProgram ends what a program running on this server left running
// (Shutdown): a daemon or detached task blocked in accept, a read, recv or
// sleepMs returns — every listener and connection closed, every channel
// closed, every sleep cut short — and each task, failing, ends. Without it
// a host that shut a program down (a test, `facet exec` embedders) kept its
// daemons serving, its tickers ticking and its sockets open for the rest of
// the process.
func (s *Server) haltProgram() {
	s.haltOnce.Do(func() {
		close(s.halted)
		s.channels.closeAll()
		s.netConns.closeAll()
	})
}

// ioNowMs implements nowMs(): the wall clock in milliseconds since the Unix
// epoch — now()'s instant at the resolution a timestamp on the wire (a
// heartbeat's, a telemetry batch's) is written in.
func ioNowMs() (any, error) {
	return int(time.Now().UnixMilli()), nil
}
