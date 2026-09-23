package compile

import (
	"strings"
	"testing"
)

// bytesBuildApp is the byte-buffer proving example: `sumBytes` allocates a
// zero-filled buffer with `bytes(n)`, fills it byte-by-byte with an indexed
// write in a loop (`buf[i] = i * 7 & 255`, kept in 0-255 by construction),
// then sums it back with a second loop reading it with `len` + an indexed
// read (`buf[j]`) — the same two-pass shape internal/compile/array_test.go's
// arraySumApp proves for plain arrays, but over a byte buffer instead. This
// is the IR-shape half of the proof; runtime/bytes_test.go is the
// live-over-HTTP half.
const bytesBuildApp = `app A:
    proc sumBytes(n: int) -> int:
        let mut buf = bytes(n)
        let mut i = 0
        loop i < n:
            buf[i] = (i * 7) & 255
            i = i + 1
        let mut total = 0
        let mut j = 0
        loop j < len(buf):
            total = total + buf[j]
            j = j + 1
        return total
    state result: int = 0
    action run(n: int):
        let r = do sumBytes(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestBytesBufferCompiles(t *testing.T) {
	g, err := String(bytesBuildApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 1 {
		t.Fatalf("want 1 proc, got %d", len(g.Procs))
	}
	p := g.Procs[0]
	// let(buf), let(i), loop, let(total), let(j), loop, return.
	wantOps := []string{"let", "let", "loop", "let", "let", "loop", "return"}
	if len(p.Body) != len(wantOps) {
		t.Fatalf("proc body = %+v, want %d statements (%v)", p.Body, len(wantOps), wantOps)
	}
	for i, want := range wantOps {
		if p.Body[i].Op != want {
			t.Errorf("body[%d].Op = %q, want %q", i, p.Body[i].Op, want)
		}
	}
	// `let mut buf = bytes(n)` lowers its RHS to a "call" IR node named "bytes"
	// — an ordinary builtin call, exactly like `append`'s.
	bufLet := p.Body[0]
	if bufLet.Target != "buf" || bufLet.Value == nil || bufLet.Value.Kind != "call" || bufLet.Value.Name != "bytes" {
		t.Fatalf("buf let = %+v, want Target buf, Value a `bytes(...)` call", bufLet)
	}
	// The first loop's body index-writes into buf — Op "indexset", and (this is
	// the whole point of the byte-buffer specialization) Bytes must be true so
	// the runtime range-checks the written value to 0-255, unlike a plain
	// array's indexset (see TestArrayIndexSetIsNotBytesFlagged below).
	buildLoop := p.Body[2]
	if buildLoop.Op != "loop" || len(buildLoop.Body) != 2 {
		t.Fatalf("build loop = %+v, want 2 statements (indexset buf, assign i)", buildLoop)
	}
	bufSet := buildLoop.Body[0]
	if bufSet.Op != "indexset" || bufSet.Target != "buf" {
		t.Fatalf("buf indexset = %+v, want Op indexset Target buf", bufSet)
	}
	if !bufSet.Bytes {
		t.Errorf("buf indexset .Bytes = false, want true — buf was declared via bytes(n), so its index-writes must be range-checked")
	}
	// The second loop reads buf back with len()/index — identical IR shape to a
	// plain array's (see arraySumApp), since a byte buffer IS an array value at
	// runtime; only the write path (above) diverges.
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
	idx := sumLoop.Body[0].Value.R
	if idx == nil || idx.Kind != "index" {
		t.Fatalf("buf[j] read = %+v, want Kind index", idx)
	}
}

// arrayIndexSetNotBytesApp is TestBytesBufferCompiles's control: a PLAIN
// array's indexset must NOT carry Bytes — only a bytes(n)-declared local
// gets the 0-255 write check, so an ordinary array keeps holding any int
// (this is what lets the existing array machinery — index read/write,
// len, append, copy-on-assign — serve both without a byte buffer silently
// tightening what a plain array accepts).
const arrayIndexSetNotBytesApp = `app A:
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

func TestArrayIndexSetIsNotBytesFlagged(t *testing.T) {
	g, err := String(arrayIndexSetNotBytesApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	set := g.Procs[0].Body[1]
	if set.Op != "indexset" {
		t.Fatalf("body[1].Op = %q, want indexset", set.Op)
	}
	if set.Bytes {
		t.Errorf("plain array indexset .Bytes = true, want false — only a bytes(n)-declared local should be range-checked")
	}
}

func TestBytesDeclarationErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"index-assigning into a non-array/bytes local is rejected",
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
			"bytes() takes exactly one argument",
			`app A:
    state result: int = 0
    proc bad() -> int:
        let buf = bytes()
        return len(buf)
    action run():
        let r = do bad()
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			"bytes",
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

// TestBytesFillIntListStructField: a byte buffer is an [int] whose writes are
// range-checked, so it fills an `[int]` struct field — the same thing it
// already does for a `-> [int]` proc return — while a field of any other
// element type still refuses it.
func TestBytesFillIntListStructField(t *testing.T) {
	ok := `app A:
    struct Frame:
        payload: [int]
    proc mk(n: int) -> Frame:
        let mut buf = bytes(n)
        buf[0] = 7
        return Frame{payload: buf}
    state result: int = 0
    action run:
        result = 1
`
	if _, err := String(ok); err != nil {
		t.Fatalf("bytes into an [int] field refused: %v", err)
	}
	bad := strings.Replace(ok, "payload: [int]", "payload: [text]", 1)
	_, err := String(bad)
	if err == nil || !strings.Contains(err.Error(), `field "payload" of Frame{...} wants [text], got bytes`) {
		t.Fatalf("bytes into a [text] field: want a refusal, got %v", err)
	}
}
