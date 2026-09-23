package runtime

import "fmt"

// taskHandle is the runtime value a `let h = spawn ProcName(args)` statement
// binds into the spawning proc's frame (see proccompile.go's "spawn" case,
// there) — an opaque handle to a goroutine already running, joined exactly
// once by a later `join h` in the same block (internal/ir/build.go's
// checkSpawnsJoined proves that join exists at compile time).
//
// It is a pointer type never touched by cloneCompositeValue (which only
// special-cases []any/map[any]any — see its doc), so it flows through a
// frame binding by reference, as it must: joining a COPY of the handle would
// be meaningless, there is exactly one real goroutine and exactly one done
// channel to wait on.
type taskHandle struct {
	done   chan struct{} // closed when the goroutine finishes, however it finishes
	result any
	err    error
}

// spawnTask starts fn as a real goroutine (Stage 1's reference/bootstrap
// implementation — a genuine OS-scheduled Go goroutine, not a green thread or
// a scheduler of this runtime's own devising) and returns a handle for it
// immediately, without waiting. A panic inside fn — an out-of-bounds slip
// past every static check, a bad type assertion, anything — is recovered
// here and turned into an ordinary error on the handle, exactly like any
// other proc failure (a missing file, an out-of-bounds array read): it
// surfaces cleanly when the handle is joined, and it never takes down the
// process the way an unrecovered goroutine panic otherwise would.
func spawnTask(fn func() (any, error)) *taskHandle {
	h := &taskHandle{done: make(chan struct{})}
	go func() {
		defer close(h.done)
		defer func() {
			if r := recover(); r != nil {
				h.err = fmt.Errorf("spawned task panicked: %v", r)
			}
		}()
		h.result, h.err = fn()
	}()
	return h
}

// join blocks until h's goroutine finishes and returns what it produced.
// Safe to call exactly once per handle (internal/ir/build.go's
// checkSpawnsJoined enforces this statically — a handle is removed from
// pendingSpawns the moment it is joined, and any further reference to it,
// including a second join, is a compile error).
func (h *taskHandle) join() (any, error) {
	<-h.done
	return h.result, h.err
}
