package selfhost

// The fct FacetQL client (fabric_facetql_{client,placement}.fct, over
// http_client.fct and the runtime's outbound TCP primitive) driving a REAL
// FacetQL server — facetql's own Rust binary, built from ../facetql with
// cargo and started on a fresh data directory — through the flows of
// fabric/crates/fabric-facetql/tests/live.rs: stats, placement write/read-
// back and registry load, the engine's compare-and-set refusing a stale
// update and a stale delete, a bad token failing closed, a kind larger than
// one page walked by cursor, claim's single winner, and the rest of the
// client surface (point read, multiget, count, predicate query, an
// all-or-nothing transaction).
//
// Skips only when cargo (or the facetql checkout) is absent. The binary is
// rebuilt by cargo on every run, so it can never be stale; set
// FCT_FACETQL_SERVER_TARGET to choose where it builds. The server runs with
// FACETQL_ALLOW_PLAINTEXT (the fct runtime has no TLS) and its per-identity
// rate limits off (the cursor flow writes 600 rows as fast as it can; rate
// limiting is not what it tests).

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	fqlLiveOnce sync.Once
	fqlLiveBin  string
	fqlLiveErr  string
)

func fqlLiveServerBin(t *testing.T) string {
	t.Helper()
	fqlLiveOnce.Do(func() {
		cargo, err := exec.LookPath("cargo")
		if err != nil {
			fqlLiveErr = "cargo is not installed"
			return
		}
		if _, err := os.Stat("../../facetql/Cargo.toml"); err != nil {
			fqlLiveErr = "no facetql checkout beside fct"
			return
		}
		target := os.Getenv("FCT_FACETQL_SERVER_TARGET")
		if target == "" {
			target = filepath.Join(os.TempDir(), "fct-facetql-server")
		}
		cmd := exec.Command(cargo, "build", "--release", "--quiet", "--bin", "facetql")
		cmd.Dir = "../../facetql"
		cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
		if out, err := cmd.CombinedOutput(); err != nil {
			fqlLiveErr = "cargo build failed: " + err.Error() + "\n" + string(out)
			return
		}
		fqlLiveBin = filepath.Join(target, "release", "facetql")
	})
	if fqlLiveBin == "" {
		if strings.HasPrefix(fqlLiveErr, "cargo build failed") {
			t.Fatal(fqlLiveErr)
		}
		t.Skip("facetql server unavailable: " + fqlLiveErr)
	}
	return fqlLiveBin
}

