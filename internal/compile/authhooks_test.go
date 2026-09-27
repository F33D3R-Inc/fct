package compile

import (
	"strings"
	"testing"
)

// An app with `auth` gets the built-in auth actions appended after its own
// have been indexed; the api routes and stream hooks resolved through that
// index must still write into the graph's actions. They once wrote into the
// array the append had just replaced, so a hooked action kept its
// entity-write reason (facets/live.fct's joinRoom and leaveRoom).
func TestAuthAppKeepsHookAndRouteReasons(t *testing.T) {
	g, err := String(`app A:
    auth
    entity Room:
        id: int
        viewers: int
    policy member:
        actor != "guest"
    type ViewersDTO:
        n: int
    action joinRoom(id: int) -> ViewersDTO:
        set Room(id).viewers = Room(id).viewers + 1
        return ViewersDTO{n: Room(id).viewers}
    action leaveRoom(id: int):
        set Room(id).viewers = Room(id).viewers - 1
    stream "/live/{id}/events" requires member:
        viewers: ViewersDTO "Viewer count."
        connect -> joinRoom as viewers
        disconnect -> leaveRoom
    view Home at "/":
        text "x"
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	want := map[string]string{
		"joinRoom":  "runs on connect of stream /live/{id}/events",
		"leaveRoom": "runs on disconnect of stream /live/{id}/events",
	}
	for _, a := range g.Actions {
		if w, ok := want[a.Name]; ok {
			if !strings.HasPrefix(a.Reason, w) {
				t.Errorf("action %s reason = %q, want it to start %q", a.Name, a.Reason, w)
			}
			delete(want, a.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("actions missing from the graph: %v", want)
	}
}
