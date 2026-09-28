package runtime

// Nested list types in procs (`[[T]]`: parameters, returns, struct
// fields, locals) and an action binding a proc's struct result. A nested
// list's elements are lists: indexed twice, copied on write level by
// level, never taken for an int/float list, and handed across a `do`
// without being coerced element by element to T.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

const nestedListApp = `app A:
    struct Grid:
        cells: [[int]]
        name: text
    struct Pt:
        n: int
        name: text
        tags: [text]
    proc grid(rows: int, cols: int) -> [[int]]:
        let mut g = []
        let mut r = 0
        loop r < rows:
            let mut row = []
            let mut c = 0
            loop c < cols:
                row = append(row, r * 100000 + c)
                c = c + 1
            g = append(g, row)
            r = r + 1
        return g
    proc total(g: [[int]]) -> int:
        let mut t = 0
        let mut r = 0
        loop r < len(g):
            let mut c = 0
            loop c < len(g[r]):
                t = t + g[r][c]
                c = c + 1
            r = r + 1
        return t
    proc scale(m: [[float]], k: float) -> [[float]]:
        let mut out = []
        let mut r = 0
        loop r < len(m):
            let mut row = []
            let mut c = 0
            loop c < len(m[r]):
                row = append(row, m[r][c] * k)
                c = c + 1
            out = append(out, row)
            r = r + 1
        return out
    proc run() -> text:
        let g = do grid(3, 4)
        let mut h = g
        let mut row0 = h[0]
        row0[1] = -5
        h[0] = row0
        let s = Grid{cells: g, name: "g"}
        let t1 = do total(g)
        let t2 = do total(h)
        let m = do scale([[1.5, 2.0], [0.5]], 2.0)
        return "" + len(g) + "," + len(g[2]) + "," + g[2][3] + "," + t1 + "," + t2 + "," + s.cells[1][2] + "," + len(s.cells) + "," + m[0][0] + "," + m[1][0] + "," + len(m[1])
    proc mkPt(n: int) -> Pt:
        return Pt{n: n * 3, name: "p" + n, tags: ["a", "b"]}
    state result: text = ""
    action run:
        let r = do run()
        let g = do grid(2, 2)
        result = r + "|" + len(g)
    action pt(n: int):
        let p = do mkPt(n)
        result = p.name + ":" + p.n + ":" + len(p.tags)
    view Home at "/":
        box:
            text "{result}"
`

func TestNestedListTypes(t *testing.T) {
	g, err := compile.String(nestedListApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// the IR carries the depth
	b, _ := json.Marshal(g)
	for _, want := range []string{`"retDepth":2`, `"depth":2`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("IR lacks %s", want)
		}
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// grid(3,4): g[r][c] = r*100000+c; total = 4*(0+100000+200000) + 3*(0+1+2+3)
	t1 := 4*(0+100000+200000) + 3*(0+1+2+3)
	want := strings.Join([]string{"3", "4", "200003", itoa(t1), itoa(t1 - 1 - 5), "100002", "3", "3", "1", "1"}, ",") + "|2"
	if got := postJSON(t, ts, "run", `{"args":[]}`)["result"]; got != want {
		t.Fatalf("run: result = %v\nwant       %s", got, want)
	}
	if got := postJSON(t, ts, "pt", `{"args":[7]}`)["result"]; got != "p7:21:2" {
		t.Fatalf("pt: result = %v, want p7:21:2", got)
	}
}

func TestNestedListAndStructBindingRefusals(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`app E:
    action a(g: [[int]]):
        check len(g) > 0 "x"
    view Home at "/":
        text "x"
`, "a list of lists is a proc-only type"},
		{`app E:
    action a -> [[int]]:
        return [[1]]
    view Home at "/":
        text "x"
`, "invalid return type"},
		{`app E:
    struct Pt:
        n: int
    proc mk() -> Pt:
        return Pt{n: 1}
    state r: int = 0
    action a:
        let p = do mk()
        r = p.missing
    view Home at "/":
        text "x"
`, `struct Pt has no field "missing"`},
		{`app E:
    struct Pt:
        n: int
    proc mk() -> Pt:
        return Pt{n: 1}
    action a -> Pt:
        let p = do mk()
        return p
    view Home at "/":
        text "x"
`, "a proc-only type with no wire form"},
	} {
		_, err := compile.String(c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want an error containing %q, got %v", c.want, err)
		}
	}
}
