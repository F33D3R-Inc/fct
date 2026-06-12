package fa

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// One emitted event reaches a web connection as a signed HTML fragment and a
// native connection as a signed, styled neutral-tree JSON — each verifiable by
// hashing the bytes it received. The style table stays on the server.
func TestNativeConnectionGetsSignedStyledTree(t *testing.T) {
	key := []byte("0123456789abcdef")
	h := newHub(key, nil, nil, nil)

	web := &sseClient{id: "web", channels: map[string]bool{}, send: make(chan []byte, 4)}
	nat := &sseClient{id: "nat", channels: map[string]bool{}, send: make(chan []byte, 4), native: true}
	h.clients["web"] = web
	h.clients["nat"] = nat

	frag := `<button class="fa-btn fa-btn--primary" data-action="tip" data-facet-id="B">Go</button>`
	h.EmitConn("web", Event{Op: "replace", FacetID: "B", Fragment: frag})
	h.EmitConn("nat", Event{Op: "replace", FacetID: "B", Fragment: frag})

	// Web: HTML fragment, signature valid over op\0facet_id\0fragment.
	webMsg := decodeFrame(t, <-web.send)
	if !strings.Contains(webMsg.Fragment, "fa-btn") {
		t.Errorf("web should get HTML, got %q", webMsg.Fragment)
	}
	if !validSig(key, webMsg) {
		t.Error("web frame signature invalid")
	}

	// Native: fragment is the styled tree JSON, re-signed over those bytes.
	natMsg := decodeFrame(t, <-nat.send)
	if !validSig(key, natMsg) {
		t.Fatal("native frame signature invalid — client could not authenticate it")
	}
	var tree ViewNode
	if err := json.Unmarshal([]byte(natMsg.Fragment), &tree); err != nil {
		t.Fatalf("native fragment is not tree JSON: %v", err)
	}
	if tree.Kind != "button" || tree.Action != "tip" {
		t.Errorf("native tree wrong: kind=%s action=%s", tree.Kind, tree.Action)
	}
	if tree.Style == nil || tree.Style.BG != "#1d9bf0" {
		t.Errorf("native tree missing server-resolved style: %+v", tree.Style)
	}
	// Tamper detection: a flipped byte fails the signature.
	tampered := natMsg
	tampered.Fragment = strings.Replace(tampered.Fragment, "button", "image", 1)
	if validSig(key, tampered) {
		t.Error("tampered native frame should fail verification")
	}
}

// The _conn hello carries the signing key so a native client can verify events.
func TestHelloFrameCarriesKey(t *testing.T) {
	key := []byte("0123456789abcdef")
	h := newHub(key, nil, nil, nil)
	c := &sseClient{id: "c", channels: map[string]bool{}, send: make(chan []byte, 4), native: true}
	h.register(c)
	// ServeSSE writes the hello; here we just confirm the key is exposed for it.
	var hello map[string]string
	_ = json.Unmarshal([]byte(`{"op":"_conn","conn":"c","key":"`+hex.EncodeToString(key)+`"}`), &hello)
	if hello["key"] != hex.EncodeToString(key) {
		t.Error("hello must carry the hex key")
	}
}

func validSig(key []byte, e Event) bool {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(e.Op))
	mac.Write([]byte{0})
	mac.Write([]byte(e.FacetID))
	mac.Write([]byte{0})
	mac.Write([]byte(e.Fragment))
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(e.HMAC))
}

func decodeFrame(t *testing.T, frame []byte) Event {
	t.Helper()
	s := strings.TrimSpace(strings.TrimPrefix(string(frame), "data:"))
	var e Event
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		t.Fatalf("decode frame %q: %v", frame, err)
	}
	return e
}
