package runtime

import (
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"facet/internal/compile"
)

// Profiles at the root (`/:handle`, `/:handle/followers`) beside literal
// routes: the literal or more specific route wins whatever order the views
// are declared in, so /settings/followers is the settings page and
// /ada/followers ada's followers.
func TestMoreSpecificRouteWins(t *testing.T) {
	g, err := compile.String(`app R:
    view Profile at "/:handle":
        text "profile of {handle}"
    view Followers at "/:handle/followers":
        text "followers of {handle}"
    view Work at "/:handle/work/:id":
        text "work {id} by {handle}"
    view Settings at "/settings/:section":
        text "settings {section}"
    view Live at "/live":
        text "live now"
    view Stream at "/live/:id":
        text "stream {id}"
`)
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
	for path, want := range map[string]string{
		"/ada": "profile of ada", "/live": "live now", "/ada/followers": "followers of ada",
		"/settings/followers": "settings followers", "/live/followers": "stream followers",
		"/ada/work/7": "work 7 by ada",
	} {
		res, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(string(b), "")
		if !strings.Contains(text, want) {
			t.Errorf("GET %s: %d want %q: %.300s", path, res.StatusCode, want, b)
		}
	}
	if !strings.Contains(string(clientJS), "function routeMoreSpecific(") {
		t.Error("the client must choose routes by the same specificity rule")
	}
}
