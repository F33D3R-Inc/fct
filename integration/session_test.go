package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// sessionCookie returns the raw fa_sid the app has given this client, or "".
func (a *app) sessionCookie() string {
	u, err := url.Parse(a.url("/"))
	if err != nil {
		a.t.Fatalf("parsing the app url: %v", err)
	}

	for _, c := range a.jar.Cookies(u) {
		if c.Name == "fa_sid" {
			return c.Value
		}
	}

	return ""
}

// The session identifier must change when the session's privilege changes.
//
// This is session fixation, and it is the reason the rule exists: an attacker
// obtains a perfectly ordinary session from the app (just by visiting it), gets
// the victim's browser to adopt that identifier — a sibling subdomain writing the
// cookie, a MITM on a plaintext hop, an XSS — and then waits. The victim logs in,
// the server stamps the identity onto the identifier the attacker already holds,
// and the attacker is now signed in as the victim without ever seeing a password.
//
// The cookie being signed does not help: the attacker's session is one the server
// legitimately issued and legitimately signed. What defeats the attack is issuing
// a NEW identifier at the moment the privilege changes, so the one the attacker
// planted is authenticated to nobody.
//
// The same applies to logout, in the other direction: a logout that leaves the
// identifier valid means anyone who captured it still holds a usable session.
func TestTheSessionIdentifierChangesWithPrivilege(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, notesApp)

	// An ordinary anonymous visit — this is the identifier an attacker would
	// obtain and plant.
	if code, _ := a.get("/"); code != 200 {
		t.Fatal("the app did not serve an anonymous visitor")
	}

	before := a.sessionCookie()
	if before == "" {
		t.Fatal("no session cookie was issued to an anonymous visitor")
	}

	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}

	afterSignup := a.sessionCookie()
	if afterSignup == before {
		t.Errorf("the session identifier did not change when the visitor became\n"+
			"a signed-in user — this is session fixation.\n"+
			"\n"+
			"signIn() (runtime/auth.go) stamps the identity onto the session id\n"+
			"that already existed; nothing in the tree rotates it. An identifier\n"+
			"planted before login is authenticated after it.\n"+
			"\n"+
			"cookie before and after: %s", before)
	}

	// And again across a logout/login cycle.
	beforeLogout := a.sessionCookie()
	if code, body := a.action("logout"); code != 200 {
		t.Fatalf("logout: %d %s", code, body)
	}
	if a.sessionCookie() == beforeLogout {
		t.Error("the session identifier survived a logout — an identifier captured\n" +
			"while signed in is still a usable session afterwards")
	}
}

// A session identifier must not be guessable.
//
// These are minted from a process counter (`s1`, `s2`, …), so the id of every
// session the server has ever issued is known by construction and the count is
// public information. The cookie's signature is what currently stands between a
// guess and a hijack, which makes one secret the whole defence.
//
// It also matters where the raw identifier travels: under clustering the sid is
// the address of a `__session` node in the shared database, so anything that can
// read that kind can enumerate `s1..sN` — the signature never enters that path.
// A CSPRNG identifier costs nothing and removes the entire class.
func TestSessionIdentifiersAreNotSequential(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, notesApp)

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		a.newSession()
		if code, _ := a.get("/"); code != 200 {
			t.Fatalf("visit %d: not served", i)
		}

		raw := a.sessionCookie()
		if raw == "" {
			t.Fatalf("visit %d: no session cookie", i)
		}

		// The cookie is "sid.signature"; the identifier is what precedes the dot.
		sid := raw
		if unescaped, err := url.QueryUnescape(raw); err == nil {
			sid = unescaped
		}
		if i := strings.LastIndex(sid, "."); i > 0 {
			sid = sid[:i]
		}

		seen[sid] = true

		// A counter is the failure this looks for: `s1`, `s2`, `s-<instance>-3`.
		trimmed := strings.TrimPrefix(sid, "s")
		if idx := strings.LastIndex(trimmed, "-"); idx >= 0 {
			trimmed = trimmed[idx+1:]
		}
		if isAllDigits(trimmed) && len(trimmed) < 12 {
			t.Errorf("session id %q is a small integer — identifiers are minted "+
				"from a counter, so every id the server has issued is known by "+
				"construction and only the cookie signature prevents a hijack", sid)
			return
		}
	}

	if len(seen) != 5 {
		t.Errorf("5 fresh visitors produced %d distinct session ids", len(seen))
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var _ = http.StatusOK
