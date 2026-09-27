package runtime

// `s.f = do P(args)`, `xs[i] = do P(args)` and `a.b.c = do P(args)`: a
// proc call's result written through a field, an index or a nested path
// (internal/parser/parser.go's parseProcBody: the result lands in a `let
// mut` temporary numbered like a nested write's, and the statement is the
// ordinary write of it). Before this, only a bare local could take a `do`
// result and every such site needed a hand-written local.

import (
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

const doFieldTargetApp = `app A:
    struct Pair:
        a: int
        b: [int]
    struct Box:
        p: Pair
    proc seven() -> int:
        return 7
    proc twoThree() -> [int]:
        return [2, 3]
    proc run() -> text:
        let mut p = Pair{a: 0, b: []}
        p.a = do seven()
        p.b = do twoThree()
        let snap = p.b
        p.b = do twoThree()
        let mut xs = [0, 0]
        xs[1] = do seven()
        let mut box = Box{p: Pair{a: 1, b: [9]}}
        box.p.a = do seven()
        box.p.b[0] = do seven()
        return "" + p.a + "," + len(p.b) + "," + len(snap) + "," + xs[1] + "," + box.p.a + "," + box.p.b[0]
    state result: text = ""
    action run:
        let r = do run()
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestDoResultIntoFieldIndexAndNestedTargets(t *testing.T) {
	g, err := compile.String(doFieldTargetApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	deltas := postJSON(t, ts, "run", `{"args":[]}`)
	if got, want := deltas["result"], "7,2,2,7,7,7"; got != want {
		t.Fatalf("result = %v, want %s", got, want)
	}
}

// The target still has to be a writable place: a garbage target with a
// `do` right-hand side is refused as any other assignment's would be.
func TestDoResultIntoInvalidTargetIsRefused(t *testing.T) {
	_, err := compile.String(`app A:
    proc seven() -> int:
        return 7
    proc run() -> int:
        let mut n = 0
        7 = do seven()
        return n
    action run:
        let r = do run()
`)
	if err == nil {
		t.Fatal("a literal as a do assignment target compiled")
	}
}
