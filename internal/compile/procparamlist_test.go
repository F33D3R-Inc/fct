package compile

import (
	"strings"
	"testing"
)

// listParamApp is the shape this test file exists for: a proc parameter that
// is itself list-typed (`xs: [int]`) — previously a hard compile error
// ("parameter %q cannot be a list", internal/parser/parser.go's
// parseSignature, called with allowList=false for a proc's own params). The
// fix flips that to allowList=true for parseProc specifically, the same
// capability `service` operations already had. sumList's body exercises
// exactly the two things a list-typed local needs to keep working once it
// arrives as a PARAMETER rather than a `let`/`let mut`-bound local: `len(xs)`
// and an indexed read `xs[i]` — both of which internal/ir/build.go's
// checkIndexTypes would incorrectly reject as "not an array" if the fix only
// flipped the parser flag and forgot to also tag a list-typed parameter's
// static type as arrayType (e.Params's `types` map) in internal/ir/build.go's
// proc().
const listParamApp = `app A:
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

func TestProcListParamCompiles(t *testing.T) {
	g, err := String(listParamApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var found bool
	for _, p := range g.Procs {
		if p.Name != "sumList" {
			continue
		}
		found = true
		if len(p.Params) != 1 {
			t.Fatalf("sumList params = %+v, want exactly 1", p.Params)
		}
		prm := p.Params[0]
		if prm.Name != "xs" {
			t.Errorf("param name = %q, want xs", prm.Name)
		}
		if prm.Type != "int" {
			t.Errorf("param element type = %q, want int (the list's element type, not \"array\")", prm.Type)
		}
		if !prm.List {
			t.Errorf("param List = false, want true — a `[int]` parameter must be tagged as a list in the IR (ir.Param.List)")
		}
		// The body must have compiled at all: len(xs)/xs[i] inside a proc whose
		// OWN parameter is list-typed is exactly what checkIndexTypes previously
		// had no way to prove safe (see this file's doc comment) — reaching this
		// point at all is the regression proof.
		if len(p.Body) == 0 {
			t.Fatalf("sumList has no compiled body")
		}
	}
	if !found {
		t.Fatalf("proc %q missing from compiled IR", "sumList")
	}
}

// listParamRejectsScalarIndexApp is the control case: an ordinary scalar
// parameter must still be refused for indexing exactly as before — proving
// the fix didn't loosen checkIndexTypes into accepting everything, only what
// is genuinely list-typed.
const listParamRejectsScalarIndexApp = `app A:
    proc bad(x: int) -> int:
        return x[0]
    action run(x: int):
        do bad(x)
`

func TestProcScalarParamStillRejectsIndexing(t *testing.T) {
	_, err := String(listParamRejectsScalarIndexApp)
	if err == nil {
		t.Fatalf("expected a compile error indexing a scalar (int) parameter, got none")
	}
	if !strings.Contains(err.Error(), "not an array or map") {
		t.Errorf("error = %q, want it to explain %q is not an array or map", err.Error(), "x")
	}
}

// listParamTextApp proves the fix isn't int-specific: any primitive element
// type is a legal list-parameter element type, exactly as it already is for a
// list RETURN type.
const listParamTextApp = `app A:
    proc joinAll(parts: [text]) -> text:
        let mut out = ""
        let mut i = 0
        loop i < len(parts):
            out = out + parts[i]
            i = i + 1
        return out
    state result: text = ""
    action run():
        let r = do joinAll(["a", "b", "c"])
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcListParamTextElementCompiles(t *testing.T) {
	if _, err := String(listParamTextApp); err != nil {
		t.Fatalf("compile: %v", err)
	}
}
