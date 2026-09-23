package selfhost

// driver.fct is the end-to-end self-hosted compiler over a WHOLE .fct
// app's source text (parseProgramSrc -> per-declaration lowering ->
// view_build.fct's view compiler -> a native IRGraph -> irGraphJSON),
// reached here as compileSrc(src) -> text through the same httptest server
// every other selfhost test drives.
//
// This test compares the driver's WHOLE IR against the real compiler's own
// json.Marshal(*ir.IR) output for every example app — every top-level key,
// declarations and views/pages/bindings/routes/depGraph alike —
// structurally (via generic map[string]interface{} equality), not byte for
// byte, since the two emitters don't promise the same key order.
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
	"strings"
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

// driverWant compiles path with the real compiler and returns its whole IR
// as a generic map.
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
	return full
}

// driverGot runs compileFile on path through the live driver server — the
// self-hosted counterpart of compile.File: the entry file plus every file
// it imports, read through the io.file capability from the sandboxed data
// directory (the fct checkout, see loadDriverFileApp) — and returns its
// whole IR JSON as a generic map.
func driverGot(t *testing.T, ts *httptest.Server, root, path string) (map[string]interface{}, string) {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		t.Fatal(err)
	}
	d := postExprJSON(t, ts, "runCompileFile", root, filepath.ToSlash(rel))
	return driverDecode(t, d)
}

// driverGotSrc runs compileSrc on one file's source text (no imports).
func driverGotSrc(t *testing.T, ts *httptest.Server, src string) (map[string]interface{}, string) {
	t.Helper()
	d := postExprJSON(t, ts, "runCompileSrc", src)
	return driverDecode(t, d)
}

func driverDecode(t *testing.T, d map[string]interface{}) (map[string]interface{}, string) {
	t.Helper()
	j, ok := d["compiledOut"].(string)
	if !ok {
		t.Fatalf("no compiledOut delta: %+v", d)
	}
	var full map[string]interface{}
	if err := json.Unmarshal([]byte(j), &full); err != nil {
		return nil, fmt.Sprintf("compiledOut is not valid JSON: %v\nraw: %s", err, j)
	}
	if e, bad := full["error"]; bad {
		return nil, fmt.Sprintf("driver refused the program: %v", e)
	}
	return full, ""
}

// loadDriverFileApp boots driver.fct with its io.file sandbox rooted at the
// workspace that holds fct, facets/, website/ and apps/, so compileFile can
// read every program under test and whatever it imports.
func loadDriverFileApp(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACET_DATA_DIR", root)
	return loadDriverApp(t), root
}

// driverDiff describes the first place the two IRs disagree — the path
// down to the deepest differing value, not a two-map dump.
func driverDiff(want, got map[string]interface{}) string {
	return driverPathDiff("", want, got)
}

func driverPathDiff(path string, want, got interface{}) string {
	switch w := want.(type) {
	case map[string]interface{}:
		g, ok := got.(map[string]interface{})
		if !ok {
			return driverShowDiff(path, want, got)
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		var sorted []string
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			wv, wok := w[k]
			gv, gok := g[k]
			if wok != gok {
				return fmt.Sprintf("%s.%s: want present=%v, got present=%v", path, k, wok, gok)
			}
			if d := driverPathDiff(path+"."+k, wv, gv); d != "" {
				return d
			}
		}
		return ""
	case []interface{}:
		g, ok := got.([]interface{})
		if !ok || len(g) != len(w) {
			return driverShowDiff(path, want, got)
		}
		for i := range w {
			if d := driverPathDiff(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); d != "" {
				return d
			}
		}
		return ""
	}
	if !reflect.DeepEqual(want, got) {
		return driverShowDiff(path, want, got)
	}
	return ""
}

func driverShowDiff(path string, want, got interface{}) string {
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(got)
	ws, gs := string(wb), string(gb)
	if len(ws) > 600 {
		ws = ws[:600] + "…"
	}
	if len(gs) > 600 {
		gs = gs[:600] + "…"
	}
	return fmt.Sprintf("%s differs:\n  want %s\n  got  %s", path, ws, gs)
}

