package integration

import (
	"strings"
	"testing"
)

// The live reference app: every `live/` facet compiled together and rendered on
// both sides — the browse grid, the theater page with its chat and tips, the
// guest gate on chat, and the creator console behind `requires member`.
func TestLiveAppRendersOnBothSides(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "live.fct")

	for _, step := range []struct {
		name string
		args []any
	}{
		{"signup", []any{"ada", "pw12345678"}},
		{"createChannel", []any{"Building live", "Software"}},
		{"mintKey", []any{1}},
		{"setSource", []any{1, "/m/stream.m3u8", "/m/thumb.jpg", false}},
		{"goLive", []any{1}},
		{"heartbeat", []any{1, 1834}},
		{"pin", []any{1, "Be kind."}},
		{"say", []any{1, "hello chat #facet"}},
		{"tip", []any{1, 500, "great stream"}},
	} {
		if code, body := a.action(step.name, step.args...); code != 200 {
			t.Fatalf("%s: %d %s", step.name, code, body)
		}
	}

	// The creator sees the console with the masked key; a guest is turned away.
	code, dash := a.get("/dashboard")
	if code != 200 || !strings.Contains(mountedMarkup(dash), `class="fa-box x-console"`) || !strings.Contains(serverText(dash), "live") {
		t.Errorf("the creator console did not render for its owner (%d): %q", code, serverText(dash))
	}
	if strings.Contains(serverText(dash), "live_ada_") {
		t.Errorf("the stream key must be masked until revealed: %q", serverText(dash))
	}
	if !strings.Contains(serverText(dash), "live_") {
		t.Errorf("the masked key prefix should show: %q", serverText(dash))
	}

	a.newSession() // a guest from here on
	if code, _ := a.get("/dashboard"); code == 200 {
		t.Errorf("the console is `requires member`; a guest got %d", code)
	}

	code, browse := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	// The key is its own entity, ranged only by its owner — so it is nowhere in
	// a guest's page: not the markup, not the bootstrap, not a row payload.
	if strings.Contains(browse, "live_ada_") {
		t.Errorf("a guest's browse page carries a stream key")
	}
	if m := mountedMarkup(browse); !strings.Contains(m, `class="fa-box x-streamcard"`) || !strings.Contains(m, "1.8K watching") || !strings.Contains(m, `href="/live/1"`) || !strings.Contains(m, `class="x-grid"`) {
		t.Errorf("the browse grid is missing the live channel card: %q", serverText(browse))
	}

	code, page := a.get("/live/1")
	if code != 200 {
		t.Fatalf("GET /live/1: %d", code)
	}
	if strings.Contains(page, "live_ada_") {
		t.Errorf("a guest's theater page carries a stream key")
	}
	if strings.Contains(serverText(page), "Maturecontent") {
		t.Errorf("a channel that is not mature must not be gated")
	}
	markup := mountedMarkup(page)
	for _, want := range []string{
		`class="fa-box x-media x-aspect x-aspect-16x9 x-stage"`, ` autoplay`, `class="fa-row x-streaminfo"`, `class="fa-box x-goal"`,
		`class="fa-box x-chat"`, `class="fa-row x-chat-pinned"`, `class="fa-row x-chatline"`, `class="fa-box x-tips"`,
		`href="/browse/Software"`, `href="/tag/facet"`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("the theater page is missing %s", want)
		}
	}
	text := serverText(page)
	for _, want := range []string{"Bekind.", "hellochat", "tipped5.00", "Logintochat", "1.8Kwatching", "Tipgoal"} {
		if !strings.Contains(text, want) {
			t.Errorf("the theater page is missing %q in %q", want, text)
		}
	}
	if strings.Contains(markup, `data-fa-action="deleteChat"`) {
		t.Errorf("a guest must not see the mod tools")
	}

	run, _ := runClientAgainst(t, a, page, nil)
	for _, want := range [][2]string{
		{"class", "fa-box x-media x-aspect x-aspect-16x9 x-stage"}, {"class", "fa-box x-chat"}, {"class", "fa-row x-chatline"},
		{"class", "fa-box x-goal"}, {"href", "/browse/Software"},
	} {
		if !hasAttr(run.Attrs, want[0], want[1]) && !hasHref(run.Hrefs, want[1]) {
			t.Errorf("the client's render lost %s=%q", want[0], want[1])
		}
	}
	for _, want := range []string{"hellochat", "tipped5.00", "Logintochat"} {
		if !strings.Contains(run.Text, want) {
			t.Errorf("the client's render is missing %q: %q", want, run.Text)
		}
	}
}

// A mature channel is gated for a fresh visitor, and the gate hides the player.
func TestMatureChannelIsGated(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "live.fct")
	for _, step := range []struct {
		name string
		args []any
	}{
		{"signup", []any{"ada", "pw12345678"}},
		{"createChannel", []any{"After dark", "Just Chatting"}},
		{"setSource", []any{1, "/m/s.m3u8", "/m/t.jpg", true}},
		{"goLive", []any{1}},
	} {
		if code, body := a.action(step.name, step.args...); code != 200 {
			t.Fatalf("%s: %d %s", step.name, code, body)
		}
	}
	a.newSession()
	code, page := a.get("/live/1")
	if code != 200 {
		t.Fatalf("GET /live/1: %d", code)
	}
	if !strings.Contains(serverText(page), "Maturecontent") || strings.Contains(mountedMarkup(page), "x-stage") {
		t.Errorf("a mature channel should show the gate and not the player: %q", serverText(page))
	}
	// Confirming (a client action on a @client cell) reveals the theater.
	_, after := runClientAgainst(t, a, page, []driveStep{{Sel: `[data-fa-action="confirmAge"]`, Do: "click"}})
	if !hasAttr(after.Attrs, "class", "fa-box x-media x-aspect x-aspect-16x9 x-stage") {
		t.Errorf("after confirming, the player should render: %v", after.Attrs)
	}
}

func hasHref(hrefs []string, want string) bool {
	for _, h := range hrefs {
		if h == want {
			return true
		}
	}
	return false
}
