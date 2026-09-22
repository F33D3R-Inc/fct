package selfhost

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

// proc_lower.fct is the self-hosted port of internal/ir/build.go's proc() +
// procBlock(). This pins it to the real thing: every proc below is compiled
// by the REAL compiler (compile.String over a small app) and its ir.Proc
// json.Marshal'ed, and by proc_lower.fct's lowerProcJSON over the same
// header line + body text; the two JSON strings are unmarshaled into
// ir.Proc and compared with reflect.DeepEqual, then compared byte for byte.
// The compile-error cases check that the real compiler refuses the proc
// with a message the port's lowerProcDiag reproduces as a substring.

func loadProcLowerApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("proc_lower.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/proc_lower.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// procCase is one proc of the fixture app: its header line exactly as
// written in the source, and its body lines (each already carrying the
// 8-space indentation it has under `app ... :` / `proc ... :`, so the very
// same text is handed to the port — stmt.fct's body parser is indentation-
// relative, as parseProcBody is).
type procCase struct {
	name   string
	header string
	body   string
}

var procLowerFiles = []struct{ name, typ, path string }{
	{"Config", "text", "config.txt"},
	{"Blob", "bytes", "blob.bin"},
}

// procLowerGood are the procs the real compiler accepts; they reference
// each other by `do`/`spawn`, so they are always compiled as one app.
var procLowerGood = []procCase{
	{"counters", "proc counters(n: int) -> int:", `
        let base = n * 2
        let mut acc = base
        acc = acc + 1
        acc = acc - n
        return acc`},
	{"loopy", "proc loopy(n: int) -> int:", `
        let mut i = 0
        let mut total = 0
        loop i < n:
            i = i + 1
            if i == 3:
                continue
            if i > 7:
                break
            total = total + i
        return total`},
	{"branchy", "proc branchy(x: int, name: text) -> text:", `
        if x > 10:
            if name == "":
                return "big-anon"
            else:
                return "big-" + name
        else:
            let mut s = "small"
            if x < 0:
                s = "negative"
            return s + ":" + name`},
	{"listy", "proc listy(n: int) -> [int]:", `
        let mut xs = []
        let mut i = 0
        loop i < n:
            xs = append(xs, i * i)
            i = i + 1
        if n > 0:
            xs[0] = 100
        return xs`},
	{"buffer", "proc buffer(n: int) -> int:", `
        let mut buf = bytes(n)
        let mut i = 0
        loop i < n:
            buf[i] = i + 1
            i = i + 1
        return len(buf) + buf[0]`},
	{"useList", "proc useList(n: int) -> int:", `
        let xs = do listy(n)
        do counters(n)
        let total = len(xs) + xs[0]
        return total`},
	{"parallel", "proc parallel(n: int) -> int:", `
        let h1 = spawn counters(n)
        let h2 = spawn listy(n)
        let a = do counters(1)
        let r1 = join h1
        join h2
        return r1 + a`},
	{"files", "proc files(content: text) -> text uses io.file:", `
        let ok = write Config(content)
        let back = read Config()
        write Config(back + "!")
        let raw = read Blob()
        let okb = write Blob(raw)
        if ok && okb:
            return back
        return bytesToText(raw)`},
	{"noisy", "proc noisy(n: int) -> int:", `
        print(n)
        let m = print(n + 1)
        return m`},
	{"quiet", "proc quiet(n: int):", `
        let x = n + 1
        print(x)
        return`},
	{"mapped", "proc mapped(k: text, opt: int?, names: [text]) -> int:", `
        let mut m = {"a": 1, "b": 2}
        m[k] = len(names)
        let mut i = 0
        loop i < len(names):
            m[names[i]] = i
            i = i + 1
        return m[k]`},
}

// procLowerBad are the procs the real compiler refuses, each with the
// message substring both sides must produce.
var procLowerBad = []struct {
	pc   procCase
	want string
}{
	{procCase{"e1", "proc e1(n: int) -> int:", `
        let x = n
        x = x + 1
        return x`}, `"x" is not mutable`},
	{procCase{"e2", "proc e2(n: int) -> int:", `
        let mut i = 0
        loop i < n:
            break
            i = i + 1
        return i`}, "break must be the last statement of its block"},
	{procCase{"e3", "proc e3(n: int) -> int:", `
        if n > 0:
            return 1`}, "must end with `return <expr>` on every path"},
	{procCase{"e4", "proc e4(n: int) -> int:", `
        let h = spawn counters(n)
        return n`}, `has a spawned task handle "h" still unjoined when returning`},
	{procCase{"e5", "proc e5(n: int) -> int:", `
        y = n + 1
        return y`}, `"y" is not declared in proc "e5"`},
	{procCase{"e6", "proc e6(n: int) -> int:", `
        let x = n + missing
        return x`}, `unknown reference "missing"`},
	{procCase{"e7", "proc e7(n: int) -> text:", `
        let s = read Config()
        return s`}, `requires capability "io.file"`},
	{procCase{"e8", "proc e8(n: int) -> int:", `
        let mut i = 0
        continue
        return i`}, `continue outside a loop in proc "e8"`},
	{procCase{"e9", "proc e9(n: int) -> int:", `
        let x = n
        let x = n + 1
        return x`}, `"x" is already declared in proc "e9"`},
	{procCase{"e10", "proc e10(n: int) -> int:", `
        let x = n
        x[0] = 1
        return x`}, `"x" is not mutable`},
	{procCase{"e11", "proc e11(n: int) -> int:", `
        let r = do quiet(n)
        return n`}, `proc "quiet" returns nothing`},
	{procCase{"e12", "proc e12(n: int) -> int:", `
        let h = spawn counters(n)
        let r = join h
        join h
        return r`}, `"h" was already joined`},
	{procCase{"e13", "proc e13(n: int) -> int:", `
        let mut n2 = n
        return n2
        n2 = 1`}, "return must be the last statement of its block"},
	{procCase{"e14", "proc e14(n: int) -> int:", `
        let x = n
        return`}, `proc "e14" returns int, so ` + "`return`" + ` needs a value`},
	{procCase{"e15", "proc e15(n: int):", `
        return n`}, `proc "e15" declares no return type`},
}

func procLowerAppSrc(procs []procCase) string {
	var b strings.Builder
	b.WriteString("app ProcLowerCases:\n")
	for _, f := range procLowerFiles {
		b.WriteString("    file " + f.name + ": " + f.typ + " at \"" + f.path + "\"\n")
	}
	b.WriteString("\n")
	for _, p := range procs {
		b.WriteString("    " + p.header + "\n")
		b.WriteString(strings.TrimPrefix(p.body, "\n") + "\n\n")
	}
	b.WriteString("    view Home at \"/\":\n        box:\n            text \"x\"\n")
	return b.String()
}

func procLowerGraph(t *testing.T) *ir.IR {
	t.Helper()
	g, err := compile.String(procLowerAppSrc(procLowerGood))
	if err != nil {
		t.Fatalf("compile fixture app: %v", err)
	}
	return g
}

func goProcJSON(t *testing.T, g *ir.IR, name string) string {
	t.Helper()
	for _, p := range g.Procs {
		if p.Name != name {
			continue
		}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Fatalf("proc %s not found in the compiled graph", name)
	return ""
}

// procLowerSigs is the port's parallel-list view of e.procSigs — every
// proc of the fixture app, good ones and (for the error cases) the one
// under test, read off the REAL compiled graph so the port never guesses
// a signature.
func procLowerSigs(g *ir.IR, extra procCase) (names, rets, retLists string) {
	var ns, rs, ls []string
	for _, p := range g.Procs {
		ns = append(ns, p.Name)
		rs = append(rs, p.Ret)
		if p.RetList {
			ls = append(ls, "true")
		} else {
			ls = append(ls, "false")
		}
	}
	if extra.name != "" {
		head := strings.TrimSuffix(strings.TrimSpace(extra.header), ":")
		ret := ""
		if i := strings.Index(head, "->"); i >= 0 {
			ret = strings.TrimSpace(head[i+2:])
			if j := strings.Index(ret, " uses "); j >= 0 {
				ret = strings.TrimSpace(ret[:j])
			}
		}
		list := strings.HasPrefix(ret, "[")
		ret = strings.Trim(ret, "[]")
		ns = append(ns, extra.name)
		rs = append(rs, ret)
		if list {
			ls = append(ls, "true")
		} else {
			ls = append(ls, "false")
		}
	}
	return strings.Join(ns, "\n"), strings.Join(rs, "\n"), strings.Join(ls, "\n")
}

func selfLowerProc(t *testing.T, ts *httptest.Server, g *ir.IR, pc procCase, extra procCase) (jsonOut, diag string) {
	t.Helper()
	names, rets, lists := procLowerSigs(g, extra)
	var fn, ft, fp []string
	for _, f := range procLowerFiles {
		fn = append(fn, f.name)
		ft = append(ft, f.typ)
		fp = append(fp, f.path)
	}
	d := postExprJSON(t, ts, "runLowerProc",
		pc.header, strings.TrimPrefix(pc.body, "\n"),
		names, rets, lists,
		strings.Join(fn, "\n"), strings.Join(ft, "\n"), strings.Join(fp, "\n"),
		"")
	j, _ := d["procLowerResult"].(string)
	dg, _ := d["procLowerDiag"].(string)
	return j, dg
}

func assertSameProc(t *testing.T, name, got, want string) {
	t.Helper()
	var gp, wp ir.Proc
	if err := json.Unmarshal([]byte(got), &gp); err != nil {
		t.Fatalf("%s: selfhost JSON does not unmarshal into ir.Proc: %v\n  %s", name, err, got)
	}
	if err := json.Unmarshal([]byte(want), &wp); err != nil {
		t.Fatalf("%s: real compiler JSON does not unmarshal into ir.Proc: %v\n  %s", name, err, want)
	}
	if !reflect.DeepEqual(gp, wp) {
		t.Errorf("%s: selfhost proc lowering disagrees with the real compiler:\n  got  %s\n  want %s", name, got, want)
		return
	}
	if got != want {
		t.Errorf("%s: same ir.Proc, but the JSON spelling differs:\n  got  %s\n  want %s", name, got, want)
	}
}

// TestProcLowerMatchesGo: every accepted proc lowers to the identical
// ir.Proc, spelled identically — let/let mut/reassign, loop + break/
// continue, nested if/else with returns in both arms, list build with
// append + index assignment, a bytes(n) buffer with the `bytes` flag on
// its indexset, `do` bound (list-returning) and unbound, spawn/join,
// read/write on declared files (text and bytes), a bare `print`
// exprstmt, a no-return proc with a bare `return`, and a map with
// index-assigned keys plus optional/list params.
func TestProcLowerMatchesGo(t *testing.T) {
	ts := loadProcLowerApp(t)
	g := procLowerGraph(t)
	for _, pc := range procLowerGood {
		t.Run(pc.name, func(t *testing.T) {
			want := goProcJSON(t, g, pc.name)
			got, diag := selfLowerProc(t, ts, g, pc, procCase{})
			if diag != "" {
				t.Fatalf("%s: selfhost refused a proc the real compiler accepts: %s", pc.name, diag)
			}
			assertSameProc(t, pc.name, got, want)
		})
	}
}

// TestProcLowerDiagnosticsMatchGo: every refused proc is refused by both,
// and the real compiler's message contains the same substring the port's
// lowerProcDiag reports (the port's whole message is, in turn, contained
// in the real one, since it spells build.go's fmt.Sprintf verbatim).
func TestProcLowerDiagnosticsMatchGo(t *testing.T) {
	ts := loadProcLowerApp(t)
	g := procLowerGraph(t)
	for _, c := range procLowerBad {
		t.Run(c.pc.name, func(t *testing.T) {
			_, err := compile.String(procLowerAppSrc(append(append([]procCase{}, procLowerGood...), c.pc)))
			if err == nil {
				t.Fatalf("%s: real compiler accepted a proc this case expects it to refuse", c.pc.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: real compiler's message %q does not contain %q — fixture is stale", c.pc.name, err.Error(), c.want)
			}
			got, diag := selfLowerProc(t, ts, g, c.pc, c.pc)
			if diag == "" {
				t.Fatalf("%s: selfhost accepted a proc the real compiler refuses (%v); got %s", c.pc.name, err, got)
			}
			if got != "" {
				t.Errorf("%s: a refused proc must lower to no JSON, got %s", c.pc.name, got)
			}
			if !strings.Contains(diag, c.want) {
				t.Errorf("%s: selfhost diag %q does not contain %q\n  real: %v", c.pc.name, diag, c.want, err)
			}
			if !strings.Contains(err.Error(), diag) {
				t.Errorf("%s: selfhost diag is not a substring of the real message:\n  got  %q\n  real %q", c.pc.name, diag, err.Error())
			}
		})
	}
}
