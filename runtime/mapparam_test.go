package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"facet/internal/compile"
)

// Map-typed proc parameters, returns and struct fields: `{K: V}`.
const mapParamApp = `app M:
    struct Tally:
        counts: {text: int}
        label: text

    struct Pt:
        x: int

    proc bump(m0: {text: int}, k: text) -> {text: int}:
        let mut m = m0
        m[k] = m[k] + 1
        return m

    proc total(m: {text: int}, keys: [text]) -> int:
        let mut t = 0
        let mut i = 0
        loop i < len(keys):
            t = t + m[keys[i]]
            i = i + 1
        return t

    proc byId(ps: [Pt]) -> {int: Pt}:
        let mut out = {}
        let mut i = 0
        loop i < len(ps):
            out[ps[i].x] = ps[i]
            i = i + 1
        return out

    # value semantics: the caller's map is untouched by the callee's write;
    # a struct's map field grows in place and copies on share
    proc semantics() -> text:
        let mut a = {}
        a["x"] = 1
        let b = do bump(a, "x")
        let c = do bump(b, "y")
        let mut t = Tally{counts: c, label: "t"}
        let snap = t
        t.counts = do bump(t.counts, "x")
        let pts = do byId([Pt{x: 3}, Pt{x: 5}])
        let p5 = pts[5]
        let n = do total(t.counts, ["x", "y"])
        return "" + a["x"] + "|" + b["x"] + "|" + c["y"] + "|" + t.counts["x"] + "|" + snap.counts["x"] + "|" + len(pts) + "|" + p5.x + "|" + n

    action run -> text:
        let r = do semantics()
        return r

    view Home at "/":
        text "m"
`

func TestMapParamsFieldsReturns(t *testing.T) {
	g, err := compile.String(mapParamApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	got, err := srv.RunValue("ada", "member", true, "run", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := toStr(got); s != "1|2|1|3|2|2|5|4" {
		t.Errorf("semantics = %q, want 1|2|1|3|2|2|5|4", s)
	}
	// IR: the map shape survives emit
	b, _ := json.Marshal(g)
	for _, want := range []string{`"map":true`, `"key":"text"`, `"retMap":true`, `"retKey":"int"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("IR lacks %s", want)
		}
	}
}

// A decoded JSON object handed to a map parameter becomes a map with the
// declared key type; a map leaving the engine encodes as a JSON object.
func TestMapParamJSONBoundary(t *testing.T) {
	g, err := compile.String(mapParamApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	m := srv.procArgMap("int", "Pt", map[string]any{"7": map[string]any{"x": 7.0}})
	mm, ok := m.(map[any]any)
	if !ok || mm[7] == nil {
		t.Fatalf("decoded object → {int: Pt}: %#v", m)
	}
	if _, ok := mm[7].(structVal); !ok {
		t.Errorf("value converted to the struct: %T", mm[7])
	}
	out, _ := json.Marshal(plainValue(map[any]any{3: bytesVal{1}, "a": &structObj{lay: newStructLayout("Pt", []string{"x"}), vals: []any{2}}}))
	if string(out) != `{"3":[1],"a":{"x":2}}` {
		t.Errorf("encoded %s", out)
	}
}

func TestMapTypeErrors(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`app E:
    proc f(m: {bool: int}) -> int:
        return 0
    view Home at "/":
        text "x"
`, "map key must be int or text"},
		{`app E:
    proc f(m: {text: int}) -> int:
        return m["a"]
    proc g() -> int:
        let n = do f(3)
        return n
    view Home at "/":
        text "x"
`, "is a {text: int} map"},
		{`app E:
    proc f(m: {text: int}) -> int:
        let mut x = m
        x[1] = 2
        return 0
    view Home at "/":
        text "x"
`, "its key must be text"},
		{`app E:
    struct S:
        m: {int: text}
    proc f() -> int:
        let s = S{m: [1]}
        return 0
    view Home at "/":
        text "x"
`, "wants {int: text}"},
	} {
		_, err := compile.String(c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want error containing %q, got %v", c.want, err)
		}
	}
}

// Borrowed parameters: a callee that only reads its argument leaves the
// caller's ownership in place, and a callee that retains any part of it
// (a field, a field of a field) is not a borrower — the caller's later
// in-place write must not show through what the callee returned.
const borrowApp = `app B:
    struct In:
        xs: [int]
    struct Out:
        inner: In
        tag: text

    proc peek(o: Out) -> int:
        return o.inner.xs[0] + len(o.inner.xs)

    proc grab(o: Out) -> [int]:
        return o.inner.xs

    proc run() -> text:
        let mut o = Out{inner: In{xs: [1, 2]}, tag: "t"}
        let n = do peek(o)
        o.inner.xs[0] = 5
        let kept = do grab(o)
        o.inner.xs[0] = 9
        let n2 = do peek(o)
        return "" + n + "|" + kept + "|" + o.inner.xs + "|" + n2

    action go -> text:
        let r = do run()
        return r
    view Home at "/":
        text "b"
`

func TestBorrowedParams(t *testing.T) {
	g, err := compile.String(borrowApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	got, err := srv.RunValue("ada", "member", true, "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := toStr(got); s != "3|5,2|9,2|11" {
		t.Errorf("run = %q, want 3|5,2|9,2|11", s)
	}
	pc := srv.codeFor(srv.byProc["peek"], srv.byProc["peek"].Params, srv.byProc["peek"].Body)
	if !pc.borrow[0] {
		t.Error("peek only reads its parameter: it borrows")
	}
	pc2 := srv.codeFor(srv.byProc["grab"], srv.byProc["grab"].Params, srv.byProc["grab"].Body)
	if pc2.borrow[0] {
		t.Error("grab returns a field of a field of its parameter: it does not borrow")
	}
}
