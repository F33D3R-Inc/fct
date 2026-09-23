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

func ioSleepMs(ms int) (any, error) {
	if ms < 0 || ms > maxSleepMs {
		return nil, fmt.Errorf("sleepMs: %d ms is out of range (must be 0-%d)", ms, maxSleepMs)
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	return true, nil
}

// ioNowMs implements nowMs(): the wall clock in milliseconds since the Unix
// epoch — now()'s instant at the resolution a timestamp on the wire (a
// heartbeat's, a telemetry batch's) is written in.
func ioNowMs() (any, error) {
	return int(time.Now().UnixMilli()), nil
}
