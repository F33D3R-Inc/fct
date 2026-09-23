package runtime

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"facet/internal/compile"
)

// ioPagedRunApp exercises the random-access file builtins a pager and a
// crash-safe small-file update are built from: fileSize, writeFileAt (at an
// offset past the end, and over existing bytes), readFileAt, syncFile and
// renameFile.
const ioPagedRunApp = `app P:
    proc page(n: int, fill: int) -> [int]:
        let mut b = bytes(n)
        let mut i = 0
        loop i < n:
            b[i] = (fill + i) % 256
            i = i + 1
        return b
    proc run(path: text) -> text uses io.file:
        let before = fileSize(path)
        let p0 = do page(4, 10)
        let p2 = do page(4, 200)
        let w0 = writeFileAt(path, 0, p0)
        let w2 = writeFileAt(path, 8, p2)
        let s = syncFile(path)
        let size = fileSize(path)
        let back = readFileAt(path, 8, 4)
        let hole = readFileAt(path, 4, 4)
        let over = do page(2, 7)
        let w1 = writeFileAt(path, 1, over)
        let head = readFileAt(path, 0, 4)
        let tmp = writeFileAt("ckpt.tmp", 0, over)
        let r = renameFile("ckpt.tmp", "ckpt")
        let moved = fileExists("ckpt.tmp")
        return "" + before + "|" + size + "|" + back[0] + "," + back[3] + "|" + hole[0] + "|" + head[0] + "," + head[1] + "," + head[2] + "," + head[3] + "|" + fileSize("ckpt") + "|" + moved
    proc short(path: text) -> text uses io.file:
        let b = readFileAt(path, 0, 100)
        return "" + len(b)
    state result: text = ""
    action go(path: text):
        let out = do run(path)
        result = out
    action tooFar(path: text):
        let out = do short(path)
        result = out
    view Home at "/":
        text "{result}"
`

func TestPagedFileBuiltinsLive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACET_DATA_DIR", dir)
	g, err := compile.String(ioPagedRunApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "go", `{"args":["pages/data.pg"]}`)
	// absent (-1); 12 bytes after writing [0,4) and [8,12); bytes 200 and
	// 203 back; the gap reads as zeros; the overwrite at 1 replaced two
	// bytes in place; the renamed file holds 2 bytes and the temp is gone.
	want := "-1|12|200,203|0|10,7,8,13|2|false"
	if got := fmt.Sprint(deltas["result"]); got != want {
		t.Fatalf("result = %q, want %q", got, want)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pages", "data.pg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 12 || data[0] != 10 || data[1] != 7 || data[8] != 200 {
		t.Fatalf("file on disk = %v", data)
	}
	// A read past the end is an error, never a short buffer.
	res, err := postRaw(t, ts, "tooFar", `{"args":["pages/data.pg"]}`)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode < 400 {
		t.Fatalf("readFileAt past the end answered %d, want an error", res.StatusCode)
	}
}

func TestPagedFileBuiltinsNeedCapability(t *testing.T) {
	src := "app P:\n    proc f(p: text) -> int:\n        return fileSize(p)\n    view Home at \"/\":\n        text \"x\"\n"
	if _, err := compile.String(src); err == nil {
		t.Fatal("fileSize compiled in a proc without `uses io.file`")
	}
	src2 := "app P:\n    view Home at \"/\":\n        text \"{fileSize(\"a\")}\"\n"
	if _, err := compile.String(src2); err == nil {
		t.Fatal("fileSize compiled outside a proc")
	}
}
