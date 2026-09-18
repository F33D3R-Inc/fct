package compile

import (
	"strings"
	"testing"
)

// mapFreqApp is the map-support proving example: `freqOf` builds a fixed
// five-value array literal (proc parameters cannot be array/map-typed in
// this milestone — see internal/compile/array_test.go's arraySumApp, which
// has the same constraint — so the "collection" a real algorithm needs is
// built from scratch inside the proc body, exactly as that one does), then
// counts each value's occurrences into a map with a single `loop` pass —
// `counts[xs[i]] = counts[xs[i]] + 1` relies on this milestone's chosen
// missing-key semantics (a nil read, not an error — see runtime/eval.go's
// "index" case) to increment a not-yet-seen key with no separate `has`
// guard, the same idiom Python/JS's `dict.get(k, 0) + 1` and `d[k] =
// (d[k] || 0) + 1` lean on for the same reason. This is the IR-shape half of
// the proof; runtime/map_test.go is the live-over-HTTP half.
const mapFreqApp = `app A:
    proc freqOf(a: int, b: int, c: int, d: int, e: int, target: int) -> int:
        let xs = [a, b, c, d, e]
        let mut counts = {}
        let mut i = 0
        loop i < len(xs):
            counts[xs[i]] = counts[xs[i]] + 1
            i = i + 1
        return counts[target]
    state result: int = 0
    action run(a: int, b: int, c: int, d: int, e: int, target: int):
        let r = do freqOf(a, b, c, d, e, target)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestMapFreqCompiles(t *testing.T) {
	g, err := String(mapFreqApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 1 {
		t.Fatalf("want 1 proc, got %d", len(g.Procs))
	}
	p := g.Procs[0]
	// let(xs), let(counts), let(i), loop, return.
	wantOps := []string{"let", "let", "let", "loop", "return"}
	if len(p.Body) != len(wantOps) {
		t.Fatalf("proc body = %+v, want %d statements (%v)", p.Body, len(wantOps), wantOps)
	}
	for i, want := range wantOps {
		if p.Body[i].Op != want {
			t.Errorf("body[%d].Op = %q, want %q", i, p.Body[i].Op, want)
		}
	}
	// `let mut counts = {}` lowers its RHS to an empty "map" IR expression —
	// the map-literal counterpart of arraySumApp's `let mut xs = []` lowering
	// to an empty "list".
	countsLet := p.Body[1]
	if countsLet.Target != "counts" || countsLet.Value == nil || countsLet.Value.Kind != "map" {
		t.Fatalf("counts let = %+v, want Target counts, Value.Kind map", countsLet)
	}
	if len(countsLet.Value.Keys) != 0 || len(countsLet.Value.Args) != 0 {
		t.Fatalf("counts let value = %+v, want no keys/values (an empty map literal)", countsLet.Value)
	}
	// The loop's body reassigns counts[xs[i]] via an "indexset" statement
	// (not a mutating expression) — the map-set counterpart of
	// arraySumApp's array-build loop reassigning xs via a "call" to append.
	loop := p.Body[3]
	if loop.Op != "loop" || len(loop.Body) != 2 {
		t.Fatalf("loop = %+v, want 2 statements (indexset counts, assign i)", loop)
	}
	set := loop.Body[0]
	if set.Op != "indexset" || set.Target != "counts" {
		t.Fatalf("counts set = %+v, want Op indexset Target counts", set)
	}
	// The key (`xs[i]`) is itself an array index read — an "index" IR node,
	// reusing the same Obj/Key fields a map's own index machinery does.
	if set.Key == nil || set.Key.Kind != "index" {
		t.Fatalf("indexset Key = %+v, want an `xs[i]` index read", set.Key)
	}
	if set.Key.Obj == nil || set.Key.Obj.Kind != "ref" || set.Key.Obj.Name != "xs" {
		t.Fatalf("indexset Key.Obj = %+v, want ref to xs", set.Key.Obj)
	}
	// The value (`counts[xs[i]] + 1`) is a `+` binary expression whose left
	// side reads the not-yet-incremented count back out of the same map.
	if set.Value == nil || set.Value.Kind != "bin" || set.Value.Op != "+" {
		t.Fatalf("indexset Value = %+v, want a `+` binary expression", set.Value)
	}
	if set.Value.L == nil || set.Value.L.Kind != "index" || set.Value.L.Obj == nil || set.Value.L.Obj.Name != "counts" {
		t.Fatalf("indexset Value.L = %+v, want a `counts[...]` index read", set.Value.L)
	}
	// The final `return counts[target]` reads the map back by a plain `ref`
	// key (target, a proc parameter) rather than a nested index expression —
	// the ordinary map-get shape.
	ret := p.Body[4]
	if ret.Op != "return" || ret.Value == nil || ret.Value.Kind != "index" {
		t.Fatalf("return = %+v, want Op return, Value.Kind index", ret)
	}
	if ret.Value.Obj == nil || ret.Value.Obj.Name != "counts" {
		t.Fatalf("return Value.Obj = %+v, want ref to counts", ret.Value.Obj)
	}
	if ret.Value.Key == nil || ret.Value.Key.Kind != "ref" || ret.Value.Key.Name != "target" {
		t.Fatalf("return Value.Key = %+v, want ref to target", ret.Value.Key)
	}
}

// mapSetApp is the map-set proving example: `m[k] = v` lowers to the exact
// same "indexset" IR shape an array's `xs[i] = v` does (internal/compile/
// array_test.go's TestIndexAssignCompiles) — the whole point of reusing
// IndexAssign's syntax for a map rather than inventing a separate node —
// distinguished only by what Target's own type turns out to be at runtime
// (see runtime/server.go's execProcBlock, "indexset" case).
const mapSetApp = `app A:
    proc replace(k: text, v: int) -> int:
        let mut m = {"a": 1}
        m[k] = v
        return m[k]
    state result: int = 0
    action run(k: text, v: int):
        let r = do replace(k, v)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestMapSetCompiles(t *testing.T) {
	g, err := String(mapSetApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	p := g.Procs[0]
	// let, indexset, return.
	if len(p.Body) != 3 {
		t.Fatalf("proc body = %+v, want 3 statements (let, indexset, return)", p.Body)
	}
	mLet := p.Body[0]
	if mLet.Value == nil || mLet.Value.Kind != "map" {
		t.Fatalf("m let = %+v, want Value.Kind map", mLet)
	}
	if len(mLet.Value.Keys) != 1 || mLet.Value.Keys[0].Kind != "lit" || mLet.Value.Keys[0].VType != "text" {
		t.Fatalf("m let value keys = %+v, want one text-literal key", mLet.Value.Keys)
	}
	set := p.Body[1]
	if set.Op != "indexset" {
		t.Fatalf("body[1].Op = %q, want indexset", set.Op)
	}
	if set.Target != "m" {
		t.Errorf("indexset Target = %q, want m", set.Target)
	}
	if set.Key == nil || set.Key.Kind != "ref" || set.Key.Name != "k" {
		t.Errorf("indexset Key = %+v, want a ref to k", set.Key)
	}
	if set.Value == nil || set.Value.Kind != "ref" || set.Value.Name != "v" {
		t.Errorf("indexset Value = %+v, want a ref to v", set.Value)
	}
	if p.Body[2].Op != "return" {
		t.Errorf("body[2].Op = %q, want return", p.Body[2].Op)
	}
}

func TestMapDeclarationErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"a map key that is plainly not int or text is rejected at compile time",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let m = {true: 1}
        return m[true]
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"map key must be int or text",
		},
		{
			"a map key that is plainly not int or text is rejected on the write side too",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let mut m = {}
        m[true] = 1
        return 0
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"map key must be int or text",
		},
		{
			"index-assigning into an immutable map let is rejected",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let m = {"a": 1}
        m["a"] = 9
        return m["a"]
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"not mutable",
		},
		{
			"index-assigning a map key into a non-array-non-map local is rejected",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let mut x = 5
        x["a"] = 9
        return x
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"is not an array or map",
		},
		{
			"a map literal outside a proc is rejected",
			`app A:
    state result: int = 0
    action run():
        check len({1: 2}) > 0 "must not be empty"
        result = 1
    view Home at "/":
        box:
            text "{result}"
`,
			"only available inside a proc",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
