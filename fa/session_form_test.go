package fa

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestSessionRoundTripAndTamperRejected(t *testing.T) {
	sm := NewSessions([]byte("0123456789abcdef0123456789abcdef"), SessionInsecure())

	rec := httptest.NewRecorder()
	sm.Save(rec, map[string]string{"uid": "ada", "role": "admin"})
	cookie := rec.Result().Cookies()[0]

	// Round-trip: a valid cookie loads its values.
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	if got := sm.Identity(r); got != "ada" {
		t.Errorf("Identity = %q, want ada", got)
	}
	if got := sm.Get(r, "role"); got != "admin" {
		t.Errorf("role = %q, want admin", got)
	}

	// Tamper: corrupt the payload (first byte) so the signature no longer matches
	// → the session must be rejected as if absent.
	bad := *cookie
	first := byte('A')
	if cookie.Value[0] == 'A' {
		first = 'B'
	}
	bad.Value = string(first) + cookie.Value[1:]
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.AddCookie(&bad)
	if got := sm.Identity(r2); got != "" {
		t.Errorf("tampered session should be empty, got %q", got)
	}

	// HttpOnly + SameSite are set (defense in depth).
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Error("session cookie missing HttpOnly/SameSite hardening")
	}
}

func TestSessionClear(t *testing.T) {
	sm := NewSessions([]byte("0123456789abcdef"), SessionInsecure())
	rec := httptest.NewRecorder()
	sm.Clear(rec)
	c := rec.Result().Cookies()[0]
	if c.MaxAge >= 0 {
		t.Errorf("Clear should expire the cookie, MaxAge=%d", c.MaxAge)
	}
}

func TestSessionAsAppIdentify(t *testing.T) {
	app := New([]byte(`{}`), WithSigningKey([]byte("0123456789abcdef")))
	sm := app.Sessions(SessionInsecure())
	app.Identify(sm.Identity) // wires login → SSE delivery identity

	rec := httptest.NewRecorder()
	sm.Save(rec, map[string]string{"uid": "user-42"})
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(rec.Result().Cookies()[0])
	if id := app.identityOf(r); id != "user-42" {
		t.Errorf("app identity from session = %q, want user-42", id)
	}
}

func postForm(values url.Values) *http.Request {
	r := httptest.NewRequest("POST", "/submit", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestFormValidationAccumulatesErrors(t *testing.T) {
	f := NewForm(postForm(url.Values{
		"email":    {"not-an-email"},
		"password": {"short"},
		"bio":      {"hi"},
	}))
	f.Required("email", "Email required").Email("email", "Bad email")
	f.Required("password", "Password required").MinLen("password", 8, "Min 8 chars")
	f.Required("name", "Name required") // missing entirely

	if f.Valid() {
		t.Fatal("form should be invalid")
	}
	if f.Error("email") != "Bad email" {
		t.Errorf("email error = %q", f.Error("email"))
	}
	if f.Error("password") != "Min 8 chars" {
		t.Errorf("password error = %q", f.Error("password"))
	}
	if f.Error("name") != "Name required" {
		t.Errorf("name error = %q", f.Error("name"))
	}
	// A passing field has no error.
	if f.Error("bio") != "" {
		t.Errorf("bio should pass, got %q", f.Error("bio"))
	}
}

func TestFormValidPath(t *testing.T) {
	f := NewForm(postForm(url.Values{
		"email":    {"ada@example.com"},
		"password": {"longenough"},
		"confirm":  {"longenough"},
	}))
	f.Required("email", "x").Email("email", "x")
	f.Required("password", "x").MinLen("password", 8, "x")
	f.Confirm("password", "confirm", "Passwords must match")
	f.Matches("email", regexp.MustCompile(`example\.com$`), "x")
	if !f.Valid() {
		t.Errorf("form should be valid, errors: %v", f.Errors)
	}
}

func TestFormFromPayload(t *testing.T) {
	// A handler validating an inline-edited field from an event payload.
	f := FormFromPayload(map[string]string{"display_name": ""})
	f.Required("display_name", "Display name is required")
	if f.Valid() {
		t.Error("empty payload field should fail Required")
	}
}
