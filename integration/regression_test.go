package integration

import (
	"strings"
	"testing"
)

// An app whose only use of a free-text field is a substring search — the shape
// that took the site down.
const searchApp = `app Search:
    auth
    state q: text = "" @client
    entity Note:
        id: int
        author: text
        body: text
    action write(author: text, body: text):
        add Note { author: author, body: body }
    view Home at "/":
        box:
            input bind q
            for n in Note where contains(lower(n.body), lower(q)) limit 20:
                text "{n.body}"
            for n in Note where n.author == q limit 20:
                text "{n.author}"
`

// A long value in a free-text field must not stop the app from starting.
//
// The compiler indexes fields a query uses, and it used to mark any field a
// `where` merely *read* — so `contains(lower(n.body), q)` earned `body` an index.
// No ordered index answers a substring search, so it was pure write cost
// everywhere; on FacetQL, whose index keys are bounded, it was fatal. Indexes are
// reconciled at startup, so the first post longer than the bound made the app
// refuse to boot — permanently, from one ordinary long post.
//
// The fix is that a field earns an index by being *compared*, not mentioned.
// `author` is compared and should still be indexed; `body` is only an argument to
// a call and should not be. This test asserts the consequence rather than the
// mechanism: write something long, restart, and still be serving.
func TestALongTextFieldDoesNotPreventStartup(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, searchApp)

	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}

	// Comfortably past FacetQL's 512-byte index-key bound.
	long := strings.Repeat("a very ordinary sentence that goes on. ", 40)

	if code, body := a.action("write", "alice", long); code != 200 {
		t.Fatalf("writing a long note: %d %s", code, body)
	}

	// The restart is the test: Init reconciles the declared indexes, and an index
	// the engine refuses is a startup that never completes.
	b := startApp(t, e, searchApp)

	code, html := b.get("/")
	if code != 200 {
		t.Fatalf("the app did not serve after a restart with a long field: %d", code)
	}
	if !strings.Contains(html, "a very ordinary sentence") {
		t.Error("the long note is not on the page")
	}
}

const countingApp = `app Counting:
    auth
    entity Note:
        id: int
        body: text
    entity Star:
        id: int
        note: int
    action write(body: text):
        add Note { body: body }
    action star(note: int):
        add Star { note: note }
    view Home at "/":
        box:
            for n in Note limit 20:
                text "{n.body} has {count(s in Star where s.note == n.id)} stars"
`

// A number that arrives as text must be the number, not zero.
//
// Every value crossing a boundary into the runtime is text: a route parameter, an
// HTML form field, an `<input>`'s value, a JSON argument. `toInt` had no string
// case, so it answered 0 for all of them — and `star("1")` wrote `note: 0`,
// attaching the row to a note that does not exist, and answered {"ok":true}. The
// request looked successful and the data was wrong, which is the worst pair.
func TestANumberSentAsTextIsNotSilentlyZero(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, countingApp)

	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("write", "the first note"); code != 200 {
		t.Fatalf("write: %d %s", code, body)
	}

	// The string "1", exactly as a route parameter or a form field would arrive.
	if code, body := a.action("star", "1"); code != 200 {
		t.Fatalf("star with a textual id: %d %s", code, body)
	}

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	if !strings.Contains(html, "has 1 stars") {
		t.Errorf("the star did not attach to note 1 — a textual number became 0.\n"+
			"page said: %s", between(html, "the first note", "stars"))
	}
}

// And text that is not a number must be refused, not substituted.
//
// `toInt` is total — it is called from rendering and comparison, which have
// nowhere to report a failure — so it still answers 0 for garbage. The boundary
// that *can* refuse is action-argument coercion, and it must, because
// substituting 0 for something a caller wrote turns a malformed request into a
// successful write of the wrong row.
func TestTextThatIsNotANumberIsRefused(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, countingApp)

	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}

	code, body := a.action("star", "not-a-number")
	if code == 200 {
		t.Errorf("an unparseable int argument was accepted: %s", body)
	}
	if code != 400 {
		t.Errorf("expected 400 for a malformed argument, got %d: %s", code, body)
	}
}

// between returns the text between two markers, for a readable failure message.
func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return "(marker not found)"
	}
	rest := s[i:]
	if j := strings.Index(rest, to); j >= 0 {
		return rest[:j+len(to)]
	}
	return rest[:min(len(rest), 200)]
}
