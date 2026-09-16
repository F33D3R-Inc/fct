package integration

import (
	"strings"
	"testing"
	"time"
)

// The messaging reference app: a thread between two people, unread counts and
// read receipts from the read markers on the thread row, a stranger kept out,
// and the friend-request flow — server render and client render both.
func TestMessagesAppRendersOnBothSides(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "messages.fct")
	act := func(name string, args ...any) {
		t.Helper()
		if code, body := a.action(name, args...); code != 200 {
			t.Fatalf("%s: %d %s", name, code, body)
		}
	}

	// ada starts a thread with grace and says two things.
	act("signup", "ada", "pw12345678")
	act("startChat", "grace")
	act("send", 1, "hi grace")
	act("send", 1, "are you there? #facet")
	if code, body := a.action("startChat", "ada"); code == 200 {
		t.Errorf("messaging yourself should be refused, got %d %s", code, body)
	}

	code, inbox := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	m := mountedMarkup(inbox)
	if !strings.Contains(m, `class="fa-row x-inbox-item"`) || !strings.Contains(m, `href="/dm/1"`) || !strings.Contains(serverText(inbox), "areyouthere?") {
		t.Errorf("ada's inbox should list the thread with its last message: %q", serverText(inbox))
	}
	if strings.Contains(m, `class="fa-badge"`) {
		t.Errorf("ada wrote the messages; nothing is unread for her")
	}
	code, thread := a.get("/dm/1")
	if code != 200 {
		t.Fatalf("GET /dm/1: %d", code)
	}
	tm := mountedMarkup(thread)
	if strings.Count(tm, `class="fa-row x-msg x-mine-true"`) != 2 || strings.Contains(tm, "x-mine-false") {
		t.Errorf("both bubbles are ada's own: %s", tm)
	}
	if strings.Contains(serverText(thread), "Seen") {
		t.Errorf("grace has not read anything yet; no receipt should show")
	}

	// grace: two unread, then reads, then replies — and ada's bubbles get their receipt.
	a.newSession()
	act("signup", "grace", "pw12345678")
	code, inbox = a.get("/")
	if code != 200 {
		t.Fatalf("GET / as grace: %d", code)
	}
	if !strings.Contains(mountedMarkup(inbox), `class="fa-badge">2</span>`) {
		t.Errorf("grace should see 2 unread: %s", mountedMarkup(inbox))
	}
	act("markRead", 1)
	code, inbox = a.get("/")
	if strings.Contains(mountedMarkup(inbox), `class="fa-badge"`) {
		t.Errorf("after marking read, no unread pill")
	}
	// Timestamps are unix seconds and a read marker at second T covers messages
	// sent at T, so the reply has to land in a later second than ada's marker.
	time.Sleep(1100 * time.Millisecond)
	act("send", 1, "here!")
	code, thread = a.get("/dm/1")
	tm = mountedMarkup(thread)
	if strings.Count(tm, "x-mine-false") != 2 || strings.Count(tm, "x-mine-true") != 1 {
		t.Errorf("grace sees ada's two and her own one: %s", tm)
	}
	run, _ := runClientAgainst(t, a, thread, nil)
	for _, want := range []string{"higrace", "here!", "areyouthere?"} {
		if !strings.Contains(run.Text, want) {
			t.Errorf("the client's thread render is missing %q: %q", want, run.Text)
		}
	}
	if !hasAttr(run.Attrs, "class", "fa-row x-msg x-mine-true") {
		t.Errorf("the client lost the mine/theirs class: %v", run.Attrs)
	}

	// ada is back: her messages now carry "Seen", and grace's reply is unread.
	a.newSession()
	act("login", "ada", "pw12345678")
	code, thread = a.get("/dm/1")
	if strings.Count(serverText(thread), "Seen") != 2 {
		t.Errorf("both of ada's messages were read by grace: %q", serverText(thread))
	}
	code, inbox = a.get("/")
	if !strings.Contains(mountedMarkup(inbox), `class="fa-badge">1</span>`) {
		t.Errorf("ada has one unread reply: %s", mountedMarkup(inbox))
	}

	// A stranger sees neither the messages nor the composer.
	a.newSession()
	act("signup", "mallory", "pw12345678")
	code, thread = a.get("/dm/1")
	if code != 200 {
		t.Fatalf("GET /dm/1 as a stranger: %d", code)
	}
	if strings.Contains(thread, "hi grace") || strings.Contains(mountedMarkup(thread), "x-composer") {
		t.Errorf("a stranger must not see a thread's messages or composer")
	}
	if code, _ := a.action("send", 1, "let me in"); code == 200 {
		t.Errorf("a stranger's send must be refused")
	}

	// Friend requests: grace asks, ada accepts, grace shows up under Friends.
	act("request", "ada")
	a.newSession()
	act("login", "ada", "pw12345678")
	code, inbox = a.get("/")
	if !strings.Contains(mountedMarkup(inbox), `data-fa-action="accept"`) || !strings.Contains(serverText(inbox), "mallory") {
		t.Errorf("ada should see mallory's request with an Accept button: %q", serverText(inbox))
	}
	act("accept", 1)
	code, people := a.get("/people")
	if code != 200 {
		t.Fatalf("GET /people: %d", code)
	}
	if !strings.Contains(mountedMarkup(people), `class="fa-row x-contact"`) || !strings.Contains(mountedMarkup(people), `data-fa-action="startChat"`) {
		t.Errorf("an accepted friend shows as a contact with a Message button: %q", serverText(people))
	}
}
