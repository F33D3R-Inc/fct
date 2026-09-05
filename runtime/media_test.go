package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
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

// A signature is a grant with a deadline; a column is forever. Minting the
// signature at upload and storing the result put the first inside the second, so
// `Product.cover` held a URL that was 200 on upload and 403 a minute later —
// permanently — and every cover stored before signing was switched on was on 403s
// from the moment it was.
//
// The fix is that the row holds a durable reference and the `image` node mints the
// signature per render. This pins all four halves of it:
//
//   - a durable reference renders as a signed, valid src;
//   - a row that already holds a fully-signed URL — every row written by the old
//     code, expired or not — renders as a FRESH valid src, so upgrading does not
//     break the data that is already there;
//   - an external https:// cover is rendered exactly as stored, never signed;
//   - the signatures the client needs travel to it as data (@media), keyed by the
//     value the row holds, because facet.js must never hold the signing key.
//
// Without the fix the first two render the stored value verbatim: the durable
// reference has no signature at all (403) and the stale one keeps the expired one
// (403), which is exactly what the storefront saw.
func TestImageSrcIsSignedPerRender(t *testing.T) {
	t.Setenv("FACET_MEDIA_TTL", "60")
	g, err := compile.String(`app Shop:
    entity Product:
        id: int
        cover: text
    action addProduct(cover: text):
        add Product { cover: cover }
    view Home at "/":
        box:
            for p in Product:
                image "{p.cover}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}

	// A signature minted long ago and stored in the row, the way the old upload
	// path did it: expired, and a 403 forever.
	stalePast := time.Now().Add(-time.Hour).Unix()
	stale := fmt.Sprintf("/uploads/old.png?exp=%d&sig=%s", stalePast, mediaSig("old.png", stalePast))
	external := "https://cdn.example.com/other.png"
	for _, cover := range []string{"/uploads/new.png", stale, external} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["addProduct"], []any{cover}); status != http.StatusOK {
			t.Fatalf("seed %q: %d %s", cover, status, msg)
		}
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	page := string(httpGetBytes(t, ts.URL+"/"))

	srcs := imageSrcs(page)
	if len(srcs) != 3 {
		t.Fatalf("expected 3 rendered images, got %d:\n%s", len(srcs), page)
	}
	for i, want := range []string{"new.png", "old.png"} {
		u, err := url.Parse(srcs[i])
		if err != nil {
			t.Fatalf("src %q: %v", srcs[i], err)
		}
		if !mediaAccessOK(want, u.Query()) {
			t.Errorf("rendered src for %s does not verify: %q", want, srcs[i])
		}
	}
	if got := srcs[1]; strings.Contains(got, strconv.FormatInt(stalePast, 10)) {
		t.Errorf("a row holding an already-signed URL must be re-signed, not replayed: %q", got)
	}
	if srcs[2] != external {
		t.Errorf("an external cover must be rendered untouched, got %q want %q", srcs[2], external)
	}

	// The client renders the same three images and holds no signing key, so the
	// signatures reach it as data — keyed by the value the row holds, which is what
	// its own evaluator will produce.
	var state map[string]any
	if err := json.Unmarshal([]byte(embeddedJSON(t, page, "fa-state")), &state); err != nil {
		t.Fatal(err)
	}
	grants, _ := state["@media"].(map[string]any)
	if len(grants) != 2 {
		t.Fatalf("@media should carry a grant for each of this app's media values, got %v", state["@media"])
	}
	for _, stored := range []string{"/uploads/new.png", stale} {
		got := toStr(grants[stored])
		u, err := url.Parse(got)
		if err != nil || got == "" {
			t.Fatalf("no usable grant for %q: %q", stored, got)
		}
		name := strings.TrimPrefix(u.Path, "/uploads/")
		if !mediaAccessOK(name, u.Query()) {
			t.Errorf("the grant handed to the client for %q does not verify: %q", stored, got)
		}
	}
	if _, signed := grants[external]; signed {
		t.Error("an external URL must never be given a grant")
	}
}

// imageSrcs pulls the src of every rendered <img>, in document order, decoded the
// way a browser would (a signed URL carries `&` between exp and sig, which the
// renderer escapes).
func imageSrcs(page string) []string {
	var out []string
	for _, part := range strings.Split(page, `<img`)[1:] {
		i := strings.Index(part, ` src="`)
		if i < 0 {
			continue
		}
		rest := part[i+len(` src="`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			continue
		}
		out = append(out, html.UnescapeString(rest[:j]))
	}
	return out
}

// embeddedJSON returns the text of one of the page's bootstrap <script> blocks.
func embeddedJSON(t *testing.T, page, id string) string {
	t.Helper()
	open := `id="` + id + `"`
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatalf("page carries no #%s block", id)
	}
	rest := page[i:]
	start := strings.Index(rest, ">")
	end := strings.Index(rest, "</script>")
	if start < 0 || end < 0 {
		t.Fatalf("#%s block is malformed", id)
	}
	return html.UnescapeString(rest[start+1 : end])
}
