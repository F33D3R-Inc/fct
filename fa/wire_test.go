package fa

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWireVersionNegotiation(t *testing.T) {
	app := New([]byte(`{}`))
	mux := http.NewServeMux()
	app.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer app.Shutdown()

	get := func(path string, header string) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if header != "" {
			req.Header.Set("FA-Wire-Version", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	// A mismatched version — header (native) or ?v= (web) — fails loud at
	// connect with 426 and announces the server's version.
	for _, tc := range []struct{ path, header string }{
		{"/sse", "99"},
		{"/sse?v=99", ""},
	} {
		resp := get(tc.path, tc.header)
		if resp.StatusCode != http.StatusUpgradeRequired {
			t.Errorf("GET %s (header %q) = %d, want 426", tc.path, tc.header, resp.StatusCode)
		}
		if got := resp.Header.Get("FA-Wire-Version"); got != WireVersion {
			t.Errorf("426 response should announce the server version, got %q", got)
		}
		resp.Body.Close()
	}

	// A matching version (and the absent legacy case, which means v1) connects,
	// and the hello frame carries the server's wire version.
	for _, header := range []string{WireVersion, ""} {
		resp := get("/sse", header)
		if resp.StatusCode != 200 {
			t.Fatalf("GET /sse (header %q) = %d, want 200", header, resp.StatusCode)
		}
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil {
			t.Fatalf("reading hello frame: %v", err)
		}
		if !strings.Contains(line, `"op":"_conn"`) || !strings.Contains(line, `"v":"`+WireVersion+`"`) {
			t.Errorf("hello frame should carry op _conn and v %q, got %q", WireVersion, line)
		}
		resp.Body.Close()
	}
}
