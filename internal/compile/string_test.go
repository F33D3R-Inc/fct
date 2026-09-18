package compile

import (
	"strings"
	"testing"
)

// stringOpsCompileApp exercises split/slice/charAt in call position inside a
// proc body — the IR-shape half of the proof; runtime/string_test.go is the
// live-over-HTTP half (compiled and run, with results cross-checked against
// Go's own strings.Split/substring slicing).
const stringOpsCompileApp = `app A:
    proc doSplit(s: text, sep: text) -> [text]:
        return split(s, sep)
    proc doSlice(s: text, a: int, b: int) -> text:
        return slice(s, a, b)
    proc doCharAt(s: text, i: int) -> text:
        return charAt(s, i)
    state result: [text] = []
    action run(s: text, sep: text):
        let r = do doSplit(s, sep)
        result = r
    view Home at "/":
        box:
            text "{len(result)}"
`

func TestStringDecompositionBuiltinsCompile(t *testing.T) {
	g, err := String(stringOpsCompileApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 3 {
		t.Fatalf("want 3 procs, got %d", len(g.Procs))
	}
	byName := map[string]int{}
	for i, p := range g.Procs {
		byName[p.Name] = i
	}

	splitProc := g.Procs[byName["doSplit"]]
	if len(splitProc.Body) != 1 || splitProc.Body[0].Op != "return" {
		t.Fatalf("doSplit body = %+v, want a single `return`", splitProc.Body)
	}
	splitCall := splitProc.Body[0].Value
	if splitCall == nil || splitCall.Kind != "call" || splitCall.Name != "split" {
		t.Fatalf("doSplit return value = %+v, want a `split(...)` call", splitCall)
	}
	if len(splitCall.Args) != 2 {
		t.Fatalf("split(...) has %d args, want 2 (s, sep)", len(splitCall.Args))
	}

	sliceProc := g.Procs[byName["doSlice"]]
	sliceCall := sliceProc.Body[0].Value
	if sliceCall == nil || sliceCall.Kind != "call" || sliceCall.Name != "slice" {
		t.Fatalf("doSlice return value = %+v, want a `slice(...)` call", sliceCall)
	}
	if len(sliceCall.Args) != 3 {
		t.Fatalf("slice(...) has %d args, want 3 (s, start, end)", len(sliceCall.Args))
	}

	charAtProc := g.Procs[byName["doCharAt"]]
	charAtCall := charAtProc.Body[0].Value
	if charAtCall == nil || charAtCall.Kind != "call" || charAtCall.Name != "charAt" {
		t.Fatalf("doCharAt return value = %+v, want a `charAt(...)` call", charAtCall)
	}
	if len(charAtCall.Args) != 2 {
		t.Fatalf("charAt(...) has %d args, want 2 (s, i)", len(charAtCall.Args))
	}
}

// TestSplitReturnIsArrayIndexable proves split's result is tagged exactly
// like append/bytes's own array-typed results (internal/ir/build.go's
// inferProcType) — so a `let`-bound split result can be indexed
// (`lines[0]`) and len()'d in the same proc, the composition
// selfhost/source.fct's port needs.
func TestSplitReturnIsArrayIndexable(t *testing.T) {
	src := `app A:
    proc firstLine(src: text) -> text:
        let lines = split(src, "\n")
        return lines[0]
    state result: text = ""
    action run(src: text):
        let r = do firstLine(src)
        result = r
    view Home at "/":
        box:
            text "{result}"
`
	if _, err := String(src); err != nil {
		t.Fatalf("compile: %v", err)
	}
}

// TestStringDecompositionBuiltinArityErrors proves split/slice/charAt each
// enforce their fixed arity the same way every other pure builtin does
// (internal/ir/build.go's pureBuiltinArity/checkBuiltins) — a wrong argument
// count is a clean compile error, not a silent nil-arg runtime surprise.
func TestStringDecompositionBuiltinArityErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"split with one argument",
			`app A:
    proc bad(s: text) -> [text]:
        return split(s)
    state result: [text] = []
    action run(s: text):
        let r = do bad(s)
        result = r
    view Home at "/":
        box:
            text "{len(result)}"
`,
			"split takes 2 argument(s), got 1",
		},
		{
			"slice with two arguments",
			`app A:
    proc bad(s: text, a: int) -> text:
        return slice(s, a)
    state result: text = ""
    action run(s: text, a: int):
        let r = do bad(s, a)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"slice takes 3 argument(s), got 2",
		},
		{
			"charAt with three arguments",
			`app A:
    proc bad(s: text, a: int, b: int) -> text:
        return charAt(s, a, b)
    state result: text = ""
    action run(s: text, a: int, b: int):
        let r = do bad(s, a, b)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"charAt takes 2 argument(s), got 3",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(c.src)
			if err == nil {
				t.Fatalf("expected a compile error containing %q, got none", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected a compile error containing %q, got: %v", c.want, err)
			}
		})
	}
}
