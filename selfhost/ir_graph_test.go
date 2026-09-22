package selfhost

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// ir_graph.fct loads a whole compiled app. Every example app, the facets
// showcase, and several of the self-host ports themselves (rich proc bodies)
// must round-trip byte for byte: json.Marshal(graph) in, the port's
// irGraphJSON out, identical.
var irGraphApps = []string{
	"../examples/overlay.fct", "../examples/ledger.fct", "../examples/social.fct",
	"../examples/stage.fct", "../examples/timer.fct", "../examples/metadata.fct",
	"../examples/media.fct", "../examples/secure.fct", "../examples/popover.fct",
	"../examples/identity.fct", "../examples/typing_indicator.fct", "../examples/chirp.fct",
	"../examples/zones.fct", "../examples/service.fct", "../examples/webhook.fct",
	"../examples/daemon.fct", "../examples/trigger.fct", "../examples/counter.fct",
}

// The larger graphs (the facets showcase at ~70 KB of IR, and the self-host
// ports at 200-450 KB) are round-tripped by TestIRGraphRoundTripsLargeApps,
// which is what the runtime's text-builtin performance is measured against.
var irGraphLargeApps = []string{
	"../../facets/f33d3r.fct", "action_stmt.fct", "lower.fct", "database.fct", "ir_view.fct",
}

func loadIRGraphApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("ir_graph.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/ir_graph.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestIRGraphRoundTripsWholeApps(t *testing.T) {
	roundTripGraphs(t, irGraphApps)
}

func TestIRGraphRoundTripsLargeApps(t *testing.T) {
	if testing.Short() {
		t.Skip("large graphs: run without -short")
	}
	roundTripGraphs(t, irGraphLargeApps)
}

func roundTripGraphs(t *testing.T, paths []string) {
	t.Helper()
	ts := loadIRGraphApp(t)
	for _, path := range paths {
		g, err := compile.File(filepath.Join(path))
		if err != nil {
			t.Fatalf("compile %s: %v", path, err)
		}
		want, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		d := postExprJSON(t, ts, "runIRGraphRoundTrip", string(want))
		got, _ := d["irGraphBack"].(string)
		if got != string(want) {
			// Report the first differing position, not two megabytes.
			i := 0
			for i < len(got) && i < len(want) && got[i] == want[i] {
				i++
			}
			lo := i - 120
			if lo < 0 {
				lo = 0
			}
			hiG, hiW := i+160, i+160
			if hiG > len(got) {
				hiG = len(got)
			}
			if hiW > len(want) {
				hiW = len(want)
			}
			t.Errorf("%s differs at byte %d:\n  got  …%s…\n  want …%s…", path, i, got[lo:hiG], want[lo:hiW])
		}
	}
}

// TestIRGraphOneFixture round-trips the single graph named by
// FACET_GRAPH_FIXTURE (a path relative to selfhost/), for profiling the
// interpreter on one large input: go test -run OneFixture -cpuprofile cpu.out.
func TestIRGraphOneFixture(t *testing.T) {
	path := os.Getenv("FACET_GRAPH_FIXTURE")
	if path == "" {
		t.Skip("set FACET_GRAPH_FIXTURE")
	}
	roundTripGraphs(t, []string{path})
}
