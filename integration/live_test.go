package integration

import (
	"strings"
	"testing"
)

// Live on the site (f33d3r_com.fct), over the product's LiveStream and
// LiveChatMsg rows: going live from the studio, the browse grid, the theater
// page with its pinned line, chat, tip goal and tips, the guest gate on chat,
// and the console behind `requires member`.
func TestLiveAppRendersOnBothSides(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "f33d3r_com.fct")
	act := func(name string, args ...any) {
		t.Helper()
		if code, body := a.action(name, args...); code != 200 {
			t.Fatalf("%s: %d %s", name, code, body)
		}
	}
	act("webSignup", "ada", "pw12345678")
	act("webGoLive", "Building live", "Software")
	act("pinLiveMessage", 1, "Be kind.")
	act("sendLiveChat", 1, "hello chat #facet")
	code, dash := a.get("/studio")
	if code != 200 || !strings.Contains(mountedMarkup(dash), `class="fa-box x-console"`) || !strings.Contains(serverText(dash), "live") {
		t.Errorf("the console did not render for its owner (%d): %q", code, serverText(dash))
	}

	a.newSession() // a guest from here on
	if code, body := a.get("/studio"); code == 200 && strings.Contains(body, "x-console") {
		t.Errorf("the console is `requires member`; a guest saw it")
	}
	code, browse := a.get("/live")
	if code != 200 {
		t.Fatalf("GET /live: %d", code)
	}
	if m := mountedMarkup(browse); !strings.Contains(m, `class="fa-box x-streamcard"`) || !strings.Contains(m, `href="/live/1"`) || !strings.Contains(m, `class="fa-box x-grid"`) {
		t.Errorf("the browse grid is missing the live room card: %q", serverText(browse))
	}

	code, page := a.get("/live/1")
	if code != 200 {
		t.Fatalf("GET /live/1: %d", code)
	}
	if strings.Contains(serverText(page), "Maturecontent") {
		t.Errorf("a room that is not mature must not be gated")
	}
	markup := mountedMarkup(page)
	for _, want := range []string{
		`class="fa-box x-media x-aspect x-aspect-16x9 x-stage"`, `class="fa-row x-streaminfo"`,
		`class="fa-box x-chat"`, `class="fa-row x-chat-pinned"`, `class="fa-row x-chatline"`, `class="fa-box x-tips"`,
		`href="/browse/Software"`, `href="/tag/facet"`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("the theater page is missing %s", want)
		}
	}
	text := serverText(page)
	for _, want := range []string{"Bekind.", "hellochat", "Logintochat.", "0watching"} {
		if !strings.Contains(text, want) {
			t.Errorf("the theater page is missing %q in %q", want, text)
		}
	}
	if strings.Contains(markup, `data-fa-action="releaseLiveChat"`) {
		t.Errorf("a guest must not see the host's tools")
	}

	run, _ := runClientAgainst(t, a, page, nil)
	for _, want := range [][2]string{
		{"class", "fa-box x-media x-aspect x-aspect-16x9 x-stage"}, {"class", "fa-box x-chat"}, {"class", "fa-row x-chatline"},
		{"href", "/browse/Software"},
	} {
		if !hasAttr(run.Attrs, want[0], want[1]) && !hasHref(run.Hrefs, want[1]) {
			t.Errorf("the client's render lost %s=%q", want[0], want[1])
		}
	}
	for _, want := range []string{"hellochat", "Logintochat."} {
		if !strings.Contains(run.Text, want) {
			t.Errorf("the client's render is missing %q: %q", want, run.Text)
		}
	}
}

// A tip into a live room moves AET on the ledger, raises the room's goal
// total and lands in its chat as a tip line.
func TestLiveTipLandsInTheRoom(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "f33d3r_com.fct")
	for _, step := range []struct {
		name string
		args []any
	}{
		{"webSignup", []any{"ada", "pw12345678"}},
		{"webGoLive", []any{"Tips welcome", "Music"}},
	} {
		if code, body := a.action(step.name, step.args...); code != 200 {
			t.Fatalf("%s: %d %s", step.name, code, body)
		}
	}
	a.newSession()
	if code, body := a.action("webSignup", "bob", "pw12345678"); code != 200 {
		t.Fatalf("signup bob: %d %s", code, body)
	}
	// An unfunded tip is refused and nothing lands.
	if code, _ := a.action("webTipStream", 1, "1", "great stream"); code == 200 {
		t.Errorf("an unfunded tip must be refused")
	}
	if _, page := a.get("/live/1"); strings.Contains(serverText(page), "tipped") {
		t.Errorf("a refused tip must not reach the chat")
	}
}

// A mature room is gated for a fresh visitor, and the gate hides the stage.
func TestMatureChannelIsGated(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "f33d3r_com.fct")
	for _, step := range []struct {
		name string
		args []any
	}{
		{"webSignup", []any{"ada", "pw12345678"}},
		{"webGoLive", []any{"After dark", "Just Chatting", true}},
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
		t.Errorf("a mature room should show the gate and not the stage: %q", serverText(page))
	}
	// Confirming is remembered in the visitor's session: the next render of
	// the room shows the stage, on the server and the client.
	if code, body := a.action("confirmAge"); code != 200 {
		t.Fatalf("confirmAge: %d %s", code, body)
	}
	_, page = a.get("/live/1")
	if strings.Contains(serverText(page), "Maturecontent") || !strings.Contains(mountedMarkup(page), "x-stage") {
		t.Errorf("after confirming, the stage should render: %q", serverText(page))
	}
	run, _ := runClientAgainst(t, a, page, nil)
	if !hasAttr(run.Attrs, "class", "fa-box x-media x-aspect x-aspect-16x9 x-stage") {
		t.Errorf("the client's render of the confirmed room lost the stage")
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
