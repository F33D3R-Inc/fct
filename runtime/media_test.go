package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

const mediaApp = `app M:
    entity Doc:
        id: int
        url: text
    action save(url: text):
        add Doc { url: url }
    view Home at "/":
        box:
            text "{count(Doc)}"
`

func newMediaServer(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.String(mediaApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	srv.uploadDir = t.TempDir()
	srv.uploadSessions = map[string]*uploadSession{}
	return httptest.NewServer(srv.Handler())
}

func TestSignedMediaAccess(t *testing.T) {
	t.Setenv("FACET_MEDIA_TTL", "60")
	if !signedMedia() {
		t.Fatal("FACET_MEDIA_TTL=60 should enable signed media")
	}
	u := mediaURL("photo.jpg")
	if !strings.Contains(u, "exp=") || !strings.Contains(u, "sig=") {
		t.Fatalf("signed media URL should carry exp+sig, got %q", u)
	}
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	if !mediaAccessOK("photo.jpg", q) {
		t.Fatal("a freshly signed link should verify")
	}
	// Tampered signature is rejected.
	bad := url.Values{"exp": {q.Get("exp")}, "sig": {"deadbeef"}}
	if mediaAccessOK("photo.jpg", bad) {
		t.Fatal("a tampered signature must be rejected")
	}
	// Expired link is rejected.
	pastTS := time.Now().Add(-time.Hour).Unix()
	expired := mediaSig("photo.jpg", pastTS)
	exp := url.Values{"exp": {strconv.FormatInt(pastTS, 10)}, "sig": {expired}}
	if mediaAccessOK("photo.jpg", exp) {
		t.Fatal("an expired link must be rejected")
	}
}

func TestUnsignedMediaIsPublic(t *testing.T) {
	t.Setenv("FACET_MEDIA_TTL", "")
	if signedMedia() {
		t.Fatal("no TTL means public media")
	}
	if u := mediaURL("a.png"); u != "/uploads/a.png" {
		t.Fatalf("public media URL should be the bare path, got %q", u)
	}
	if !mediaAccessOK("a.png", url.Values{}) {
		t.Fatal("public media should serve with no signature")
	}
}

func TestChunkedUploadAssembles(t *testing.T) {
	ts := newMediaServer(t)
	defer ts.Close()

	post := func(path string, body io.Reader) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// init
	r := post("/upload/init", strings.NewReader(`{"filename":"clip.mp4"}`))
	if r.StatusCode != 200 {
		t.Fatalf("init: want 200, got %d", r.StatusCode)
	}
	id := readField(t, r, "id")

	// two chunks
	want := []byte(strings.Repeat("A", 1000) + strings.Repeat("B", 500))
	for _, part := range [][]byte{want[:1000], want[1000:]} {
		r = post("/upload/chunk?id="+id, bytes.NewReader(part))
		if r.StatusCode != 200 {
			t.Fatalf("chunk: want 200, got %d", r.StatusCode)
		}
		r.Body.Close()
	}

	// finish → URL
	r = post("/upload/finish?id="+id, nil)
	if r.StatusCode != 200 {
		t.Fatalf("finish: want 200, got %d", r.StatusCode)
	}
	mediaPath := readField(t, r, "url")

	// the assembled file matches the concatenated chunks exactly
	got := httpGetBytes(t, ts.URL+mediaPath)
	if !bytes.Equal(got, want) {
		t.Fatalf("assembled upload mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

func TestHLSContentTypes(t *testing.T) {
	cases := map[string]string{
		".m3u8": "application/vnd.apple.mpegurl",
		".ts":   "video/mp2t",
		".m4s":  "video/iso.segment",
		".mpd":  "application/dash+xml",
		".txt":  "",
	}
	for ext, want := range cases {
		if got := hlsContentType(ext); got != want {
			t.Errorf("hlsContentType(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestSignedHLSPlaylistRewrite(t *testing.T) {
	t.Setenv("FACET_MEDIA_TTL", "60")
	g, _ := compile.String(mediaApp)
	srv, _ := NewInMemory(g)
	dir := t.TempDir()
	srv.uploadDir = dir
	if err := os.WriteFile(dir+"/stream.m3u8",
		[]byte("#EXTM3U\n#EXTINF:6.0,\nseg0.ts\n#EXTINF:6.0,\nseg1.ts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// fetch the playlist via its own signed URL
	body := httpGetBytes(t, ts.URL+mediaURL("stream.m3u8"))
	text := string(body)
	// tags pass through untouched; segment URIs are rewritten with a signature
	if !strings.Contains(text, "#EXTM3U") || !strings.Contains(text, "#EXTINF") {
		t.Fatalf("playlist tags should be preserved:\n%s", text)
	}
	if !strings.Contains(text, "seg0.ts?exp=") || !strings.Contains(text, "seg1.ts?exp=") {
		t.Fatalf("segment URIs should be signed:\n%s", text)
	}
	// the signature minted for a segment must actually verify
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "seg0.ts?") {
			q, _ := url.ParseQuery(strings.SplitN(line, "?", 2)[1])
			if !mediaAccessOK("seg0.ts", q) {
				t.Fatal("rewritten segment signature should verify")
			}
		}
	}
}

// helpers

func readField(t *testing.T, r *http.Response, key string) string {
	t.Helper()
	defer r.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return toStr(m[key])
}

func httpGetBytes(t *testing.T, u string) []byte {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}
