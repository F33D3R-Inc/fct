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

// maxUploadBytes caps a single upload (10 MiB) so the endpoint cannot be used to
// exhaust disk with one request.
const maxUploadBytes = 10 << 20

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
// public URL as JSON: {"url":"/uploads/<name>"}. It is rate-limited and CSRF-
// guarded like every other browser mutation.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.guardMutation(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
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
	writeJSON(w, map[string]any{"url": "/uploads/" + name})
}

// handleUploads serves a stored upload by name, read-only. It refuses any name
// with a path separator so a request can never escape the upload directory.
func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/uploads/")
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.uploadDir, name))
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
