package integration

import (
	"strings"
	"testing"
)

// Rotation must preserve the session, not just replace its name.
//
// The integration assertion that the identifier changed is satisfied just as
// well by a rotation that loses the session — and that would sign the user out
// the instant they signed in. This checks the thing the user would notice.
const whoamiApp = `app WhoAmI:
    auth
    entity Note:
        id: int
        body: text
    action write(body: text):
        add Note { body: body }
    view Home at "/":
        box:
            text "signed in as {actor}"
`

func TestRotationKeepsTheUserSignedIn(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, whoamiApp)

	// Compared against the page's text, not its markup: an interpolated value
	// renders inside a bound <span>, so the sentence is never contiguous in the
	// HTML even when it is exactly what the page says.
	if _, html := a.get("/"); !strings.Contains(serverText(html), "signedinasguest") {
		t.Fatal("a fresh visitor should be a guest")
	}

	before := a.sessionCookie()

	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}

	if a.sessionCookie() == before {
		t.Fatal("the identifier did not rotate")
	}

	// The point: the NEW identifier is the signed-in one.
	if _, html := a.get("/"); !strings.Contains(serverText(html), "signedinasalice") {
		t.Error("the rotated session lost its identity — signing in signed the user out")
	}

	// A write must still be authorized under the rotated session.
	if code, body := a.action("write", "still me"); code != 200 {
		t.Errorf("writing after rotation: %d %s", code, body)
	}

	// And logout must genuinely end it.
	if code, body := a.action("logout"); code != 200 {
		t.Fatalf("logout: %d %s", code, body)
	}
	if _, html := a.get("/"); !strings.Contains(serverText(html), "signedinasguest") {
		t.Error("the user is still signed in after logging out")
	}
}
