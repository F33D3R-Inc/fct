package runtime

import (
	"fmt"
	"sync"
)

// sharedCells backs `shared name: Type` declarations (see ast.Shared's doc):
// one process-local, in-memory value per declared cell, which every proc and
// daemon body of this instance reads and replaces by name. A proc body's read
// is lowered to the "$shared.get" intrinsic and an assignment to
// "$shared.set" (internal/ir/build.go's lowerSharedRefs/procBlock), both
// dispatched from callProcBuiltin.
//
// This is the language's Arc<RwLock<T>>: a read is one consistent snapshot
// of the whole value, an assignment replaces the whole value, and neither
// blocks on anything but this registry's own lock — never s.mu, the durable
// store's, so a daemon's concurrently running handlers can read the cell
// while an action is mid-flight, and an action (via a proc it calls) can
// publish a new value while handlers are serving.
//
// Sharing a value between goroutines is safe for the same reason spawn's
// arguments are: the value read out is never owned by the reading frame (a
// builtin call's result is not "fresh" — see proccompile.go's fresh), so a
// frame that mutates what it read copies first, and the setter's own frame
// gave up ownership of what it stored when it passed it as an argument.
type sharedCells struct {
	mu   sync.RWMutex
	vals map[string]any
}

func newSharedCells() *sharedCells {
	return &sharedCells{vals: map[string]any{}}
}

// get is one snapshot of cell name. Reading a cell nothing has assigned is an
// error naming it — a shared cell has no default, so "not yet published" can
// never be mistaken for an empty value.
func (c *sharedCells) get(name string) (any, error) {
	c.mu.RLock()
	v, ok := c.vals[name]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("shared cell %q was read before anything assigned it", name)
	}
	return v, nil
}

// set replaces cell name's whole value.
func (c *sharedCells) set(name string, v any) {
	c.mu.Lock()
	c.vals[name] = v
	c.mu.Unlock()
}
