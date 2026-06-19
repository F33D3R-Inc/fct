package runtime

// Durable jobs — background work that survives a restart and runs exactly once
// across a fleet. The in-memory ticker of v1.2.0 lost scheduled work on a crash
// and double-ran it on every instance; this replaces it with a persistent queue
// in Postgres:
//
//   - Every scheduled `every Ns` job becomes a cron entry; on each interval one
//     instance (whoever wins ReserveCron) enqueues a row, so the job fires once
//     fleet-wide, not once per instance.
//   - Workers lease due rows with FOR UPDATE SKIP LOCKED — many workers drain the
//     queue without ever running the same job twice.
//   - A failed job is retried with exponential backoff; once it exhausts its
//     attempts it is dead-lettered (status='dead', kept for inspection) instead of
//     looping forever.
//
// `on start` jobs still run inline at boot (they seed state the rest of startup
// may read); everything periodic flows through the durable queue.

import (
	"net/http"
	"time"

	"log/slog"

	"facet/internal/ir"
)

const (
	systemSID      = "__system"
	jobPollEvery   = 1 * time.Second
	jobBackoffBase = 5 * time.Second
	jobBackoffCap  = 10 * time.Minute
	defaultWorkers = 2
)

// jobQueue runs this instance's workers and cron schedulers against the shared
// durable queue.
type jobQueue struct {
	srv  *Server
	stop chan struct{}
}

// StartJobs launches the app's background work. Each `on start` job runs once now
// (inline, under a synthetic system/admin session). Each `every Ns` job is driven
// by the durable queue: a cron scheduler enqueues it fleet-wide and a pool of
// workers executes it. Returns immediately; workers and schedulers run in the
// background.
func (s *Server) StartJobs() {
	s.mu.Lock()
	if s.sessions[systemSID] == nil {
		sys := s.newSession("system", roleAdmin)
		sys.verified = true
		s.sessions[systemSID] = sys
	}
	s.mu.Unlock()

	// Phase 6: the declarative retention sweep (no-op unless FACET_RETENTION is set)
	// runs for the process lifetime, independent of the periodic-job machinery.
	s.startRetention()

	// `on start` jobs run inline, in declared order, before periodic work begins.
	for _, j := range s.ir.Jobs {
		if !j.OnStart {
			continue
		}
		if act := s.byAction[j.Action]; act != nil {
			s.runAction(systemSID, act, nil)
		}
	}

	hasPeriodic := false
	for _, j := range s.ir.Jobs {
		if j.Every > 0 {
			hasPeriodic = true
			break
		}
	}
	// Nothing periodic and no durable queue to drain on a single-process dev run:
	// skip the worker pool entirely so a `facet run` with only on-start jobs adds
	// no background goroutines.
	if !hasPeriodic && !clusterEnabled() {
		return
	}

	q := &jobQueue{srv: s, stop: make(chan struct{})}
	s.jobs = q
	for _, j := range s.ir.Jobs {
		if j.Every > 0 {
			go q.schedule(j)
		}
	}
	workers := defaultWorkers
	for i := 0; i < workers; i++ {
		go q.work(i)
	}
	go q.reportDepth()
}

// schedule drives one `every Ns` job: it primes the cron row to the next tick (so
// the interval, not startup, sets the first run) and then, each interval, the one
// instance that wins ReserveCron enqueues a durable job.
func (q *jobQueue) schedule(j ir.Job) {
	interval := time.Duration(j.Every) * time.Second
	// Prime: create the cron row dated one interval out without enqueuing now.
	_, _ = q.srv.store.ReserveCron(j.Name, time.Now().Add(interval))
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-q.stop:
			return
		case <-t.C:
			won, err := q.srv.store.ReserveCron(j.Name, time.Now().Add(interval))
			if err != nil {
				q.srv.obs.log.Warn("cron reserve", slog.String("job", j.Name), slog.Any("error", err))
				continue
			}
			if !won {
				continue // a peer enqueued this tick
			}
			if err := q.srv.store.EnqueueJob(&durableJob{Queue: "cron", Action: j.Action}); err != nil {
				q.srv.obs.log.Warn("cron enqueue", slog.String("job", j.Name), slog.Any("error", err))
			}
		}
	}
}

// work is one worker: it leases due jobs and runs them, retrying with backoff and
// dead-lettering on exhaustion. Idle workers poll on a short interval.
func (q *jobQueue) work(id int) {
	worker := q.srv.cluster.id() + "-" + itoa(id)
	for {
		select {
		case <-q.stop:
			return
		default:
		}
		j, err := q.srv.store.ClaimJob(worker)
		if err != nil {
			q.srv.obs.log.Warn("claim job", slog.Any("error", err))
			time.Sleep(jobPollEvery)
			continue
		}
		if j == nil {
			time.Sleep(jobPollEvery)
			continue
		}
		q.run(j)
	}
}

// run executes one leased job and records its outcome. A panic in an action is
// caught and treated as a failure so one bad job cannot take a worker down.
func (q *jobQueue) run(j *durableJob) {
	defer func() {
		if r := recover(); r != nil {
			q.finish(j, "panic in action", r != nil)
		}
	}()
	act := q.srv.byAction[j.Action]
	if act == nil {
		// Unknown action: dead-letter immediately, retrying cannot help.
		_ = q.srv.store.FinishJob(j.ID, "dead", "unknown action "+j.Action, time.Time{})
		q.srv.obs.metrics.observeJob("dead")
		return
	}
	_, status, msg := q.srv.runAction(systemSID, act, j.Args)
	if status == http.StatusOK {
		_ = q.srv.store.FinishJob(j.ID, "done", "", time.Time{})
		q.srv.obs.metrics.observeJob("done")
		return
	}
	q.finish(j, msg, true)
}

// finish reschedules a failed job with backoff, or dead-letters it once it has
// used up its attempts.
func (q *jobQueue) finish(j *durableJob, msg string, failed bool) {
	if !failed {
		return
	}
	if j.Attempts >= j.MaxAttempts {
		_ = q.srv.store.FinishJob(j.ID, "dead", msg, time.Time{})
		q.srv.obs.metrics.observeJob("dead")
		q.srv.obs.log.Error("job dead-lettered",
			slog.String("action", j.Action), slog.Int("attempts", j.Attempts), slog.String("error", msg))
		return
	}
	next := time.Now().Add(backoff(j.Attempts))
	_ = q.srv.store.FinishJob(j.ID, "pending", msg, next)
	q.srv.obs.metrics.observeJob("retry")
}

// reportDepth keeps the queue-depth gauge fresh for /metrics.
func (q *jobQueue) reportDepth() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-q.stop:
			return
		case <-t.C:
			if n, err := q.srv.store.PendingJobs(); err == nil {
				q.srv.obs.metrics.setJobQueueDepth(n)
			}
		}
	}
}

// stopAll halts this instance's workers and schedulers (graceful shutdown).
func (q *jobQueue) stopAll() {
	if q == nil {
		return
	}
	close(q.stop)
}

// backoff is the exponential delay before retrying attempt n (1-based), capped.
func backoff(attempt int) time.Duration {
	d := jobBackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= jobBackoffCap {
			return jobBackoffCap
		}
	}
	return d
}

// id returns this instance's cluster id, or "solo" when clustering is off.
func (c *cluster) id() string {
	if c == nil {
		return "solo"
	}
	return c.instanceID
}
