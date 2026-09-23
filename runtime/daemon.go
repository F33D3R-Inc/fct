package runtime

// Daemons — detached, process-lifetime background tasks (see ast.Daemon's doc
// for the full language-level design). A daemon is deliberately NOT a Job with
// the join requirement dropped:
//
//   - A `job every Ns` is fleet-wide exactly-once (runtime/jobs.go's
//     ReserveCron: one instance wins each tick) and its "body" is a reference
//     to one already-declared zero-argument action. That is the right shape
//     for scheduled work a fleet should only do once (a nightly digest), and
//     the wrong shape for a background service every instance needs its own
//     copy of (a listener, a local worker) or for anything that doesn't fit
//     "call this one action every N seconds" (an unbounded accept loop).
//   - A daemon runs on THIS instance, unconditionally, with no cross-instance
//     coordination at all, and its Body is real imperative code — the same
//     statement vocabulary a `proc` has (loop/if/spawn/join/I-O), plus `act
//     ActionName(args)` (ir.Stmt's "actcall" op — see proccompile.go's case)
//     for touching entity/session state.
//
// A daemon is never joined — there is no handle for anything to hold. If its
// Body ever returns or panics, this file logs it and that one goroutine ends;
// nothing else in the process is affected, and nothing supervises it back to
// life (no restart policy — a real supervisor, if fabric-daemon ever needs
// one, is future work, not something to fake here).
import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"facet/internal/ir"
)

// startDaemons launches every declared daemon in its own goroutine and
// returns immediately — called from StartJobs, at the same boot point an
// `on start` Job runs, so a daemon's own state (if any, via `act`) is seeded
// before the rest of startup relies on it, exactly like an on-start Job's
// existing contract.
func (s *Server) startDaemons() {
	for i := range s.ir.Daemons {
		d := &s.ir.Daemons[i]
		if d.Every > 0 {
			go s.runDaemonTicking(d)
		} else {
			go s.runDaemonOnce(d)
		}
	}
}

// RunDaemons runs a program that is a service rather than a command — one
// with daemons and no `proc main` (`facet exec server.fct`): its `on start`
// jobs run first, inline and in order, exactly as StartJobs runs them before
// a server's daemons (so a daemon may rely on the state they set up — a
// listener's configuration, a published snapshot); then every daemon is
// started as startDaemons starts it, and the call returns only when all of
// them have ended (a ticking daemon never does), so the process lives as
// long as its service does.
func (s *Server) RunDaemons() error {
	if len(s.ir.Daemons) == 0 {
		return fmt.Errorf("%s has no daemon to run", s.ir.App)
	}
	s.runOnStartJobs()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for i := range s.ir.Daemons {
		d := &s.ir.Daemons[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.Every > 0 {
				s.runDaemonTicking(d)
				return
			}
			if err := s.runDaemonBody(d); err != nil {
				mu.Lock()
				if first == nil {
					first = fmt.Errorf("daemon %s: %w", d.Name, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// A service whose daemon failed ends in failure: a command's exit
	// status is how whatever started it learns why it stopped.
	return first
}

// HasMain reports whether the program declares `proc main` (a command).
func (s *Server) HasMain() bool { return s.byProc["main"] != nil }

// runDaemonTicking runs d.Body once per Every-second tick, forever, on this
// instance alone (no ReserveCron — see this file's doc for why that dedup is
// wrong here). Each tick gets a fresh frame: no local variable survives from
// one tick to the next, only whatever an `act` call persisted to the store —
// the same "each firing is independent" shape runtime/jobs.go's periodic
// jobs already have. A tick that fails is logged and simply skipped; the
// daemon itself keeps running and tries again next tick, so one bad
// iteration (a transient action failure) can't permanently kill a background
// service the rest of the app depends on.
func (s *Server) runDaemonTicking(d *ir.Daemon) {
	t := time.NewTicker(time.Duration(d.Every) * time.Second)
	defer t.Stop()
	for range t.C {
		s.runDaemonBody(d)
	}
}

// runDaemonOnce runs d.Body exactly once, in this one goroutine, for the life
// of the process — the shape a body with its own internal `loop true:` wants
// (a socket accept loop, a queue-drain loop, anything that blocks on an
// external event rather than a clock). If Body ever falls off the end or
// returns, this daemon's goroutine simply ends.
func (s *Server) runDaemonOnce(d *ir.Daemon) {
	s.runDaemonBody(d)
}

// runDaemonBody is one execution of a daemon's body: a fresh top-level frame
// (mirroring runProcLocked's own), panic-safe exactly like spawnTask's own
// recovery (a bug in a daemon must not take the whole process down with it —
// there is no caller here to propagate a panic to), and its error, if any,
// only logged: nothing is waiting on a daemon's result the way a `join`
// waits on a spawned task's, so there is nowhere else for a failure to go.
func (s *Server) runDaemonBody(d *ir.Daemon) (failed error) {
	defer func() {
		if r := recover(); r != nil {
			s.obs.log.Error("daemon panicked", slog.String("daemon", d.Name), slog.Any("panic", r))
			failed = fmt.Errorf("panicked: %v", r)
		}
	}()
	pc := s.codeFor(d, nil, d.Body)
	fr := pc.newFrame()
	_, err := runStmts(pc.body, fr)
	pc.release(fr)
	if err != nil {
		s.obs.log.Error("daemon failed", slog.String("daemon", d.Name), slog.Any("error", err))
	}
	return err
}

// detachProc implements `detach ProcName(args)` (see ast.Detach's doc; the
// statement lowers to the "$detach" intrinsic): it starts the named proc on
// its own goroutine and returns at once. Only a daemon body can reach this
// (internal/ir/build.go refuses detach anywhere else), so the task it starts
// shares the daemon's process lifetime and, like the daemon's own body, runs
// under no request's lock. Nothing joins it, so a failure or a panic is
// logged here — the same treatment runDaemonBody gives a daemon's — and ends
// only that task.
//
// args is copied: the caller's argument slice may be a stack buffer that is
// reused the moment this returns.
func (s *Server) detachProc(name string, args []any) (any, error) {
	p := s.byProc[name]
	if p == nil {
		return nil, fmt.Errorf("detach calls unknown proc %q", name)
	}
	vals := append([]any(nil), args...)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.obs.log.Error("detached proc panicked", slog.String("proc", name), slog.Any("panic", r))
			}
		}()
		if _, err := s.runProcLocked(p, vals); err != nil {
			s.obs.log.Error("detached proc failed", slog.String("proc", name), slog.Any("error", err))
		}
	}()
	return true, nil
}
