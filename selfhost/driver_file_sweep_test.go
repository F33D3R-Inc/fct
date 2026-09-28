package selfhost

// TestDriverFileSweep: every .fct program under the trees the self-hosted
// compiler must own — selfhost's own ports (the fabric ones included) and
// their testdata, the runtime's test programs, every example, and the
// product facets — compiled by both compilers, entry file plus everything
// it imports. A file that is only ever imported (a module) is compiled as
// an entry too: both compilers then refuse it, or both accept it.
//
// TestDriverInlineAppSweep covers the inline apps the Go tests carry; this
// covers the whole programs the language's newest forms land in first
// (`shared` cells, `detach` and the daemon-context procs it makes, nested
// field/index writes, `proc main`), so a form the driver has not been
// ported to fails here the day a program uses it. When the real compiler
// accepts the program the driver's IR must be json.Marshal's bytes exactly
// (every number and string spelled as encoding/json spells it); when it
// refuses it, the driver must refuse it with the same text, compile.File's
// `file: line N: …` included. driverFileSweepUnported may name
// programs the driver knowingly does not match yet; it can only shrink.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"facet/internal/compile"
)

// driverFileSweepRoots are walked recursively for .fct files.
var driverFileSweepRoots = []string{".", "../runtime/testdata", "../examples", "../../facets"}

// driverFileSweepUnported: program path → the missing port. Empty: every
// program matches.
var driverFileSweepUnported = map[string]string{}

func TestDriverFileSweep(t *testing.T) {
	var files []string
	for _, root := range driverFileSweepRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".fct") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
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
				g, werr := compile.File(f)
				raw := driverGotRaw(t, ts, root, f)
				var b []byte
				if werr == nil {
					b, _ = json.Marshal(g)
				}
				problem := ""
				// the IR must be the real compiler's json.Marshal bytes; a
				// mismatch is decoded and diffed to say where it differs
				if werr != nil || raw != string(b) {
					got, gerr := driverDecodeRaw(raw)
					switch {
					case werr != nil && gerr == "":
						problem = fmt.Sprintf("the real compiler refuses it (%v) but the driver accepts it — a check is not ported", werr)
					case werr != nil:
						// both refuse: with the same words, `file: line N:`
						// included, as compile.File's error spells them
						if got := strings.TrimPrefix(gerr, "driver refused the program: "); got != werr.Error() {
							problem = fmt.Sprintf("both refuse it, but in different words:\n want %s\n got  %s", werr.Error(), got)
						}
					case gerr != "":
						problem = "the real compiler accepts it but the driver refuses it: " + strings.TrimPrefix(gerr, "driver refused the program: ")
					default:
						var want map[string]interface{}
						json.Unmarshal(b, &want)
						if d := driverDiff(want, got); d != "" {
							problem = "the driver's IR differs from the real compiler's — not ported:\n" + d
						} else {
							problem = "the same IR spelled differently — " + driverByteDiff(string(b), raw)
						}
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
	t.Logf("%d programs: %d match, byte for byte", len(files), matched)
}

// driverByteDiff names the first byte where the driver's IR text departs
// from json.Marshal's (a number or string spelled another way), with context.
func driverByteDiff(want, got string) string {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	from := i - 80
	if from < 0 {
		from = 0
	}
	clip := func(s string) string {
		end := i + 80
		if end > len(s) {
			end = len(s)
		}
		if from > len(s) {
			return ""
		}
		return s[from:end]
	}
	return fmt.Sprintf("first difference at byte %d:\n want …%s…\n got  …%s…", i, clip(want), clip(got))
}
