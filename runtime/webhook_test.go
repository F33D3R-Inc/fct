package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// An inbound webhook runs its action with the JSON body decoded into the action's
// params — but only after the HMAC over the raw body checks out. A valid signature
// writes a row; a bad or missing one is rejected with 403 and writes nothing.
const payHookApp = `app Pay:
    entity Payment:
        id: int
        ref: text
        cents: int
    action record(ref: text, cents: int):
        add Payment { ref: ref, cents: cents }
    webhook "/hooks/pay" -> record
    view Home at "/":
        box:
            text "{count(Payment)}"
`

func signHook(body []byte) string {
	mac := hmac.New(sha256.New, ring().signKey)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func postHook(t *testing.T, url, body, sig string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if sig != "" {
		req.Header.Set("X-Facet-Signature", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestWebhookRunsActionWhenSigned(t *testing.T) {
	g, err := compile.String(payHookApp)
	if err != nil {
		t.Fatal(err)
	}
	// The JSON API publishes an entity only when the app says so — the default is
	// closed (runtime/apiread.go). This test reads Payment over it, so it says so.
	t.Setenv(apiReadEnv, "Payment")
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	rows := func() int {
		r, _ := getJSON(t, ts.URL+"/api/Payment")["rows"].([]any)
		return len(r)
	}

	body := `{"ref":"inv_1","cents":4200}`

	// Missing signature → 403, nothing written.
	if code := postHook(t, ts.URL+"/hooks/pay", body, ""); code != http.StatusForbidden {
		t.Fatalf("unsigned webhook: want 403, got %d", code)
	}
	// Wrong signature → 403, nothing written.
	if code := postHook(t, ts.URL+"/hooks/pay", body, "deadbeef"); code != http.StatusForbidden {
		t.Fatalf("mis-signed webhook: want 403, got %d", code)
	}
	if rows() != 0 {
		t.Fatalf("rejected webhooks must write nothing, got %d rows", rows())
	}

	// Valid signature → 200, one Payment row with the decoded fields.
	if code := postHook(t, ts.URL+"/hooks/pay", body, signHook([]byte(body))); code != http.StatusOK {
		t.Fatalf("signed webhook: want 200, got %d", code)
	}
	if rows() != 1 {
		t.Fatalf("signed webhook should write one row, got %d", rows())
	}
}
