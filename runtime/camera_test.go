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

// `camera bind shot label "Capture"`: a live viewfinder, a shutter and a
// flip, and a capture-from-camera file input for a browser without one; the
// shutter's still is uploaded and its URL written to the @client text cell.
func TestCameraCapturesIntoATextCell(t *testing.T) {
	g, err := compile.String(`app C:
    state shot: text = "" @client
    view Home at "/":
        camera bind shot label "Capture"
        text "shot={shot}"
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
	for _, want := range []string{`data-fa-camera="shot"`, `<video class="fa-camera-view" autoplay playsinline muted>`,
		`data-fa-camera-shutter aria-label="Capture"`, `data-fa-camera-flip`, `accept="image/*" capture="environment" data-fa-upload="shot"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the camera's markup lacks %s", want)
		}
	}
	for _, bad := range []string{
		"app B:\n    state shot: text = \"\"\n    view V at \"/\":\n        camera bind shot\n",
		"app B:\n    state shots: [text] = [] @client\n    view V at \"/\":\n        camera bind shots\n",
		"app B:\n    view V at \"/\":\n        camera bind nothing\n",
	} {
		if _, err := compile.String(bad); err == nil {
			t.Errorf("a camera must bind a declared @client text cell:\n%s", bad)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	fn := func(name string) string {
		for _, head := range []string{"  async function " + name + "(", "  function " + name + "("} {
			start := strings.Index(src, head)
			if start < 0 {
				continue
			}
			depth := 0
			for i := start; i < len(src); i++ {
				switch src[i] {
				case '{':
					depth++
				case '}':
					if depth--; depth == 0 {
						return src[start : i+1]
					}
				}
			}
		}
		t.Fatalf("assets/facet.js has no function %s", name)
		return ""
	}
	script := fn("stopDetachedCameras") + fn("startCamera") + fn("captureCamera") + `
const cameras = new Set();
const store = {};
const classes = new Set();
const box = { classList: { add: (c) => classes.add(c), remove: (c) => classes.delete(c) },
  getAttribute: () => "shot", setAttribute() {}, removeAttribute() {},
  querySelector: () => ({ videoWidth: 640, videoHeight: 480 }) };
globalThis.document = { contains: () => true, createElement: () => ({ getContext: () => ({ drawImage() {} }),
  toBlob: (cb) => cb(new Blob(["jpeg"])) }) };
globalThis.File = class { constructor(parts, name, opts) { this.name = name; this.type = opts.type; } };
let sent = null;
async function uploadSingle(f) { sent = f; return { url: "/uploads/capture.jpg" }; }
function mergeMedia() {}
function refresh() {}
function showError() {}
(async () => {
  await startCamera(box); // no navigator.mediaDevices here: the file input stands in
  captureCamera(box);
  await new Promise((r) => setTimeout(r, 10));
  console.log(JSON.stringify({ off: classes.has("fa-camera-off"), shot: store.shot, name: sent && sent.name, type: sent && sent.type }));
})();
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	want := `{"off":true,"shot":"/uploads/capture.jpg","name":"capture.jpg","type":"image/jpeg"}`
	if strings.TrimSpace(string(out)) != want {
		t.Errorf("client camera = %s, want %s", out, want)
	}
}
