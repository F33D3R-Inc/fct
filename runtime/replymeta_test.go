package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A failed check's `code "…"` is the error code a declared route (and a
// dispatched variant) answers with; without one the code is the status's
// name. A committed action's `header "Name" expr` rides its HTTP reply —
// never a failed one's, and never a value that could split the response.
const replyMetaApp = `app R:
    type Out:
        url: text
    entity Seen:
        id: int
        note: text
    message Mutation tag "event_type":
        | go as "go" -> goTo since "2026-09-22" "Go somewhere.":
            where: text "Where."
    action goTo(where: text):
        check where != "" "say where" code "where_required" status 400
        check where != "nowhere" "nowhere to go" status 409
        header "HX-Redirect" "https://pay.example/" + where
        add Seen { note: where }
    action lookup(where: text) -> Out:
        check where != "bad" "no such policy" status 400 code "unknown_policy"
        header "X-Trace" "t-" + where
        return Out{url: where}
    action split(where: text):
        header "HX-Redirect" "a\r\nSet-Cookie: x=1" + where
    api POST "/go" -> goTo since "2026-09-22"
    api GET "/lookup" -> lookup since "2026-09-22"
    api POST "/split" -> split since "2026-09-22"
    api POST "/events" -> Mutation since "2026-09-22"
    view Home at "/":
        text "{count(Seen)}"
`

func TestCheckCodeAndReplyHeaders(t *testing.T) {
	g, err := compile.String(replyMetaApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	do := func(method, path, body string) (*http.Response, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		return r, string(raw)
	}

	r, body := do("POST", "/go", `{"where":""}`)
	if r.StatusCode != 400 || !strings.Contains(body, `"code":"where_required"`) || !strings.Contains(body, "say where") {
		t.Fatalf("coded check = %d %s", r.StatusCode, body)
	}
	if h := r.Header.Get("HX-Redirect"); h != "" {
		t.Fatalf("a failed action set a header: %q", h)
	}
	r, body = do("POST", "/go", `{"where":"nowhere"}`)
	if r.StatusCode != 409 || !strings.Contains(body, `"code":"conflict"`) {
		t.Fatalf("uncoded check = %d %s", r.StatusCode, body)
	}
	r, body = do("POST", "/go", `{"where":"acct_1"}`)
	if r.StatusCode != 200 || r.Header.Get("HX-Redirect") != "https://pay.example/acct_1" {
		t.Fatalf("header on success = %d %q %s", r.StatusCode, r.Header.Get("HX-Redirect"), body)
	}
	// The status and code clauses in either order, on a reply-bearing GET.
	r, body = do("GET", "/lookup?where=bad", "")
	if r.StatusCode != 400 || !strings.Contains(body, `"code":"unknown_policy"`) {
		t.Fatalf("code before status = %d %s", r.StatusCode, body)
	}
	r, body = do("GET", "/lookup?where=ok", "")
	if r.StatusCode != 200 || r.Header.Get("X-Trace") != "t-ok" || !strings.Contains(body, `"url":"ok"`) {
		t.Fatalf("reply with header = %d %q %s", r.StatusCode, r.Header.Get("X-Trace"), body)
	}
	// A value that would split the response fails the action instead.
	r, body = do("POST", "/split", `{"where":"x"}`)
	if r.StatusCode != 500 || strings.Contains(r.Header.Get("Set-Cookie"), "x=1") || r.Header.Get("HX-Redirect") != "" || !strings.Contains(body, `"code":"server_error"`) {
		t.Fatalf("header injection = %d %v %s", r.StatusCode, r.Header, body)
	}
	// The dispatch lane carries both too.
	r, body = do("POST", "/events", `{"event_type":"go","where":""}`)
	if r.StatusCode != 400 || !strings.Contains(body, `"code":"where_required"`) {
		t.Fatalf("dispatched coded check = %d %s", r.StatusCode, body)
	}
	r, _ = do("POST", "/events", `{"event_type":"go","where":"acct_2"}`)
	if r.StatusCode != 204 || r.Header.Get("HX-Redirect") != "https://pay.example/acct_2" {
		t.Fatalf("dispatched header = %d %q", r.StatusCode, r.Header.Get("HX-Redirect"))
	}
	// A missing session is `unauthenticated`, as the legacy API taught clients.
	if errorCode(401) != "unauthenticated" || errorCode(429) != "rate_limited" || errorCode(500) != "server_error" || errorCode(404) != "not_found" || errorCode(503) != "unavailable" || errorCode(502) != "upstream_unavailable" {
		t.Fatalf("default codes: %s %s %s %s", errorCode(401), errorCode(429), errorCode(500), errorCode(404))
	}
}

// The compiler refuses a header the runtime owns, a name that is not an HTTP
// token, and a malformed code clause.
func TestHeaderAndCodeCompileErrors(t *testing.T) {
	base := "app R:\n    entity Seen:\n        id: int\n    action a(x: text):\n        %s\n    view Home at \"/\":\n        text \"hi\"\n"
	for _, tc := range []struct{ stmt, want string }{
		{`header "Set-Cookie" x`, "written by the runtime"},
		{`header "access-control-allow-origin" x`, "written by the runtime"},
		{`header "X-Session-Token" x`, "written by the runtime"},
		{`header "Bad Name" x`, "not an HTTP token"},
		{`header "" x`, "header needs"},
		{`header "HX-Redirect"`, "header needs"},
		{`check x != "" "m" code "Not-Snake"`, ""},
	} {
		_, err := compile.String(strings.Replace(base, "%s", tc.stmt, 1))
		if tc.want == "" {
			// An invalid code spelling is not a code clause; the trailing text
			// is then part of the message and fails to parse.
			if err == nil {
				t.Errorf("%s compiled", tc.stmt)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.stmt, err, tc.want)
		}
	}
}
