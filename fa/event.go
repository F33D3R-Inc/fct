// Package fa is the Facet Architecture server runtime library — the piece every
// FA application imports (the "react-dom" of FA, except it runs on the server).
// It owns the SSE transport, HMAC-signed event push, and the /events router, so
// application code never hand-writes streaming or connection plumbing.
//
// Generated per-app code (from `fct build`) provides typed render functions and
// a manifest; this library provides the runtime they plug into.
package fa

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Event is a single server→client DOM mutation pushed over SSE. It mirrors the
// shape the client runtime (runtime/fa-runtime.js) applies.
type Event struct {
	Op       string    `json:"op"`                 // replace | append | prepend | remove
	FacetID  string    `json:"facet_id"`           // target data-facet-id
	Fragment string    `json:"fragment,omitempty"` // new HTML (web clients)
	Tree     *ViewNode `json:"tree,omitempty"`     // styled neutral tree (native clients)
	HMAC     string    `json:"hmac,omitempty"`     // set by the hub before send
}

// sign attaches an HMAC-SHA256 over the event's meaningful fields so the client
// can reject any fragment that did not originate from this server. The message
// layout (op \0 facet_id \0 fragment) is mirrored exactly in fa-runtime.js.
//
// A zero-length key disables signing (dev mode); the client then skips
// verification too.
func sign(key []byte, e *Event) {
	if len(key) == 0 {
		return
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(e.Op))
	mac.Write([]byte{0})
	mac.Write([]byte(e.FacetID))
	mac.Write([]byte{0})
	mac.Write([]byte(e.Fragment))
	e.HMAC = hex.EncodeToString(mac.Sum(nil))
}
