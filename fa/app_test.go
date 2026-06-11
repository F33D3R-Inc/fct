package fa

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recompute mirrors fa-runtime.js's client-side verification: op \0 facet_id \0
// fragment. If this matches, the JS port (same bytes) will verify too.
func recompute(key []byte, e Event) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(e.Op))
	mac.Write([]byte{0})
	mac.Write([]byte(e.FacetID))
	mac.Write([]byte{0})
	mac.Write([]byte(e.Fragment))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestSignRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	e := Event{Op: "replace", FacetID: "LikeButton:post:p1", Fragment: "<button>hi</button>"}
	sign(key, &e)
	if e.HMAC == "" {
		t.Fatal("sign produced no HMAC")
	}
	if e.HMAC != recompute(key, e) {
		t.Fatal("HMAC does not match client-side recomputation")
	}
	// tampering with the fragment must invalidate the signature
	e2 := e
	e2.Fragment = "<button>EVIL</button>"
	if e2.HMAC == recompute(key, e2) {
		t.Fatal("tampered fragment still verified — signing is broken")
	}
}

func TestSignDisabledWithoutKey(t *testing.T) {
	e := Event{Op: "remove", FacetID: "x"}
	sign(nil, &e)
	if e.HMAC != "" {
		t.Fatal("expected no HMAC with empty key (dev mode)")
	}
}

// testConn registers a fake SSE connection on the hub and returns it.
func testConn(h *Hub, identity string) *sseClient {
	c := &sseClient{id: newConnID(), identity: identity, channels: make(map[string]bool), send: make(chan []byte, 8)}
	h.register(c)
	return c
}

func gotEvent(t *testing.T, c *sseClient) (Event, bool) {
	t.Helper()
	select {
	case line := <-c.send:
		s := strings.TrimSpace(strings.TrimPrefix(string(line), "data: "))
		var e Event
		if err := json.Unmarshal([]byte(s), &e); err != nil {
			t.Fatalf("frame not JSON: %v (%q)", err, line)
		}
		return e, true
	default:
		return Event{}, false
	}
}

func postEvent(t *testing.T, mux *http.ServeMux, body string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestBroadcastReachesAllClients(t *testing.T) {
	h := newHub([]byte("key-key-key-key-key-key-key-key!"), nil, nil)
	a, b := testConn(h, ""), testConn(h, "")
	h.Broadcast(Event{Op: "replace", FacetID: "F:1", Fragment: "<b>x</b>"})
	for _, c := range []*sseClient{a, b} {
		e, ok := gotEvent(t, c)
		if !ok || e.FacetID != "F:1" || e.HMAC == "" {
			t.Fatalf("broadcast not received correctly: ok=%v ev=%+v", ok, e)
		}
	}
}

// TestActorOnlyDelivery is the C1 proof: a handler's returned events reach ONLY
// the acting connection, never other connected clients.
func TestActorOnlyDelivery(t *testing.T) {
	app := New([]byte(`{}`))
	app.On("post.like", func(ctx Ctx) ([]Event, error) {
		return []Event{{Op: "replace", FacetID: "LikeButton:post:" + ctx.Payload["postId"], Fragment: "<b>liked</b>"}}, nil
	})
	mux := http.NewServeMux()
	app.Mount(mux)

	actor := testConn(app.hub, "")     // the clicker
	bystander := testConn(app.hub, "") // someone else online

	code := postEvent(t, mux, `{"type":"post.like","payload":{"postId":"p1"},"conn":"`+actor.id+`"}`)
	if code != http.StatusNoContent {
		t.Fatalf("POST /events = %d, want 204", code)
	}
	if e, ok := gotEvent(t, actor); !ok || !strings.Contains(e.FacetID, "p1") {
		t.Fatalf("actor did not receive its update: ok=%v ev=%+v", ok, e)
	}
	if _, ok := gotEvent(t, bystander); ok {
		t.Fatal("LEAK: bystander received the actor's update")
	}
}

// TestConnSpoofRejected: a connection bound to one identity cannot be driven by
// another identity.
func TestConnSpoofRejected(t *testing.T) {
	app := New([]byte(`{}`)).Identify(func(r *http.Request) string { return r.Header.Get("X-User") })
	app.On("x", func(ctx Ctx) ([]Event, error) { return nil, nil })
	mux := http.NewServeMux()
	app.Mount(mux)

	alice := testConn(app.hub, "alice")
	req := httptest.NewRequest("POST", "/events", strings.NewReader(`{"type":"x","payload":{},"conn":"`+alice.id+`"}`))
	req.Header.Set("X-User", "mallory") // different identity claims alice's conn
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("spoofed conn = %d, want 403", rec.Code)
	}
}

func TestChannelAuthDenyByDefault(t *testing.T) {
	app := New([]byte(`{}`)) // no ChannelAuth set
	mux := http.NewServeMux()
	app.Mount(mux)
	c := testConn(app.hub, "")
	code := postEvent(t, mux, `{"type":"fa.subscribe","payload":{"channel":"secret"},"conn":"`+c.id+`"}`)
	if code != http.StatusForbidden {
		t.Fatalf("subscribe with no ChannelAuth = %d, want 403 (deny by default)", code)
	}
}

// TestChannelFanout: with ChannelAuth allowing, only the subscriber receives a
// channel emit.
func TestChannelFanout(t *testing.T) {
	app := New([]byte(`{}`)).ChannelAuth(func(identity, channel string) bool { return channel == "post:9" })
	mux := http.NewServeMux()
	app.Mount(mux)

	sub := testConn(app.hub, "")
	other := testConn(app.hub, "")
	if code := postEvent(t, mux, `{"type":"fa.subscribe","payload":{"channel":"post:9"},"conn":"`+sub.id+`"}`); code != http.StatusNoContent {
		t.Fatalf("authorized subscribe = %d, want 204", code)
	}

	app.hub.EmitChannel("post:9", Event{Op: "replace", FacetID: "Stats:post:9", Fragment: "<b>10</b>"})
	if e, ok := gotEvent(t, sub); !ok || e.FacetID != "Stats:post:9" {
		t.Fatalf("subscriber missed channel emit: ok=%v ev=%+v", ok, e)
	}
	if _, ok := gotEvent(t, other); ok {
		t.Fatal("LEAK: non-subscriber received channel emit")
	}
}

func TestUnknownEventIs404(t *testing.T) {
	app := New([]byte(`{}`))
	mux := http.NewServeMux()
	app.Mount(mux)
	req := httptest.NewRequest("POST", "/events", strings.NewReader(`{"type":"nope","payload":{}}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown event = %d, want 404", rec.Code)
	}
}

func TestSecureHeaders(t *testing.T) {
	app := New([]byte(`{}`))
	mux := http.NewServeMux()
	app.HandlePage(mux, ShellOptions{}, func(r *http.Request) template.HTML { return "<p>hi</p>" })

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q (got %q)", want, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Error("script-src must not allow 'unsafe-inline'")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
}

func TestMountServesRuntimeAndManifest(t *testing.T) {
	app := New([]byte(`{"facets":[{"name":"X"}]}`))
	mux := http.NewServeMux()
	app.Mount(mux)

	for _, tc := range []struct{ path, wantCT, wantSub string }{
		{"/manifest.json", "application/json", `"name":"X"`},
		{"/fa-runtime.js", "javascript", "data-facet-id"},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s = %d", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), tc.wantCT) {
			t.Errorf("%s content-type = %q", tc.path, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Body.String(), tc.wantSub) {
			t.Errorf("%s body missing %q", tc.path, tc.wantSub)
		}
	}
}
