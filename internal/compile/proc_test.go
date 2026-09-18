package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// procApp is Milestone 1's proving example: a proc computing a running total
// across a few `let mut` locals (real, reassignable mutation — not just a
// single `let` bind), called from an action via `do`, with the result flowing
// back through the action's ordinary `let x = do …` bind into a state cell a
// view renders. This is the shape ROADMAP.md's "Decision superseded: full
// self-hosting" section describes: `proc` is a new peer declaration kind, not a
// Turing-complete `action`.
const procApp = `app A:
    proc sum3(a: int, b: int, c: int) -> int:
        let mut total = a
        total = total + b
        total = total + c
        return total
    state result: int = 0
    action compute(x: int, y: int, z: int):
        let r = do sum3(x, y, z)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcCompiles(t *testing.T) {
	g, err := String(procApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 1 {
		t.Fatalf("want 1 proc, got %d: %+v", len(g.Procs), g.Procs)
	}
	p := g.Procs[0]
	if p.Name != "sum3" {
		t.Fatalf("proc name = %q, want sum3", p.Name)
	}
	if len(p.Params) != 3 || p.Params[0].Name != "a" || p.Params[0].Type != "int" {
		t.Fatalf("proc params = %+v, want a,b,c:int", p.Params)
	}
	if p.Ret != "int" || p.RetList {
		t.Fatalf("proc return = %q list=%v, want int scalar", p.Ret, p.RetList)
	}
	// Body shape: let, assign, assign, return — the mutable running total,
	// straight-line, exactly as written (Milestone 1: no loops/if to reorder or
	// fold statements).
	if len(p.Body) != 4 {
		t.Fatalf("want 4 body statements (let/assign/assign/return), got %d: %+v", len(p.Body), p.Body)
	}
	wantOps := []string{"let", "assign", "assign", "return"}
	for i, want := range wantOps {
		if p.Body[i].Op != want {
			t.Errorf("body[%d].Op = %q, want %q", i, p.Body[i].Op, want)
		}
	}
	if p.Body[0].Target != "total" {
		t.Errorf("let target = %q, want total", p.Body[0].Target)
	}
	if p.Body[3].Value == nil {
		t.Errorf("return statement carries no value")
	}

	// The calling action: `do sum3(x, y, z)` lowers to a Stmt{Op:"do"} carrying
	// the proc's name (reusing the Service field, like a service call), the
	// bind, and the proc's declared return type/list-ness for the runtime to
	// coerce the result — mirroring a request→response service `call` exactly.
	var compute *ir.Action
	for i := range g.Actions {
		if g.Actions[i].Name == "compute" {
			compute = &g.Actions[i]
		}
	}
	if compute == nil {
		t.Fatalf("action %q missing from IR", "compute")
	}
	var do *ir.Stmt
	for i := range compute.Body {
		if compute.Body[i].Op == "do" {
			do = &compute.Body[i]
		}
	}
	if do == nil {
		t.Fatalf("action %q has no `do` statement in its body: %+v", "compute", compute.Body)
	}
	if do.Service != "sum3" || do.Bind != "r" || do.Ret != "int" || do.RetList {
		t.Errorf("do stmt = proc:%q bind:%q ret:%q list:%v, want proc:sum3 bind:r ret:int scalar",
			do.Service, do.Bind, do.Ret, do.RetList)
	}
	if len(do.Args) != 3 {
		t.Errorf("do stmt has %d args, want 3", len(do.Args))
	}

	// A proc is unconditionally server-executed and has no client mirror, so an
	// action calling one must be server-placed — a client-placed action runs in
	// facet.js, which cannot execute proc code.
	if compute.Placement != "server" {
		t.Errorf("action calling a proc must be server-placed, got %q", compute.Placement)
	}
	if !strings.Contains(compute.Reason, "proc") {
		t.Errorf("placement reason should mention the proc, got %q", compute.Reason)
	}
}

// A proc with no declared return type may still be called fire-and-forget
// (`do log(x)`, no bind) from an action.
const voidProcApp = `app A:
    state seen: int = 0
    proc noop(x: int):
        let mut y = x
        y = y + 1
    action touch(x: int):
        do noop(x)
        seen = x
    view Home at "/":
        box:
            text "{seen}"
