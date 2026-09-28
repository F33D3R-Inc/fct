package runtime

import (
	goruntime "runtime"
	"testing"

	"facet/internal/compile"
)

// A list threaded through calls (`let mut out = append(parts, x)`, `out =
// grow(out, …)`) is grown in place: the parameter was handed over, the
// callee's result comes back owned, and `append` of a local at its last use
// takes its list rather than copying it. Every case here is one where a
// copy is required or not — the answers are what copying gives.
const appendMoveApp = `app AM:
    proc grow(parts: [text], depth: int) -> [text]:
        let mut out = append(parts, "<" + depth)
        if depth > 0:
            out = grow(out, depth - 1)
            out = grow(out, depth - 1)
        out = append(out, ">")
        return out
    # the source is read after: it keeps its own elements
    proc keep() -> text:
        let a = ["x"]
        let b = append(a, "y")
        let c = append(a, "z")
        return join(a, "") + "|" + join(b, "") + "|" + join(c, "")
    # the element reads the source: no move
    proc selfRef() -> text:
        let a = ["x", "y"]
        let b = append(a, "" + len(a))
        return join(a, "") + "|" + join(b, "")
    # a caller's value passed and still used: the callee's append copies
    proc addOne(xs: [text]) -> [text]:
        return append(xs, "!")
    proc callerKeeps() -> text:
        let a = ["p", "q"]
        let b = addOne(a)
        return join(a, "") + "|" + join(b, "")
    # moved in a loop body that does not declare it: never moved
    proc inLoop() -> text:
        let base = ["b"]
        let mut out = ""
        let mut i = 0
        loop i < 3:
            let got = append(base, "" + i)
            out = out + join(got, "") + ";"
            i = i + 1
        return out + join(base, "")
    proc all() -> text:
        let g = grow([], 3)
        let a = do keep()
        let b = do selfRef()
        let c = do callerKeeps()
        let d = do inLoop()
        return "" + len(g) + "," + join(g, "") + "|" + a + "|" + b + "|" + c + "|" + d
    state got: text = ""
    action run:
        let r = do all()
        got = r
    view Home at "/":
        text "{got}"
`

func TestAppendMoveIsInvisible(t *testing.T) {
	g, err := compile.String(appendMoveApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	var grow func(parts []string, depth int) []string
	grow = func(parts []string, depth int) []string {
		out := append(append([]string{}, parts...), "<"+itoa(depth))
		if depth > 0 {
			out = grow(out, depth-1)
			out = grow(out, depth-1)
		}
		return append(out, ">")
	}
	gs := grow(nil, 3)
	joined := ""
	for _, s := range gs {
		joined += s
	}
	want := itoa(len(gs)) + "," + joined + "|x|xy|xz|xy|xy2|pq|pq!|b0;b1;b2;b"
	for k := 0; k < 2; k++ {
		if _, err := srv.Run("ada", "member", true, "run", nil); err != nil {
			t.Fatal(err)
		}
		if got := srv.StateValue("got"); got != want {
			t.Fatalf("got  %v\nwant %s", got, want)
		}
	}
}

const appendMoveCostApp = `app AC:
    proc grow(parts: [int], depth: int) -> [int]:
        let mut out = append(parts, depth)
        if depth > 0:
            out = grow(out, depth - 1)
            out = grow(out, depth - 1)
        out = append(out, 0)
        return out
    state got: int = 0
    action run:
        let r = grow([], 13)
        got = len(r)
`

// 2^15 elements threaded through 16k calls: linear in the list, where a
// copy per step would allocate some 16k × 16k slots (about 4 GB).
func TestAppendMoveThroughCallsIsLinear(t *testing.T) {
	g, err := compile.String(appendMoveCostApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	var m0, m1 goruntime.MemStats
	goruntime.ReadMemStats(&m0)
	if _, err := srv.Run("ada", "member", true, "run", nil); err != nil {
		t.Fatal(err)
	}
	goruntime.ReadMemStats(&m1)
	if got := toInt(srv.StateValue("got")); got != 1<<15-2 {
		t.Fatalf("len = %d", got)
	}
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 64<<20 {
		t.Fatalf("threading the list allocated %d MB — it is being copied at each step", alloc>>20)
	}
}
