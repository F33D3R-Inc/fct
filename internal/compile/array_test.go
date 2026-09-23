package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// arraySumApp is the array-support proving example: `sumSquares` builds an
// array from scratch with a `loop` + `append` (functional — each call returns
// a NEW array, see runtime/eval.go's callBuiltin "append" case), then sums it
// back with a second `loop` that reads it back with `len` + an indexed read
// (`xs[j]`) — a real two-pass array algorithm, not a one-line literal-and-
// return. This is the IR-shape half of the proof; runtime/array_test.go is
// the live-over-HTTP half.
const arraySumApp = `app A:
    proc sumSquares(n: int) -> int:
        let mut xs = []
        let mut i = 1
        loop i <= n:
            xs = append(xs, i * i)
            i = i + 1
        let mut total = 0
        let mut j = 0
        loop j < len(xs):
            total = total + xs[j]
            j = j + 1
        return total
    state result: int = 0
    action run(n: int):
        let r = do sumSquares(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestArraySumCompiles(t *testing.T) {
	g, err := String(arraySumApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 1 {
		t.Fatalf("want 1 proc, got %d", len(g.Procs))
	}
	p := g.Procs[0]
	// let(xs), let(i), loop, let(total), let(j), loop, return.
	wantOps := []string{"let", "let", "loop", "let", "let", "loop", "return"}
	if len(p.Body) != len(wantOps) {
		t.Fatalf("proc body = %+v, want %d statements (%v)", p.Body, len(wantOps), wantOps)
	}
	for i, want := range wantOps {
		if p.Body[i].Op != want {
			t.Errorf("body[%d].Op = %q, want %q", i, p.Body[i].Op, want)
		}
	}
	// `let mut xs = []` lowers its RHS to an empty "list" IR expression.
	xsLet := p.Body[0]
	if xsLet.Target != "xs" || xsLet.Value == nil || xsLet.Value.Kind != "list" {
		t.Fatalf("xs let = %+v, want Target xs, Value.Kind list", xsLet)
	}
	// The first loop's body reassigns xs to `append(xs, i * i)` — a "call" IR
	// node named "append", not a mutating statement (see the design note in
	// runtime/eval.go).
	buildLoop := p.Body[2]
	if buildLoop.Op != "loop" || len(buildLoop.Body) != 2 {
		t.Fatalf("build loop = %+v, want 2 statements (assign xs, assign i)", buildLoop)
	}
	xsAssign := buildLoop.Body[0]
	if xsAssign.Op != "assign" || xsAssign.Target != "xs" {
		t.Fatalf("xs assign = %+v, want Op assign Target xs", xsAssign)
	}
	if xsAssign.Value == nil || xsAssign.Value.Kind != "call" || xsAssign.Value.Name != "append" {
		t.Fatalf("xs assign value = %+v, want a `call` to `append`", xsAssign.Value)
	}
	if len(xsAssign.Value.Args) != 2 {
		t.Fatalf("append call has %d args, want 2 (xs, i * i)", len(xsAssign.Value.Args))
	}
	// The second loop's condition is `j < len(xs)` — len is an ordinary "call"
	// IR node, exactly like any other builtin.
	sumLoop := p.Body[5]
	if sumLoop.Op != "loop" {
		t.Fatalf("sum loop = %+v, want Op loop", sumLoop)
	}
	cond := sumLoop.Value
	if cond == nil || cond.Kind != "bin" || cond.Op != "<" {
		t.Fatalf("sum loop cond = %+v, want a `<` comparison", cond)
	}
	if cond.R == nil || cond.R.Kind != "call" || cond.R.Name != "len" {
		t.Fatalf("sum loop cond RHS = %+v, want a `len(...)` call", cond.R)
	}
	// The sum loop's body reads `xs[j]` — an "index" IR node reusing Obj (the
	// array) and Key (the index expression), exactly like "get"/"eget" reuse
	// those same fields for their own addressing sub-expressions.
	if len(sumLoop.Body) != 2 {
		t.Fatalf("sum loop body = %+v, want 2 statements (assign total, assign j)", sumLoop.Body)
	}
	totalAssign := sumLoop.Body[0]
	if totalAssign.Op != "assign" || totalAssign.Target != "total" {
		t.Fatalf("total assign = %+v, want Op assign Target total", totalAssign)
	}
	// total = total + xs[j]; the RHS is `total + xs[j]`, so R is the index read.
	rhs := totalAssign.Value
	if rhs == nil || rhs.Kind != "bin" || rhs.Op != "+" {
		t.Fatalf("total assign value = %+v, want a `+` binary expression", rhs)
	}
	idx := rhs.R
	if idx == nil || idx.Kind != "index" {
		t.Fatalf("xs[j] read = %+v, want Kind index", idx)
	}
	if idx.Obj == nil || idx.Obj.Kind != "ref" || idx.Obj.Name != "xs" {
		t.Fatalf("index Obj = %+v, want ref to xs", idx.Obj)
	}
	if idx.Key == nil || idx.Key.Kind != "ref" || idx.Key.Name != "j" {
		t.Fatalf("index Key = %+v, want ref to j", idx.Key)
	}

	var run *ir.Action
	for i := range g.Actions {
		if g.Actions[i].Name == "run" {
			run = &g.Actions[i]
		}
	}
	if run == nil || run.Placement != "server" {
		t.Fatalf("action %q should exist and be server-placed (it calls a proc), got %+v", "run", run)
	}
}

// indexAssignApp is the indexed-write proving example: `xs[i] = v` lowers to
// its own IR statement shape (Op "indexset"), distinct from a plain
// reassignment (Op "assign") — it carries an extra sub-expression (the
// index) a bare-name target never has.
const indexAssignApp = `app A:
    proc replaceFirst(v: int) -> int:
        let mut xs = [1, 2, 3]
        xs[0] = v
        return xs[0]
    state result: int = 0
    action run(v: int):
        let r = do replaceFirst(v)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestIndexAssignCompiles(t *testing.T) {
	g, err := String(indexAssignApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	p := g.Procs[0]
	// let, indexset, return.
	if len(p.Body) != 3 {
		t.Fatalf("proc body = %+v, want 3 statements (let, indexset, return)", p.Body)
	}
	if p.Body[0].Op != "let" {
		t.Errorf("body[0].Op = %q, want let", p.Body[0].Op)
	}
	set := p.Body[1]
	if set.Op != "indexset" {
		t.Fatalf("body[1].Op = %q, want indexset", set.Op)
	}
	if set.Target != "xs" {
		t.Errorf("indexset Target = %q, want xs", set.Target)
	}
	if set.Key == nil || set.Key.Kind != "lit" {
		t.Errorf("indexset Key = %+v, want a literal (0)", set.Key)
	}
	if set.Value == nil || set.Value.Kind != "ref" || set.Value.Name != "v" {
		t.Errorf("indexset Value = %+v, want a ref to v", set.Value)
	}
	if p.Body[2].Op != "return" {
		t.Errorf("body[2].Op = %q, want return", p.Body[2].Op)
	}
}

func TestArrayDeclarationErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"indexing a non-array local is rejected",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let x = 5
        return x[0]
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"is not an array",
		},
		{
			"index-assigning into an immutable let is rejected",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let xs = [1, 2, 3]
        xs[0] = 9
        return xs[0]
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
			"index-assigning into a non-array local is rejected",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let mut x = 5
        x[0] = 9
        return x
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"is not an array",
		},
		{
			"index-assigning into an undeclared name is rejected",
			`app A:
    state result: int = 0
    proc bad() -> int:
        xs[0] = 9
        return 0
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"is not declared",
		},
		{
			"indexing a value that is neither a list nor json outside a proc is rejected",
			`app A:
    state name: text = "x"
    view Home at "/":
        box:
            text "{name[0]}"
`,
			"neither",
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
