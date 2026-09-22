package selfhost

// driver.fct is the missing root piece the rest of selfhost/ never built: a
// single end-to-end compiler over a WHOLE .fct app's source text
// (parseProgramSrc -> per-declaration lowering -> a native IRGraph ->
// irGraphJSON), reached here as compileSrc(src) -> text through the same
// httptest server every other selfhost test drives.
//
// driver.fct's own header states its scope precisely: it assembles the
// DECLARATION half of a compiled app (enums/records/structs/states/
// derives/policies/entities/actions/procs/services/jobs/webhooks/triggers,
// plus the fixed `auth` table) and deliberately does NOT assemble
// views/pages/bindings/routes/depGraph — the view-compilation stage
// (segs -> binding ids, `for`/`if` -> dynamic regions) has no self-hosted
// port anywhere in this directory. So this test compares only the
// DECLARATION-LEVEL subset of the driver's JSON against the same subset of
// the real compiler's own json.Marshal(*ir.IR) output for every example app
// — structurally (via generic map[string]interface{} equality), not byte
// for byte, since the two emitters don't promise the same key order.
//
// Every example is listed explicitly below, in one of two buckets:
// driverPassingExamples (must match; a regression fails the suite) or
// driverKnownFailing (must NOT yet match, with the reason on record; if one
// starts matching, move it up — the failing set can only shrink, never grow
// silently).

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

func loadDriverApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("driver.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/driver.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// driverSupportedKeys is exactly the set of top-level IR JSON keys
// driver.fct's compileGraph ever populates — the declaration half of the
// graph. Anything else (types/messages/daemons/components/files/api/
// stream/theme/css/assets/routes/pages/bindings/view/depGraph) is real
// compiler surface with no self-hosted port yet, so it is excluded from
// the comparison rather than compared against an empty stand-in.
var driverSupportedKeys = []string{
	"app", "auth", "entities", "records", "structs", "enums",
	"states", "derives", "policies", "actions", "procs", "jobs",
	"webhooks", "triggers", "services",
}

func driverProjectSupported(full map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, k := range driverSupportedKeys {
		if v, ok := full[k]; ok {
			out[k] = v
		}
	}
	return out
}

// driverWant compiles path with the real compiler and returns the
// declaration-level subset of its IR as a generic map.
func driverWant(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	g, err := compile.File(path)
	if err != nil {
		t.Fatalf("real compiler refused %s: %v", path, err)
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var full map[string]interface{}
	if err := json.Unmarshal(b, &full); err != nil {
		t.Fatal(err)
	}
	return driverProjectSupported(full)
}

// driverGot runs compileSrc on src through the live driver server and
// returns the declaration-level subset of its own IR JSON as a generic map.
func driverGot(t *testing.T, ts *httptest.Server, src string) (map[string]interface{}, string) {
	t.Helper()
	d := postExprJSON(t, ts, "runCompileSrc", src)
	j, ok := d["compiledOut"].(string)
	if !ok {
		t.Fatalf("no compiledOut delta: %+v", d)
	}
	var full map[string]interface{}
	if err := json.Unmarshal([]byte(j), &full); err != nil {
		return nil, fmt.Sprintf("compiledOut is not valid JSON: %v\nraw: %s", err, j)
	}
	return driverProjectSupported(full), ""
}

// driverDiff describes the first top-level key that disagrees, for a short,
// pointed failure/limitation message instead of a two-map dump.
func driverDiff(want, got map[string]interface{}) string {
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		wv, wok := want[k]
		gv, gok := got[k]
		if wok != gok {
			return fmt.Sprintf("key %q: want present=%v, got present=%v", k, wok, gok)
		}
		if !reflect.DeepEqual(wv, gv) {
			wb, _ := json.Marshal(wv)
			gb, _ := json.Marshal(gv)
			ws, gs := string(wb), string(gb)
			if len(ws) > 400 {
				ws = ws[:400] + "…"
			}
			if len(gs) > 400 {
				gs = gs[:400] + "…"
			}
			return fmt.Sprintf("key %q differs:\n  want %s\n  got  %s", k, ws, gs)
		}
	}
	return ""
}

func driverReadSrc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// driverPassingExamples: the declaration-level IR matches the real
// compiler's exactly today. A regression here is a real bug in driver.fct
// or in whatever selfhost port it depends on.
var driverPassingExamples = []string{
	"../examples/counter.fct",
	"../examples/timer.fct",
	"../examples/webhook.fct",
	"../examples/trigger.fct",
	"../examples/daemon.fct",
	"../examples/echo_daemon.fct",
	"../examples/fieldauth.fct",
	"../examples/identity.fct",
	"../examples/media.fct",
	"../examples/overlay.fct",
	"../examples/popover.fct",
	"../examples/service.fct",
	"../examples/stage.fct",
}

// driverKnownFailing: does NOT yet match, with the concrete reason
// (verified against the actual diff, not guessed) a specific unported
// piece causes it. If one of these starts matching, move its name up into
// driverPassingExamples rather than leaving it here. Three distinct real
// causes, each confirmed against the real compiler's output:
//
//  1. qualifyRowRefs (internal/parser/parser.go): the real parser rewrites
//     a bare field name inside an entity's `read:`/`derive` expression into
//     `Get{Ref{"$row"}, name}` before Build ever sees it. The self-hosted
//     parseExprTree never does this, so the same expression lowers as a
//     bare "ref" here instead of a "get" of "$row" — confirmed on
//     typing_indicator.fct's `Typing.read: who != actor` (want has
//     `$row.who`, got has a bare `who` ref) and zones.fct's `Position.read`
//     agg predicate.
//  2. view-level index inference (internal/ir/build.go's comparedItemFields/
//     markIndex, called only from the VIEW range-node compiler, never from
//     an action's own `for`): a field referenced in a VIEW's
//     `for x in E by field` or `for x in E where x.field ...` is marked
//     Index:true on the entity. Views are entirely outside this driver's
//     declared scope (no view-compilation port exists at all), so this
//     driver never sees the view and never marks the field — confirmed on
//     chirp.fct/social.fct (`by created desc`), inbox.fct
//     (`where m.to == 1 by sent desc`), ledger.fct (`by amount desc`), and
//     secure.fct (`by created desc`), each a bare index:true diff on
//     exactly the field its view orders/filters by.
//  3. theme is unparsed at the Program level, not just unassembled by this
//     driver: program.fct's own declKeywordOf/parseProgramSrc dispatch has
//     no case for the `theme`/`theme dark`/`css` keywords at all (they
//     fall into unknownKeywords), so this driver has no way to know a dark
//     or named theme was declared — and the real compiler injects a
//     synthetic `@client` state named `theme` whenever one is, which
//     confirmed on metadata.fct (`theme dark:` present; want's states
//     include a synthetic "theme" cell, got's don't).
//  4. import resolution: parseProgramSrc parses exactly one file's own
//     text and has no `import` keyword case at all — an `import "x.fct"`
//     line falls into unknownKeywords and whatever declarations that file
//     contributes are never merged in. The real compiler (compile.File)
//     does resolve and merge imports, so any example that imports another
//     .fct file disagrees with this driver by exactly the imported
//     declarations — confirmed on fabric_node.fct, whose `want` actions
//     list has one extra action (from its `import "fabric_protocol.fct"`,
//     which merges in that file's own protocol-demo driver action) that
//     `got` never sees.
var driverKnownFailing = map[string]string{
	"../examples/chirp.fct":            "view-level index inference (cause 2): `for p in Post by created desc` marks Post.created indexed; view compilation isn't ported",
	"../examples/social.fct":           "view-level index inference (cause 2): `for p in Post by created desc` marks Post.created indexed",
	"../examples/ledger.fct":           "view-level index inference (cause 2): `for o in Order by amount desc` marks Order.amount indexed",
	"../examples/inbox.fct":            "view-level index inference (cause 2): `for m in Message where m.to == 1 by sent desc` marks Message.sent indexed",
	"../examples/secure.fct":           "view-level index inference (cause 2): `for n in Note by created desc` marks Note.created indexed",
	"../examples/zones.fct":            "qualifyRowRefs (cause 1): Position.read's agg predicate reads $row.zone; self-hosted parseExprTree never qualifies it",
	"../examples/typing_indicator.fct": "qualifyRowRefs (cause 1): Typing.read (`who != actor`) reads $row.who; self-hosted parseExprTree never qualifies it",
	"../examples/metadata.fct":         "theme unparsed at the Program level (cause 3): `theme dark:` should inject a synthetic @client `theme` state; program.fct has no theme dispatch case at all",
	"../examples/fabric_node.fct":      "no import resolution (cause 4): this file's `import \"fabric_protocol.fct\"` merges in that file's own extra action in the real compiler; parseProgramSrc never resolves imports",
}

func TestDriverCompileSrcMatchesCompiler(t *testing.T) {
	ts := loadDriverApp(t)

	all := append([]string{}, driverPassingExamples...)
	for name := range driverKnownFailing {
		all = append(all, name)
	}
	sort.Strings(all)

	type row struct {
		name   string
		status string
		detail string
	}
	var table []row

	for _, path := range all {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			src := driverReadSrc(t, path)
			want := driverWant(t, path)
			got, parseErr := driverGot(t, ts, src)
			reason, known := driverKnownFailing[path]

			if parseErr != "" {
				if known {
					table = append(table, row{path, "KNOWN-FAIL", parseErr})
					t.Logf("known limitation (%s): %s", reason, parseErr)
					return
				}
				t.Fatalf("compileSrc produced unparseable output: %s", parseErr)
			}

			diff := driverDiff(want, got)
			if diff == "" {
				if known {
					t.Fatalf("example %s now matches the real compiler — remove it from driverKnownFailing and add it to driverPassingExamples", path)
				}
				table = append(table, row{path, "PASS", ""})
				return
			}

			if known {
				table = append(table, row{path, "KNOWN-FAIL", reason})
				t.Logf("known limitation (%s):\n%s", reason, diff)
				return
			}
			t.Fatalf("declaration-level IR mismatch:\n%s", diff)
		})
	}

	t.Logf("driver.fct compileSrc vs. real compiler, declaration-level IR, %d examples:", len(table))
	for _, r := range table {
		t.Logf("  %-8s %s", r.status, r.name)
	}
}
