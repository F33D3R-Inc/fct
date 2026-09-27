package selfhost

// The self-hosted runtime's boot: the compiler's IR document (facets/api's
// 4 MB one) read by ir_json.fct's parser and built into an IRGraph. The
// budget is per byte of document plus a floor, so a change that makes the
// loader super-linear, or per-character where it should be per-token, is
// caught here — not when a runtime takes half a minute to boot.
//
// History: 11.6 s for this document before ir_json.fct read strings by
// search (the `indexOf` builtin) and objects in one pass over their keys;
// ~3 s after, on a loaded machine (~0.7 µs/byte).

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"facet/internal/compile"
)

func TestIRGraphBootLoadWithinBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("loads a multi-megabyte IR: run without -short")
	}
	g, err := compile.File("../../facets/api/main.fct")
	if err != nil {
		t.Fatalf("compile facets/api/main.fct: %v", err)
	}
	doc, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc) < 1<<20 {
		t.Fatalf("facets/api/main.fct's IR is %d bytes — the budget below assumes a multi-megabyte document; pick a larger fixture", len(doc))
	}
	ts := loadIRGraphApp(t)
	budget := time.Second + time.Duration(2*len(doc))*time.Microsecond
	start := time.Now()
	d := postExprJSON(t, ts, "runIRGraphLoad", string(doc))
	took := time.Since(start)
	got, _ := d["irGraphLoaded"].(string)
	want := fmt.Sprintf("procs=%d actions=%d entities=%d pages=%d", len(g.Procs), len(g.Actions), len(g.Entities), len(g.Pages))
	if got != want {
		t.Fatalf("loaded graph summary = %q, want %q", got, want)
	}
	t.Logf("%d bytes loaded in %s (%.2f µs/byte, budget %s)", len(doc), took, float64(took.Microseconds())/float64(len(doc)), budget)
	if took > budget {
		t.Fatalf("loading %d bytes of IR took %s, over the budget of %s (1 s + 2 µs/byte)", len(doc), took, budget)
	}
}
