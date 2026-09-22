package selfhost

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// ir_decl.fct reads and re-emits every flat declaration of a compiled app.
// Each declaration of the shapes app (the same one ir_json_test.go compiles,
// plus enums, field annotations, a read policy, an entity derive, a policy
// with params, procs and a private list state) must round-trip byte for
// byte through the port.
const irDeclApp = irJSONApp + `    entity Note:
        id: int
        author: text @required
        body: text @min(1) @max(500)
        tag: Status
        score: int @unique
        secret: text @secret
        derive shout: text = upper(body)
        read: author == actor
    state hidden: [int] = [] @private
    state maybe: text? = ""
    policy owns(id: int):
        Note(id).author == actor
    proc grow(xs: [int], n: int) -> [int]:
        let mut out = xs
        let mut i = 0
        loop i < n:
            out = append(out, i)
            i = i + 1
        return out
    action rate(id: int, n: int) @optimistic:
        requires owns(id), member
        set Note(id).score = n
`

func loadIRDeclApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("ir_decl.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/ir_decl.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestIRDeclRoundTripsEveryDeclaration(t *testing.T) {
	g, err := compile.String(irDeclApp)
	if err != nil {
		t.Fatalf("compile the shapes app: %v", err)
	}
	ts := loadIRDeclApp(t)
	check := func(kind string, v any, label string) {
		t.Helper()
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		d := postExprJSON(t, ts, "runIRDeclRoundTrip", kind, string(want))
		if got, _ := d["irDeclBack"].(string); got != string(want) {
			t.Errorf("%s %s:\n  got  %s\n  want %s", kind, label, got, want)
		}
	}
	for _, e := range g.Entities {
		check("entity", e, e.Name)
	}
	for _, s := range g.States {
		check("state", s, s.Name)
	}
	for _, d := range g.Derives {
		check("derive", d, d.Name)
	}
	for _, p := range g.Policies {
		check("policy", p, p.Name)
	}
	for _, a := range g.Actions {
		check("action", a, a.Name)
	}
	for _, p := range g.Procs {
		check("proc", p, p.Name)
	}
	for _, e := range g.Enums {
		check("enum", e, e.Name)
	}
	if len(g.Entities) < 4 || len(g.Actions) < 2 || len(g.Procs) < 2 || len(g.Enums) < 1 {
		t.Fatalf("the shapes app compiled thinner than expected: %d entities, %d actions, %d procs, %d enums", len(g.Entities), len(g.Actions), len(g.Procs), len(g.Enums))
	}
}
