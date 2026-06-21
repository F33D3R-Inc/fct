package runtime

// Media handoff — the runtime services that move large files in and out: signed,
// expiring URLs (so media links are access-controlled, not permanently public),
// resumable chunked uploads (so a file larger than one request can be sent in
// pieces), and HLS delivery (so video streams play with correct MIME types and,
// when signing is on, per-segment-signed playlists). All env-configured, like the
// rest of the runtime's operational surface — the language only names `upload`.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// mediaEnvInt reads a positive integer env var, falling back to def.
func mediaEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// mediaTTL is the lifetime, in seconds, of a signed media URL (0 = signing off,
// the default — media is served from a public, unguessable path as before).
func mediaTTL() int { return mediaEnvInt("FACET_MEDIA_TTL", 0) }

// signedMedia reports whether media URLs are signed and time-limited.
func signedMedia() bool { return mediaTTL() > 0 }

// singleUploadCap caps one multipart POST to /upload (default 10 MiB).
func singleUploadCap() int64 { return int64(mediaEnvInt("FACET_MAX_UPLOAD_MB", 10)) << 20 }

// mediaTotalCap caps a whole assembled chunked upload (default 200 MiB) — the
// ceiling on a resumable transfer, independent of the per-request limit.
func mediaTotalCap() int64 { return int64(mediaEnvInt("FACET_MAX_MEDIA_MB", 200)) << 20 }

// mediaURL is the URL a client stores for a just-stored file. When signing is on
// it carries an expiry and an HMAC over name+expiry, so the link grants
// time-limited access; otherwise it is the bare public path (backward compatible).
func mediaURL(name string) string {
	if !signedMedia() {
		return "/uploads/" + name
	}
	exp := time.Now().Add(time.Duration(mediaTTL()) * time.Second).Unix()
	return fmt.Sprintf("/uploads/%s?exp=%d&sig=%s", name, exp, mediaSig(name, exp))
}

// mediaSig is the HMAC-SHA256 (hex) over "<name>:<exp>", keyed by the master
// signing key — the proof that a media link was minted by this server.
func mediaSig(name string, exp int64) string {
	mac := hmac.New(sha256.New, ring().signKey)
	fmt.Fprintf(mac, "%s:%d", name, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// mediaAccessOK authorizes a read of name. With signing off, every read is
// allowed; with it on, the query must carry an unexpired exp and a matching sig.
func mediaAccessOK(name string, q url.Values) bool {
	if !signedMedia() {
		return true
	}
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(q.Get("sig")), []byte(mediaSig(name, exp)))
}

// hlsContentType returns the MIME type for an HLS/DASH part, or "" for anything
// else (so the caller falls back to Go's extension detection).
func hlsContentType(ext string) string {
	switch ext {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".ts":
		return "video/mp2t"
	case ".m4s":
		return "video/iso.segment"
	case ".mpd":
		return "application/dash+xml"
	}
	return ""
}

// serveHLSPlaylist serves an .m3u8. With signing off it streams the file as-is;
// with signing on it rewrites each segment/variant URI to carry its own fresh
// signature, so a player can fetch the parts of a protected stream. Segments are
// flat siblings in the upload dir (bare names), matching how uploads are stored.
func (s *Server) serveHLSPlaylist(w http.ResponseWriter, full string) {
	data, err := os.ReadFile(full)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	if !signedMedia() {
		w.Write(data)
		return
	}
	exp := time.Now().Add(time.Duration(mediaTTL()) * time.Second).Unix()
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue // a tag or blank line — not a URI
		}
		lines[i] = fmt.Sprintf("%s?exp=%d&sig=%s", t, exp, mediaSig(t, exp))
	}
	w.Write([]byte(strings.Join(lines, "\n")))
}

// uploadSession is one in-flight resumable upload: the open temp file the chunks
// append to, the final name to publish it under, and the running byte count.
type uploadSession struct {
	name string   // final stored name (random token + safe extension)
	tmp  string   // temp ".part" path the chunks accumulate in
	f    *os.File // open handle to tmp, appended to per chunk
	n    int64    // bytes written so far (enforced against mediaTotalCap)
}

// handleUploadChunked dispatches the resumable-upload protocol under /upload/:
// init opens a session, chunk appends bytes, finish publishes the file, and abort
// discards it. A file too big for one /upload POST is sent as a sequence of
// chunks and assembled server-side. Every step is rate-limited and CSRF-guarded.
func (s *Server) handleUploadChunked(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.guardMutation(w, r, true) {
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/upload/") {
	case "init":
		s.uploadInit(w, r)
	case "chunk":
		s.uploadChunk(w, r)
	case "finish":
		s.uploadFinish(w, r)
	case "abort":
		s.uploadAbort(w, r)
	default:
		http.NotFound(w, r)
	}
}

// uploadInit opens a resumable session: it mints the final name (preserving a safe
// extension from the client filename), creates the temp file, and returns the
// session id the client threads through the chunk and finish calls.
func (s *Server) uploadInit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Filename string `json:"filename"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	if err := os.MkdirAll(s.uploadDir, 0o755); err != nil {
		http.Error(w, "cannot store upload", http.StatusInternalServerError)
		return
	}
	id := randomName()
	name := randomName() + safeExt(req.Filename)
	tmp := filepath.Join(s.uploadDir, id+".part")
	f, err := os.Create(tmp)
	if err != nil {
		http.Error(w, "cannot start upload", http.StatusInternalServerError)
		return
	}
	s.uploadMu.Lock()
	s.uploadSessions[id] = &uploadSession{name: name, tmp: tmp, f: f}
	s.uploadMu.Unlock()
	writeJSON(w, map[string]any{"id": id})
}

// uploadChunk appends one chunk's raw body to its session's temp file, enforcing
// the total-size cap so a resumable upload cannot grow without bound.
func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	s.uploadMu.Lock()
	sess := s.uploadSessions[r.URL.Query().Get("id")]
	s.uploadMu.Unlock()
	if sess == nil {
		http.Error(w, "unknown upload session", http.StatusNotFound)
		return
	}
	body := http.MaxBytesReader(w, r.Body, singleUploadCap())
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	n, err := io.Copy(sess.f, body)
	sess.n += n
	if err != nil || sess.n > mediaTotalCap() {
		http.Error(w, "upload too large or write failed", http.StatusRequestEntityTooLarge)
		return
	}
	writeJSON(w, map[string]any{"received": sess.n})
}

// uploadFinish closes the session's temp file, publishes it under the final name,
// and returns the media URL (signed when signing is on). The session is dropped.
func (s *Server) uploadFinish(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.uploadMu.Lock()
	sess := s.uploadSessions[id]
	delete(s.uploadSessions, id)
	s.uploadMu.Unlock()
	if sess == nil {
		http.Error(w, "unknown upload session", http.StatusNotFound)
		return
	}
	sess.f.Close()
	final := filepath.Join(s.uploadDir, sess.name)
	if err := os.Rename(sess.tmp, final); err != nil {
		os.Remove(sess.tmp)
		http.Error(w, "cannot finalize upload", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"url": mediaURL(sess.name)})
}

// uploadAbort discards an in-flight session and its temp file.
func (s *Server) uploadAbort(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.uploadMu.Lock()
	sess := s.uploadSessions[id]
	delete(s.uploadSessions, id)
	s.uploadMu.Unlock()
	if sess != nil {
		sess.f.Close()
		os.Remove(sess.tmp)
	}
	writeJSON(w, map[string]any{"ok": true})
}
