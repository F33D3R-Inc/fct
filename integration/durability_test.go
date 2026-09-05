package integration

import (
	"strings"
	"testing"
)

// The smallest app that still exercises the whole path: an entity, a write
// action, and a view that reads it back.
const notesApp = `app Notes:
    auth
    entity Note:
        id: int
        body: text
        created: int
    action write(body: text):
        add Note { body: body, created: now() }
    view Home at "/":
        box:
            text "notes: {count(Note)}"
            for n in Note by created desc limit 20:
                text "{n.body}"
`

// A write must survive the loss of everything above the disk.
//
// This is the claim the whole project rests on — that FacetQL is a database and
// not a cache — and it is not provable by any test that stays inside one
// process. So: write through the running app, kill the app, kill the engine,
// start both again against the same directory, and read it back.
func TestAWriteSurvivesRestartingBothProcesses(t *testing.T) {
	e := startEngine(t)

	a := startApp(t, e, notesApp)
	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("write", "durable across a restart"); code != 200 {
		t.Fatalf("write: %d %s", code, body)
	}

	if code, html := a.get("/"); code != 200 || !strings.Contains(html, "durable across a restart") {
		t.Fatalf("the note is not on the page it was just written to: %d", code)
	}

	// Everything in memory goes. The data directory is all that survives.
	e.stop()

	revived := startEngineIn(t, e.dir)
	b := startApp(t, revived, notesApp)

	code, html := b.get("/")
	if code != 200 {
		t.Fatalf("GET / after restart: %d", code)
	}
	if !strings.Contains(html, "durable across a restart") {
		t.Error("the note did not survive restarting the engine and the app")
	}
}

// A page must not carry rows the request did not ask for.
//
// The number here is the whole point: a view that renders 20 rows should send
// something like 20 rows, and the failure mode this guards against is sending
// every row in the table. That shipped — 50 000 rows and 8.1 MB of HTML to
// render 20 — and it is invisible to any test that only checks the page is
// correct, because the page was correct. It was just enormous.
func TestAPageDoesNotShipTheWholeTable(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, notesApp)

	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}

	const rows = 500
	for i := 0; i < rows; i++ {
		if code, body := a.action("write", "note"); code != 200 {
			t.Fatalf("write %d: %d %s", i, code, body)
		}
	}

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	// The view's own `limit 20` is the contract. A generous ceiling, because the
	// point is to catch "the entire table" and not to pin an exact payload.
	const ceiling = 100

	state := pageState(t, html)
	if notes, ok := state["Note"].([]any); ok && len(notes) > ceiling {
		t.Errorf("the page carries %d of %d rows for a view that renders 20 —\n"+
			"the client is being sent the table instead of the page.\n"+
			"\n"+
			"THIS IS A KNOWN OPEN BUG, and this test is its acceptance criterion.\n"+
			"scope() copies every entity's full row slice into the render scope and\n"+
			"that scope is serialized into fa-state. Measured at 50k rows: 8.1 MB of\n"+
			"HTML to render 20. See the AGENT_LOG entry \"fct ships the whole database\n"+
			"to every browser\". A fix is in flight; if you are that fix, this test\n"+
			"going green is what done looks like.", len(notes), rows)
	}

	if len(html) > 512*1024 {
		t.Errorf("a page rendering 20 rows is %d bytes; the payload is not "+
			"bounded by the query", len(html))
	}
}
