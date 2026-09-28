package integration

import (
	"strings"
	"testing"
	"time"
)

// Messaging on the site, over the product's own rows: a direct thread between
// two people, unread counts on the member rows and "Seen" receipts from each
// member's read marker, a stranger kept out, and the contact-request flow a
// contact policy turns a first message into — server render and client render.
func TestMessagesAppRendersOnBothSides(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "f33d3r_com.fct")
	act := func(name string, args ...any) {
		t.Helper()
		if code, body := a.action(name, args...); code != 200 {
			t.Fatalf("%s: %d %s", name, code, body)
		}
	}
	page := func(path string) string {
		t.Helper()
		code, body := a.get(path)
		if code != 200 {
			t.Fatalf("GET %s: %d", path, code)
		}
		return body
	}

	// grace exists; ada starts a thread with her and says two things.
	act("webSignup", "grace", "pw12345678")
	a.newSession()
	act("webSignup", "ada", "pw12345678")
	act("webMessage", "grace", "hi grace")
	act("sendMessage", 1, "are you there? #facet")
	if code, body := a.action("webMessage", "ada", "me again"); code == 200 {
		t.Errorf("messaging yourself should be refused, got %d %s", code, body)
	}

	inbox := page("/messages")
	m := mountedMarkup(inbox)
	if !strings.Contains(m, `class="fa-row x-inbox-item"`) || !strings.Contains(m, `href="/dm/1"`) || !strings.Contains(serverText(inbox), "areyouthere?") {
		t.Errorf("ada's inbox should list the thread with its last message: %q", serverText(inbox))
	}
	if strings.Contains(m, `class="fa-badge"`) {
		t.Errorf("ada wrote the messages; nothing is unread for her")
	}
	tm := mountedMarkup(page("/dm/1"))
	if strings.Count(tm, `class="fa-row x-msg x-mine-true"`) != 2 || strings.Contains(tm, "x-mine-false") {
		t.Errorf("both bubbles are ada's own: %s", tm)
	}
	if strings.Contains(serverText(page("/dm/1")), "Seen") {
		t.Errorf("grace has not read anything yet; no receipt should show")
	}

	// grace: two unread, then reads, then replies — and ada's bubbles get their receipt.
	a.newSession()
	act("webLogin", "grace", "pw12345678")
	if m := mountedMarkup(page("/messages")); !strings.Contains(m, `class="fa-badge">2</span>`) {
		t.Errorf("grace should see 2 unread: %s", m)
	}
	act("markRead", 1)
	if strings.Contains(mountedMarkup(page("/messages")), `class="fa-badge"`) {
		t.Errorf("after marking read, no unread pill")
	}
	// Timestamps are unix seconds and a read marker at second T covers messages
	// sent at T, so the reply has to land in a later second than grace's marker.
	time.Sleep(1100 * time.Millisecond)
	act("sendMessage", 1, "here!")
	thread := page("/dm/1")
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
	act("webLogin", "ada", "pw12345678")
	if st := serverText(page("/dm/1")); strings.Count(st, "Seen") != 2 {
		t.Errorf("both of ada's messages were read by grace: %q", st)
	}
	if m := mountedMarkup(page("/messages")); !strings.Contains(m, `class="fa-badge">1</span>`) {
		t.Errorf("ada has one unread reply: %s", m)
	}
	// ada now takes first messages only from people she follows.
	act("setContactPolicy", "followers")

	// A stranger sees neither the messages nor the composer.
	a.newSession()
	act("webSignup", "mallory", "pw12345678")
	thread = page("/dm/1")
	if strings.Contains(thread, "hi grace") || strings.Contains(mountedMarkup(thread), "x-composer") {
		t.Errorf("a stranger must not see a thread's messages or composer")
	}
	if code, _ := a.action("sendMessage", 1, "let me in"); code == 200 {
		t.Errorf("a stranger's send must be refused")
	}

	// Contact requests: mallory's first message to ada is held as a request,
	// ada accepts, and mallory shows up among her contacts.
	act("webMessage", "ada", "can we talk?")
	a.newSession()
	act("webLogin", "ada", "pw12345678")
	people := page("/people")
	if !strings.Contains(mountedMarkup(people), `data-fa-action="decideContactRequest"`) || !strings.Contains(serverText(people), "@mallory") || !strings.Contains(serverText(people), "canwetalk?") {
		t.Errorf("ada should see mallory's request with an Accept button: %q", serverText(people))
	}
	act("decideContactRequest", 1, "accept")
	act("follow", "grace")
	people = page("/people")
	pm := mountedMarkup(people)
	if strings.Contains(pm, "x-request") {
		t.Errorf("an accepted request leaves the list: %s", pm)
	}
	if !strings.Contains(pm, `class="fa-row x-contact"`) || !strings.Contains(pm, `data-fa-form="webMessage"`) || !strings.Contains(serverText(people), "@mallory") {
		t.Errorf("an accepted contact shows with a way to message them: %q", serverText(people))
	}
	// grace, whom ada follows and already wrote to, is reached through the open thread.
	if !strings.Contains(pm, `href="/dm/1"`) {
		t.Errorf("a contact with an open thread links to it: %s", pm)
	}
}
