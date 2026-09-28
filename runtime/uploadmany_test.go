package runtime

import (
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"facet/internal/compile"
)

// `upload bind files` into a `[text]` cell takes many files: the input is
// `multiple`, and the client uploads each chosen file in turn and appends its
// URL. A single-file upload binds a text cell; any other cell is refused.
func TestUploadIntoAListTakesManyFiles(t *testing.T) {
	g, err := compile.String(`app U:
    state files: [text] = [] @client
    state one: text = "" @client
    view Home at "/":
        upload bind files label "Add files"
        upload bind one label "A file"
        text "{len(files)} files"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	res, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	page := string(b)
	if !strings.Contains(page, `data-fa-upload="files" multiple>`) || strings.Contains(page, `data-fa-upload="one" multiple`) {
		t.Errorf("the [text] upload must be multiple and the text one not:\n%s", page)
	}
	for _, bad := range []string{
		"app B:\n    state ns: [int] = [] @client\n    view V at \"/\":\n        upload bind ns\n",
		"app B:\n    state n: int = 0 @client\n    view V at \"/\":\n        upload bind n\n",
	} {
		if _, err := compile.String(bad); err == nil {
			t.Errorf("an upload into a non-text cell must be refused:\n%s", bad)
		}
	}

	// The client: every chosen file uploaded, each URL appended.
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "  async function upload(")
	if start < 0 {
		t.Fatal("assets/facet.js has no async function upload")
	}
	fn := src[start:]
	depth, end := 0, -1
	for i := 0; i < len(fn) && end < 0; i++ {
		switch fn[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				end = i + 1
			}
		}
	}
	script := fn[:end] + `
const CHUNK_BYTES = 4 * 1024 * 1024;
const store = { files: ["/uploads/old.png"] };
let n = 0;
async function uploadSingle(file) { n++; return { url: "/uploads/" + file.name }; }
async function uploadChunked(file) { throw new Error("small files take the single path"); }
function mergeMedia() {}
function refresh() {}
function showError(i, m) { console.log("ERR " + m); }
const input = { multiple: true, files: [{ name: "a.jpg", size: 10 }, { name: "b.mp4", size: 20 }], getAttribute: () => "files", closest: () => null };
upload(input).then(() => {
  const single = { multiple: false, files: [{ name: "c.png", size: 1 }, { name: "d.png", size: 1 }], getAttribute: () => "one", closest: () => null };
  return upload(single);
}).then(() => console.log(JSON.stringify({ files: store.files, one: store.one, n })));
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	want := `{"files":["/uploads/old.png","/uploads/a.jpg","/uploads/b.mp4"],"one":"/uploads/c.png","n":3}`
	if strings.TrimSpace(string(out)) != want {
		t.Errorf("client upload = %s, want %s", out, want)
	}
}
