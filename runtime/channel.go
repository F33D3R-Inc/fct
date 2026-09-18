package runtime

import (
	"fmt"
	"sync"
)

// channelRegistry backs the `channel()`/`send`/`recv` builtins — Milestone
// 5's minimal inter-task communication primitive. A channel value in the
// language is deliberately just an int handle (see internal/ir/build.go's
// inferProcType, "channel" case, for why that needed no new type anywhere in
// the compiler's type system: an int is already a legal proc parameter, let
// local, array element, and argument to another proc, so a channel handle
// gets all of that for free just by being one). Under the hood, that int
// indexes into a real buffered Go channel kept here.
//
// This has its own mutex, deliberately never s.mu (the durable-store lock
// runActionLocked/runProcLocked's callers hold for a whole action's/proc's
// synchronous execution — see runtime/server.go). send/recv block, sometimes
// for a long time (until some other goroutine, typically a spawned task,
// receives/sends), so they must never be reachable while holding a lock
// anything else needs — a channel is pure in-process, in-memory
// communication between goroutines, with no relation to the entity store at
// all, so it needs no relation to that lock either. This mutex only ever
// guards the registry's own bookkeeping (minting an id, looking one up), not
// the blocking send/receive itself, which happens on the looked-up Go
// channel after this mutex is already released.
type channelRegistry struct {
	mu    sync.Mutex
	next  int
	chans map[int]chan string
}

func newChannelRegistry() *channelRegistry {
	return &channelRegistry{chans: map[int]chan string{}}
}

// channelBufferSize is the one buffer size every channel gets — a small,
// fixed bound (LANGUAGE.md's "genuinely minimal" instruction for this
// milestone: no arbitrary-capacity CSP feature set, just enough buffering
// that a spawned sender doesn't have to rendezvous with its receiver on
// every single value).
const channelBufferSize = 1

// create mints a fresh channel and returns its handle.
func (r *channelRegistry) create() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	id := r.next
	r.chans[id] = make(chan string, channelBufferSize)
	return id
}

// lookup resolves a handle to its underlying Go channel, or an error naming
// the bad handle — the runtime backstop for a handle value that didn't
// actually come from channel() (a plain int an author passed by mistake),
// exactly the "clean error, not a panic" contract every other builtin in
// this codebase already has for a bad-but-not-statically-provable input.
func (r *channelRegistry) lookup(id int) (chan string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.chans[id]
	if !ok {
		return nil, fmt.Errorf("channel %d does not exist", id)
	}
	return ch, nil
}

// send blocks until value is delivered into ch's buffer (or a receiver takes
// it directly, once the buffer is full) — a real, blocking send, not a
// best-effort post. Returns true on success; the only failure is a bad
// handle.
func (r *channelRegistry) send(id int, value string) (any, error) {
	ch, err := r.lookup(id)
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	ch <- value
	return true, nil
}

// recv blocks until a value is available on ch and returns it. The only
// failure is a bad handle — an empty channel is not an error, it is simply a
// block, the entire point of a channel existing.
func (r *channelRegistry) recv(id int) (any, error) {
	ch, err := r.lookup(id)
	if err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	return <-ch, nil
}
