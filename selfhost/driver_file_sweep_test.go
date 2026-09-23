package selfhost

// TestDriverFileSweep: every .fct program in this tree — selfhost's own
// ports (the fabric ones included) and the runtime's test programs —
// compiled by both compilers, entry file plus everything it imports.
//
// TestDriverInlineAppSweep covers the inline apps the Go tests carry; this
// covers the whole programs the language's newest forms land in first
// (`shared` cells, `detach` and the daemon-context procs it makes, nested
// field/index writes, `proc main`), so a form the driver has not been
// ported to fails here the day a program uses it. The same rule as the
// inline sweep: the same whole IR when the real compiler accepts the
// program, a refusal when it refuses it. driverFileSweepUnported may name
// programs the driver knowingly does not match yet; it can only shrink.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"facet/internal/compile"
)

var driverFileSweepSources = []string{"*.fct", "../runtime/testdata/*.fct"}

// driverFileSweepUnported: program path → the missing port. Empty: every
// program matches.
var driverFileSweepUnported = map[string]string{}

func TestDriverFileSweep(t *testing.T) {
	var files []string
	for _, pat := range driverFileSweepSources {
		m, err := filepath.Glob(pat)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	// FCT_DRIVER_SWEEP_ONLY narrows the sweep to the named programs, for
	// looking at one failure without waiting for all of them.
	if only := os.Getenv("FCT_DRIVER_SWEEP_ONLY"); only != "" {
		files = strings.Fields(only)
	} else if len(files) < 100 {
		t.Fatalf("found only %d programs — the scan is broken", len(files))
	}
	type result struct{ file, problem string }
	jobs := make(chan string)
	results := make(chan result, len(files))
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		ts, root := loadDriverFileApp(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				_, werr := compile.File(f)
				got, gerr := driverGot(t, ts, root, f)
				problem := ""
				switch {
				case werr != nil && gerr == "":
					problem = fmt.Sprintf("the real compiler refuses it (%v) but the driver accepts it — a check is not ported", werr)
				case werr != nil:
				case gerr != "":
					problem = "the real compiler accepts it but the driver refuses it: " + strings.TrimPrefix(gerr, "driver refused the program: ")
				default:
					g, _ := compile.File(f)
					b, _ := json.Marshal(g)
					var want map[string]interface{}
					json.Unmarshal(b, &want)
					if d := driverDiff(want, got); d != "" {
						problem = "the driver's IR differs from the real compiler's — not ported:\n" + d
					}
				}
				results <- result{f, problem}
			}
		}()
	}
	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	close(results)
	byFile := map[string]string{}
	for r := range results {
		byFile[r.file] = r.problem
	}
	matched := 0
	for _, f := range files {
		problem := byFile[f]
		reason, known := driverFileSweepUnported[f]
		switch {
		case problem == "" && known:
			t.Errorf("%s now matches — remove it from driverFileSweepUnported (%q)", f, reason)
		case problem == "":
			matched++
		case known:
			t.Logf("known unported (%s): %s", reason, problem)
		default:
			t.Errorf("%s: %s", f, problem)
		}
	}
	for f := range driverFileSweepUnported {
		if _, ok := byFile[f]; !ok {
			t.Errorf("driverFileSweepUnported names %s, which no longer exists — remove it", f)
		}
	}
	t.Logf("%d programs: %d match", len(files), matched)
}
