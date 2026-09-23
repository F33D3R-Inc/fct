package runtime

// Horizontal scale — making the server stateless so you can run many instances
// behind a load balancer. Two pieces of per-process state stop that today: the
// SSE fan-out (a change on instance A must reach a client connected to instance
// B) and sessions (a request can land on any instance). Both move to the shared
// database:
//
//   - Cross-instance pub/sub rides FacetQL's own live feed (POST /publish, GET
//     /events) — the same SSE bus the engine already uses for its own node/edge
//     change notifications, carrying one more kind of message alongside them.
//     When an action changes entities, the instance POSTs a compact message
//     naming them; every other instance's /events subscription decodes it,
//     reloads those rows, and fans them out to its own SSE clients. No
//     Redis/NATS to operate — the database you already run is the bus. This
//     used to ride Postgres LISTEN/NOTIFY; it moved here once FacetQL reached
//     parity and the Postgres backend was excised (AGENT_LOG.md, "Changelog —
//     2026-09-06 (evening)").
//   - Sessions move to a shared table (see store.go). The in-memory map becomes a
//     write-through cache: a request that lands on a cold instance rehydrates the
//     session from the database, so any instance can serve any user.
//
// Clustering is opt-in with FACET_CLUSTER=1 (a single-process dev run keeps the
// fast in-memory path). The instance id distinguishes a server's own publishes
// from its peers'.
//
// Clustering only means something over a store every instance actually shares —
// the in-memory store is one process's private map, so FACET_CLUSTER=1 there
// would have each instance "cluster" with itself. startCluster refuses that
// combination with a clear error rather than silently doing nothing.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// clusterChannel is carried in the publish payload for parity with the old
// Postgres NOTIFY channel name; FacetQL's /publish has no channel concept of
// its own (every publish lands on the one /events feed, scoped by the
// caller's identity, not by name — see facetql's PublishRequest doc comment)
// — see fqClient.publish. Not dead weight: fabric's frontdoor reads this same
// field out of the raw request body to route a publish to exactly one shard
// by keyspace when FacetQL runs behind it (fabric/crates/fabric-facetql/src/
// frontdoor/plan.rs), which is a sharding concern, not an audience one, so a
// single constant here is correct — every instance of this service shares
// one channel name because they are one tenant, not many.
const clusterChannel = "facet_events"

// cluster carries cross-instance state for one server: this process's identity
// and the cancel function that stops its /events subscription on shutdown.
type cluster struct {
	srv        *Server
	instanceID string
	cancel     context.CancelFunc
}

// clusterSubscribeTimeout bounds how long startup waits for FacetQL to accept
// the event-feed subscription.
const clusterSubscribeTimeout = 15 * time.Second

// clusterEnabled reports whether horizontal scale is turned on.
func clusterEnabled() bool { return os.Getenv("FACET_CLUSTER") == "1" }

// clusterEvent is the payload published to and read back from FacetQL's live
// feed: who sent it, which entities changed, and which session ids ended (a
// logout, a re-key on `establish`, a `revoke`). The session list is what makes
// the per-instance session map a cache of the shared table rather than a copy
// that outlives it: a peer evicts an ended id the moment it hears of it.
type clusterEvent struct {
	Origin   string   `json:"origin"`
	Entities []string `json:"entities,omitempty"`
	Sessions []string `json:"sessions,omitempty"` // ended session ids
	Visitors []string `json:"visitors,omitempty"` // `session` keys a `revoke` ended, on every instance
	Restated []string `json:"restated,omitempty"` // actors whose sessions a `restate` re-roled
}

