package compile

import (
	"strings"
	"testing"
)

// TestFloatLiteralAndArithmeticCompiles proves the basic float shape
// compiles: a float parameter, a float `let`, float arithmetic
// (`+`/`-`/`*`/`/`) between two floats, a float comparison, and a float
// return type inside a proc body (parameters, `let`/`let mut` locals, return
// types) — float is real everywhere in the language now (see isPrimitive's
// doc and TestFloatAcceptedEverywhere below), but a proc's own arithmetic is
// still where it is exercised most, so this stays the baseline case.
//
// roundIt takes plain ints and converts them to float itself, entirely
// inside the proc layer, which is the pattern every other test in this file
// uses too — not because an action couldn't pass a float argument to `do`
// directly (it can), but to keep this file's baseline cases minimal.
func TestFloatLiteralAndArithmeticCompiles(t *testing.T) {
	src := `app A:
    proc scaleAndCompare(x: float, y: float) -> float:
        let mut total = x + y
        total = total - 1.0
        total = total * 2.0
        total = total / 4.0
        if total > 0.0:
            return total
        else:
            return 0.0
    proc roundIt(a: int, b: int) -> int:
        let z = do scaleAndCompare(toFloat(a), toFloat(b))
        return round(z)
    state result: int = 0
    action run(a: int, b: int):
        let r = do roundIt(a, b)
        result = r
    view Home at "/":
        box:
            text "{result}"
`
	g, err := String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 2 {
		t.Fatalf("want 2 procs, got %d: %+v", len(g.Procs), g.Procs)
	}
	p := g.Procs[0]
	if p.Name != "scaleAndCompare" {
		t.Fatalf("proc[0] name = %q, want scaleAndCompare", p.Name)
	}
	if p.Ret != "float" || p.RetList {
		t.Fatalf("proc return = %q list=%v, want float scalar", p.Ret, p.RetList)
	}
	if len(p.Params) != 2 || p.Params[0].Type != "float" || p.Params[1].Type != "float" {
		t.Fatalf("proc params = %+v, want x,y:float", p.Params)
	}
	// `let mut total = x + y` — the `+` lowers to a "bin" IR node; its literal
	// operands elsewhere in the body (1.0, 2.0, 4.0, 0.0) must carry VType
	// "float", distinct from an int literal's "int".
	totalLet := p.Body[0]
	if totalLet.Op != "let" || totalLet.Target != "total" {
		t.Fatalf("body[0] = %+v, want let total = x + y", totalLet)
	}
	if totalLet.Value == nil || totalLet.Value.Kind != "bin" || totalLet.Value.Op != "+" {
		t.Fatalf("total's initializer = %+v, want a `+` bin node", totalLet.Value)
	}
	// total = total - 1.0
	sub := p.Body[1]
	if sub.Op != "assign" || sub.Value == nil || sub.Value.Kind != "bin" || sub.Value.Op != "-" {
		t.Fatalf("body[1] = %+v, want assign total = total - 1.0", sub)
	}
	oneLit := sub.Value.R
	if oneLit == nil || oneLit.Kind != "lit" || oneLit.VType != "float" {
		t.Fatalf("1.0 literal = %+v, want Kind lit VType float", oneLit)
	}
	if f, ok := oneLit.Val.(float64); !ok || f != 1.0 {
		t.Fatalf("1.0 literal Val = %#v (%T), want float64(1.0)", oneLit.Val, oneLit.Val)
	}
	// roundIt round-trips through toFloat/round — int arguments converted to
	// float entirely inside the proc layer, and the float result converted
	// back to int via round() before crossing back into the action.
	p2 := g.Procs[1]
	if p2.Ret != "int" {
		t.Fatalf("roundIt return = %q, want int", p2.Ret)
	}
	if len(p2.Params) != 2 || p2.Params[0].Type != "int" || p2.Params[1].Type != "int" {
		t.Fatalf("roundIt params = %+v, want a,b:int", p2.Params)
	}
	retStmt := p2.Body[len(p2.Body)-1]
	if retStmt.Op != "return" || retStmt.Value == nil || retStmt.Value.Kind != "call" || retStmt.Value.Name != "round" {
		t.Fatalf("roundIt's return = %+v, want `return round(...)`", retStmt)
	}
}

// TestFloatArrayAndMapValuesCompile proves a float works as an array element
// and as a map VALUE (not a key — see TestFloatDeclarationErrors for why a
// float key is refused) inside a proc, per this milestone's stated minimum
// bar.
func TestFloatArrayAndMapValuesCompile(t *testing.T) {
	src := `app A:
    proc sumFloats() -> float:
        let xs = [1.5, 2.5, 3.0]
        let mut total = 0.0
        let mut i = 0
        loop i < len(xs):
            total = total + xs[i]
            i = i + 1
        let scores = {"a": 1.5, "b": 2.5}
        return total + scores["a"]
    proc sumFloatsAsInt() -> int:
        let total = do sumFloats()
        return round(total)
    state result: int = 0
    action run():
        let r = do sumFloatsAsInt()
        result = r
    view Home at "/":
        box:
            text "{result}"
`
	g, err := String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 2 {
		t.Fatalf("want 2 procs, got %d", len(g.Procs))
	}
}

func TestFloatDeclarationErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"mixed int+float arithmetic is a compile error, not a promotion",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let x = 1 + 2.5
        return 0
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`,
			"needs matching numeric types",
		},
		{
			"float minus int is a compile error",
			`app A:
    state result: int = 0
    proc bad(x: float) -> int:
        let y = x - 1
        return 0
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`,
			"needs matching numeric types",
		},
		{
			"comparing an int to a float is a compile error",
			`app A:
    state result: int = 0
    proc bad(x: float) -> int:
        if x > 1:
            return 1
        else:
            return 0
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`,
			"needs matching numeric types",
		},
		{
			"%% is int-only: a float left operand is rejected",
			`app A:
    state result: int = 0
    proc bad(x: float) -> int:
        let y = x % 2
        return 0
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`,
			"% is int-only",
		},
		{
			"%% is int-only: a float right operand is rejected",
			`app A:
    state result: int = 0
    proc bad(x: float) -> int:
        let y = 5 % x
        return 0
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`,
			"% is int-only",
		},
		{
			"min/max with mismatched numeric types is a compile error",
			`app A:
    state result: int = 0
    proc bad(x: float) -> int:
        let y = min(x, 1)
        return 0
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`,
			"needs matching numeric types",
		},
		{
			"a bitwise operator refuses a float operand",
			`app A:
    state result: int = 0
    proc bad(x: float) -> int:
        let y = 1 & x
        return y
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`,
			"needs int operands",
		},
		{
			"a float literal cannot be a map key",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let m = {1.5: 1}
        return m[1.5]
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`,
			"map key must be int or text",
		},
		{
			"a float local cannot key a map on the write side either",
			`app A:
    state result: int = 0
    proc bad(x: float) -> int:
        let mut m = {}
        m[x] = 1
        return 0
    action run():
        result = 0
    view Home at "/":
        box:
            text "{result}"
`,
			"map key must be int or text",
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

// TestFloatAcceptedEverywhere proves float is a real first-class scalar
// throughout the language now, not gated to a proc body: a bare float
// literal or toFloat() in an action/check, a float-typed entity field,
// state cell, component parameter, action parameter, and record field, and
// an action binding a float-returning proc's result via `let` — every shape
// this test used to require the compiler to reject. There is no longer a
// proc-only carve-out for float: the wire (contract, entity storage, the
// client runtime) all have a real float representation now. (The type-
// mismatch rules in TestFloatDeclarationErrors above are unrelated to this
// gate and are untouched: `int + float` is still refused, just not because
// float is out of bounds outside a proc.)
func TestFloatAcceptedEverywhere(t *testing.T) {
	cases := []struct{ name, src string }{
		{
			"a float literal outside a proc",
			`app A:
    state result: int = 0
    action run():
        check 1.5 > 0.0 "must be positive"
        result = 1
    view Home at "/":
        box:
            text "{result}"
`,
		},
		{
			"toFloat outside a proc",
			`app A:
    state result: int = 0
    action run():
        check toFloat(1) > 0 "unreachable"
        result = 1
    view Home at "/":
        box:
            text "{result}"
`,
		},
		{
			"an action binds a float-returning proc's result",
			`app A:
    proc pi() -> float:
        return 3.14
    state result: int = 0
    action run():
        let r = do pi()
        result = 1
    view Home at "/":
        box:
            text "{result}"
`,
		},
		{
			"an entity field is float-typed",
			`app A:
    entity Item:
        weight: float
    state result: int = 0
    action run():
        result = 1
    view Home at "/":
        box:
            text "{result}"
`,
		},
		{
			"a state cell is float-typed",
			`app A:
    state weight: float = 0.0
    action run():
        weight = 1.0
    view Home at "/":
        box:
            text "{weight}"
`,
		},
		{
			"a component parameter is float-typed",
			`app A:
    component Badge(weight: float):
        text "{weight}"
    state result: int = 0
    action run():
        result = 1
    view Home at "/":
        box:
            use Badge(1.5)
            text "{result}"
`,
		},
		{
			"an action parameter is float-typed",
			`app A:
    state result: int = 0
    action run(x: float):
        result = 1
    view Home at "/":
        box:
            text "{result}"
`,
		},
		{
			"a record field is float-typed",
			`app A:
    record Stats:
        weight: float
    state result: int = 0
    action run():
        result = 1
    view Home at "/":
        box:
            text "{result}"
`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := String(c.src); err != nil {
				t.Fatalf("compile: %v, want float accepted here", err)
			}
		})
	}
}

// TestFloatFireAndForgetDoIsAllowedFromAction proves the containment
// boundary is exactly "an action cannot BIND a float result" — not "an
// action cannot call a float-returning proc at all". A fire-and-forget `do`
// (no `let` bind) never brings the float value into the action's own scope,
// so it is unaffected by the restriction tested in TestFloatDeclarationErrors
// above.
func TestFloatFireAndForgetDoIsAllowedFromAction(t *testing.T) {
	src := `app A:
    proc pi() -> float:
        return 3.14
    state result: int = 0
    action run():
        do pi()
        result = 1
    view Home at "/":
        box:
            text "{result}"
`
	if _, err := String(src); err != nil {
		t.Fatalf("compile: %v, want a fire-and-forget do of a float-returning proc to be allowed", err)
	}
}

// TestFloatProcToProcCallPassesFloatArgsFreely proves floats flow freely
// between procs (both always server-executed, both understand floats) — only
// the proc/action boundary is gated, not proc-to-proc `do`. The action still
// only ever passes an int across into the proc layer (see
// TestFloatLiteralAndArithmeticCompiles's doc for why); asInt converts it to
// float itself before handing it to quadruple/double.
func TestFloatProcToProcCallPassesFloatArgsFreely(t *testing.T) {
	src := `app A:
    proc double(x: float) -> float:
        return x * 2.0
    proc quadruple(x: float) -> float:
        let y = do double(x)
        let z = do double(y)
        return z
    proc asInt(n: int) -> int:
        let q = do quadruple(toFloat(n))
        return round(q)
    state result: int = 0
    action run(n: int):
        let r = do asInt(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`
	_, err := String(src)
	if err != nil {
		t.Fatalf("compile: %v, want proc-to-proc float args to be allowed", err)
	}
}
