package fa

import (
	"fmt"
	"net/http"
)

// WireVersion is the version of the SSE wire format this server speaks: the
// shape of Event frames, the hello (_conn) frame, the HMAC layout, and the
// /events request body. It is part of the frozen 1.0 surface (see
// STABILITY.md) and only changes with a breaking wire change.
//
// Negotiation: a client declares the version it speaks when connecting —
// native clients via the FA-Wire-Version header, the web runtime via ?v=
// (EventSource cannot set headers). Absent means "1" (clients that predate
// negotiation). A mismatch is rejected at connect time with 426 Upgrade
// Required and an explicit message, so an outdated native client fails loud
// at the handshake instead of weird at render time. The hello frame also
// carries the server's version ("v"), which clients verify on every
// (re)connect — that direction catches a new client against an old server.
const WireVersion = "1"

// clientWireVersion extracts the wire version a connecting client declared.
func clientWireVersion(r *http.Request) string {
	if v := r.Header.Get("FA-Wire-Version"); v != "" {
		return v
	}
	if v := r.URL.Query().Get("v"); v != "" {
		return v
	}
	return "1" // pre-negotiation clients speak v1
}

// checkWireVersion rejects a connect from a client speaking a different wire
// version. Returns false (response already written) on mismatch.
func checkWireVersion(w http.ResponseWriter, r *http.Request) bool {
	v := clientWireVersion(r)
	if v == WireVersion {
		return true
	}
	w.Header().Set("FA-Wire-Version", WireVersion)
	http.Error(w, fmt.Sprintf("fa: unsupported wire version %q — this server speaks %q; upgrade the client or the server", v, WireVersion),
		http.StatusUpgradeRequired)
	return false
}
