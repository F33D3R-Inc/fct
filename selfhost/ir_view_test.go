package selfhost

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// ir_view.fct reads and re-emits the compiler's pages. The repo's own example
// apps (and the facets library's showcase) exercise every node kind through
// the real compiler, so every page, node, binding and route of each must
// round-trip byte for byte through the port.
var irViewExamples = []string{
	"../examples/overlay.fct", "../examples/ledger.fct", "../examples/social.fct",
	"../examples/stage.fct", "../examples/timer.fct", "../examples/metadata.fct",
	"../examples/media.fct", "../examples/secure.fct", "../examples/popover.fct",
	"../examples/identity.fct", "../examples/typing_indicator.fct", "../examples/chirp.fct",
	"../examples/zones.fct", "../../facets/f33d3r.fct",
}

func loadIRViewApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("ir_view.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/ir_view.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestIRViewRoundTripsPagesAndRoutes(t *testing.T) {
	ts := loadIRViewApp(t)
	check := func(kind string, v any, label string) {
		t.Helper()
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		d := postExprJSON(t, ts, "runIRViewRoundTrip", kind, string(want))
		if got, _ := d["irViewBack"].(string); got != string(want) {
			t.Errorf("%s %s:\n  got  %s\n  want %s", kind, label, got, want)
		}
	}
	pages, nodes := 0, 0
	for _, path := range irViewExamples {
		g, err := compile.File(filepath.Join(path))
		if err != nil {
			t.Fatalf("compile %s: %v", path, err)
		}
		for _, p := range g.Pages {
			pages++
			check("page", p, path+" "+p.Name)
			for i, n := range p.View {
				nodes++
				check("node", n, fmt.Sprintf("%s %s/%s#%d", path, p.Name, n.Kind, i))
			}
			for _, b := range p.Bindings {
				check("binding", b, path+" "+b.ID)
			}
		}
		for _, r := range g.Routes {
			check("route", r, path+" "+r.Path)
		}
	}
	if pages < 15 || nodes < 15 {
		t.Fatalf("the examples exercised only %d pages and %d top-level nodes", pages, nodes)
	}
}