// fqlLiveStart runs a fresh FacetQL and returns its base URL; the admin
// token is "fabtok".
func fqlLiveStart(t *testing.T) string {
	t.Helper()
	bin := fqlLiveServerBin(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	var log strings.Builder
	cmd := exec.Command(bin, "start")
	cmd.Env = append(os.Environ(),
		"FACETQL_DATA_DIR="+t.TempDir(),
		fmt.Sprintf("FACETQL_PORT=%d", port),
		"FACETQL_TOKENS=fabtok:fabric:admin",
		"FACETQL_MASTER_KEY="+facetqlCheckKey,
		"FACETQL_ALLOW_PLAINTEXT=1",
		"FACETQL_RATE_READ=off", "FACETQL_RATE_WRITE=off", "FACETQL_RATE_BULK=off", "FACETQL_RATE_ADMIN=off",
	)
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(60 * time.Second)
	for {
		req, _ := http.NewRequest("GET", base+"/stats", nil)
		req.Header.Set("x-api-key", "fabtok")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("facetql did not come up on %s:\n%s", base, log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// fqlLiveFields parses a flow's `name=value` lines.
func fqlLiveFields(t *testing.T, out string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			t.Fatalf("malformed flow line %q in:\n%s", l, out)
		}
		m[k] = v
	}
	return m
}

func TestFabricFacetqlLiveAgainstRealFacetql(t *testing.T) {
	base := fqlLiveStart(t)
	ts := fqltApp(t, "fabric_facetql_placement.fct")
	flow := func(name string, shard, rows int) map[string]string {
		t.Helper()
		return fqlLiveFields(t, fqltCall(t, ts, "fqlLiveOut", "fqlLiveRun", name, base, "fabtok", shard, rows, ""))
	}
	want := func(t *testing.T, m map[string]string, expect map[string]string) {
		t.Helper()
		for k, v := range expect {
			if m[k] != v {
				t.Errorf("%s = %q, want %q (all: %v)", k, m[k], v, m)
			}
		}
	}

	t.Run("stats_reads_the_engines_own_counters", func(t *testing.T) {
		m := flow("stats", 0, 0)
		want(t, m, map[string]string{"first": "ok", "second": "ok", "readsMonotonic": "true", "writesMonotonic": "true", "hasVersion": "true", "maxConcurrent": "512"})
		if m["pageSize"] == "0" || m["pageSize"] == "" {
			t.Errorf("page size: %v", m)
		}
	})

	t.Run("a_placement_is_written_and_read_back", func(t *testing.T) {
		m := flow("writeRead", 9001, 0)
		want(t, m, map[string]string{
			"create": "ok", "createVersion": "1",
			"get":      "ok found=true",
			"readBack": "__fabric_placement:9001:1:2 v1 db-a 9001 us-east",
			"address":  "__fabric_placement:9001:1:2",
			"registry": "ok located=true dbms=db-a", "registryVersion": "1",
			"secondCreate": "Conflict", "cleanup": "ok",
		})
	})

	t.Run("a_stale_update_is_refused_by_the_engines_compare_and_set", func(t *testing.T) {
		m := flow("stale", 9002, 0)
		want(t, m, map[string]string{
			"create": "ok", "firstUpdate": "ok v2", "staleUpdate": "PreconditionFailed",
			"current":     "__fabric_placement:9002:1:2 v2 db-b 9002 eu-west",
			"staleRemove": "PreconditionFailed", "survived": "true",
			"remove": "ok", "gone": "ok found=false",
		})
		if !strings.HasPrefix(m["staleUpdateText"], "facetql compare-and-set refused (412): ") {
			t.Errorf("stale update text: %q", m["staleUpdateText"])
		}
	})

	t.Run("a_bad_token_fails_closed_rather_than_reading_as_healthy", func(t *testing.T) {
		m := flow("badToken", 0, 0)
		want(t, m, map[string]string{"stats": "Unauthorized", "unhealthy": "true"})
		if strings.Contains(m["text"], "not-a-real-token") || !strings.HasPrefix(m["text"], "facetql refused the token (401): ") {
			t.Errorf("error text: %q", m["text"])
		}
	})

	t.Run("a_kind_larger_than_one_page_is_walked_by_cursor", func(t *testing.T) {
		m := flow("cursor", 9005, 600)
		want(t, m, map[string]string{"createFailures": "0", "load": "ok", "ours": "600", "distinct": "600", "cleanupFailures": "0"})
	})

	t.Run("claim_has_exactly_one_winner", func(t *testing.T) {
		m := flow("claim", 9900, 0)
		want(t, m, map[string]string{"create": "ok", "first": "ok true", "second": "ok false", "missing": "NotFound", "listed": "ok true", "cleanup": "ok"})
	})

	poller := fqltApp(t, "fabric_facetql_poller.fct")
	pollerFlow := func(name string) map[string]string {
		t.Helper()
		return fqlLiveFields(t, fqltCall(t, poller, "pollerOut", "pollerRunLive", name, base, "fabtok", ""))
	}

	t.Run("polling_stats_drives_the_existing_optimizer_path", func(t *testing.T) {
		// The version the registry must end up announcing is the server's
		// own, as its /stats reports it.
		req, _ := http.NewRequest("GET", base+"/stats", nil)
		req.Header.Set("x-api-key", "fabtok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var stats struct {
			Version string `json:"version"`
		}
		json.NewDecoder(resp.Body).Decode(&stats)
		resp.Body.Close()
		if stats.Version == "" {
			t.Fatal("the live server reports no version")
		}
		m := pollerFlow("optimizer")
		want(t, m, map[string]string{
			"registeredBefore": "unreachable", "firstOutcome": "Baseline", "secondOutcome": "Sampled",
			"healthAfter": "healthy", "version": stats.Version, "profiles": "1",
			"opsPositive": "true", "coordinate": "4,5", "scored": "true 9003",
		})
	})

	t.Run("run_polls_rounds_an_interval_apart", func(t *testing.T) {
		m := pollerFlow("run")
		want(t, m, map[string]string{"outcome": "Sampled", "health": "healthy", "observations": "true", "waited": "true"})
	})

	t.Run("open_events_streams_the_servers_own_frames", func(t *testing.T) {
		m := flow("events", 0, 0)
		want(t, m, map[string]string{"opened": "true 200 text/event-stream", "write": "true", "seen": "true true"})
		if !strings.HasPrefix(m["bad"], "ERR Unauthorized true facetql refused the token (401): ") {
			t.Errorf("bad token: %q", m["bad"])
		}
	})

	t.Run("a_bad_token_fails_closed_in_the_poller_too", func(t *testing.T) {
		m := pollerFlow("badToken")
		want(t, m, map[string]string{"healthy": "false", "failure": "Unauthorized", "serviceable": "false", "analyzer": "0"})
	})

	t.Run("the_rest_of_the_client_surface", func(t *testing.T) {
		m := flow("surface", 0, 0)
		want(t, m, map[string]string{
			"create":   "ok,ok",
			"get":      `ok true fqlive:a 1/é {"x":1,"y":2,"z":3,"q":4} {"n":1,"tag":"x"} Private`,
			"missing":  "ok false",
			"multiget": "ok fqlive:a 1/é,fqlive:b",
			"count":    "ok 2",
			"query":    "ok fqlive:b next=",
			"refused":  "PreconditionFailed count=2",
			"applied":  `ok b={"n":6}`,
			"cleared":  "ok count=0",
		})
	})
}

// CellMover between two real FacetQL instances: a snapshot in batches,
// writes landing on the source while the copy is live reconciled from the
// SSE feed, a verification that compares every field, a tampered copy
// refused, and a source with an edge refused outright.
func TestFabricFacetqlMoverAgainstRealFacetql(t *testing.T) {
	src := fqlLiveStart(t)
	dst := fqlLiveStart(t)
	ts := fqltApp(t, "fabric_facetql_mover.fct")
	m := fqlLiveFields(t, fqltCall(t, ts, "moverOut", "moverRunLive", "copy", src, dst, "fabtok", ""))
	edge := "true NotIdentical 'src' holds 1 edge(s). FacetQL exposes edges only per node (GET /node/:address/edges/out) and refuses an edge whose far endpoint is not readable on the instance it is written to, so an edge leaving this cell cannot be reconstructed on the destination. Copying the nodes alone would produce a destination that looks complete and is not"
	for k, v := range map[string]string{
		"seeded":      "35",
		"subscribe":   "true",
		"snapshotErr": "",
		"rowsCopied":  "30",
		"writes":      "true,true,true,true",
		"verified":    "verified 30",
		"report":      "30 true 3 3",
		"tampered":    "true failed 'Post:5' differs in `data` between 'src' and 'dst'",
		"edge":        edge,
	} {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	// Batches of 7 over pages of 10: progress is reported per committed
	// batch, with the running totals.
	if !strings.HasPrefix(m["progress"], "7,") || !strings.Contains(m["progress"], ";30,") {
		t.Errorf("progress: %q", m["progress"])
	}
	// Three in-scope writes observed (the Session write is out of scope),
	// all three reconciled and applied.
	if m["observed"] != "3 applied=3 reconciled=3" {
		t.Errorf("feed counts: %q", m["observed"])
	}
	if m["resident"] != "true 230" { // ten 7-byte {"n":i} rows and twenty 8-byte ones
		t.Errorf("resident bytes: %q", m["resident"])
	}
}

// The same mover scenario run by the real Rust CellMover (fabric_check
// `fql-mover`) against its own fresh pair of FacetQL instances: every line
// the port reports must be the line the Rust reports.
func TestFabricFacetqlMoverMatchesRustMover(t *testing.T) {
	bin := laCheck(t)
	rustSrc, rustDst := fqlLiveStart(t), fqlLiveStart(t)
	out, err := exec.Command(bin, "fql-mover", rustSrc, rustDst, "fabtok").Output()
	if err != nil {
		t.Fatalf("fabric_check fql-mover: %v", err)
	}
	rust := fqlLiveFields(t, strings.TrimSuffix(string(out), "\n"))

	src, dst := fqlLiveStart(t), fqlLiveStart(t)
	ts := fqltApp(t, "fabric_facetql_mover.fct")
	port := fqlLiveFields(t, fqltCall(t, ts, "moverOut", "moverRunLive", "copy", src, dst, "fabtok", ""))

	if len(rust) != len(port) {
		t.Fatalf("rust reported %d fields, port %d:\nrust: %v\nport: %v", len(rust), len(port), rust, port)
	}
	for k, v := range rust {
		if port[k] != v {
			t.Errorf("%s:\n port: %q\n rust: %q", k, port[k], v)
		}
	}
}
