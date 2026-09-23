package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// The block form of `stream` names each event apart from its payload type,
// so two events can share one DTO; `emit name Dto{…}` picks one of them, an
// unnamed emit goes out under the single name that carries its type. Every
// frame is numbered, a reconnect with Last-Event-ID (or ?last_event_id=)
// replays what was missed and only what the subscriber may see, and the
// connect frame is a typed, unnumbered hello.
const namedStreamApp = `app S:
    type WorkRemoved:
        work_id: int
    type Notify:
        unread: int
    policy member:
        actor != "guest"
    stream "/api/v2/events" requires member rate read since "2026-09-18":
        post_deleted: WorkRemoved since "2026-09-06" "A work was deleted; remove it from every screen."
        work_removed: WorkRemoved since "2026-09-13" "A watched work is gone."
        notify: Notify since "2026-09-06" "The unread notification count changed."
    action deletePost(id: int):
        emit post_deleted WorkRemoved{work_id: id}
    action unwatch(id: int):
        emit work_removed WorkRemoved{work_id: id} to actor
    action bump(n: int, who: text):
        emit Notify{unread: n} to who
    view Home at "/":
        text "x"
`

func TestNamedStreamEvents(t *testing.T) {
	g, err := compile.String(namedStreamApp)
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

	ada, closeAda := subscribe(t, ts, "/api/v2/events", "ada")
	hello := next(t, ada)
	if !strings.HasPrefix(hello, "event: hello\ndata: ") {
		t.Fatalf("first frame should be the unnumbered hello, got %q", hello)
	}
	var h map[string]any
	json.Unmarshal([]byte(strings.TrimPrefix(hello, "event: hello\ndata: ")), &h)
	if h["session_id"] == "" || h["server_time"] == nil || h["schema_version"] == nil ||
		h["contract_version"] != srv.contractVersion() {
		t.Fatalf("hello payload = %v", h)
	}

	if _, err := srv.Run("ada", "member", true, "deletePost", []any{7}); err != nil {
		t.Fatal(err)
	}
	if f := next(t, ada); f != "id: 1\nevent: post_deleted\ndata: {\"work_id\":7}" {
		t.Fatalf("named emit = %q", f)
	}
	if _, err := srv.Run("ada", "member", true, "bump", []any{3, "ada"}); err != nil {
		t.Fatal(err)
	}
	if f := next(t, ada); f != "id: 2\nevent: notify\ndata: {\"unread\":3}" {
		t.Fatalf("unnamed emit = %q", f)
	}
	closeAda()

	// While ada is away: one event for her, one for bob, one broadcast.
	for _, run := range []struct {
		name string
		args []any
	}{{"bump", []any{4, "ada"}}, {"bump", []any{9, "bob"}}, {"deletePost", []any{8}}} {
		if _, err := srv.Run("ada", "member", true, run.name, run.args); err != nil {
			t.Fatal(err)
		}
	}
	// Reconnecting after id 2 replays ids 3 and 5 — never bob's 4.
	for _, resume := range []struct{ header, query string }{{"2", ""}, {"", "2"}} {
		frames, stop := subscribeResume(t, ts, "/api/v2/events", "ada", resume.header, resume.query)
		if f := next(t, frames); !strings.HasPrefix(f, "event: hello") {
			t.Fatalf("resume should still open with hello, got %q", f)
		}
		if f := next(t, frames); f != "id: 3\nevent: notify\ndata: {\"unread\":4}" {
			t.Fatalf("replay[0] = %q", f)
		}
		if f := next(t, frames); f != "id: 5\nevent: post_deleted\ndata: {\"work_id\":8}" {
			t.Fatalf("replay[1] = %q", f)
		}
		none(t, frames)
		stop()
	}

	// The contract documents every event (hello first by name order) in the
	// shape clients read, and the stream route with its resume parameters.
	resp, _ := http.Get(ts.URL + "/api/_contract")
	var doc map[string]any
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	events := doc["x-stream-events"].([]any)
	var names []string
	for _, e := range events {
		ev := e.(map[string]any)
		names = append(names, ev["name"].(string))
		if ev["stream"] != "/api/v2/events" || ev["x-since"] == "" || ev["summary"] == "" || ev["auth"] != nil {
			t.Fatalf("stream event entry = %v", ev)
		}
	}
	if strings.Join(names, ",") != "hello,notify,post_deleted,work_removed" {
		t.Fatalf("x-stream-events names = %v", names)
	}
	raw, _ := json.Marshal(events)
	if !strings.Contains(string(raw), `"payload":{"oneOf":[{"$ref":"#/components/schemas/WorkRemoved"},{"type":"null"}]}`) {
		t.Fatalf("payload shape: %s", raw)
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if schemas["HelloEventDTO"] == nil {
		t.Fatal("HelloEventDTO missing")
	}
	op := doc["paths"].(map[string]any)["/api/v2/events"].(map[string]any)["get"].(map[string]any)
	if op["operationId"] != "getEvents" || op["x-auth"] != "session" || op["x-since"] != "2026-09-18" || op["x-rate-limit"] == nil {
		t.Fatalf("stream op = %v", op)
	}
	ps, _ := json.Marshal(op["parameters"])
	if !strings.Contains(string(ps), `"Last-Event-ID"`) || !strings.Contains(string(ps), `"last_event_id"`) {
		t.Fatalf("stream op params = %s", ps)
	}
}

// subscribeResume opens a stream as `as`, resuming by header or query.
func subscribeResume(t *testing.T, ts *httptest.Server, path, as, header, query string) (<-chan string, func()) {
	t.Helper()
	p := path
	if query != "" {
		p += "?last_event_id=" + query
	}
	if header == "" {
		return subscribe(t, ts, p, as)
	}
	// subscribe has no header hook; set it through a one-off transport.
	orig := http.DefaultClient.Transport
	http.DefaultClient.Transport = headerTransport{"Last-Event-ID", header}
	defer func() { http.DefaultClient.Transport = orig }()
	return subscribe(t, ts, p, as)
}

type headerTransport struct{ k, v string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set(h.k, h.v)
	return http.DefaultTransport.RoundTrip(r)
}

func TestNamedStreamGates(t *testing.T) {
	base := "app G:\n    type Ping:\n        n: int\n    type Pong:\n        n: int\n" + "DECL" + "    view Home at \"/\":\n        text \"x\"\n"
	cases := []struct{ name, decl, want string }{
		{"ambiguous unnamed emit", "    stream \"/s\":\n        a: Ping\n        b: Ping\n    action p():\n        emit Ping{n: 1}\n", "name the event"},
		{"named emit of an unknown event", "    stream \"/s\":\n        a: Ping\n    action p():\n        emit c Ping{n: 1}\n", "no stream carries an event \"c\""},
		{"named emit with the wrong payload", "    stream \"/s\":\n        a: Ping\n        b: Pong\n    action p():\n        emit a Pong{n: 1}\n", "no stream carries an event \"a\""},
		{"duplicate event name", "    stream \"/s\":\n        a: Ping\n        a: Pong\n", "declares event \"a\" twice"},
		{"hello is the runtime's", "    stream \"/s\":\n        hello: Ping\n", "connect frame"},
		{"capitalized event name", "    stream \"/s\":\n        Ping: Ping\n", "lowercase identifier"},
		{"both forms", "    stream \"/s\": Ping\n        a: Pong\n", "not both"},
		{"bad rate", "    stream \"/s\" rate fast: Ping\n", "read, write or auth"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := compile.String(strings.Replace(base, "DECL", c.decl, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	// Two names for one type is fine when every emit names its event.
	ok := strings.Replace(base, "DECL", "    stream \"/s\":\n        a: Ping\n        b: Ping\n    action p():\n        emit a Ping{n: 1}\n        emit b Ping{n: 2}\n", 1)
	if _, err := compile.String(ok); err != nil {
		t.Fatalf("named emits over a shared type: %v", err)
	}
}
