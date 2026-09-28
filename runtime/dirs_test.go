package runtime

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
)

// dirTool is a command over the directory builtins: `grant <dir>` grants
// the directory, then the rest of args are steps run in order, each
// writing one line: `write <path> <content>`, `read <path>`, `list <path>`,
// `mkdir <path>`, `grantSub <dir>` (a path the program computed, which
// must not widen anything), `line` and `rest` (stdin).
const dirTool = `app DirTool:
    proc main(args: [text]) -> int uses io.file, io.console:
        let mut i = 0
        loop i < len(args):
            let op = args[i]
            if op == "grant":
                let g = grantDir(args[i + 1])
                let w = writeStdout("grant " + g + "\n")
                i = i + 2
            else:
                if op == "grantSub":
                    let g2 = grantDir(args[i + 1] + "/sub")
                    let w2 = writeStdout("grantSub " + g2 + "\n")
                    i = i + 2
                else:
                    if op == "write":
                        let ok = writeFile(args[i + 1], args[i + 2])
                        let w3 = writeStdout("write " + ok + "\n")
                        i = i + 3
                    else:
                        if op == "read":
                            let w4 = writeStdout("read " + readFile(args[i + 1]) + "\n")
                            i = i + 2
                        else:
                            if op == "list":
                                let names = listDir(args[i + 1])
                                let w5 = writeStdout("list " + join(names, ",") + "\n")
                                i = i + 2
                            else:
                                if op == "mkdir":
                                    let m = makeDir(args[i + 1])
                                    let w6 = writeStdout("mkdir " + m + "\n")
                                    i = i + 2
                                else:
                                    if op == "line":
                                        let l = readStdinLine()
                                        let w7 = writeStdout("line [" + l + "]\n")
                                        i = i + 1
                                    else:
                                        let r = readStdin()
                                        let w8 = writeStdout("rest [" + r + "]\n")
                                        i = i + 1
        return 0
`

func dirToolRun(t *testing.T, sandbox, stdin string, args ...string) (string, error) {
	t.Helper()
	g, err := compile.String(dirTool)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDataDir(sandbox)
	var out stdioSyncBuffer
	srv.SetStdio(strings.NewReader(stdin), &out, io.Discard)
	_, err = srv.RunMain(args)
	return out.String(), err
}

// TestGrantDirOpensAnOperatorNamedDirectory: a directory named on argv is
// readable, writable, listable and can have directories made in it once
// main grants it; its files are listed sorted, without subdirectories.
func TestGrantDirOpensAnOperatorNamedDirectory(t *testing.T) {
	sandbox, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, a := filepath.Join(outside, "b.dat"), filepath.Join(outside, "a.dat")
	out, err := dirToolRun(t, sandbox, "", "grant", outside, "write", b, "B", "write", a, "A",
		"read", b, "list", outside, "mkdir", filepath.Join(outside, "x", "y"), "list", filepath.Join(outside, "missing"))
	if err != nil {
		t.Fatalf("main: %v\n%s", err, out)
	}
	want := "grant true\nwrite true\nwrite true\nread B\nlist a.dat,b.dat\nmkdir true\nlist \n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	if fi, err := os.Stat(filepath.Join(outside, "x", "y")); err != nil || !fi.IsDir() {
		t.Fatalf("makeDir did not make the directory: %v", err)
	}
}

// TestGrantDirRefusesWhatTheOperatorDidNotName: without a grant the
// directory stays outside the sandbox, and a path the program computed
// (not an argument or an environment value) is not granted.
func TestGrantDirRefusesWhatTheOperatorDidNotName(t *testing.T) {
	sandbox, outside := t.TempDir(), t.TempDir()
	if _, err := dirToolRun(t, sandbox, "", "write", filepath.Join(outside, "f"), "x"); err == nil || !strings.Contains(err.Error(), "escapes the sandboxed data directory") {
		t.Fatalf("an ungranted write = %v, want a sandbox refusal", err)
	}
	out, err := dirToolRun(t, sandbox, "", "grantSub", outside, "mkdir", filepath.Join(outside, "sub", "z"))
	if !strings.HasPrefix(out, "grantSub false\n") || err == nil || !strings.Contains(err.Error(), "escapes the sandboxed data directory") {
		t.Fatalf("a computed grant = %q, %v; want false and the sandbox kept", out, err)
	}
	if _, err := dirToolRun(t, sandbox, "", "list", outside); err == nil {
		t.Fatal("listDir outside the sandbox succeeded without a grant")
	}
}

