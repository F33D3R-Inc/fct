package runtime

// growOwnedList (proccompile.go): `xs = append(xs, e)` on an owned list
// rewrites the header in the slot's own box. These pin that no other
// holder of the list — a copy bound before the append, a list element, an
// argument a callee kept, the shared `[]` literal — ever sees the change,
// and that `xs = append(xs, xs)` does not make the list contain itself.

import (
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

const listGrowApp = `app A:
    struct Box:
        xs: [int]
    proc keep(xs: [int]) -> [int]:
        return xs
    proc build(n: int) -> text:
        let mut xs = []
        let mut snaps = []
        let mut i = 0
        loop i < n:
            xs = append(xs, i)
            if i == 3:
                snaps = append(snaps, xs)
            i = i + 1
        let copy = xs
        xs = append(xs, 100)
        let kept = do keep(xs)
        xs = append(xs, 200)
        let b = Box{xs: xs}
        xs = append(xs, 300)
        let mut e1 = []
        let mut e2 = []
        e1 = append(e1, 7)
        let fresh = []
        return "" + len(snaps[0]) + "," + len(copy) + "," + len(kept) + "," + len(b.xs) + "," + len(xs) + "," + len(e1) + "," + len(e2) + "," + len(fresh) + "," + xs[n + 2]
    proc selfref() -> text:
        let mut xs = [[1]]
        xs = append(xs, [2])
        xs = append(xs, xs)
        xs = append(xs, [3])
        return "" + len(xs) + "," + len(xs[2]) + "," + xs[3][0]
    state result: text = ""
    action run(n: int):
        let a = do build(n)
        let b = do selfref()
        result = a + "|" + b
    view Home at "/":
        box:
            text "{result}"
`

func TestGrowOwnedListIsInvisibleToOtherHolders(t *testing.T) {
	g, err := compile.String(listGrowApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	deltas := postJSON(t, ts, "run", `{"args":[20]}`)
	// snaps[0] kept 4 (taken at i == 3); copy 20; kept 21; the struct's
	// list 22; xs 23; e1 one element; e2 and fresh still empty; xs[22] the
	// last append. selfref: 4 elements, the appended copy of itself still
	// the 2-element list it was, then [3].
	want := "4,20,21,22,23,1,0,0,300|4,2,3"
	if got := deltas["result"]; got != want {
		t.Fatalf("result = %v, want %s", got, want)
	}
}
