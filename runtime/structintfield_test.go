package runtime

// Int fields of struct-typed values are read natively (proccompile.go's
// structTypes / isInt "get"): through a struct parameter, a `let` of a
// struct, a nested struct field, a struct-returning `do` and a literal —
// with values past the boxed-int cache and negative ones — and the ints so
// read keep value semantics when they are stored back into fields and lists.

import (
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

const structIntFieldApp = `app A:
    struct Pos:
        off: int
        len: int
    struct Page:
        at: Pos
        id: int
        tag: text
    proc mk(off: int) -> Page:
        return Page{at: Pos{off: off, len: 7}, id: off * 3, tag: "p"}
    proc span(p: Page) -> int:
        return p.at.off + p.at.len
    proc run(base: int) -> text:
        let p = do mk(base)
        let q = p
        let end = p.at.off + p.at.len
        let mut sum = 0
        let mut i = 0
        loop i < 3:
            sum = sum + q.id - i
            i = i + 1
        let mut r = p
        r.id = r.id + 1
        let s = do span(r)
        let lit = Pos{off: -5, len: base}.off
        let xs = [p.id, r.id, q.at.off]
        let mut t = Page{at: Pos{off: p.id, len: 0}, id: r.at.off, tag: q.tag}
        t.id = t.id + p.at.len
        return "" + end + "," + sum + "," + p.id + "," + r.id + "," + s + "," + lit + "," + xs + "," + t.at.off + "," + t.id + "," + t.tag
    state result: text = ""
    action run(base: int):
        let r = do run(base)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestStructIntFieldsReadNatively(t *testing.T) {
	g, err := compile.String(structIntFieldApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	cases := []struct{ body, want string }{
		// base 100000: id 300000; sum 3*300000 - (0+1+2); r.id 300001 but
		// p.id unchanged; span(r) 100007; the literal's off -5.
		{`{"args":[100000]}`, "100007,899997,300000,300001,100007,-5," + toStr([]any{300000, 300001, 100000}) + ",300000,100007,p"},
		{`{"args":[-70000]}`, "-69993,-630003,-210000,-209999,-69993,-5," + toStr([]any{-210000, -209999, -70000}) + ",-210000,-69993,p"},
	}
	for _, c := range cases {
		deltas := postJSON(t, ts, "run", c.body)
		if got := deltas["result"]; got != c.want {
			t.Fatalf("%s: result = %v\nwant       %s", c.body, got, c.want)
		}
	}
}