`

func TestVoidProcCompiles(t *testing.T) {
	g, err := String(voidProcApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 1 || g.Procs[0].Ret != "" {
		t.Fatalf("want 1 proc with no return type, got %+v", g.Procs)
	}
	var touch *ir.Action
	for i := range g.Actions {
		if g.Actions[i].Name == "touch" {
			touch = &g.Actions[i]
		}
	}
	if touch == nil || touch.Placement != "server" {
		t.Fatalf("action %q should exist and be server-placed (it calls a proc), got %+v", "touch", touch)
	}
	var do *ir.Stmt
	for i := range touch.Body {
		if touch.Body[i].Op == "do" {
			do = &touch.Body[i]
		}
	}
	if do == nil || do.Bind != "" {
		t.Fatalf("fire-and-forget `do noop(x)` should have no bind, got %+v", do)
	}
}

func TestProcDeclarationErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"entity access rejected",
			`app A:
    entity Note:
        id: int
    proc bad():
        add Note { id: 1 }
    view Home at "/":
        box:
            text "hi"
`,
			"pure computation",
		},
		{
			"policy access rejected",
			`app A:
    proc bad():
        requires admin
    view Home at "/":
        box:
            text "hi"
`,
			"pure computation",
		},
		{
			"reassigning an immutable let",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let x = 1
        x = 2
        return x
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
			"reassigning an undeclared name",
			`app A:
    state result: int = 0
    proc bad() -> int:
        y = 2
        return y
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"not declared",
		},
		{
			"return must be the body's last statement",
			`app A:
    state result: int = 0
    proc bad() -> int:
        return 1
        let x = 2
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"last statement",
		},
		{
			"a declared return type needs a trailing return",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let mut x = 1
        x = x + 1
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"must end with",
		},
		{
			"a proc sees only its own locals, not app state",
			`app A:
    state total: int = 0
    proc bad() -> int:
        return total
    action run():
        let r = do bad()
        total = r
    view Home at "/":
        box:
            text "{total}"
`,
			"unknown reference",
		},
		{
			"do: unknown proc",
			`app A:
    state result: int = 0
    action run():
        let r = do nope()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"unknown proc",
		},
		{
			"do: wrong argument count",
			`app A:
    state result: int = 0
    proc one(a: int) -> int:
        return a
    action run():
        let r = do one(1, 2)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"expects 1 argument",
		},
		{
			"do: binding a no-return proc",
			`app A:
    state result: int = 0
    proc sideEffect():
        let mut x = 1
        x = x + 1
    action run():
        let r = do sideEffect()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"returns nothing",
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

// ── Milestone 2: loop/if control flow ───────────────────────────────────────

// loopIfApp is Milestone 2's proving example for IR shape: `classify` is an
// if/else proc where both branches return (return-complete without a trailing
// statement), and `sumTo` is a while-style `loop` accumulating into a `let mut`
// declared outside it — the canonical iterate-and-accumulate shape.
const loopIfApp = `app A:
    proc classify(x: int, limit: int) -> text:
        if x > limit:
            return "high"
        else:
            return "low"
    proc sumTo(n: int) -> int:
        let mut total = 0
        let mut i = 0
        loop i < n:
            total = total + i
            i = i + 1
        return total
    state label: text = ""
    state result: int = 0
    action run(x: int, limit: int, n: int):
        let l = do classify(x, limit)
        let s = do sumTo(n)
        label = l
        result = s
    view Home at "/":
        box:
            text "{label}"
            text "{result}"
`

func TestProcLoopIfCompiles(t *testing.T) {
	g, err := String(loopIfApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 2 {
		t.Fatalf("want 2 procs, got %d: %+v", len(g.Procs), g.Procs)
	}
	var classify, sumTo *ir.Proc
	for i := range g.Procs {
		switch g.Procs[i].Name {
		case "classify":
			classify = &g.Procs[i]
		case "sumTo":
			sumTo = &g.Procs[i]
		}
	}
	if classify == nil || sumTo == nil {
		t.Fatalf("missing proc(s): %+v", g.Procs)
	}

	// classify: one statement, an `if` whose Body/Else each hold exactly one
	// `return` — the nested, recursive Stmt shape Milestone 2 adds.
	if len(classify.Body) != 1 || classify.Body[0].Op != "if" {
		t.Fatalf("classify.Body = %+v, want a single `if` statement", classify.Body)
	}
	ifs := classify.Body[0]
	if len(ifs.Body) != 1 || ifs.Body[0].Op != "return" {
		t.Errorf("if-then = %+v, want a single return", ifs.Body)
	}
	if len(ifs.Else) != 1 || ifs.Else[0].Op != "return" {
		t.Errorf("if-else = %+v, want a single return", ifs.Else)
	}
	if ifs.Value == nil {
		t.Errorf("if statement carries no condition")
	}

	// sumTo: let, let, loop, return — the loop's own Body nests the two
	// per-iteration mutations.
	if len(sumTo.Body) != 4 {
		t.Fatalf("sumTo.Body = %+v, want 4 statements (let,let,loop,return)", sumTo.Body)
	}
	wantOps := []string{"let", "let", "loop", "return"}
	for i, want := range wantOps {
		if sumTo.Body[i].Op != want {
			t.Errorf("sumTo.Body[%d].Op = %q, want %q", i, sumTo.Body[i].Op, want)
		}
	}
	loopStmt := sumTo.Body[2]
	if loopStmt.Value == nil {
		t.Errorf("loop statement carries no condition")
	}
	if len(loopStmt.Body) != 2 || loopStmt.Body[0].Op != "assign" || loopStmt.Body[1].Op != "assign" {
		t.Errorf("loop.Body = %+v, want two assigns (total, i)", loopStmt.Body)
	}
	if len(loopStmt.Else) != 0 {
		t.Errorf("a `loop` should never carry an Else, got %+v", loopStmt.Else)
	}
}

// breakContinueApp exercises break/continue inside a loop, and a `return`
// nested two levels deep (inside an `if` inside a `loop`) — the shape that
// proves control genuinely unwinds out of both, not just the innermost block.
const breakContinueApp = `app A:
    proc firstOver(n: int, limit: int) -> int:
        let mut i = 0
        loop i < n:
            i = i + 1
            if i > limit:
                return i
        return -1
    state result: int = 0
    action run(n: int, limit: int):
        let r = do firstOver(n, limit)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcNestedReturnCompiles(t *testing.T) {
	g, err := String(breakContinueApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	p := g.Procs[0]
	// let, loop, return — the loop's Body is [assign, if(Body=[return])].
	if len(p.Body) != 3 || p.Body[2].Op != "return" {
		t.Fatalf("firstOver.Body = %+v, want let,loop,return", p.Body)
	}
	loopStmt := p.Body[1]
	if loopStmt.Op != "loop" || len(loopStmt.Body) != 2 {
		t.Fatalf("loop.Body = %+v, want assign,if", loopStmt.Body)
	}
	nestedIf := loopStmt.Body[1]
	if nestedIf.Op != "if" || len(nestedIf.Body) != 1 || nestedIf.Body[0].Op != "return" {
		t.Fatalf("nested if = %+v, want a single return in its Body", nestedIf)
	}
}

func TestProcControlFlowDeclarationErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"break outside a loop",
			`app A:
    state result: int = 0
    proc bad():
        break
    action run():
        do bad()
    view Home at "/":
        box:
            text "hi"
`,
			"break outside a loop",
		},
		{
			"continue outside a loop",
			`app A:
    state result: int = 0
    proc bad():
        continue
    action run():
        do bad()
    view Home at "/":
        box:
            text "hi"
`,
			"continue outside a loop",
		},
		{
			"return not last in a nested block",
			`app A:
    state result: int = 0
    proc bad(n: int) -> int:
        let mut i = 0
        loop i < n:
            return i
            i = i + 1
        return -1
    action run(n: int):
        let r = do bad(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"last statement of its block",
		},
		{
			"else with no matching if",
			`app A:
    state result: int = 0
    proc bad() -> int:
        else:
            return 1
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"no matching",
		},
		{
			"one-armed if used as the whole body is not return-complete",
			`app A:
    state result: int = 0
    proc bad(x: int) -> int:
        if x > 0:
            return 1
    action run(x: int):
        let r = do bad(x)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"must end with",
		},
		{
			"if/else where the else branch doesn't return is not return-complete",
			`app A:
    state result: int = 0
    proc bad(x: int) -> int:
        let mut y = 0
        if x > 0:
            return 1
        else:
            y = 2
    action run(x: int):
        let r = do bad(x)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"must end with",
		},
		{
			"loop needs a condition",
			`app A:
    state result: int = 0
    proc bad() -> int:
        loop :
            return 1
        return 0
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"loop needs a condition",
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
