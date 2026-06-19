package runtime

// Horizontal scale — making the server stateless so you can run many instances
// behind a load balancer. Two pieces of per-process state stop that today: the
// SSE fan-out (a change on instance A must reach a client connected to instance
// B) and sessions (a request can land on any instance). Both move to the shared
// database:
//
//   - Cross-instance pub/sub rides Postgres LISTEN/NOTIFY. When an action changes
//     entities, the instance NOTIFYs a compact message naming them; every other
//     instance LISTENing reloads those rows and fans them out to its own SSE
//     clients. No Redis/NATS to operate — the database you already run is the bus.
//   - Sessions move to a shared table (see store.go). The in-memory map becomes a
//     write-through cache: a request that lands on a cold instance rehydrates the
//     session from the database, so any instance can serve any user.
//
// Clustering is opt-in with FACET_CLUSTER=1 (a single-process dev run keeps the
// fast in-memory path). The instance id distinguishes a server's own NOTIFYs from
// its peers'.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/lib/pq"
)

// clusterChannel is the Postgres LISTEN/NOTIFY channel every instance shares.
const clusterChannel = "facet_events"

// cluster carries cross-instance state for one server: the inbound listener and
// this process's identity.
type cluster struct {
	srv        *Server
	instanceID string
	listener   *pq.Listener
}

// clusterEnabled reports whether horizontal scale is turned on.
func clusterEnabled() bool { return os.Getenv("FACET_CLUSTER") == "1" }

// clusterEvent is the NOTIFY payload: who sent it and which entities changed.
type clusterEvent struct {
	Origin   string   `json:"origin"`
	Entities []string `json:"entities"`
}

// startCluster wires this server into the cross-instance bus: it opens a
// dedicated LISTEN connection and reflects peers' changes into the local working
// set and SSE fan-out. Returns nil (clustering off) or an error if the listener
// cannot connect.
func startCluster(s *Server) (*cluster, error) {
	dsn := os.Getenv("FACET_DATABASE_URL")
	c := &cluster{srv: s, instanceID: randHex(8)}
	l := pq.NewListener(dsn, 2*time.Second, time.Minute, func(ev pq.ListenerEventType, err error) {
		if err != nil {
			s.obs.log.Error("cluster listener event", slog.Any("error", err))
		}
	})
	if err := l.Listen(clusterChannel); err != nil {
		l.Close()
		return nil, fmt.Errorf("listen on %s: %w", clusterChannel, err)
	}
	c.listener = l
	go c.receive()
	go c.purgeSessions()
	s.obs.log.Info("cluster enabled", slog.String("instance", c.instanceID))
	return c, nil
}

// receive consumes peers' change notifications, refreshing the working set and
// fanning out to this instance's live clients.
func (c *cluster) receive() {
	for n := range c.listener.Notify {
		if n == nil {
			continue // a reconnect tick
		}
		var ev clusterEvent
		if err := json.Unmarshal([]byte(n.Extra), &ev); err != nil {
			continue
		}
		if ev.Origin == c.instanceID {
			continue // our own change; already fanned out locally
		}
		c.srv.applyRemoteChange(ev.Entities)
	}
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
