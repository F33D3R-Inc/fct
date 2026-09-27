package runtime

// Struct values are pointers (eval.go's structVal = *structObj): holders
// share one header until a writer copies (cloneStructValue under
// fr.mutable). These pin value semantics through every way a struct gets a
// second holder — a `let` copy, a list element, an argument, a field of
// another struct, a returned value — for widths on both sides of the
// inline-storage sizes (4, 8, 16, 32 fields and beyond).

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

func TestStructCopiesAreIndependent(t *testing.T) {
	for _, width := range []int{1, 4, 5, 8, 9, 16, 17, 32, 33, 40} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			var fields, lit []string
			for i := 0; i < width; i++ {
				fields = append(fields, fmt.Sprintf("        f%d: int", i))
				lit = append(lit, fmt.Sprintf("f%d: %d", i, i))
			}
			last := fmt.Sprintf("f%d", width-1)
			// the returned text: each read joined with ","
			reads := []string{"a.f0", "a." + last, "b.f0", "b." + last, "xs[0].f0", "xs[1]." + last, "c.f0", "c." + last, "o.w.f0", "ys[0].f0", "xs[0].f0"}
			ret := `""`
			for i, r := range reads {
				if i > 0 {
					ret += ` + ","`
				}
				ret += " + " + r
			}
			src := `app A:
    struct W:
` + strings.Join(fields, "\n") + `
    struct Outer:
        w: W
        n: int
    proc pickW(ws: [W], i: int) -> W:
        return ws[i]
    proc bump(w0: W) -> W:
        let mut w = w0
        w.f0 = w.f0 + 1000
        w.` + last + ` = w.` + last + ` + 1000
        return w
    proc run() -> text:
        let mut a = W{` + strings.Join(lit, ", ") + `}
        let b = a
        a.f0 = 50
        let xs = [a, a]
        a.` + last + ` = 60
        let c = do bump(a)
        let mut o = Outer{w: a, n: 1}
        a.f0 = 70
        o.w.f0 = 80
        let mut ys = xs
        let mut y0 = do pickW(ys, 0)
        y0.f0 = 90
        ys[0] = y0
        return ` + ret + `
    state result: text = ""
    action run:
        let r = do run()
        result = r
    view Home at "/":
        box:
            text "{result}"
`
			g, err := compile.String(src)
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
			lastInit := width - 1
			// a: f0 70, last 60 (or 60 when width 1: last is f0, set to 60
			// then 70 — so both read 70). b kept the literal; xs kept a as it
			// was at 50; c is a with +1000 on both; o.w was a at 50/60 then
			// its own f0 80; ys[0] 90 while xs[0] stays 50.
			aLast, bLast, xsLast, cF0, cLast := 60, lastInit, lastInit, 1050, 1060
			if width == 1 {
				aLast, bLast, xsLast, cF0, cLast = 70, 0, 50, 2060, 2060
			}
			want := fmt.Sprintf("70,%d,0,%d,50,%d,%d,%d,80,90,50", aLast, bLast, xsLast, cF0, cLast)
			if got := deltas["result"]; got != want {
				t.Fatalf("width %d: result = %v, want %s", width, got, want)
			}
		})
	}
}
