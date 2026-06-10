package fa

import (
	"encoding/json"
	"strings"
	"testing"
)

// One emitted event must reach a web connection as an HTML fragment and a native
// connection as a server-styled neutral tree — so native clients need no style
// table of their own (single source of truth in Go).
func TestNativeConnectionGetsStyledTree(t *testing.T) {
	h := newHub([]byte("0123456789abcdef"), nil)

	web := &sseClient{id: "web", channels: map[string]bool{}, send: make(chan []byte, 4)}
	nat := &sseClient{id: "nat", channels: map[string]bool{}, send: make(chan []byte, 4), native: true}
	h.clients["web"] = web
	h.clients["nat"] = nat

	frag := `<button class="fa-btn fa-btn--primary" data-action="tip" data-facet-id="B">Go</button>`
	h.EmitConn("web", Event{Op: "replace", FacetID: "B", Fragment: frag})
	h.EmitConn("nat", Event{Op: "replace", FacetID: "B", Fragment: frag})

	webMsg := decodeFrame(t, <-web.send)
	if webMsg.Tree != nil {
		t.Error("web connection should NOT receive a tree")
	}
	if !strings.Contains(webMsg.Fragment, "fa-btn") {
		t.Errorf("web should get the HTML fragment, got %q", webMsg.Fragment)
	}

	natMsg := decodeFrame(t, <-nat.send)
	if natMsg.Tree == nil {
		t.Fatal("native connection should receive a styled tree")
	}
	if natMsg.Tree.Kind != "button" || natMsg.Tree.Action != "tip" {
		t.Errorf("native tree wrong: kind=%s action=%s", natMsg.Tree.Kind, natMsg.Tree.Action)
	}
	// The style was resolved server-side — the client needs no class table.
	if natMsg.Tree.Style == nil || natMsg.Tree.Style.BG != "#1d9bf0" {
		t.Errorf("native tree missing server-resolved style: %+v", natMsg.Tree.Style)
	}
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