func driverReadSrc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// driverPassingExamples: the whole IR matches the real
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
	"../examples/zones.fct",
	"../examples/chirp.fct",
	"../examples/inbox.fct",
	"../examples/ledger.fct",
	"../examples/secure.fct",
	"../examples/social.fct",
	"../examples/typing_indicator.fct",
	"../examples/metadata.fct",
	"../examples/fabric_node.fct",
	"../examples/modular/app.fct",
	"../examples/layered/playground.fct",
	"testdata/streams_app.fct",
	"testdata/routes/app.fct",
	"testdata/proc_derive.fct",
	"testdata/cssfrom/app.fct",
	"testdata/files.fct",
	"testdata/dispatch.fct",
	"../../facets/f33d3r.fct",
	"../../facets/home.fct",
	"../../facets/live.fct",
	"../../facets/messages.fct",
	"../../facets/smoke.fct",
	"../../facets/smoke_new.fct",
	"../../facets/smoke_new2.fct",
	"../../facets/smoke_new3.fct",
	"../../facets/smoke_new4.fct",
	"../../facets/smoke_new5.fct",
	"../../facets/smoke_new6.fct",
	"../../facets/smoke_new7.fct",
	"../../facets/smoke_new8.fct",
	"../../facets/smoke_new9.fct",
	"../../facets/smoke_new10.fct",
	"../../facets/timeline.fct",
	"../../facets/api/main.fct",
	"../../website/site.fct",
	"../../apps/game/game.fct",
}

// driverKnownFailing: does NOT yet match, with the concrete reason
// (verified against the actual diff, not guessed) a specific unported
// piece causes it. If one of these starts matching, move its name up into
// driverPassingExamples rather than leaving it here. Empty: every example
// app compiles to the real compiler's whole IR.
var driverKnownFailing = map[string]string{}

func TestDriverCompileSrcMatchesCompiler(t *testing.T) {
	ts, root := loadDriverFileApp(t)

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
			want := driverWant(t, path)
			got, parseErr := driverGot(t, ts, root, path)
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
			t.Fatalf("IR mismatch:\n%s", diff)
		})
	}

	t.Logf("driver.fct compileSrc vs. real compiler, whole IR, %d examples:", len(table))
	for _, r := range table {
		t.Logf("  %-8s %s", r.status, r.name)
	}
}

// TestDriverRouteCollision: two modules claiming one HTTP route are refused
// before the merge, naming both files, exactly as the real compiler does.
func TestDriverRouteCollision(t *testing.T) {
	ts, root := loadDriverFileApp(t)
	path := "testdata/routes/clash.fct"
	_, werr := compile.File(path)
	if werr == nil {
		t.Fatal("real compiler accepted a route declared by two modules")
	}
	_, gerr := driverGot(t, ts, root, path)
	if want := "driver refused the program: " + werr.Error(); gerr != want {
		t.Fatalf("driver refusal\n got: %s\nwant: %s", gerr, want)
	}
}

// TestDriverAuthorityOnlyCalls: a proc — or a projection that calls one —
// runs only on the authority, so a view, a policy or a plain derive that
// calls it is refused with the real compiler's own diagnostic.
func TestDriverAuthorityOnlyCalls(t *testing.T) {
	ts := loadDriverApp(t)
	const head = `app P:
    type CardDTO:
        score: int
    entity Post:
        id: int
        title: text
    proc scoreOf(t: text) -> int:
        return len(t) * 2
    derive cardDTO(p: Post): CardDTO = CardDTO{score: scoreOf(p.title)}
`
	cases := map[string]string{
		"view calls proc":       head + "    view Home at \"/\":\n        text \"{scoreOf(\"x\")}\"\n",
		"view calls projection": head + "    view Home at \"/\":\n        for p in Post:\n            text \"{cardDTO(p).score}\"\n",
		"policy calls proc":     head + "    policy long:\n        scoreOf(actor) > 4\n    view Home at \"/\":\n        text \"x\"\n",
		"derive calls proc":     head + "    derive total: int = scoreOf(\"abc\")\n    view Home at \"/\":\n        text \"x\"\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, werr := compile.String(src)
			if werr == nil {
				t.Fatal("real compiler accepted it")
			}
			_, gerr := driverGotSrc(t, ts, src)
			if gerr == "" || !strings.Contains(werr.Error(), strings.TrimPrefix(gerr, "driver refused the program: ")) {
				t.Fatalf("driver refusal\n got: %s\nwant: %s", gerr, werr)
			}
		})
	}
}

