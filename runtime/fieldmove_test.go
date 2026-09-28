package runtime

import (
	goruntime "runtime"
	"testing"

	"facet/internal/compile"
)

// Field moves (proccompile.go takePath): a field path bound or passed on
// takes the field's value with its ownership only when nothing reads the
// path, a path through it, or its local whole again — so an in-place write
// through the new holder is never observable through the old one. Each
// proc below builds its answer from values the move analysis must keep
// apart; every result is what copy semantics give.
const fieldMoveApp = `app FM:
    struct Inner:
        xs: [int]
        tag: text
    struct Outer:
        inner: Inner
        ys: [int]
        n: int
    proc mkOuter() -> Outer:
        let a = [1, 2, 3]
        let b = [7, 8]
        return Outer{inner: Inner{xs: a, tag: "t"}, ys: b, n: 5}
    proc bump(xs: [int]) -> [int]:
        let mut out = xs
        out[0] = out[0] + 100
        return out
    # moved: nothing reads o.ys after — the write is in place
    proc moved() -> text:
        let o = do mkOuter()
        let mut ys = o.ys
        ys[0] = 70
        return "" + ys[0] + "," + o.n
    # not moved: o.ys is read after the write
    proc readAfter() -> text:
        let o = do mkOuter()
        let mut ys = o.ys
        ys[0] = 70
        return "" + ys[0] + "," + o.ys[0]
    # not moved: o is read whole after
    proc wholeAfter() -> text:
        let o = do mkOuter()
        let mut ys = o.ys
        ys[1] = 80
        let again = o
        return "" + ys[1] + "," + again.ys[1]
    # a two-field path, then a read of a path through it
    proc deepPrefix() -> text:
        let o = do mkOuter()
        let mut xs = o.inner.xs
        xs[2] = 30
        let inner = o.inner
        return "" + xs[2] + "," + inner.xs[2] + "," + inner.tag
    # a two-field path, siblings read after: moved
    proc deepSibling() -> text:
        let o = do mkOuter()
        let mut xs = o.inner.xs
        xs[2] = 30
        return "" + xs[2] + "," + o.inner.tag + "," + o.ys[0] + "," + o.n
    # passed on as an argument, then read: the callee's write stays its own
    proc argThenRead() -> text:
        let o = do mkOuter()
        let r = do bump(o.inner.xs)
        return "" + r[0] + "," + o.inner.xs[0]
    # passed on at its last use
    proc argLast() -> text:
        let o = do mkOuter()
        let r = do bump(o.inner.xs)
        return "" + r[0] + "," + o.n
    # a struct literal field from a path, then the source read
    proc litThenRead() -> text:
        let o = do mkOuter()
        let mut p = Inner{xs: o.ys, tag: "p"}
        p.xs[0] = 99
        return "" + p.xs[0] + "," + o.ys[0]
    # in a loop that does not declare o: each iteration must see o.ys
    # unchanged
    proc inLoop() -> text:
        let o = do mkOuter()
        let mut out = ""
        let mut i = 0
        loop i < 3:
            let mut ys = o.ys
            ys[0] = ys[0] + i + 1
            out = out + ys[0] + ";"
            i = i + 1
        return out
    # the functional update the engine writes: moved, then rebuilt
    proc update(o0: Outer, v: int) -> Outer:
        let mut ys = o0.ys
        ys[0] = v
        return Outer{inner: o0.inner, ys: ys, n: o0.n + 1}
    proc chain() -> text:
        let mut o = do mkOuter()
        let keep = o
        o = do update(o, 11)
        o = do update(o, 12)
        return "" + o.ys[0] + "," + o.n + "," + keep.ys[0] + "," + keep.n
    proc all() -> text:
        let a = do moved()
        let b = do readAfter()
        let c = do wholeAfter()
        let d = do deepPrefix()
        let e = do deepSibling()
        let f = do argThenRead()
        let g = do argLast()
        let h = do litThenRead()
        let i = do inLoop()
        let j = do chain()
        return a + "|" + b + "|" + c + "|" + d + "|" + e + "|" + f + "|" + g + "|" + h + "|" + i + "|" + j
    state got: text = ""
    action run:
        let r = do all()
        got = r
    view Home at "/":
        text "{got}"
`

func TestFieldMovesAreInvisible(t *testing.T) {
	g, err := compile.String(fieldMoveApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	for k := 0; k < 2; k++ { // a second run reuses the compiled code
		if _, err := srv.Run("ada", "member", true, "run", nil); err != nil {
			t.Fatal(err)
		}
		want := "70,5|70,7|80,8|30,3,t|30,t,7,5|101,1|101,5|99,7|8;9;10;|12,7,7,5"
		if got := srv.StateValue("got"); got != want {
			t.Fatalf("got  %v\nwant %s", got, want)
		}
	}
}

const fieldMoveCostApp = `app FC:
    struct Box:
        xs: [int]
        n: int
    proc upd(b: Box, i: int) -> Box:
        let mut xs = b.xs
        xs[i] = i
        return Box{xs: xs, n: b.n + 1}
    proc build(n: int) -> [int]:
        let mut out = []
        let mut i = 0
        loop i < n:
            out = append(out, 0)
            i = i + 1
        return out
    proc go() -> int:
        let xs = do build(5000)
        let mut b = Box{xs: xs, n: 0}
        let mut i = 0
        loop i < 2000:
            b = do upd(b, i)
            i = i + 1
        return b.n + b.xs[1999]
    state got: int = 0
    action run:
        let r = do go()
        got = r
    view Home at "/":
        text "{got}"
`

// The engine's functional update — take a field, write it, rebuild the
// struct around it — is in place once ownership flows through the field:
// 2000 updates of a 5000-element list allocate nothing proportional to
// the list (a copy each would be 2000 × 5000 slots, some 160 MB).
func TestFieldMoveMakesFunctionalUpdateInPlace(t *testing.T) {
	g, err := compile.String(fieldMoveCostApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.Run("ada", "member", true, "run", nil); err != nil {
		t.Fatal(err)
	}
	var m0, m1 goruntime.MemStats
	goruntime.ReadMemStats(&m0)
	if _, err := srv.Run("ada", "member", true, "run", nil); err != nil {
		t.Fatal(err)
	}
	goruntime.ReadMemStats(&m1)
	if got := srv.StateValue("got"); toInt(got) != 2000+1999 {
		t.Fatalf("got %v, want %d", got, 2000+1999)
	}
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 20<<20 {
		t.Fatalf("2000 updates allocated %d MB — the field is being copied, not moved", alloc>>20)
	}
}