// TestGrantDirIsEntryOnly: like grantRead, only `proc main` may grant.
func TestGrantDirIsEntryOnly(t *testing.T) {
	_, err := compile.String(`app G:
    proc widen(p: text) -> bool uses io.file:
        let g = grantDir(p)
        return g
`)
	if err == nil || !strings.Contains(err.Error(), "grantDir(...) is only available in `proc main") {
		t.Fatalf("grantDir outside main = %v, want refused", err)
	}
}

// TestReadStdinLine: one line at a time, its newline kept, nothing past it
// consumed (readStdin then reads the rest), "" at end of input.
func TestReadStdinLine(t *testing.T) {
	out, err := dirToolRun(t, t.TempDir(), "yes\nno\nrest", "line", "line", "rest", "line")
	if err != nil {
		t.Fatal(err)
	}
	want := "line [yes\n]\nline [no\n]\nrest [rest]\nline []\n"
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

// TestIndexOfOverBytes: indexOf searches a byte buffer natively — an int
// finds that byte from a position, a list of byte values the first byte
// that is any of them — through a proc parameter as through a local.
func TestIndexOfOverBytes(t *testing.T) {
	g, err := compile.String(`app IB:
    proc scan(bs: [int], from: int) -> text:
        return "" + indexOf(bs, 34, from) + " " + indexOf(bs, [34, 92, 0, 1, 31], from) + " " + indexOf(bs, 200, from)
    proc main(args: [text]) -> int uses io.console:
        let bs = textToBytes("ab\\\\c\"d")
        let w = writeStdout(scan(bs, 0) + "|" + scan(bs, 5) + "|" + scan(bs, 9) + "|" + indexOf(textToBytes("x` + "\x01" + `y"), [0, 1, 31], 0) + "\n")
        return 0
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	var out stdioSyncBuffer
	srv.SetStdio(strings.NewReader(""), &out, io.Discard)
	if _, err := srv.RunMain(nil); err != nil {
		t.Fatal(err)
	}
	// bytes: a b \ \ c " d → the quote at 5, the first backslash at 2
	if got, want := out.String(), "5 2 -1|5 5 -1|-1 -1 -1|1\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestBytesPut: bytesPut(dst, at, src) is dst with src written at at — as a
// value (dst unchanged), in place on a buffer the slot owns, never through
// a buffer another name still shares, from a source that is a slice of the
// destination, and refused outside the buffer.
func TestBytesPut(t *testing.T) {
	g, err := compile.String(`app BP:
    proc show(b: [int]) -> text:
        let mut out = ""
        let mut i = 0
        loop i < len(b):
            out = out + b[i]
            i = i + 1
        return out
    proc main(args: [text]) -> int uses io.console:
        let a = bytes(6)
        let v = bytesPut(a, 2, [7, 8])
        let mut b = bytes(6)
        b = bytesPut(b, 1, [1, 2, 3])
        let shared = b
        b = bytesPut(b, 0, [9])
        b = bytesPut(b, 3, slice(b, 0, 3))
        let w = writeStdout(show(a) + " " + show(v) + " " + show(shared) + " " + show(b) + "\n")
        let bad = bytesPut(b, 5, [1, 2])
        return 0
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	var out stdioSyncBuffer
	srv.SetStdio(strings.NewReader(""), &out, io.Discard)
	_, err = srv.RunMain(nil)
	if got, want := out.String(), "000000 007800 012300 912912\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if err == nil || !strings.Contains(err.Error(), "bytesPut: writing 2 bytes at 5 is outside a buffer of 6 bytes") {
		t.Fatalf("an out-of-range write = %v", err)
	}
}
