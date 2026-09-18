package runtime

import (
	"net/http"
	"strings"
)

// handleAssets serves a file bundled into the compiled graph with `asset from
// "path"` (internal/compile resolves the reference at compile time; the bytes
// travel inside ir.IR.Assets exactly the way CSS travels as a string field —
// see ir.IR.Assets' own doc). It is the read-only sibling of handleUploads,
// and deliberately not the same endpoint: an upload is mutable, disk-backed
// content a user submitted at runtime; this is a fixed set the compiler baked
// into the graph, present unchanged even in a release binary that carries no
// separate directory alongside it (see cmd/facet/release.go).
//
// The name is a content hash internal/compile mints from the file's own
// bytes, so a cache-forever header is always correct: the URL a page asks for
// changes exactly when the bytes it names do.
func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if name == "" || strings.ContainsAny(name, "/\\") {
		http.NotFound(w, r)
		return
	}
	a, ok := s.ir.Assets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if a.ContentType != "" {
		w.Header().Set("Content-Type", a.ContentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(a.Bytes)
}
