package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A binding id (`b0`, `b1`, …) is an address on ONE page.
//
// The compiler mints them per page, starting at b0 every time, because they end
// up as `data-fa-bind` attributes in a document and only one page's document
// exists at a time. The client honours that: the bootstrap ships `pg.Bindings`
// and nothing else, so `bindings["b0"]` is this page's b0.
//
// The server did not. It folded every page's bindings into one map at boot —
// "the union across all pages" — and a union of a per-page namespace is a
// collision: the last page indexed won every id, so first paint evaluated some
// other page's expression in this page's scope. /tag/facet rendered `#5`, the
// value of the index page's `publishedCount`, and a route parameter that the
// other page's expression does not mention rendered as nothing at all. The
// client then repainted it correctly, so the wrong value was on screen only
// until hydration — long enough to be the page, and short enough that it was
// written off as a mystery and routed around with a component.
//
// This is the app that reproduces it: two pages, each with its own b0, one of
// them reading a route parameter the other page's b0 knows nothing about.
const twoPageBindSrc = `app Journal:
    entity Post:
        id: int
        title: text
        published: bool
    derive publishedCount: int = count(p in Post where p.published)
    view Index at "/":
        box:
            text "{publishedCount} published"
    view TagPage at "/tag/:tagname":
        box:
            text "#{tagname}"
`

var bindSpanRE = regexp.MustCompile(`<span data-fa-bind="([^"]*)">([^<]*)</span>`)

func fetchPage(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestABindingIsResolvedAgainstThePageBeingRendered(t *testing.T) {
	g, err := compile.String(twoPageBindSrc)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := fetchPage(t, ts, "/tag/facet")
	m := bindSpanRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no data-fa-bind span in the first paint of /tag/facet:\n%s", body)
	}
	if m[2] != "facet" {
		t.Errorf("first paint of /tag/facet rendered %s = %q, want %q — the binding was resolved against another page's table",
			m[1], m[2], "facet")
	}

	// The other page still resolves its own, so the fix is a scoping of the
	// lookup and not a reordering of it.
	body = fetchPage(t, ts, "/")
	m = bindSpanRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no data-fa-bind span in the first paint of /:\n%s", body)
	}
	if m[2] != "0" {
		t.Errorf("first paint of / rendered %s = %q, want %q", m[1], m[2], "0")
	}
}

var irScriptRE = regexp.MustCompile(`(?s)<script type="application/json" id="fa-ir">(.*?)</script>`)
var stateScriptRE = regexp.MustCompile(`(?s)<script type="application/json" id="fa-state">(.*?)</script>`)

// The pin, in the house pattern (link_test.go, classmerge_test.go,
// attrtext_test.go): run the client's own rule under node over exactly what the
// server shipped, and diff it against what the server painted.
//
// The client's rule is two lines of assets/facet.js — `bindings = index(ir.bindings,
// "id")` in load(), and `toStr(ev(bindings[seg.bind].expr, sc))` in appendSegs —
// and the whole of this bug was that the server answered the same question from a
// different table. Mirroring it here means the two sides are checked against each
// other rather than each against a fixture, which is the only way a disagreement
// of this shape shows up: both halves were internally consistent.
func TestFirstPaintBindsMatchTheClientsOwnTable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}
	g, err := compile.String(twoPageBindSrc)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/", "/tag/facet"} {
		body := fetchPage(t, ts, path)
		irJSON := irScriptRE.FindStringSubmatch(body)
		stateJSON := stateScriptRE.FindStringSubmatch(body)
		if irJSON == nil || stateJSON == nil {
			t.Fatalf("%s: no fa-ir/fa-state payload", path)
		}
		painted := map[string]string{}
		for _, m := range bindSpanRE.FindAllStringSubmatch(body, -1) {
			painted[m[1]] = m[2]
		}

		var script strings.Builder
		script.WriteString(`
const ir = `)
		script.WriteString(irJSON[1])
		script.WriteString(`;
const store = `)
		script.WriteString(stateJSON[1])
		script.WriteString(`;
function list(x) { return x || []; }
function index(a, k) { const m = {}; for (const x of list(a)) m[x[k]] = x; return m; }
function toStr(v) { if (v == null) return ""; if (typeof v === "boolean") return v ? "true" : "false"; return "" + v; }
function truthy(v) { return Array.isArray(v) ? v.length > 0 : !!v; }
// The one line under test: the client's binding table is this page's, keyed by id.
const bindings = index(ir.bindings, "id");
function ev(e, sc) {
  if (!e) return null;
  switch (e.kind) {
    case "lit": return e.vtype === "int" ? (e.val | 0) : e.vtype === "bool" ? !!e.val : toStr(e.val);
    case "ref": return sc[e.name];
    case "agg": {
      let rows = list(sc[e.coll]);
      if (e.where) rows = rows.filter((r) => { sc[e.var] = r; return truthy(ev(e.where, sc)); });
      return e.op === "exists" ? rows.length > 0 : rows.length;
    }
    case "get": { const o = ev(e.obj, sc); return o && typeof o === "object" ? o[e.field] : null; }
  }
  return null;
}
const out = {};
for (const id in bindings) out[id] = toStr(ev(bindings[id].expr, store));
console.log(JSON.stringify(out));
`)
		cmd := exec.Command(node, "-e", script.String())
		outBytes, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: node mirror failed: %v\n%s", path, err, outBytes)
		}
		var client map[string]string
		if err := json.Unmarshal(outBytes, &client); err != nil {
			t.Fatalf("%s: node mirror output %q: %v", path, outBytes, err)
		}
		if len(client) == 0 {
			t.Fatalf("%s: the client mirror resolved no bindings at all", path)
		}
		for id, want := range client {
			if got, ok := painted[id]; !ok {
				t.Errorf("%s: the client resolves %s = %q, but first paint painted no such span", path, id, want)
			} else if got != want {
				t.Errorf("%s: first paint painted %s = %q, the client renders %q — the two halves resolved the same id from different tables",
					path, id, got, want)
			}
		}
	}
}
