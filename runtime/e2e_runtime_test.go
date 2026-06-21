package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// The server side of @e2e: it stores whatever (sealed) value it is handed without
// transforming it, and renders the sealed interpolation as a lock placeholder with
// the ciphertext only in a data attribute — it never holds or shows plaintext. The
// sealing/opening itself is the client's (facet.js) job.
const e2eRuntimeApp = `app E:
    entity DM:
        id: int
        body: text @e2e
    action send(body: text):
        add DM { body: body }
    view Home at "/":
        box:
            for m in DM:
                text "{m.body}"
`

func TestE2EServerStoresAndRendersSealed(t *testing.T) {
	g, err := compile.String(e2eRuntimeApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The client would POST an already-sealed body; the server must store it verbatim.
	const ciph = "fae2e1:QUJD.W0RFRg=="
	resp, err := http.Post(ts.URL+"/api/send", "application/json", strings.NewReader(`{"args":["`+ciph+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send returned %d", resp.StatusCode)
	}

	// The stored row holds the ciphertext exactly — the authority applied no crypto.
	rows := srv.entities["DM"]
	if len(rows) != 1 {
		t.Fatalf("expected one DM row, got %d", len(rows))
	}
	if got := toStr(rows[0].(record)["body"]); got != ciph {
		t.Fatalf("stored body = %q, want the ciphertext verbatim %q", got, ciph)
	}

	// The rendered page shows the lock placeholder and carries the ciphertext only in
	// the data attribute the client opens — never as visible plaintext.
	page, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	html := string(body)
	if !strings.Contains(html, `data-fa-e2e="`+ciph+`"`) {
		t.Errorf("rendered HTML should carry the ciphertext in data-fa-e2e:\n%s", html)
	}
	if !strings.Contains(html, e2ePlaceholder) {
		t.Errorf("rendered HTML should show the lock placeholder %q", e2ePlaceholder)
	}
}
