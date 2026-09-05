package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// File/media uploads are a runtime service the language only names (the `upload`
// node binds the resulting URL to a client state cell). A browser POSTs the file
// to /upload on the authenticated, CSRF-guarded browser channel; the server
// writes it under the upload directory and returns its public URL, which the
// client stores and can then show or submit through an action. Files are served
// back read-only from /uploads/.

// uploadDirFromEnv resolves where uploaded files are written (and served from),
// defaulting to ./facet-uploads beside the running server.
func uploadDirFromEnv() string {
	if d := os.Getenv("FACET_UPLOAD_DIR"); d != "" {
		return d
	}
	return "facet-uploads"
}

// handleUpload accepts one multipart file field named "file", stores it under a
// random, collision-free name (keeping the original extension), and returns its
// durable reference as JSON: {"url":"/uploads/<name>"} — plus, when media signing
// is on, {"media":{...}} carrying the signature to preview it by. It is
// rate-limited and CSRF-guarded like every other browser mutation.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.guardMutation(w, r, true) {
		return
	}
	cap := singleUploadCap()
	r.Body = http.MaxBytesReader(w, r.Body, cap)
	if err := r.ParseMultipartForm(cap); err != nil {
		http.Error(w, "upload too large or malformed", http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if err := os.MkdirAll(s.uploadDir, 0o755); err != nil {
		http.Error(w, "cannot store upload", http.StatusInternalServerError)
		return
	}
	name := randomName() + safeExt(hdr.Filename)
	dst, err := os.Create(filepath.Join(s.uploadDir, name))
	if err != nil {
		http.Error(w, "cannot store upload", http.StatusInternalServerError)
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	// The client stores the durable reference and previews through the grant
	// beside it; a signature is never what lands in a row (see writeUploaded).
	writeUploaded(w, name)
}

// handleUploads serves a stored upload by name, read-only. It refuses any name
// with a path separator so a request can never escape the upload directory. When
// signed media is enabled it requires a valid, unexpired signature. HLS parts are
// served with their stream MIME types; an .m3u8 playlist is rewritten so each
// segment URI carries its own signature.
func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/uploads/")
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	if !mediaAccessOK(name, r.URL.Query()) {
		http.Error(w, "expired or invalid media link", http.StatusForbidden)
		return
	}
	full := filepath.Join(s.uploadDir, name)
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".m3u8" {
		s.serveHLSPlaylist(w, full)
		return
	}
	if ct := hlsContentType(ext); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeFile(w, r, full) // ServeFile handles Range, so segments seek cleanly
}

// randomName mints a 16-byte hex token for a stored file.
func randomName() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", b)
	}
	return hex.EncodeToString(b[:])
}

// safeExt returns the lowercased extension of an uploaded filename if it is a
// short, alphanumeric extension, else "" — so a hostile filename cannot inject a
// path or an overlong/odd suffix.
func safeExt(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" || len(ext) > 6 {
		return ""
	}
	for _, c := range ext[1:] {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
			return ""
		}
	}
	return ext
}
