package compile

import (
	"strings"
	"testing"
)

// Every action carries a human-readable placement reason (for `facet explain`),
// and it matches the computed placement.
func TestPlacementReason(t *testing.T) {
	g := mustCompile(t, `app A:
    state count: int = 0
    state bonus: int = 0 @client
    entity Post:
        id: int
        body: text
    action inc:
        count = count + 1
    action addBonus:
        bonus = bonus + 1
    action post(body: text):
        add Post { body: body }
    view M:
        box:
            text "{count}"
`)
	wantPlace := map[string]string{"inc": "server", "addBonus": "client", "post": "server"}
	for _, a := range g.Actions {
		if a.Reason == "" {
			t.Errorf("action %q has no placement reason", a.Name)
		}
		if w, ok := wantPlace[a.Name]; ok && a.Placement != w {
			t.Errorf("action %q placement = %q, want %q", a.Name, a.Placement, w)
		}
	}
	// the entity-writing action's reason should mention durable data; the client one
	// should mention the browser / no round-trip.
	byName := map[string]string{}
	for _, a := range g.Actions {
		byName[a.Name] = a.Reason
	}
	if !strings.Contains(byName["post"], "durable") {
		t.Errorf("post reason should mention durable data, got %q", byName["post"])
	}
	if !strings.Contains(byName["addBonus"], "browser") {
		t.Errorf("addBonus reason should mention the browser, got %q", byName["addBonus"])
	}
}
