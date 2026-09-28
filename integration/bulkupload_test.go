package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// Bulk upload on the site (creator_bulk_upload.html): the page takes many
// files at once (a `multiple` upload into a [text] cell), each goes up
// through the runtime's upload endpoint, and publishing makes one post per
// file with its media and kind.
func TestBulkUploadPublishesEachFile(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "f33d3r_com.fct")
	if code, body := a.action("signup", "cara", "password1", "Cara", "1990-01-01", "safe", "musician", "", "web", true); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	code, page := a.get("/create/bulk-upload")
	if code != 200 || !strings.Contains(page, `data-fa-upload="bulkFiles" multiple`) {
		t.Fatalf("GET /create/bulk-upload = %d; the file input must take many files", code)
	}
	png := tinyPNG
	one, two := uploadFile(t, a, page, "one.png", png), uploadFile(t, a, page, "two.png", png)
	if code, body := a.action("webBulkPublish", []string{one, two}, ""); code != 200 {
		t.Fatalf("webBulkPublish: %d %s", code, body)
	}
	_, profile := a.get("/cara")
	for _, u := range []string{one, two} {
		name := u[strings.LastIndex(u, "/")+1:]
		if !strings.Contains(profile, name) {
			t.Errorf("the profile should show the post made from %s", u)
		}
	}
	if code, _ := a.action("webBulkPublish", []string{}, "x"); code == 200 {
		t.Error("publishing nothing must be refused")
	}
	// Someone who is not a creator is told so and cannot publish in bulk.
	a.newSession()
	if code, body := a.action("webSignup", "dan", "pw12345678"); code != 200 {
		t.Fatalf("webSignup: %d %s", code, body)
	}
	if _, page := a.get("/create/bulk-upload"); !strings.Contains(page, "Bulk upload is for creators.") {
		t.Error("a non-creator should be told bulk upload is for creators")
	}
	if code, _ := a.action("webBulkPublish", []string{one}, "x"); code == 200 {
		t.Error("a non-creator's bulk publish must be refused")
	}
}

var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

// uploadFile sends one file through the runtime's /upload, as the page's
// file input does (with the page's CSRF token), and returns its URL.
func uploadFile(t *testing.T, a *app, page, name string, data []byte) string {
	t.Helper()
	csrf := regexp.MustCompile(`<meta name="fa-csrf" content="([^"]*)">`).FindStringSubmatch(page)
	if csrf == nil {
		t.Fatal("the page carries no CSRF token")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", a.url("/upload"), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Facet-CSRF", csrf[1])
	res, err := a.cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var out struct {
		URL string `json:"url"`
	}
	if res.StatusCode != 200 || json.Unmarshal(b, &out) != nil || out.URL == "" {
		t.Fatalf("upload %s: %d %s", name, res.StatusCode, b)
	}
	return out.URL
}

// The Visions camera (vision_camera.html): the page is a live viewfinder
// with a shutter (and a capture-from-camera input where there is none); a
// captured still shared with a caption is a Vision in the tray.
func TestVisionCameraSharesAVision(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "f33d3r_com.fct")
	if code, _ := a.get("/visions/camera"); code == 200 {
		if _, body := a.get("/visions/camera"); strings.Contains(body, "data-fa-camera") {
			t.Error("a guest must not reach the camera")
		}
	}
	if code, body := a.action("webSignup", "vera", "pw12345678"); code != 200 {
		t.Fatalf("webSignup: %d %s", code, body)
	}
	code, page := a.get("/visions/camera")
	if code != 200 || !strings.Contains(page, `data-fa-camera="visionShot"`) || !strings.Contains(page, `capture="environment"`) {
		t.Fatalf("GET /visions/camera = %d; want the viewfinder and its stand-in", code)
	}
	run, _ := runClientAgainst(t, a, page, nil)
	if !hasAttr(run.Attrs, "data-fa-camera", "visionShot") {
		t.Errorf("the client render lost the camera: %v", run.Attrs)
	}
	shot := uploadFile(t, a, page, "capture.jpg", tinyPNG)
	if code, body := a.action("webShareVision", "", "nothing"); code == 200 {
		t.Errorf("sharing no capture must be refused: %s", body)
	}
	if code, body := a.action("webShareVision", shot, "golden hour"); code != 200 {
		t.Fatalf("webShareVision: %d %s", code, body)
	}
	_, tray := a.get("/visions")
	if !strings.Contains(tray, "golden hour") || !strings.Contains(tray, shot[strings.LastIndex(shot, "/")+1:]) {
		t.Error("the tray should show the shared Vision's caption and still")
	}
}