// TestDriverCSSCommentRule: a `#id`-anchored rule inside a `css:` block
// would be silently dropped as a comment, so it is refused — while a plain
// `#` section header in the same block is left alone.
func TestDriverCSSCommentRule(t *testing.T) {
	ts := loadDriverApp(t)
	src := "app C:\n    css:\n        # buttons\n        .b { color: red }\n        #fa-root .b { border: none }\n        .c { color: blue }\n    view Home at \"/\":\n        text \"x\"\n"
	_, werr := compile.String(src)
	if werr == nil {
		t.Fatal("real compiler accepted a #-anchored css rule")
	}
	_, gerr := driverGotSrc(t, ts, src)
	if want := "driver refused the program: " + werr.Error(); gerr != want {
		t.Fatalf("driver refusal\n got: %s\nwant: %s", gerr, want)
	}
	ok := "app C:\n    css:\n        # buttons\n        .b { color: red }\n    view Home at \"/\":\n        text \"x\"\n"
	want := func() map[string]interface{} {
		g, err := compile.String(ok)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(g)
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		return m
	}()
	got, perr := driverGotSrc(t, ts, ok)
	if perr != "" {
		t.Fatal(perr)
	}
	if d := driverDiff(want, got); d != "" {
		t.Fatalf("IR mismatch:\n%s", d)
	}
}

// TestDriverStringEscapes: every quoted string — a theme value, view text,
// an expression literal — decodes exactly as strconv.Unquote does (the
// simple escapes, \x/octal bytes, \u/\U runes), and an invalid escape is
// refused.
func TestDriverStringEscapes(t *testing.T) {
	ts := loadDriverApp(t)
	srcs := map[string]string{
		"theme":   "app T:\n    theme:\n        accent \"\\u00e9\\x41\\101\\\"q\\\"\"\n        font \"\\\"Inter\\\", sans-serif\"\n    view Home at \"/\":\n        text \"x\"\n",
		"text":    "app T:\n    state n: int = 1\n    view Home at \"/\":\n        text \"tab\\there \\u263A {n} \\\\ end\"\n        text \"bytes \\xc3\\xa9 and \\U0001F600\"\n",
		"literal": "app T:\n    state s: text = \"a\\tb\\u00e9\"\n    derive d: text = s + \"\\x21\\n\"\n    view Home at \"/\":\n        text \"{d}\"\n",
	}
	for name, src := range srcs {
		t.Run(name, func(t *testing.T) {
			g, err := compile.String(src)
			if err != nil {
				t.Fatalf("real compiler refused: %v", err)
			}
			b, _ := json.Marshal(g)
			var want map[string]interface{}
			json.Unmarshal(b, &want)
			got, perr := driverGotSrc(t, ts, src)
			if perr != "" {
				t.Fatal(perr)
			}
			if d := driverDiff(want, got); d != "" {
				t.Fatalf("IR mismatch:\n%s", d)
			}
		})
	}
	bad := "app T:\n    theme:\n        accent \"\\q\"\n    view Home at \"/\":\n        text \"x\"\n"
	_, werr := compile.String(bad)
	if werr == nil {
		t.Fatal("real compiler accepted an invalid escape")
	}
	if _, perr := driverGotSrc(t, ts, bad); perr != "driver refused the program: "+werr.Error() {
		t.Fatalf("driver refusal\n got: %s\nwant: %s", perr, werr)
	}
}

// TestDriverCSSFromRefusals: a missing sibling stylesheet names the file,
// line and path; a snippet (no file to resolve against) refuses `css from`.
func TestDriverCSSFromRefusals(t *testing.T) {
	ts, root := loadDriverFileApp(t)
	path := "testdata/cssfrom/missing.fct"
	_, werr := compile.File(path)
	if werr == nil {
		t.Fatal("real compiler accepted a missing stylesheet")
	}
	if _, gerr := driverGot(t, ts, root, path); gerr != "driver refused the program: "+werr.Error() {
		t.Fatalf("driver refusal\n got: %s\nwant: %s", gerr, werr)
	}
	src := "app S:\n    css from \"a.css\"\n    view Home at \"/\":\n        text \"x\"\n"
	_, werr = compile.String(src)
	if werr == nil {
		t.Fatal("real compiler accepted css from in a snippet")
	}
	if _, gerr := driverGotSrc(t, ts, src); gerr != "driver refused the program: "+werr.Error() {
		t.Fatalf("driver refusal\n got: %s\nwant: %s", gerr, werr)
	}
}

