package runtime

// The proc engine's ownership model one level into struct fields
// (structVal.own, proccompile.go's "fieldset" and "get" cases): a field
// grown with `s.f = append(s.f, e)` grows in place while nothing else can
// see the list, and copies the moment something can — a retained read of
// the field, a second holder of the struct. These tests pin the value
// semantics that must hold either way, and the literal-list forms the
// engine folds at compile time (`x in [...]`, an inspected list literal).

import (
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

const fieldOwnershipApp = `app A:
    struct Bag:
        xs: [int]
        tag: text
    proc grow(b0: Bag, n: int) -> Bag:
        let mut b = b0
        let mut i = 0
        loop i < n:
            b.xs = append(b.xs, i)
            i = i + 1
        return b
    proc first(bs: [Bag]) -> Bag:
        return bs[0]
    proc run(n: int) -> text:
        let mut b = Bag{xs: [], tag: "b"}
        let mut i = 0
        loop i < n:
            b.xs = append(b.xs, i)
            i = i + 1
        let snap = b.xs
        b.xs = append(b.xs, 100)
        let copyB = b
        b.xs = append(b.xs, 200)
        let mut c = copyB
        c.xs = append(c.xs, 300)
        c.xs = append(c.xs, 301)
        let shared = b
        let grown = do grow(shared, 2)
        let mut fromList = do first([b])
        fromList.xs = append(fromList.xs, 400)
        let mut p = b
        let q = p
        p.xs = append(p.xs, 500)
        return "" + len(snap) + "," + len(copyB.xs) + "," + len(b.xs) + "," + len(c.xs) + "," + len(shared.xs) + "," + len(grown.xs) + "," + len(fromList.xs) + "," + len(q.xs) + "," + len(p.xs) + "," + snap[n - 1] + "," + b.xs[n + 1] + "," + c.xs[n + 2]
    state result: text = ""
    action run(n: int):
        let r = do run(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestFieldAppendKeepsValueSemantics(t *testing.T) {
	g, err := compile.String(fieldOwnershipApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	deltas := postJSON(t, ts, "run", `{"args":[5]}`)
	// snap keeps 5 (b.xs grew past it), copyB 6, b 7, c 8, shared 7 (grow
	// got its own copy: 9), the list-borne copy 8, q 7 (p's later growth
	// is p's alone: 8); and no element moved under anyone.
	want := "5,6,7,8,7,9,8,7,8,4,200,301"
	if got := deltas["result"]; got != want {
		t.Fatalf("result = %v, want %s", got, want)
	}
}

const literalMembershipApp = `app A:
    proc probe(c: text, n: int) -> text:
        let a = c in ["a", "b", "c"]
        let b = n in [1, 2, 3]
        let z = c in []
        let nested = n in [[1], [2]]
        return "" + a + "," + b + "," + z + "," + nested
    proc inspected(n: int) -> int:
        let mut total = 0
        let mut i = 0
        loop i < n:
            if i in [0, 2, 4]:
                total = total + len([1, 2, 3])
            let mut fresh = [0, 0]
            fresh[0] = i
            total = total + fresh[0]
            i = i + 1
        return total
    state result: text = ""
    action run(c: text, n: int):
        let r = do probe(c, n)
        let k = do inspected(n)
        result = r + ";" + k
    view Home at "/":
        box:
            text "{result}"
`

func TestLiteralListMembership(t *testing.T) {
	g, err := compile.String(literalMembershipApp)
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
		// inspected(n): each i in {0,2,4} adds len([1,2,3]) = 3, and the
		// fresh list (a new one per iteration) adds i.
		{`{"args":["b",2]}`, "true,true,false,false;4"},
		{`{"args":["q",9]}`, "false,false,false,false;45"},
	}
	for _, c := range cases {
		deltas := postJSON(t, ts, "run", c.body)
		if got := deltas["result"]; got != c.want {
			t.Fatalf("%s: result = %v, want %s", c.body, got, c.want)
		}
	}
}

// The mixed-kind membership rule is equal()'s, not the hash lookup's: an
// int against a list of texts still matches by spelling.
func TestLiteralListMembershipMixedKinds(t *testing.T) {
	set := constSet(&ir.Expr{Kind: "list", Args: []*ir.Expr{
		{Kind: "lit", VType: "text", Val: "1"},
		{Kind: "lit", VType: "text", Val: "2"},
	}})
	if hit, ok := set.lookup(1); ok || hit {
		t.Fatalf("an int against texts must fall back to the scan, got ok=%v hit=%v", ok, hit)
	}
	if !applyBin("in", 1, set.list).(bool) {
		t.Fatal(`1 in ["1", "2"] must hold, as equal() has it`)
	}
	if hit, ok := set.lookup("2"); !ok || !hit {
		t.Fatalf(`"2" in ["1", "2"]: ok=%v hit=%v`, ok, hit)
	}
}

// Many procs reading long texts at once take the text index's lock-free
// cache; under -race this is the check that they may.
func TestTextIndexConcurrentReaders(t *testing.T) {
	texts := make([]string, 64)
	for i := range texts {
		texts[i] = strings.Repeat("x", 200+i) + "é"
	}
	done := make(chan bool)
	for g := 0; g < 8; g++ {
		go func(g int) {
			for i := 0; i < 2000; i++ {
				s := texts[(i*7+g)%len(texts)]
				if runeSlice(s, len(s)-2, len(s)-1) != "é" || runeLen(s) != len(s)-1 {
					panic("wrong slice")
				}
			}
			done <- true
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}
