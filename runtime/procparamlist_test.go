package runtime

import (
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

// listParamSumRunApp is this fix's headline composition shape: one proc
// (buildRange) produces a `[int]`, and a SECOND proc (sumList) consumes that
// list as its own PARAMETER — not an internal local built inside the same
// proc. Before this fix, sumList's very signature (`xs: [int]`) was a hard
// compile error (internal/parser/parser.go's parseSignature, called with
// allowList=false for a proc). The action chains the two calls exactly the
// way LANGUAGE.md documents `do` composing: `let xs = do buildRange(n); let
// total = do sumList(xs)`.
const listParamSumRunApp = `app A:
    proc buildRange(n: int) -> [int]:
        let mut xs = []
        let mut i = 0
        loop i < n:
            xs = append(xs, i)
            i = i + 1
        return xs
    proc sumList(xs: [int]) -> int:
        let mut total = 0
        let mut i = 0
        loop i < len(xs):
            total = total + xs[i]
            i = i + 1
        return total
    state result: int = 0
    action run(n: int):
        let xs = do buildRange(n)
        let total = do sumList(xs)
        result = total
    view Home at "/":
        box:
            text "{result}"
`

// TestProcListParamSumLive proves the whole chain works live over HTTP: an
// action calls a proc that returns a list, then passes that exact list into a
// second proc's list-typed parameter, which iterates it with `len`/index reads
// — the two things checkIndexTypes must recognize a list PARAMETER as legal
// for, now that a parameter can carry the arrayType tag (internal/ir/build.go's
// e.proc, fixed alongside the parser flag).
func TestProcListParamSumLive(t *testing.T) {
	g, err := compile.String(listParamSumRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// buildRange(5) = [0,1,2,3,4]; sumList of that = 0+1+2+3+4 = 10.
	deltas := postJSON(t, ts, "run", `{"args":[5]}`)
	if got := toInt(deltas["result"]); got != 10 {
		t.Fatalf("sumList(buildRange(5)) over the wire = %v, want 10", deltas["result"])
	}
}

// listParamProcToProcRunApp is the same composition, but chained entirely
// INSIDE proc-land — a third proc does both `do` calls itself, with no action
// statement in between — the shape a real multi-stage algorithm (like the
// self-hosting port's line-splitting-then-tree-building pipeline) actually
// needs: one proc's own body produces a list and immediately hands it to
// another proc as an argument, without ever surfacing to the action layer.
const listParamProcToProcRunApp = `app A:
    proc buildRange(n: int) -> [int]:
        let mut xs = []
        let mut i = 0
        loop i < n:
            xs = append(xs, i)
            i = i + 1
        return xs
    proc sumList(xs: [int]) -> int:
        let mut total = 0
        let mut i = 0
        loop i < len(xs):
            total = total + xs[i]
            i = i + 1
        return total
    proc buildAndSum(n: int) -> int:
        let xs = do buildRange(n)
        let total = do sumList(xs)
        return total
    state result: int = 0
    action run(n: int):
        let r = do buildAndSum(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestProcListParamChainedInsideAnotherProcLive proves the proc-to-proc
// chaining shape (no action-level mediation) also works: buildAndSum's own
// `let xs = do buildRange(n)` local must be tagged arrayType (internal/ir/
// build.go's procBlock, ast.Do case, fixed alongside the parameter case) so
// that passing xs on to `do sumList(xs)` — and sumList indexing it — both
// type-check.
func TestProcListParamChainedInsideAnotherProcLive(t *testing.T) {
	g, err := compile.String(listParamProcToProcRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// buildRange(6) = [0..5]; sum = 15.
	deltas := postJSON(t, ts, "run", `{"args":[6]}`)
	if got := toInt(deltas["result"]); got != 15 {
		t.Fatalf("buildAndSum(6) over the wire = %v, want 15", deltas["result"])
	}
}

// listParamTextRunApp proves the element type isn't int-specific: a `[text]`
// parameter works the same way, iterated with `len`/index reads exactly like
// `[int]`, and a literal list can be passed directly as a call argument too
// (`do joinAll(["a","b","c"])`) — which exercises internal/parser/parser.go's
// splitTop, fixed alongside this task's core change so a multi-element list
// literal survives as one `do` argument instead of being torn apart at its
// own internal commas.
const listParamTextRunApp = `app A:
    proc joinAll(parts: [text]) -> text:
        let mut out = ""
        let mut i = 0
        loop i < len(parts):
            out = out + parts[i]
            i = i + 1
        return out
    state result: text = ""
    action run():
        let r = do joinAll(["ab", "cd", "ef"])
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcListParamTextLive(t *testing.T) {
	g, err := compile.String(listParamTextRunApp)
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
	if got, want := deltas["result"], "abcdef"; got != want {
		t.Fatalf("joinAll([ab,cd,ef]) over the wire = %v, want %q", got, want)
	}
}

// listParamMutationSafetyRunApp is the copy-on-assign proof the task asks
// for: mutateCopy takes xs as a parameter, copies it into a `let mut` local
// (a parameter itself is never reassignable — only a `let mut` local is — so
// mutating "the parameter" always goes through this one extra step), and
// mutates THAT copy. The action then reuses the SAME xs value — bound once
// from buildRange, passed to mutateCopy first and sumList second — to prove
// mutateCopy's internal mutation never reached back into the value sumList
// receives afterward.
//
// This end-to-end guarantee is upheld redundantly, by design, at more than
// one layer: runProcLocked's own parameter binding clones its incoming value
// (cloneCompositeValue, mirroring `let`'s own clone — see its doc), AND
// separately every array value in this runtime is already re-allocated to
// exact len==cap by cloneArrayValue/coerceRet at every prior `let`/`do`-bind
// it passed through, which by itself rules out Go's slice-append
// capacity-reuse aliasing hazard regardless of whether any one specific
// binding point clones. Removing runProcLocked's own clone (verified by hand
// while writing this test) does not make this test fail, precisely because
// of that second, independent guarantee — so this test is a live regression
// lock on the OBSERVABLE contract LANGUAGE.md and this task describe (a
// proc's list parameter is call-by-value), not a claim that it exercises one
// single line of defense.
const listParamMutationSafetyRunApp = `app A:
    proc buildRange(n: int) -> [int]:
        let mut xs = []
        let mut i = 0
        loop i < n:
            xs = append(xs, i)
            i = i + 1
        return xs
    proc mutateCopy(xs: [int]) -> int:
        let mut ys = xs
        ys[0] = 999
        return ys[0]
    proc sumList(xs: [int]) -> int:
        let mut total = 0
        let mut i = 0
        loop i < len(xs):
            total = total + xs[i]
            i = i + 1
        return total
    state mutatedOut: int = 0
    state sumOut: int = 0
    action run(n: int):
        let xs = do buildRange(n)
        let mutated = do mutateCopy(xs)
        let total = do sumList(xs)
        mutatedOut = mutated
        sumOut = total
    view Home at "/":
        box:
            text "{mutatedOut}"
            text "{sumOut}"
`

func TestProcListParamMutationSafetyLive(t *testing.T) {
	g, err := compile.String(listParamMutationSafetyRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// buildRange(5) = [0,1,2,3,4].
	deltas := postJSON(t, ts, "run", `{"args":[5]}`)
	if got := toInt(deltas["mutatedOut"]); got != 999 {
		t.Fatalf("mutateCopy's own returned ys[0] = %v, want 999 (its local copy must actually have been mutated)", deltas["mutatedOut"])
	}
	// If mutateCopy's parameter binding had aliased the caller's array instead
	// of cloning it, xs[0] would now be 999 too, and this would be
	// 999+1+2+3+4=1009 instead of the untouched 0+1+2+3+4=10.
	if got := toInt(deltas["sumOut"]); got != 10 {
		t.Fatalf("sumList(xs) after mutateCopy(xs) over the wire = %v, want 10 (xs must be unaffected — a proc parameter is bound by copy, not by reference)", deltas["sumOut"])
	}
}
