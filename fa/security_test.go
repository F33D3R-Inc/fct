package fa

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCrossOriginRejected(t *testing.T) {
	app := New([]byte(`{}`))
	app.On("x", func(c Ctx) ([]Event, error) { return nil, nil })
	mux := http.NewServeMux()
	app.Mount(mux)

	req := httptest.NewRequest("POST", "/events", strings.NewReader(`{"type":"x","payload":{},"conn":"z"}`))
	req.Host = "app.example"
	req.Header.Set("Origin", "https://evil.example") // different host
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d, want 403", rec.Code)
	}
}

func TestRateLimit(t *testing.T) {
	app := New([]byte(`{}`))
	app.limiter = newRateLimiter(0, 1) // one token, no refill
	app.On("x", func(c Ctx) ([]Event, error) { return nil, nil })
	mux := http.NewServeMux()
	app.Mount(mux)
	conn := testConn(app.hub, "")
	body := `{"type":"x","payload":{},"conn":"` + conn.id + `"}`

	if code := postEvent(t, mux, body); code != http.StatusNoContent {
		t.Fatalf("1st request = %d, want 204", code)
	}
	if code := postEvent(t, mux, body); code != http.StatusTooManyRequests {
		t.Fatalf("2nd request = %d, want 429 (rate limited)", code)
	}
}

func TestGuardBlocksUnauthorized(t *testing.T) {
	app := New([]byte(`{}`))
	ran := false
	app.On("post.delete", func(c Ctx) ([]Event, error) { ran = true; return nil, nil })
	app.Guard("post.delete", func(c Ctx) bool { return c.Payload["postId"] == "mine" })
	mux := http.NewServeMux()
	app.Mount(mux)
	conn := testConn(app.hub, "")

	code := postEvent(t, mux, `{"type":"post.delete","payload":{"postId":"yours"},"conn":"`+conn.id+`"}`)
	if code != http.StatusForbidden || ran {
		t.Fatalf("unauthorized event must be blocked: code=%d ran=%v", code, ran)
	}
	code = postEvent(t, mux, `{"type":"post.delete","payload":{"postId":"mine"},"conn":"`+conn.id+`"}`)
	if code != http.StatusNoContent || !ran {
		t.Fatalf("authorized event must run: code=%d ran=%v", code, ran)
	}
}

func TestConnCapPerIP(t *testing.T) {
	h := newHub(nil, nil)
	mk := func(ip string) *sseClient {
		return &sseClient{id: newConnID(), ip: ip, channels: make(map[string]bool), send: make(chan []byte, 1)}
	}
	for i := 0; i < maxConnsPerIP; i++ {
		if !h.register(mk("1.2.3.4")) {
			t.Fatalf("registration %d under cap should succeed", i)
		}
	}
	if h.register(mk("1.2.3.4")) {
		t.Fatal("registration over per-IP cap should be refused")
	}
	if !h.register(mk("5.6.7.8")) {
		t.Fatal("a different IP must be unaffected by another IP's cap")
	}
}