// startCluster wires this server into the cross-instance bus: it opens a
// subscription to FacetQL's GET /events and reflects peers' changes into the
// local working set and SSE fan-out. Returns an error when the store this
// server was opened against cannot back a real cluster.
func startCluster(s *Server) (*cluster, error) {
	fq, ok := s.store.(*fqStore)
	if !ok {
		return nil, fmt.Errorf(
			"FACET_CLUSTER=1 requires the facetql:// store — clustering has no " +
				"shared bus over the in-memory store, since each process would only " +
				"ever hear its own publishes")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &cluster{srv: s, instanceID: randHex(8), cancel: cancel}
	// The first subscription is opened here, before this server can serve a
	// request, and not inside receiveLoop's goroutine. FacetQL registers a
	// subscriber before it answers GET /events (the backlog and the live
	// receiver are taken under one lock), so once this call returns every
	// later publish reaches this instance. Opened asynchronously, the
	// subscription could land after the server was already caching sessions:
	// a peer's session end published in that window went to no one, and this
	// instance kept authenticating the ended id — resuming from the live
	// edge (no ?after) cannot replay what came before the first connection.
	// The stream itself has no deadline (see eventsHTTPClient), so waiting for
	// the feed to accept it is bounded here instead: a feed that never answers
	// is a startup error, not a server that hangs before it ever listens.
	timer := time.AfterFunc(clusterSubscribeTimeout, cancel)
	first, err := fq.c.streamEvents(ctx, 0)
	if !timer.Stop() {
		// the deadline cancelled ctx: whatever came back is already dead
		if first != nil {
			first.Close()
		}
		return nil, fmt.Errorf("subscribe to the FacetQL event feed: no answer within %s", clusterSubscribeTimeout)
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("subscribe to the FacetQL event feed: %w", err)
	}
	go c.receiveLoop(ctx, fq, first)
	go c.purgeSessions()
	s.obs.log.Info("cluster enabled", slog.String("instance", c.instanceID))
	return c, nil
}

// receiveLoop holds a standing subscription to FacetQL's GET /events for as
// long as ctx lives, reconnecting with backoff whenever the connection drops —
// the SSE stream is long-lived by design (see facetql's own routing comment on
// why it carries no request timeout) but a network blip, a restart of the
// FacetQL process, or the server itself going away must not leave this
// instance permanently deaf to its peers.
//
// first is the subscription startCluster already opened (see there); every
// later connection is opened here.
//
// `after` tracks the highest event sequence this instance has applied, so a
// reconnect resumes with ?after= instead of replaying (or worse, silently
// skipping) whatever was published while the connection was down. A 410 means
// that position aged out of FacetQL's retention ring; there is nothing to
// resume from at that point, so the loop drops back to the live edge and
// accepts the gap — the same tradeoff Postgres LISTEN/NOTIFY always had, since
// a NOTIFY sent to no active listener was never queued either.
func (c *cluster) receiveLoop(ctx context.Context, fq *fqStore, first io.ReadCloser) {
	var after uint64
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for ctx.Err() == nil {
		body, err := first, error(nil)
		if first == nil {
			body, err = fq.c.streamEvents(ctx, after)
		}
		first = nil
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errEventsResumeGone) {
				// Whatever was published while this instance was away is gone,
				// session ends included, so no cached session can be trusted
				// to still exist: drop them all and let each rehydrate from the
				// shared table (Server.session / sidForRequest) on its next use.
				after = 0
				c.srv.evictCachedSessions(nil)
			} else {
				c.srv.obs.log.Warn("cluster: connect to /events", slog.Any("error", err))
			}
			if !sleepOrDone(ctx, backoff) {
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		backoff = time.Second
		after = c.consume(body, after)
		body.Close()
		if ctx.Err() != nil {
			return
		}
		// The stream ended on its own (the server closed it, or a lagged
		// receiver's connection was dropped) — pause briefly rather than
		// hammering a reconnect in a tight loop.
		if !sleepOrDone(ctx, time.Second) {
			return
		}
	}
}

// sleepOrDone waits out d, or returns false early if ctx is cancelled first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// consume reads one /events connection's SSE frames until the stream ends,
// applying every clusterEvent it can decode and returning the highest
// sequence id seen — the position the next reconnect resumes from.
//
// Frames this instance itself published are skipped (already applied
// locally, same as before). Anything that is not valid clusterEvent JSON is
// silently ignored: /events also carries FacetQL's own node/edge/user change
// notifications and the engine's `feed_lagged` marker, none of which are this
// bus's concern — the old Postgres implementation had the same tolerance
// (invalid JSON on the NOTIFY channel was dropped, not fatal).
func (c *cluster) consume(body io.Reader, after uint64) uint64 {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)

	var data strings.Builder
	flush := func() {
		defer data.Reset()
		if data.Len() == 0 {
			return
		}
		var ev clusterEvent
		if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
			return
		}
		if ev.Origin == c.instanceID {
			return
		}
		if len(ev.Sessions) > 0 {
			c.srv.evictCachedSessions(ev.Sessions)
		}
		if len(ev.Restated) > 0 {
			c.srv.evictActorSessions(ev.Restated)
		}
		if len(ev.Visitors) > 0 {
			c.srv.evictVisitorSessions(ev.Visitors)
		}
		if len(ev.Entities) > 0 {
			c.srv.applyRemoteChange(ev.Entities)
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			if seq, err := strconv.ParseUint(strings.TrimSpace(line[len("id:"):]), 10, 64); err == nil {
				after = seq
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			flush()
		}
	}
	flush() // a stream that ends without a trailing blank line still owes its last event
	return after
}

