package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// postHookFull posts a webhook delivery with an optional Idempotency-Key and
// returns the status plus whether the response was an idempotent replay.
func postHookFull(t *testing.T, url, body, sig, key string) (int, bool) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if sig != "" {
		req.Header.Set("X-Facet-Signature", sig)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Facet-Idempotent-Replay") == "1"
}

func idemServer(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.String(payHookApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// A retried delivery — same payload, hence same signature — must process exactly
// once. This is the commerce-correctness fix: a retried confirmPaid can't double
// charge. Every retry replays the first outcome.
func TestWebhookIdempotentBySignature(t *testing.T) {
	ts := idemServer(t)
	rows := func() int {
		r, _ := getJSON(t, ts.URL+"/api/Payment")["rows"].([]any)
		return len(r)
	}
	body := `{"ref":"inv_1","cents":4200}`
	sig := signHook([]byte(body))

	if code, replay := postHookFull(t, ts.URL+"/hooks/pay", body, sig, ""); code != http.StatusOK || replay {
		t.Fatalf("first delivery: want 200 non-replay, got %d replay=%v", code, replay)
	}
	for i := 0; i < 3; i++ {
		code, replay := postHookFull(t, ts.URL+"/hooks/pay", body, sig, "")
		if code != http.StatusOK || !replay {
			t.Fatalf("retry %d: want 200 replay, got %d replay=%v", i, code, replay)
		}
	}
	if rows() != 1 {
		t.Fatalf("a retried delivery must write exactly one row, got %d", rows())
	}
}

// An explicit Idempotency-Key dedups even when the body differs — the sender's key
// is the identity of the delivery.
func TestWebhookIdempotentByKey(t *testing.T) {
	ts := idemServer(t)
	rows := func() int {
		r, _ := getJSON(t, ts.URL+"/api/Payment")["rows"].([]any)
		return len(r)
	}
	b1 := `{"ref":"inv_2","cents":100}`
	b2 := `{"ref":"inv_2","cents":999}` // different body, same logical delivery
	if code, _ := postHookFull(t, ts.URL+"/hooks/pay", b1, signHook([]byte(b1)), "evt_42"); code != http.StatusOK {
		t.Fatalf("first: want 200, got %d", code)
	}
	code, replay := postHookFull(t, ts.URL+"/hooks/pay", b2, signHook([]byte(b2)), "evt_42")
	if code != http.StatusOK || !replay {
		t.Fatalf("same-key retry: want 200 replay, got %d replay=%v", code, replay)
	}
	if rows() != 1 {
		t.Fatalf("same Idempotency-Key must process once, got %d rows", rows())
	}
}

// Distinct deliveries (different keys) each process.
func TestWebhookDistinctDeliveriesEachProcess(t *testing.T) {
	ts := idemServer(t)
	rows := func() int {
		r, _ := getJSON(t, ts.URL+"/api/Payment")["rows"].([]any)
		return len(r)
	}
	b := `{"ref":"inv_3","cents":500}`
	postHookFull(t, ts.URL+"/hooks/pay", b, signHook([]byte(b)), "k1")
	postHookFull(t, ts.URL+"/hooks/pay", b, signHook([]byte(b)), "k2")
	if rows() != 2 {
		t.Fatalf("two distinct deliveries should write two rows, got %d", rows())
	}
}

// A concurrent retry that arrives while the first is still in flight is told the
// delivery is in progress (409), never double-processed.
func TestWebhookConcurrentInFlightConflict(t *testing.T) {
	srv := &Server{idem: map[string]*idemRecord{}}
	key := "/hooks/pay\x00k:dup"
	if _, replay := srv.idemBegin(key); replay {
		t.Fatal("first claim should own the key, not replay")
	}
	// A second claim before idemFinish sees the in-flight marker.
	rec, replay := srv.idemBegin(key)
	if !replay || rec.done {
		t.Fatalf("concurrent claim should see an in-flight (not done) record, got replay=%v done=%v", replay, rec.done)
	}
	// After finishing, a later retry replays the recorded outcome.
	srv.idemFinish(key, http.StatusOK, `{"ok":true}`)
	rec, replay = srv.idemBegin(key)
	if !replay || !rec.done || rec.body != `{"ok":true}` {
		t.Fatalf("after finish, a retry should replay the done outcome, got %+v replay=%v", rec, replay)
	}
}
