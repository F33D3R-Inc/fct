package compile

import (
	"testing"

	"facet/internal/ir"
)

// TestShortCircuitedImpureStillForcesServerPlacement is the placement/purity
// regression this fix's short-circuit change must not break: hasImpure
// (internal/ir/build.go, via procCapabilities) decides placement STATICALLY
// at compile time, once, for the whole action — not per-call. An impure
// builtin (rand()) sitting on the right of `&&` is a reason the action must
// be server-placed even though runtime/eval.go's new short-circuit means the
// right side genuinely will NOT execute on every call (e.g. when `cond` is
// false here) — some call WILL execute it (`cond` true), and placement can't
// vary call to call, so the conservative "this expression MIGHT be impure"
// analysis has to keep walking both sides of a `&&`/`||` unconditionally,
// exactly as procCapabilities' `case ast.Bin: walk(t.L); walk(t.R)` already
// does. This pins that: if a future change made hasImpure "smart" about
// short-circuiting (e.g. skipping the right side because it's "probably
// skipped"), this action would wrongly compile to Client and this test would
// catch it.
func TestShortCircuitedImpureStillForcesServerPlacement(t *testing.T) {
	g := mustCompile(t, `
app A:
    entity Log:
        id: int
        val: bool
    action mark(cond: bool):
        add Log { val: cond && rand(100) > 50 }
    view M:
        box:
            text "{count(Log)}"
`)
	mark, ok := find(g.Actions, func(a ir.Action) bool { return a.Name == "mark" })
	if !ok {
		t.Fatal("mark did not compile")
	}
	if mark.Placement != ir.Server {
		t.Errorf("mark: rand() on the right of `&&` must still force server placement (statically decided, not per-call), got %s", mark.Placement)
	}

	// Same check with the impure builtin on the right of `||`.
	g2 := mustCompile(t, `
app B:
    entity Log:
        id: int
        val: bool
    action mark(cond: bool):
        add Log { val: cond || rand(100) > 50 }
    view M:
        box:
            text "{count(Log)}"
`)
	mark2, ok := find(g2.Actions, func(a ir.Action) bool { return a.Name == "mark" })
	if !ok {
		t.Fatal("mark did not compile (||  variant)")
	}
	if mark2.Placement != ir.Server {
		t.Errorf("mark: rand() on the right of `||` must still force server placement, got %s", mark2.Placement)
	}
}