// TestDriverFileRefusals: a `file` declaration's shape and build-time
// checks refuse exactly as the real compiler does.
func TestDriverFileRefusals(t *testing.T) {
	ts := loadDriverApp(t)
	tail := "    view Home at \"/\":\n        text \"x\"\n"
	cases := map[string]string{
		"redeclared": "app F:\n    file A: text at \"a.txt\"\n    file A: text at \"b.txt\"\n" + tail,
		"absolute":   "app F:\n    file A: text at \"/etc/a.txt\"\n" + tail,
		"bad type":   "app F:\n    file A: json at \"a.txt\"\n" + tail,
		"no path":    "app F:\n    file A: text\n" + tail,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, werr := compile.String(src)
			if werr == nil {
				t.Fatal("real compiler accepted it")
			}
			if _, gerr := driverGotSrc(t, ts, src); gerr != "driver refused the program: "+werr.Error() {
				t.Fatalf("driver refusal\n got: %s\nwant: %s", gerr, werr)
			}
		})
	}
}

// TestDriverDispatchRefusals: a dispatching route (`api POST "/p" -> Message`)
// is refused exactly as the real compiler refuses it.
func TestDriverDispatchRefusals(t *testing.T) {
	ts := loadDriverApp(t)
	head := "app D:\n    message Ev:\n        | a (x: int) -> doA\n        | b -> doB\n    action doA(x: int, y: text):\n        print(x)\n    action doB(z: [int]?):\n        print(z)\n"
	tail := "    view Home at \"/\":\n        text \"x\"\n"
	cases := map[string]string{
		"get":            head + "    api GET \"/ev\" -> Ev\n" + tail,
		"path param":     head + "    api POST \"/ev/{id}\" -> Ev\n" + tail,
		"missing param":  head + "    api POST \"/ev\" -> Ev\n" + tail,
		"no action":      "app D:\n    message Ev:\n        | a\n    api POST \"/ev\" -> Ev\n" + tail,
		"unknown action": "app D:\n    message Ev:\n        | a -> nope\n    api POST \"/ev\" -> Ev\n" + tail,
		"list mismatch":  "app D:\n    message Ev:\n        | a (xs: int) -> doA\n    action doA(xs: [int]):\n        print(xs)\n    api POST \"/ev\" -> Ev\n" + tail,
		"bad tag":        "app D:\n    message Ev tag nope:\n        | a -> doA\n    action doA():\n        print(1)\n" + tail,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, werr := compile.String(src)
			if werr == nil {
				t.Fatal("real compiler accepted it")
			}
			if _, gerr := driverGotSrc(t, ts, src); gerr != "driver refused the program: "+werr.Error() {
				t.Fatalf("driver refusal\n got: %s\nwant: %s", gerr, werr)
			}
		})
	}
}

