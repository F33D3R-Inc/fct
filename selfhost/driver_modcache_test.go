package selfhost

// The driver's module cache (driver.fct's driverModCache): a module parsed
// for one program is reused by every later program of the same driver
// process that imports it, keyed by its path and its whole source. A
// memoised compile must be exactly the cold one — the same bytes — and a
// module whose file changed must be parsed anew, never served stale.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modCacheLib = `app Lib:
    struct Pair:
        a: int
        b: text
    type Card:
        title: text
        n: int
    proc pairOf(n: int) -> Pair:
        let k = do bump(n)
        return Pair{a: k, b: "p" + k}
    private proc bump(n: int) -> int:
        return n + STEP
`

const modCacheA = `import "lib.fct"

app A:
    state total: int = 0
    proc twice(n: int) -> int:
        let p = do pairOf(n)
        return p.a * 2
    action add(n: int):
        let t = do twice(n)
        total = total + t
    action card(n: int) -> Card:
        return Card{title: "a", n: n}
    api GET "/api/card" -> card
    view Home at "/":
        text "{total}"
`

const modCacheB = `import "lib.fct"

app B:
    state label: text = ""
    proc name(n: int) -> text:
        let p = do pairOf(n)
        return p.b
    action set(n: int):
        let s = do name(n)
        label = s
    view Home at "/":
        text "{label}"
`

// writeModCacheProgram lays out lib.fct (its private proc adding step),
// a.fct and b.fct, both importing it, under dir.
func writeModCacheProgram(t *testing.T, dir, step string) {
	t.Helper()
	files := map[string]string{
		"lib.fct": strings.ReplaceAll(modCacheLib, "STEP", step),
		"a.fct":   modCacheA,
		"b.fct":   modCacheB,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// modCacheCompile runs runCompileFile and returns the IR text exactly as
// the driver wrote it, and cachedModules when the response reports it.
func modCacheCompile(t *testing.T, post func(action string, args ...any) map[string]any, root, rel string) (string, float64, bool) {
	t.Helper()
	d := post("runCompileFile", root, rel)
	out, ok := d["compiledOut"].(string)
	if !ok {
		t.Fatalf("%s: no compiledOut delta: %+v", rel, d)
	}
	if strings.HasPrefix(out, `{"error"`) {
		t.Fatalf("%s: the driver refused it: %s", rel, out)
	}
	n, has := d["cachedModules"].(float64)
	return out, n, has
}

func TestDriverModuleCacheMatchesCold(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FACET_DATA_DIR", root)
	writeModCacheProgram(t, root, "1")

	// cold: every program on a driver that has compiled nothing before
	cold := func(rel string) string {
		ts := loadDriverApp(t)
		defer ts.Close()
		post := func(action string, args ...any) map[string]any { return postExprJSON(t, ts, action, args...) }
		out, n, _ := modCacheCompile(t, post, root, rel)
		if n != 2 {
			t.Fatalf("a cold compile of %s cached %v modules, want 2 (it and lib.fct)", rel, n)
		}
		return out
	}
	coldA, coldB := cold("a.fct"), cold("b.fct")
	if coldA == coldB {
		t.Fatal("a.fct and b.fct compiled to the same IR — the fixture is broken")
	}

	// warm: one driver, the same programs in turn, lib.fct parsed once
	ts := loadDriverApp(t)
	post := func(action string, args ...any) map[string]any { return postExprJSON(t, ts, action, args...) }
	cached := 0.0
	step := func(rel, want string, wantCached float64, what string) {
		t.Helper()
		got, n, has := modCacheCompile(t, post, root, rel)
		if has {
			cached = n
		}
		if cached != wantCached {
			t.Fatalf("%s: %v modules cached, want %v", what, cached, wantCached)
		}
		if got != want {
			t.Fatalf("%s: the memoised compile differs from the cold one\n cold: %.300s\n warm: %.300s", what, want, got)
		}
	}
	step("a.fct", coldA, 2, "a.fct first")
	step("b.fct", coldB, 3, "b.fct reusing lib.fct")
	step("a.fct", coldA, 3, "a.fct again, every module cached")

	// lib.fct changes: the next compile parses the new content
	writeModCacheProgram(t, root, "5")
	coldB5 := cold("b.fct")
	if coldB5 == coldB {
		t.Fatal("changing lib.fct did not change b.fct's IR — the fixture is broken")
	}
	step("b.fct", coldB5, 4, "b.fct after lib.fct changed")
}
