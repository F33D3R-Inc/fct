package runtime

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"facet/internal/ir"
)

// TestClusterSubscribesBeforeServing: startCluster returns only once the
// FacetQL event feed has accepted this instance's subscription. A server
// that started serving (and caching sessions) before that point could miss
// a peer's session-end published in the gap — the live edge never replays
// what came before the first connection — and keep authenticating an ended
// session (integration/TestClusteredSessionEndsEverywhere's flake). The
// fake feed takes its time registering the subscriber, as a loaded one does.
func TestClusterSubscribesBeforeServing(t *testing.T) {
	var registered atomic.Bool
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(300 * time.Millisecond)
		registered.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer feed.Close()

	s := &Server{store: &fqStore{c: newFQClient(feed.URL, "t"), ents: map[string]ir.Entity{}}, obs: newObs()}
	c, err := startCluster(s)
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	if !registered.Load() {
		t.Fatal("startCluster returned before the event feed registered the subscription")
	}
}

// TestClusterRefusesAnUnreachableFeed: a feed that cannot be subscribed to
// is a startup error, not an instance that silently never hears its peers.
func TestClusterRefusesAnUnreachableFeed(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer feed.Close()
	s := &Server{store: &fqStore{c: newFQClient(feed.URL, "t"), ents: map[string]ir.Entity{}}, obs: newObs()}
	if c, err := startCluster(s); err == nil {
		c.cancel()
		t.Fatal("startCluster accepted a feed that refused the subscription")
	}
}
