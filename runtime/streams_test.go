package runtime

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

// A declared stream delivers the typed events actions emit: broadcast to every
// subscriber, or only to the subscriber signed in as the `to` actor; nothing
// from an action that failed; and the contract lists every event.
const streamApp = `app S:
    type Notify:
        who: text
        text: text
    type Ping:
        n: int
    entity Note:
        id: int
        body: text
    policy member:
        actor != "guest"
    stream "/api/v2/events" requires member: Notify, Ping
    stream "/api/v2/public": Ping
    action ping(n: int):
        emit Ping{n: n}
    action notify(who: text, text: text):
        emit Notify{who: who, text: text} to who
        add Note { body: text }
    action failing(n: int):
        emit Ping{n: n}
        check n > 0 "nope"
    view Home at "/":
        text "{count(Note)}"
`

// subscribe opens a stream as the given `?as=` identity and returns the
// frames it receives, read in the background.
func subscribe(t *testing.T, ts *httptest.Server, path, as string) (<-chan string, func()) {
	t.Helper()
	var token string
	if as != "" {
		resp, err := http.Get(ts.URL + "/?as=" + as)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == "fa_sid" {
				token = c.Value
			}
		}
	}
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("subscribe %s as %q: %d %s", path, as, resp.StatusCode, body)
	}
	frames := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var cur []string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if len(cur) > 0 {
					frames <- strings.Join(cur, "\n")
					cur = nil
				}
				continue
			}
			cur = append(cur, line)
		}
	}()
	return frames, func() { resp.Body.Close() }
}

func next(t *testing.T, frames <-chan string) string {
	t.Helper()
	select {
	case f := <-frames:
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("no frame within 3s")
	}
	return ""
}

func none(t *testing.T, frames <-chan string) {
	t.Helper()
	select {
	case f := <-frames:
		t.Fatalf("unexpected frame %q", f)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestStreamsDeliverEmittedEvents(t *testing.T) {
	g, err := compile.String(streamApp)
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

	// A gated stream refuses a guest.
	resp, _ := http.Get(ts.URL + "/api/v2/events")
	if resp.StatusCode != 401 {
		t.Fatalf("guest on a gated stream: %d", resp.StatusCode)
	}
	resp.Body.Close()

	ada, closeAda := subscribe(t, ts, "/api/v2/events", "ada")
	defer closeAda()
	bob, closeBob := subscribe(t, ts, "/api/v2/events", "bob")
	defer closeBob()
	pub, closePub := subscribe(t, ts, "/api/v2/public", "")
	defer closePub()
	for _, ch := range []<-chan string{ada, bob, pub} {
		if f := next(t, ch); !strings.HasPrefix(f, "event: hello") {
			t.Fatalf("first frame should be the hello, got %q", f)
		}
	}

	if _, err := srv.Run("ada", "member", true, "ping", []any{7}); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]<-chan string{"ada": ada, "bob": bob, "public": pub} {
		if f := next(t, ch); f != "id: 1\nevent: Ping\ndata: {\"n\":7}" {
			t.Fatalf("%s got %q", name, f)
		}
	}

	if _, err := srv.Run("ada", "member", true, "notify", []any{"bob", "hi bob"}); err != nil {
		t.Fatal(err)
	}
	f := next(t, bob)
	var payload map[string]any
	json.Unmarshal([]byte(strings.TrimPrefix(strings.Split(f, "\n")[2], "data: ")), &payload)
	if !strings.HasPrefix(f, "id: 2\nevent: Notify") || payload["text"] != "hi bob" {
		t.Fatalf("targeted event: %q", f)
	}
	none(t, ada) // not for ada
	none(t, pub) // the public stream does not carry Notify

	// A failed action emits nothing, even though its emit ran before the check.
	if _, err := srv.Run("ada", "member", true, "failing", []any{0}); err == nil {
		t.Fatal("failing should fail")
	}
	none(t, pub)

	// The contract lists the events with their stream and auth.
	r2, _ := http.Get(ts.URL + "/api/_contract")
	var doc map[string]any
	json.NewDecoder(r2.Body).Decode(&doc)
	r2.Body.Close()
	events, _ := doc["x-stream-events"].([]any)
	if len(events) != 5 { // Notify and Ping on one stream, Ping on the other, and each stream's hello
		t.Fatalf("x-stream-events = %v", events)
	}
	if paths := doc["paths"].(map[string]any); paths["/api/v2/events"] == nil {
		t.Fatal("stream route missing from paths")
	}
}

func TestStreamGates(t *testing.T) {
	base := "app G:\n    type Ping:\n        n: int\n    policy member:\n        actor != \"guest\"\n    policy owns(id: int):\n        id > 0\n" + "DECL" + "    view Home at \"/\":\n        text \"x\"\n"
	cases := []struct{ name, decl, want string }{
		{"event is not a wire type", "    stream \"/s\": Nope\n", "not a declared wire type"},
		{"emit with no stream", "    action p():\n        emit Ping{n: 1}\n", "no stream carries"},
		{"emit of a non-literal", "    stream \"/s\": Ping\n    action p():\n        emit 1\n", "wire type value"},
		{"stream requires a parameterized policy", "    stream \"/s\" requires owns: Ping\n", "takes arguments"},
		{"stream redeclared", "    stream \"/s\": Ping\n    stream \"/s\": Ping\n", "redeclared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := compile.String(strings.Replace(base, "DECL", c.decl, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
