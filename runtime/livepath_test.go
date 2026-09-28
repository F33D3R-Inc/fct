package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

// The change stream is /api/_live, in the runtime's own namespace, and /live
// belongs to the app: a page there is served and not reported as shadowed,
// and without one /live is an ordinary unknown path, never the stream.
func TestLivePathBelongsToTheApp(t *testing.T) {
	contentType := func(t *testing.T, url string) (int, string, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		ct := resp.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "text/html") {
			buf := make([]byte, 1<<16)
			n, _ := resp.Body.Read(buf)
			return resp.StatusCode, ct, string(buf[:n])
		}
		return resp.StatusCode, ct, ""
	}
	for _, c := range []struct {
		name, src string
		livePage  bool
	}{
		{"with a page at /live", "app L:\n    view Live at \"/live\":\n        text \"who is live\"\n    view Home at \"/\":\n        text \"home\"\n", true},
		{"without one", "app L:\n    view Home at \"/\":\n        text \"home\"\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, err := compile.String(c.src)
			if err != nil {
				t.Fatal(err)
			}
			srv, err := NewInMemory(g)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Shutdown()
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			if _, ct, _ := contentType(t, ts.URL+"/api/_live"); !strings.HasPrefix(ct, "text/event-stream") {
				t.Errorf("/api/_live content type %q, want the event stream", ct)
			}
			code, ct, body := contentType(t, ts.URL+"/live")
			if c.livePage {
				if !strings.HasPrefix(ct, "text/html") || !strings.Contains(body, "who is live") {
					t.Errorf("/live = %q, want the app's page", ct)
				}
				if len(ShadowedRoutes(g)) != 0 {
					t.Errorf("a page at /live is reported shadowed: %+v", ShadowedRoutes(g))
				}
			} else if strings.HasPrefix(ct, "text/event-stream") || code != http.StatusNotFound {
				t.Errorf("/live without a page = %d %q, want not found: the stream's former path is retired", code, ct)
			}
		})
	}
	if !strings.Contains(string(clientJS), `openLive("/api/_live")`) {
		t.Error("the client must open the stream at /api/_live")
	}
	if strings.Contains(string(clientJS), `"/live"`) {
		t.Error("the client must not fall back to /live, which is the app's path")
	}
}
