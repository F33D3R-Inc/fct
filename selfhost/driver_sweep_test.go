package selfhost

// TestDriverInlineAppSweep: every inline app in the Go compiler's and the
// runtime's own tests, compiled by both compilers.
//
// Those tests are where a new language feature lands first — each round of
// compiler work adds `app X:` sources to runtime/*_test.go and
// internal/compile/*_test.go that exercise it. This test finds every one
// (a backquoted Go string beginning `app Name:`), compiles it with the real
// compiler (compile.String) and with the self-hosted driver (compileSrc),
// and requires the two to agree: the same whole IR when the real compiler
// accepts the app, a refusal when it refuses it. So a feature the driver has
// not been ported to fails here the day its first test is written, naming
// the file, the app and the first IR path that differs (or the refusal that
// is missing) — drift between the Go compiler and selfhost cannot pass
// unnoticed.
//
// driverSweepUnported may list apps (by driverSweepKey) the driver
// knowingly does not match yet, each with the missing port named. It can
// only shrink: an entry whose app now matches, or that names no app any
// more, fails the test.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"facet/internal/compile"
)

// driverSweepSources are the test files scanned for inline apps.
var driverSweepSources = []string{
	"../runtime/*_test.go",
	"../internal/compile/*_test.go",
}

// driverSweepUnported: apps the driver does not match yet — key → the
// missing port. Empty: every inline app matches.
var driverSweepUnported = map[string]string{}

var inlineAppRe = regexp.MustCompile("(?s)`(app [A-Za-z_][A-Za-z0-9_]*:\n.*?)`")

// driverSweepKey names an inline app stably: its file, its app name and a
// short hash of its source (an index would shift whenever a test is added).
func driverSweepKey(file, src string) string {
	name := strings.TrimSuffix(strings.Fields(src)[1], ":")
	sum := sha256.Sum256([]byte(src))
	return filepath.Base(file) + ":" + name + ":" + hex.EncodeToString(sum[:4])
}

type sweepApp struct {
	key, file, src string
}

func collectInlineApps(t *testing.T) []sweepApp {
	t.Helper()
	var out []sweepApp
	seen := map[string]bool{}
	for _, pat := range driverSweepSources {
		files, err := filepath.Glob(pat)
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(files)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range inlineAppRe.FindAllStringSubmatch(string(b), -1) {
				k := driverSweepKey(f, m[1])
				if seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, sweepApp{key: k, file: f, src: m[1]})
			}
		}
	}
	return out
}

func TestDriverInlineAppSweep(t *testing.T) {
	ts := loadDriverApp(t)
	apps := collectInlineApps(t)
	if len(apps) < 150 {
		t.Fatalf("found only %d inline apps — the scan is broken", len(apps))
	}
	stillUnported := map[string]bool{}
	matched, refusedBoth := 0, 0
	for _, a := range apps {
		a := a
		t.Run(a.key, func(t *testing.T) {
			g, werr := compile.String(a.src)
			got, gerr := driverGotSrc(t, ts, a.src)
			var problem string
			switch {
			case werr != nil && gerr == "":
				problem = fmt.Sprintf("the real compiler refuses it (%v) but the driver accepts it — a check is not ported", werr)
			case werr != nil:
				refusedBoth++
				return
			case gerr != "":
				problem = fmt.Sprintf("the real compiler accepts it but the driver refuses it: %s", strings.TrimPrefix(gerr, "driver refused the program: "))
			default:
				b, _ := json.Marshal(g)
				var want map[string]interface{}
				json.Unmarshal(b, &want)
				if d := driverDiff(want, got); d != "" {
					problem = "the driver's IR differs from the real compiler's — not ported:\n" + d
				}
			}
			reason, known := driverSweepUnported[a.key]
			if problem == "" {
				matched++
				if known {
					t.Fatalf("%s now matches — remove it from driverSweepUnported (%q)", a.key, reason)
				}
				return
			}
			if known {
				stillUnported[a.key] = true
				t.Logf("known unported (%s): %s", reason, problem)
				return
			}
			t.Fatalf("inline app %s in %s: %s\n--- source ---\n%s", a.key, a.file, problem, a.src)
		})
	}
	for k := range driverSweepUnported {
		if !stillUnported[k] {
			found := false
			for _, a := range apps {
				found = found || a.key == k
			}
			if !found {
				t.Errorf("driverSweepUnported names %s, which no longer exists — remove it", k)
			}
		}
	}
	t.Logf("%d inline apps: %d match, %d refused by both", len(apps), matched, refusedBoth)
}
