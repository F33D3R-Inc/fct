package runtime

import (
	"fmt"
	"sync"
	"time"
)

// channelRegistry backs the `channel()`/`send`/`recv` builtins — Milestone
// 5's minimal inter-task communication primitive. A channel value in the
// language is deliberately just an int handle (see internal/ir/build.go's
// inferProcType, "channel" case, for why that needed no new type anywhere in
// the compiler's type system: an int is already a legal proc parameter, let
// local, array element, and argument to another proc, so a channel handle
// gets all of that for free just by being one). Under the hood, that int
// indexes into a bounded queue kept here (fctChan).
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
// the blocking send/receive itself, which waits on the looked-up
// channel's own lock after this mutex is already released.
type channelRegistry struct {
	mu    sync.Mutex
	next  int
	chans map[int]*fctChan
}

// fctChan is one channel: a bounded queue (channelBufferSize) under its own
// mutex, rather than a Go chan, so that awaitAny can ask "is a value
// waiting?" of several channels at once without taking the value, and so
// that closeChannel can release every task blocked on the channel without
// the panic a Go close would cause a blocked sender.
type fctChan struct {
	mu       sync.Mutex
	cond     *sync.Cond
	queue    []string
	closed   bool
	watchers map[chan struct{}]bool // awaitAny calls waiting on this channel
}

func newChannelRegistry() *channelRegistry {
	return &channelRegistry{chans: map[int]*fctChan{}}
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
	c := &fctChan{watchers: map[chan struct{}]bool{}}
	c.cond = sync.NewCond(&c.mu)
	r.chans[id] = c
	return id
}

// lookup resolves a handle. A handle that was minted and then closed
// resolves to (nil, true, nil) — closed is a state, not a mistake; a value
// that never came from channel() is an error naming it, the runtime backstop
// every other builtin has for a bad-but-not-statically-provable input.
func (r *channelRegistry) lookup(id int) (*fctChan, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.chans[id]
	if ok {
		return c, false, nil
	}
	if id >= 1 && id <= r.next {
		return nil, true, nil
	}
	return nil, false, fmt.Errorf("channel %d does not exist", id)
}

// notify wakes every awaitAny watching c. c.mu is held.
func (c *fctChan) notify() {
	for w := range c.watchers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// send blocks until value is in ch's buffer. It answers true once it is,
// and false when the channel is (or becomes, while this send waits) closed:
// a receiver that has gone away is a normal event, not a failure of the
// sender. A handle that never existed is an error.
func (r *channelRegistry) send(id int, value string) (any, error) {
	c, closed, err := r.lookup(id)
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	if closed {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.queue) >= channelBufferSize && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		return false, nil
	}
	c.queue = append(c.queue, value)
	c.cond.Broadcast()
	c.notify()
	return true, nil
}

// recv blocks until a value is available on ch and returns it. An empty
// channel is not an error, it is simply a block; a closed one is — there is
// nothing left it could ever deliver.
func (r *channelRegistry) recv(id int) (any, error) {
	c, closed, err := r.lookup(id)
	if err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	if closed {
		return nil, fmt.Errorf("recv: channel %d is closed", id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.queue) == 0 && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		return nil, fmt.Errorf("recv: channel %d is closed", id)
	}
	v := c.queue[0]
	c.queue = c.queue[1:]
	c.cond.Broadcast()
	return v, nil
}

// close implements `closeChannel(ch) -> bool`: the handle is released —
// every send blocked on it, and every later one, answers false; every recv
// fails; what was buffered is discarded. True when this call closed it,
// false when it was already closed.
func (r *channelRegistry) close(id int) (any, error) {
	r.mu.Lock()
	c, ok := r.chans[id]
	if !ok {
		minted := id >= 1 && id <= r.next
		r.mu.Unlock()
		if minted {
			return false, nil
		}
		return nil, fmt.Errorf("closeChannel: channel %d does not exist", id)
	}
	delete(r.chans, id)
	r.mu.Unlock()
	c.mu.Lock()
	c.closed = true
	c.queue = nil
	c.cond.Broadcast()
	c.notify()
	c.mu.Unlock()
	return true, nil
}

// awaitAny implements `awaitAny(chans: [int], ms: int) -> int`: it waits
// until one of the channels has a value to receive (or is closed) and
// answers that channel's position in chans — the first such, in order —
// without taking the value; the next recv on it does. -1 when ms
// milliseconds pass first; ms 0 only looks, a negative ms waits for as long
// as it takes. It is how a task waits on several sources at once, or on
// one with a deadline. A value it reports can still be taken by another
// receiver of the same channel first, so the guarantee is a sole
// consumer's.
func (r *channelRegistry) awaitAny(ids []int, ms int) (any, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("awaitAny: no channels to wait on")
	}
	chans := make([]*fctChan, len(ids))
	for i, id := range ids {
		c, closed, err := r.lookup(id)
		if err != nil {
			return nil, fmt.Errorf("awaitAny: %w", err)
		}
		if closed {
			return i, nil
		}
		chans[i] = c
	}
	w := make(chan struct{}, 1)
	for _, c := range chans {
		c.mu.Lock()
		c.watchers[w] = true
		c.mu.Unlock()
	}
	defer func() {
		for _, c := range chans {
			c.mu.Lock()
			delete(c.watchers, w)
			c.mu.Unlock()
		}
	}()
	var timeout <-chan time.Time
	if ms >= 0 {
		t := time.NewTimer(time.Duration(ms) * time.Millisecond)
		defer t.Stop()
		timeout = t.C
	}
	for {
		for i, c := range chans {
			c.mu.Lock()
			ready := len(c.queue) > 0 || c.closed
			c.mu.Unlock()
			if ready {
				return i, nil
			}
		}
		select {
		case <-w:
		case <-timeout:
			return -1, nil
		}
	}
}

// intListArg reads a builtin's [int] argument.
func intListArg(name string, v any) ([]int, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected a list of channel handles", name)
	}
	out := make([]int, len(arr))
	for i, x := range arr {
		out[i] = toInt(x)
	}
	return out, nil
}
