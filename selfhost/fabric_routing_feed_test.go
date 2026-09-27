package selfhost

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// fabric_routing_feed.fct carries the routing a fabricd publishes to front
// doors in other processes (fabric_frontdoor_main.fct with
// FRONTDOOR_ROUTING_FEED), and fabric_runtime_mechanism.fct's cutover waits
// for those doors. The Rust fabric has no equivalent to hold it against:
// fabric-facetql's FrontDoor is a library whose only process is fabricd,
// which hands it each table in-process (control.rs publish_routing). So the
// checks here are against the crate's own semantics:
//
//   - the codec loses nothing a door routes by: every table round-trips to
//     the same digest table_check.fct compares with the crate
//     (TestFabricLeafBTable) and to the same answer for every route asked;
//   - with no door subscribed the cutover is the crate's
//     (TestFabricRuntimeGoldenMatchesRust is unchanged by the gate);
//   - with doors, authority moves only once every door confirmed a table in
//     which the move is fenced — what the in-process door gets for free,
//     since publish_routing returns before the loop moves on.
//
// The live half — a standalone door following a real daemon through a real
// migration, and failing closed when the daemon goes away — is
// TestFabricStandaloneDoorFollowsTheDaemon (fabric_daemon_test.go) and
// integration/allfct_stack_test.go's standalone-door stacks.

var (
	feedCheckOnce sync.Once
	feedCheckSrv  *httptest.Server
	feedCheckErr  error
)

func feedCheck(t *testing.T, action string) []string {
	t.Helper()
	feedCheckOnce.Do(func() {
		g, err := compile.File(filepath.Join("testdata", "fabric_feed", "feed_check.fct"))
		if err != nil {
			feedCheckErr = err
			return
		}
		srv, err := runtime.NewInMemory(g)
		if err != nil {
			feedCheckErr = err
			return
		}
		feedCheckSrv = httptest.NewServer(srv.Handler())
	})
	if feedCheckErr != nil {
		t.Fatalf("compile testdata/fabric_feed/feed_check.fct: %v", feedCheckErr)
	}
	d := postJSON(t, feedCheckSrv, action)
	out, _ := d["feedOut"].(string)
	return strings.Split(out, "\n")
}

func feedExpect(t *testing.T, action string, want []string) {
	t.Helper()
	got := feedCheck(t, action)
	if len(got) != len(want) {
		t.Fatalf("%s: %d lines, want %d:\n%s", action, len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s line %d:\n got %s\nwant %s", action, i, got[i], want[i])
		}
	}
}

// Every table shape a door can be handed round-trips to the same crate
// digest and the same routes, and re-encodes byte for byte; the feed's
// answer carries the table only to a door that has not adopted it, and an
// answer naming no lease is refused (a door must never route without one).
func TestFabricRoutingFeedCodec(t *testing.T) {
	var want []string
	for i := 0; i < 14; i++ {
		want = append(want, fmt.Sprintf("table %d: ok", i))
	}
	want = append(want,
		"array: false a routing table is a JSON object",
		"empty object: true generation 0 nodes 0",
		"answer to a stale door: true generation 7 lease 2000 table true digest-equal true",
		"answer to a current door: true table false",
		"answer with no lease: false the routing feed named no lease",
	)
	feedExpect(t, "runFeedCodec", want)
}

// The daemon's leases: in id order, renewed by a poll, the least
// confirmation (as a u64) the one that counts, a door forgotten only once
// its lease and its in-flight bound have both run out (b: heard at 1000,
// bound 500, lease 2000 -> live through 3500). The door's accounting: it
// confirms what nothing in flight is older than; a registration that has
// not said which table it read holds the last confirmation; a fence retry
// that re-reads the newer table moves it on.
func TestFabricRoutingFeedLeases(t *testing.T) {
	feedExpect(t, "runFeedLeases", []string{
		"subscribed=false confirmed=0 doors=",
		"subscribed=true confirmed=7 doors=a,b",
		"subscribed=true confirmed=9 doors=a,b",
		"subscribed=true confirmed=9 doors=a,b",
		"subscribed=true confirmed=12 doors=a",
		"subscribed=false confirmed=0 doors=",
		"subscribed=true confirmed=5 doors=x,y",
		"none in flight: 10",
		"registered, table not read: 4",
		"reading 8: 8",
		"reading 8 and 10: 8",
		"a fence retry re-read 10: 10",
		"answered: 11",
		"a keep-alive connection's next request: 1",
	})
}

// The cutover with the copy verified and drained: with no door it
// completes in this poll, exactly as the crate's does (phase 4 = Cleanup,
// writes go to db-b); with a door that has not confirmed the fenced table
// it waits with the fence up (phase 3 = Cutover, writes refused behind the
// fence); confirmed, or past it, authority moves.
func TestFabricRoutingFeedGate(t *testing.T) {
	feedExpect(t, "runFeedGate", []string{
		"fence recorded: true at the fenced table's generation: true",
		"no door: complete writes->db-b phase=4",
		"a door behind the fence: running(900) writes->refused phase=3",
		"a door that confirmed the fence: complete writes->db-b phase=4",
		"a door past the fence: complete writes->db-b phase=4",
	})
}
