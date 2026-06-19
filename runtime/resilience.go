package runtime

// Resilience — running for real means surviving deploys, slow clients, and
// disasters without dropping requests or data.
//
//   - Graceful shutdown: on SIGTERM/SIGINT (the signal an orchestrator sends to
//     roll a pod) the server stops accepting new connections, lets in-flight
//     requests finish within a deadline, halts the job workers, and closes the
//     database — so a deploy never severs a live request or a running job.
//   - Timeouts: a slow or stuck client cannot pin a connection forever
//     (ReadHeaderTimeout/IdleTimeout). The SSE stream is deliberately exempt from
//     a write deadline — it is a long-lived push channel.
//   - Caching: an optional short-TTL micro-cache (FACET_API_CACHE_TTL) in front of
//     the read-heavy `GET /api/<Entity>` lists, invalidated the instant any entity
//     changes, so a hot feed is served from memory without ever going stale.
//   - Backups / DR: `facet backup` writes a portable logical snapshot (schema +
//     every row) that `facet restore` replays into a fresh database.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"facet/internal/ir"
)

const shutdownGrace = 25 * time.Second

// Serve runs the HTTP server with production timeouts and blocks until a
// shutdown signal arrives, then drains gracefully. It replaces a bare
// http.ListenAndServe so a `facet run` is deploy-safe out of the box.
func (s *Server) Serve(addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// A slow client cannot hold a connection open indefinitely. No WriteTimeout:
		// the SSE stream is a long-lived push and must not be cut off mid-stream.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		s.obs.log.Info("shutting down", slog.String("signal", sig.String()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(ctx) // stop accepting, drain in-flight requests
	s.Shutdown()             // halt workers, close cluster + store
	return err
}

// Shutdown releases the server's background resources: job workers, the cluster
// listener, and the database. Safe to call once after Serve returns.
func (s *Server) Shutdown() {
	s.jobs.stopAll()
	if s.cluster != nil && s.cluster.listener != nil {
		s.cluster.listener.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
}

// ── API read cache ─────────────────────────────────────────────────────────────

// apiCache is a short-TTL, version-invalidated cache for entity-list reads. Each
// cached entry remembers the write version it was built at; any entity change
// bumps the global version, so the next read misses and the cache can never serve
// data that predates a write.
type apiCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]cacheEntry
	version uint64 // bumped (atomically) on every entity write
}

type cacheEntry struct {
	body    []byte
	version uint64
	expires time.Time
}

// newAPICache reads FACET_API_CACHE_TTL (seconds); 0/unset disables caching.
func newAPICache() *apiCache {
	secs := 0
	if v := os.Getenv("FACET_API_CACHE_TTL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			secs = n
		}
	}
	if secs == 0 {
		return nil
	}
	return &apiCache{ttl: time.Duration(secs) * time.Second, entries: map[string]cacheEntry{}}
}

// get returns a fresh cached body for key, or false.
func (c *apiCache) get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.version != atomic.LoadUint64(&c.version) || time.Now().After(e.expires) {
		return nil, false
	}
	return e.body, true
}

// put stores a response body under key, stamped with the current version.
func (c *apiCache) put(key string, body []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) > 4096 {
		c.entries = map[string]cacheEntry{} // bound memory under a wide key space
	}
	c.entries[key] = cacheEntry{body: body, version: atomic.LoadUint64(&c.version), expires: time.Now().Add(c.ttl)}
}

// invalidate bumps the version so every prior entry is stale at once.
func (c *apiCache) invalidate() {
	if c == nil {
		return
	}
	atomic.AddUint64(&c.version, 1)
}

// ── backup / restore (DR) ──────────────────────────────────────────────────────

// backupFile is the on-disk shape of a logical snapshot: the app, a timestamp,
// and every entity's rows. It is portable across databases and restores into a
// fresh one, so it is the disaster-recovery floor under the live database's own
// physical backups.
type backupFile struct {
	App  string           `json:"app"`
	At   string           `json:"at"`
	Data map[string][]any `json:"data"`
	Meta map[string]int   `json:"nextId"`
}

// Backup writes a logical snapshot of every entity to w (used by `facet backup`).
// It reads the live working set, so it is a consistent point-in-time copy.
func (s *Server) Backup(w io.Writer) error {
	s.mu.Lock()
	bf := backupFile{App: s.ir.App, At: time.Now().UTC().Format(time.RFC3339), Data: map[string][]any{}, Meta: map[string]int{}}
	for ent, rows := range s.entities {
		cp := make([]any, len(rows))
		copy(cp, rows)
		bf.Data[ent] = cp
		bf.Meta[ent] = s.nextID[ent]
	}
	s.mu.Unlock()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(bf)
}

// Restore replays a logical snapshot into the store (used by `facet restore`),
// writing every row through the durable layer. Existing rows with the same id are
// overwritten (upsert), so a restore is idempotent.
func (s *Server) Restore(r io.Reader) (int, error) {
	var bf backupFile
	if err := json.NewDecoder(r).Decode(&bf); err != nil {
		return 0, fmt.Errorf("read backup: %w", err)
	}
	count := 0
	for _, e := range s.ir.Entities {
		rows := bf.Data[e.Name]
		for _, raw := range rows {
			row := asRecord(raw)
			if row == nil {
				continue
			}
			if err := s.store.Save(e.Name, row); err != nil {
				return count, fmt.Errorf("restore %s: %w", e.Name, err)
			}
			count++
		}
	}
	return count, nil
}

// asRecord coerces a decoded JSON object into a record (string-keyed map).
func asRecord(raw any) record {
	if m, ok := raw.(map[string]any); ok {
		return record(m)
	}
	if m, ok := raw.(record); ok {
		return m
	}
	return nil
}

// Backup opens a server (loading the working set) and writes a snapshot to w —
// the entry point `facet backup` calls without a long-running server.
func Backup(graph *ir.IR, w io.Writer) error {
	s, err := New(graph)
	if err != nil {
		return err
	}
	defer s.Shutdown()
	return s.Backup(w)
}

// Restore opens a server and replays a snapshot from r — the entry point
// `facet restore` calls.
func Restore(graph *ir.IR, r io.Reader) (int, error) {
	s, err := New(graph)
	if err != nil {
		return 0, err
	}
	defer s.Shutdown()
	return s.Restore(r)
}