// purgeSessions periodically drops expired rows from the shared session table so
// it does not grow without bound.
func (c *cluster) purgeSessions() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		if err := c.srv.store.PurgeExpiredSessions(); err != nil {
			c.srv.obs.log.Warn("purge sessions", slog.Any("error", err))
		}
	}
}

// publish announces a set of changed entities to peers (no-op if clustering is
// off). The local fan-out has already happened; this reaches the other instances.
func (c *cluster) publish(entities []string) {
	if c == nil || len(entities) == 0 {
		return
	}
	payload, err := json.Marshal(clusterEvent{Origin: c.instanceID, Entities: entities})
	if err != nil {
		return
	}
	if err := c.srv.store.Notify(string(payload)); err != nil {
		c.srv.obs.log.Warn("cluster publish", slog.Any("error", err))
	}
}

// publishEndedSessions announces session ids, and `session` keys, that no
// longer exist, so every peer evicts its cached copies (see clusterEvent).
func (c *cluster) publishEndedSessions(sids, visitors []string) {
	if c == nil || len(sids)+len(visitors) == 0 {
		return
	}
	payload, err := json.Marshal(clusterEvent{Origin: c.instanceID, Sessions: sids, Visitors: visitors})
	if err != nil {
		return
	}
	if err := c.srv.store.Notify(string(payload)); err != nil {
		c.srv.obs.log.Warn("cluster publish sessions", slog.Any("error", err))
	}
}

// evictCachedSessions drops the named session ids from this instance's cache —
// every one of them when sids is nil. Nothing is lost: a session that still
// exists in the shared table rehydrates on its next request, and one that was
// ended elsewhere no longer authenticates here.
func (s *Server) evictCachedSessions(sids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sids == nil {
		for sid := range s.sessions {
			if sid != systemSID && sid != toolSID {
				delete(s.sessions, sid)
			}
		}
	}
	for _, sid := range sids {
		delete(s.sessions, sid)
	}
	s.obs.metrics.setSessions(int64(len(s.sessions)))
}

// publishRestated tells peers these actors' sessions were re-roled in the
// shared table, so they drop their cached copies.
func (c *cluster) publishRestated(actors []string) {
	if c == nil || len(actors) == 0 {
		return
	}
	payload, err := json.Marshal(clusterEvent{Origin: c.instanceID, Restated: actors})
	if err != nil {
		return
	}
	if err := c.srv.store.Notify(string(payload)); err != nil {
		c.srv.obs.log.Warn("cluster publish restate", slog.Any("error", err))
	}
}

// evictActorSessions drops every cached session signed in as one of these
// actors (a peer's `restate`): the next request rehydrates it, new role and
// all, from the shared session table.
func (s *Server) evictActorSessions(actors []string) {
	re := map[string]bool{}
	for _, a := range actors {
		re[a] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sid, ses := range s.sessions {
		if re[ses.actor] && sid != systemSID && sid != toolSID {
			delete(s.sessions, sid)
		}
	}
	s.obs.metrics.setSessions(int64(len(s.sessions)))
}

// evictVisitorSessions drops every cached session carrying one of these
// `session` keys (a peer's `revoke`).
func (s *Server) evictVisitorSessions(visitors []string) {
	ended := map[string]bool{}
	for _, v := range visitors {
		ended[v] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sid, ses := range s.sessions {
		if ended[ses.visitor] && sid != systemSID && sid != toolSID {
			delete(s.sessions, sid)
		}
	}
	s.obs.metrics.setSessions(int64(len(s.sessions)))
}

// applyRemoteChange reloads the named entities from the database into the working
// set and fans the new rows out to this instance's SSE clients, so a change made
// on a peer converges here.
func (s *Server) applyRemoteChange(entities []string) {
	deltas := map[string]any{}
	s.mu.Lock()
	for _, ent := range entities {
		if ent == reservedUserEntity {
			continue
		}
		rows, err := s.store.Load(ent)
		if err != nil {
			continue
		}
		s.entities[ent] = rows
		// keep the id counter ahead of anything a peer inserted.
		for _, r := range rows {
			if m, ok := r.(record); ok {
				if id := toInt(m["id"]); id > s.nextID[ent] {
					s.nextID[ent] = id
				}
			}
		}
		deltas[ent] = rows
	}
	s.mu.Unlock()
	s.fanout(deltas)
}
