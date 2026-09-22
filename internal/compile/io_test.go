package compile

import (
	"strings"
	"testing"
)

// ioFileApp declares `uses io.file` and calls readFile/writeFile — the
// capability-declared shape the compiler must accept. This is the IR-shape
// half of the file I/O proof; runtime/io_test.go is the live-over-HTTP half.
const ioFileApp = `app A:
    proc save(path: text, content: text) -> bool uses io.file:
        return writeFile(path, content)
    proc load(path: text) -> text uses io.file:
        return readFile(path)
    state result: text = ""
    action run(path: text, content: text):
        let ok = do save(path, content)
        let back = do load(path)
        result = back
    view Home at "/":
        box:
            text "{result}"
`

func TestIOFileCapabilityCompiles(t *testing.T) {
	g, err := String(ioFileApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 2 {
		t.Fatalf("want 2 procs, got %d: %+v", len(g.Procs), g.Procs)
	}
	names := map[string]bool{}
	for _, p := range g.Procs {
		names[p.Name] = true
	}
	if !names["save"] || !names["load"] {
		t.Fatalf("want procs save,load, got %+v", names)
	}
}

// ioNetApp mirrors ioFileApp for the network capability: `uses io.net` plus
// httpGet/httpPost.
const ioNetApp = `app A:
    proc fetch(url: text) -> text uses io.net:
        return httpGet(url)
    proc send(url: text, body: text) -> text uses io.net:
        return httpPost(url, body)
    state result: text = ""
    action run(url: text, body: text):
        let r = do fetch(url)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestIONetCapabilityCompiles(t *testing.T) {
	g, err := String(ioNetApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(g.Procs) != 2 {
		t.Fatalf("want 2 procs, got %d: %+v", len(g.Procs), g.Procs)
	}
}

// TestIOCapabilityMustBeDeclared is the compile-time enforcement proof the
// task's own milestone description calls for: a proc that calls readFile (or
// httpGet) WITHOUT declaring the matching `uses` capability must be rejected
// at compile time with a clear error naming both the missing capability and
// the builtin that needed it.
func TestIOCapabilityMustBeDeclared(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string // every one of these substrings must appear in the error
	}{
		{
			"readFile without uses io.file",
			`app A:
    proc load(path: text) -> text:
        return readFile(path)
    state result: text = ""
    action run(path: text):
        let r = do load(path)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			[]string{"io.file", "readFile", "load"},
		},
		{
			"writeFile without uses io.file",
			`app A:
    proc save(path: text, content: text) -> bool:
        return writeFile(path, content)
    state result: int = 0
    action run(path: text, content: text):
        let ok = do save(path, content)
        result = 1
    view Home at "/":
        box:
            text "{result}"
`,
			[]string{"io.file", "writeFile", "save"},
		},
		{
			"httpGet without uses io.net",
			`app A:
    proc fetch(url: text) -> text:
        return httpGet(url)
    state result: text = ""
    action run(url: text):
        let r = do fetch(url)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			[]string{"io.net", "httpGet", "fetch"},
		},
		{
			"httpPost without uses io.net",
			`app A:
    proc send(url: text, body: text) -> text:
        return httpPost(url, body)
    state result: text = ""
    action run(url: text, body: text):
        let r = do send(url, body)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			[]string{"io.net", "httpPost", "send"},
		},
		{
			"declaring io.net does not grant io.file",
			`app A:
    proc load(path: text) -> text uses io.net:
        return readFile(path)
    state result: text = ""
    action run(path: text):
        let r = do load(path)
        result = r
    view Home at "/":
        box:
            text "{result}"
`,
			[]string{"io.file", "readFile"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(c.src)
			if err == nil {
				t.Fatalf("want a compile error, got none")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should mention %q", err.Error(), want)
				}
			}
		})
	}
}

// TestIOBuiltinsRejectedOutsideProc proves readFile/writeFile/httpGet/httpPost
// are proc-only — barred from an action body entirely, the same way bitwise
// operators are (checkNoBitwise's own precedent) — since only a proc is
// unconditionally server-executed with no client mirror to disagree with a
// real I/O effect.
func TestIOBuiltinsRejectedOutsideProc(t *testing.T) {
	src := `app A:
    state result: text = ""
    action run(path: text):
        result = readFile(path)
    view Home at "/":
        box:
            text "{result}"
`
	_, err := String(src)
	if err == nil || !strings.Contains(err.Error(), "only available inside a proc") {
		t.Fatalf("want a proc-only rejection, got %v", err)
	}
}

// TestUnknownCapabilityRejected proves a typo'd or nonexistent capability name
// in a `uses` clause is a clear compile error, not a silently-never-satisfied
// declaration.
func TestUnknownCapabilityRejected(t *testing.T) {
	src := `app A:
    proc load(path: text) -> text uses io.database:
        return readFile(path)
    state result: text = ""
    action run(path: text):
        let r = do load(path)
        result = r
    view Home at "/":
        box:
            text "{result}"
`
	_, err := String(src)
	if err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Fatalf("want an unknown-capability rejection, got %v", err)
	}
}

// TestExprStmtWriteFileCompiles proves writeFile can be called as a bare
// statement (fire-and-forget, its bool result discarded) — not only bound via
// `let` — the shape ast.ExprStmt exists for.
const ioExprStmtApp = `app A:
    proc save(path: text, content: text) -> int uses io.file:
        writeFile(path, content)
        return len(content)
    state result: int = 0
    action run(path: text, content: text):
        let n = do save(path, content)
        result = n
    view Home at "/":
        box:
            text "{result}"
`

func TestExprStmtWriteFileCompiles(t *testing.T) {
	g, err := String(ioExprStmtApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	p := g.Procs[0]
	if len(p.Body) != 2 {
		t.Fatalf("want 2 body statements (exprstmt, return), got %d: %+v", len(p.Body), p.Body)
	}
	if p.Body[0].Op != "exprstmt" {
		t.Errorf("body[0].Op = %q, want exprstmt", p.Body[0].Op)
	}
}