// TestDriverLanguageForms: newer language forms, each compiled by both
// compilers to the same whole IR — a proc's `let mut x = do …`, `x = do …`
// and struct field writes; a check message that interpolates; indexing a
// list outside a proc; a type two streams carry, emitted unnamed. Then the
// refusals those forms bring with them.
func TestDriverLanguageForms(t *testing.T) {
	ts := loadDriverApp(t)
	same := map[string]string{
		"proc reassign":  "app P:\n    struct Pt:\n        x: int\n        y: int\n        tag: text\n    proc double(n: int) -> int:\n        return n * 2\n    proc build(n: int) -> [int]:\n        let mut out = []\n        let mut i = 0\n        loop i < n:\n            out = append(out, i)\n            i = i + 1\n        return out\n    proc run() -> text:\n        let mut acc = do double(1)\n        let mut i = 0\n        loop i < 3:\n            acc = do double(acc)\n            i = i + 1\n        let mut xs = do build(2)\n        xs = do build(4)\n        let mut p = Pt{x: 1, y: 2, tag: \"a\"}\n        p.x = acc\n        p.tag = p.tag + \"b\"\n        let q = p\n        p.y = 99\n        return acc + \" \" + len(xs) + \" \" + p.x + \",\" + p.y + \",\" + p.tag + \" \" + q.y\n    state got: text = \"\"\n    action go():\n        let r = do run()\n        got = r\n    view Home at \"/\":\n        text \"{got}\"\n",
		"check message":  "app C:\n    state count: int = 0\n    action bump(n: int, who: text):\n        check n > 0 \"{who}: n must be positive, got {n}\" status 400\n        count = count + n\n    view Home at \"/\":\n        text \"{count}\"\n",
		"list index":     "app A:\n    state flags: [int] = []\n    view Home at \"/\":\n        box:\n            text \"{flags[0]}\"\n",
		"shared payload": "app S:\n    type Ping:\n        n: int\n    policy member:\n        actor != \"guest\"\n    stream \"/api/v2/events\" requires member: Ping\n    stream \"/api/v2/public\": Ping\n    action ping(n: int):\n        emit Ping{n: n}\n    view Home at \"/\":\n        text \"x\"\n",
	}
	for name, src := range same {
		t.Run(name, func(t *testing.T) {
			g, err := compile.String(src)
			if err != nil {
				t.Fatalf("real compiler refused: %v", err)
			}
			b, _ := json.Marshal(g)
			var want map[string]interface{}
			json.Unmarshal(b, &want)
			got, perr := driverGotSrc(t, ts, src)
			if perr != "" {
				t.Fatal(perr)
			}
			if d := driverDiff(want, got); d != "" {
				t.Fatalf("IR mismatch:\n%s", d)
			}
		})
	}
	refused := map[string]string{
		"text index":      "indexing (`x[i]`) outside a proc reads a list or a `json` value; this is neither (arrays and maps are proc-local values)",
		"field not mut":   "is not mutable — declare it `let mut p = …` to assign its fields",
		"no such field":   "struct \"Pt\" has no field \"z\"",
		"reassign no mut": "is not mutable — declare it `let mut a = …` to reassign it",
		"ambiguous emit":  "emit Ping{…}: stream \"/e\" carries Ping as a and b — name the event: emit a Ping{…}",
		"wire no field":   "type \"D\" has no field \"z\"",
		"wire twice":      "field \"n\" set twice in a D{...} literal",
		"wire kind":       "D.n is a number on the wire, but this value is a string",
		"derive declared": "derive \"f\" is declared int, but its definition is text",
		"derive arg":      "derive \"f\" parameter \"n\" is int, but argument 1 is text",
		"record list":     "\"vs\" is a list of Verdict — access a field on one element, not the whole list",
	}
	srcs := map[string]string{
		"text index":      "app A:\n    state name: text = \"x\"\n    view Home at \"/\":\n        text \"{name[0]}\"\n",
		"field not mut":   "app P:\n    struct Pt:\n        x: int\n    proc f() -> int:\n        let p = Pt{x: 1}\n        p.x = 2\n        return p.x\n    view Home at \"/\":\n        text \"x\"\n",
		"no such field":   "app P:\n    struct Pt:\n        x: int\n    proc f() -> int:\n        let mut p = Pt{x: 1}\n        p.z = 2\n        return p.x\n    view Home at \"/\":\n        text \"x\"\n",
		"reassign no mut": "app P:\n    proc one() -> int:\n        return 1\n    proc f() -> int:\n        let a = do one()\n        a = do one()\n        return a\n    view Home at \"/\":\n        text \"x\"\n",
		"ambiguous emit":  "app S:\n    type Ping:\n        n: int\n    stream \"/e\":\n        a: Ping\n        b: Ping\n    action ping(n: int):\n        emit Ping{n: n}\n    view Home at \"/\":\n        text \"x\"\n",
		"wire no field":   "app W:\n    type D:\n        n: int\n    action a(n: int) -> D:\n        return D{z: n}\n    view Home at \"/\":\n        text \"x\"\n",
		"wire twice":      "app W:\n    type D:\n        n: int\n    action a(n: int) -> D:\n        return D{n: n, n: 2}\n    view Home at \"/\":\n        text \"x\"\n",
		"wire kind":       "app W:\n    type D:\n        n: int\n    action a(t: text) -> D:\n        let s = t + \"!\"\n        return D{n: s}\n    view Home at \"/\":\n        text \"x\"\n",
		"derive declared": "app W:\n    derive f(n: int): int = \"x\" + n\n    view Home at \"/\":\n        text \"x\"\n",
		"derive arg":      "app W:\n    derive f(n: int): int = n + 1\n    derive g(t: text): int = f(t)\n    view Home at \"/\":\n        text \"x\"\n",
		"record list":     "app A:\n    record Verdict:\n        score: int\n    service Brain at \"http://b:9000\":\n        rank(body: text) -> [Verdict]\n    state best: int = 0\n    action go(body: text):\n        let vs = call Brain.rank(body)\n        best = vs.score\n    view H at \"/\":\n        text \"{best}\"\n",
	}
	for name, want := range refused {
		t.Run(name, func(t *testing.T) {
			src := srcs[name]
			_, werr := compile.String(src)
			if werr == nil || !strings.Contains(werr.Error(), want) {
				t.Fatalf("real compiler: %v (want it to say %q)", werr, want)
			}
			_, gerr := driverGotSrc(t, ts, src)
			if !strings.Contains(gerr, want) {
				t.Fatalf("driver: %q (want it to say %q)", gerr, want)
			}
		})
	}
}
