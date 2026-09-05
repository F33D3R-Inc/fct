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

// ── a media reference is durable; its signature is not ──────────────────────
//
// The signature used to be minted ONCE, at upload, and whatever came back was
// what the app stored: `Product.cover` held "/uploads/ab12.png?exp=...&sig=...".
// A signature is a grant with a deadline and a column is forever, so the row
// outlived the grant it was holding — with FACET_MEDIA_TTL=60 a cover was 200 on
// upload and 403 a minute later, permanently, with the row still pointing at it.
// Turning signing ON also broke every cover already stored, since those bare
// paths carried no signature at all.
//
// The rule, therefore: a row stores a DURABLE REFERENCE ("/uploads/<name>"), and
// the signature is minted per render, by the node that renders it. A reference
// does not expire, so a column can hold one.
//
// THREE THINGS A STORED VALUE CAN BE, and mediaDurableRef is the one place that
// decides which:
//
//   - a durable reference, "/uploads/<name>" — sign it at render;
//   - a value that is ALREADY a fully-signed URL of ours,
//     "/uploads/<name>?exp=...&sig=..." — every row written before this change is
//     one. Its query is a spent grant, not part of its identity, so the name is
//     taken and a fresh signature minted over it. Old rows keep working, and they
//     stop expiring;
//   - anything else — an "https://..." cover on someone else's host, a "data:"
//     URI, an app's own static path. Not ours, not signable, returned untouched.
//     Appending exp/sig to a foreign URL would corrupt it.
//
// ONE SIGNER, TWO RENDERERS. The server and facet.js both render an `image`, and
// only the server holds the signing key. facet.js therefore does not sign and
// cannot: the signature travels to it as data, in the "@media" grant map that
// rides the bootstrap, every /region answer, every SSE frame and every upload
// response (see mediaGrants and facet.js mediaSrc). Its half is a lookup with a
// fallthrough, so the worst a divergence can do is render the reference the row
// already held. Mirroring mediaSig into JavaScript would have meant shipping the
// master signing key to every browser, which is not a divergence risk — it is the
// end of signed media.

// mediaPathPrefix is the path every stored upload is served from. It is the
// marker that says a value is this server's media rather than someone else's.
const mediaPathPrefix = "/uploads/"

// mediaDurableRef classifies a stored media value and, when it is one of this
// server's uploads, returns the durable reference for it — the value with any
// signature stripped. The second result is false for everything this server must
// not sign: an absolute URL on another host, a data: URI, an empty value, or any
// path that is not an upload.
//
// The name is validated the same way handleUploads validates it, so a hostile
// stored value cannot produce a reference that escapes the upload directory —
// this function's output ends up in a src attribute and in an HMAC.
func mediaDurableRef(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, mediaPathPrefix) {
		// Covers "https://...", "//cdn...", "data:...", "" and any other path. A
		// scheme or an authority can never appear before the prefix, so this one
		// test separates ours from everyone else's.
		return "", false
	}
	name := v[len(mediaPathPrefix):]
	// A spent grant is a query, and a query is not part of the reference.
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", false
	}
	return mediaPathPrefix + name, true
}

// mediaSrc turns a stored media value into the URL to render RIGHT NOW. It is the
// single definition of that rule: the server's `image`/`video` render calls it,
// and the grant map facet.js consults is built out of it.
//
// With signing off it normalizes and otherwise does nothing, which is what keeps
// the default (public, unguessable paths) exactly as it was — including for a row
// written while signing was on, whose stale query is dropped rather than served.
func mediaSrc(v string) string {
	ref, ok := mediaDurableRef(v)
	if !ok {
		return v // not ours to sign, and not ours to rewrite
	}
	if !signedMedia() {
		return ref
	}
	return mediaURL(strings.TrimPrefix(ref, mediaPathPrefix))
}

// mediaGrants is the signature map that travels to facet.js: every media value
// present in a client-bound payload, mapped to the URL to render it by.
//
// It is keyed by the RAW value found in the payload, not by the normalized
// reference, and that is deliberate — the client looks up exactly the string its
// own evaluator produced from the state it was given. A row still holding a
// long-expired signed URL is therefore keyed by that expired URL and rendered
// through a fresh one, with no client-side parsing of a value it must not parse.
//
// Nothing is minted when signing is off: the payload is not walked, the map is
// empty, and the client falls through to the reference as it always has.
func mediaGrants(payload any) map[string]string {
	if !signedMedia() {
		return nil
	}
	out := map[string]string{}
	collectMediaGrants(payload, out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// collectMediaGrants walks a client-bound payload (state cells, region result
// sets, SSE deltas — maps, slices and scalars) and mints a grant for every media
// value it finds. Any string may be a media reference: the client renders an
// `image` from whatever expression the author wrote, so narrowing this to the
// fields the renderer "should" read would be a second, weaker model of the same
// thing.
func collectMediaGrants(v any, out map[string]string) {
	switch t := v.(type) {
	case string:
		if _, ok := mediaDurableRef(t); ok {
			if src := mediaSrc(t); src != t {
				out[t] = src
			}
		}
	case []any:
		for _, e := range t {
			collectMediaGrants(e, out)
		}
	case map[string]any:
		for _, e := range t {
			collectMediaGrants(e, out)
		}
	case map[string][]any:
		for _, e := range t {
			collectMediaGrants(any(e), out)
		}
	}
}

// writeUploaded answers an upload with the two different things the client needs
// and that used to be conflated into one.
//
// `url` is the DURABLE REFERENCE — what the bound state cell holds and what an
// action writes into the row. It never expires, because it grants nothing.
//
// `media` is that reference's grant for right now, in the same shape every other
// client payload carries it, so the just-uploaded file previews immediately even
// with signing on. Returning the signed URL as `url` is what put a deadline
// inside a column in the first place: the preview needed a signature, the column
// needed a reference, and one field could not be both.
func writeUploaded(w http.ResponseWriter, name string) {
	ref := mediaPathPrefix + name
	out := map[string]any{"url": ref}
	if g := mediaGrants(ref); len(g) > 0 {
		out["media"] = g
	}
	writeJSON(w, out)
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
// and returns its durable reference (with a render-time grant beside it, exactly
// as handleUpload does). The session is dropped.
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
	writeUploaded(w, sess.name)
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
