package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

// A stream with path parameters is one stream per value: `emit … on id`
// reaches only that room's subscribers. Its connect hook runs as the
// subscriber with the path values (refusing the connection when it fails,
// its reply sent first as the named event) and its disconnect hook runs when
// the connection closes.
const roomApp = `app R:
    type ChatDTO:
        body: text
    type ViewersDTO:
        count: int
    entity Room:
        id: int
        name: text
    entity Viewer:
        id: int
        room: int
        who: text
    policy member:
        actor != "guest"
    stream "/api/v2/rooms/{id}/events" requires member rate read since "2026-09-18":
        connect -> joinRoom as viewers
        disconnect -> leaveRoom
        chat: ChatDTO since "2026-09-06" "A chat message was posted."
        viewers: ViewersDTO since "2026-09-06" "The viewer count; also sent once on connect."
    action seed():
        add Room { name: "a" }
        add Room { name: "b" }
    action joinRoom(id: int) -> ViewersDTO:
        requires member
        check exists(r in Room where r.id == id) "no such room" status 404
        add Viewer { room: id, who: actor }
        emit viewers ViewersDTO{count: count(v in Viewer where v.room == id)} on id
        return ViewersDTO{count: count(v in Viewer where v.room == id)}
    action leaveRoom(id: int):
        remove v in Viewer where v.room == id && v.who == actor
        emit viewers ViewersDTO{count: count(v in Viewer where v.room == id)} on id
    action say(id: int, body: text):
        emit chat ChatDTO{body: body} on id
    action viewers(id: int) -> int:
        return count(v in Viewer where v.room == id)
    view Home at "/":
        text "x"
`

func TestParameterizedStreams(t *testing.T) {
	g, err := compile.String(roomApp)
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
	srv.Run("ada", "member", true, "seed", nil)

	// The room stream declares no hello: the connect hook's reply is first.
	a1, closeA1 := subscribe(t, ts, "/api/v2/rooms/1/events", "ada")
	if f := next(t, a1); f != "event: viewers\ndata: {\"count\":1}" {
		t.Fatalf("connect reply = %q", f)
	}
	// A second viewer in room 1: the first sees the count move.
	c1, closeC1 := subscribe(t, ts, "/api/v2/rooms/1/events", "cy")
	if f := next(t, c1); f != "event: viewers\ndata: {\"count\":2}" {
		t.Fatalf("second connect reply = %q", f)
	}
	if f := next(t, a1); !strings.HasSuffix(f, "event: viewers\ndata: {\"count\":2}") {
		t.Fatalf("join broadcast = %q", f)
	}
	closeC1()
	if f := next(t, a1); !strings.HasSuffix(f, "event: viewers\ndata: {\"count\":1}") {
		t.Fatalf("leave broadcast = %q", f)
	}
	b2, closeB2 := subscribe(t, ts, "/api/v2/rooms/2/events", "bob")
	next(t, b2) // its connect reply
	none(t, a1) // bob joined room 2, not room 1

	srv.Run("ada", "member", true, "say", []any{1, "hi room one"})
	if f := next(t, a1); !strings.Contains(f, "event: chat") || !strings.Contains(f, "hi room one") {
		t.Fatalf("room 1 chat = %q", f)
	}
	none(t, b2)

	// A missing room refuses the connection with the connect hook's status.
	req, _ := http.NewRequest("GET", ts.URL+"/api/v2/rooms/9/events", nil)
	resp, _ := http.Get(ts.URL + "/?as=ada")
	for _, c := range resp.Cookies() {
		if c.Name == "fa_sid" {
			req.Header.Set("Authorization", "Bearer "+c.Value)
		}
	}
	resp.Body.Close()
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 404 {
		t.Fatalf("unknown room = %d", r2.StatusCode)
	}

	// Leaving runs the disconnect hook.
	closeA1()
	for i := 0; i < 50; i++ {
		if v, _ := srv.RunValue("ada", "member", true, "viewers", []any{1}); toInt(v) == 0 {
			break
		}
		if i == 49 {
			t.Fatal("disconnect hook did not run")
		}
		sleepMs(20)
	}
	closeB2()

	var doc map[string]any
	r3, _ := http.Get(ts.URL + "/api/_contract")
	json.NewDecoder(r3.Body).Decode(&doc)
	r3.Body.Close()
	op := doc["paths"].(map[string]any)["/api/v2/rooms/{id}/events"].(map[string]any)["get"].(map[string]any)
	ps, _ := json.Marshal(op["parameters"])
	if op["operationId"] != "getRoomsIdEvents" || !strings.Contains(string(ps), `"in":"path","name":"id"`) || op["responses"].(map[string]any)["404"] == nil {
		t.Fatalf("stream op = %v", op)
	}
}

func TestParameterizedStreamGates(t *testing.T) {
	cases := map[string][2]string{
		"emit without on":     {"emit chat ChatDTO{body: body} on id", "name the instance"},
		"hook param mismatch": {"    action leaveRoom(id: int):", "no more and no fewer"},
		"connect wrong reply": {"connect -> joinRoom as viewers", "must return ViewersDTO"},
		"api shadows stream":  {"    view Home", "is also declared as a stream"},
	}
	repl := map[string]string{
		"emit without on":     "emit chat ChatDTO{body: body}",
		"hook param mismatch": "    action leaveRoom(id: int, x: int):",
		"connect wrong reply": "connect -> viewers as viewers",
		"api shadows stream":  "    api GET \"/api/v2/rooms/{id}/events\" -> viewers\n    view Home",
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compile.String(strings.Replace(roomApp, c[0], repl[name], 1))
			if err == nil || !strings.Contains(err.Error(), c[1]) {
				t.Fatalf("want %q, got %v", c[1], err)
			}
		})
	}
}

func sleepMs(n int) { <-time.After(time.Duration(n) * time.Millisecond) }
