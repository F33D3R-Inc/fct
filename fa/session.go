package fa

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// SessionManager issues and reads signed-cookie sessions. The session is a small
// string map serialized into a cookie and HMAC-signed with the app's signing key,
// so a client can read it but cannot forge or tamper with it (any edit fails the
// signature check and the session is treated as absent). Cookies are HttpOnly,
// SameSite=Lax, and Secure by default.
//
// This is the auth backbone: Save() after a successful login, Identity() as the
// App.Identify resolver, Clear() on logout. FA does not pick a password store or
// OAuth provider for you — you verify credentials however you like, then record
// the resulting user id in the session.
type SessionManager struct {
	key      []byte
	name     string
	maxAge   time.Duration
	secure   bool
	sameSite http.SameSite
}

// SessionOption configures a SessionManager.
type SessionOption func(*SessionManager)

// SessionName sets the cookie name (default "fa_session").
func SessionName(name string) SessionOption { return func(s *SessionManager) { s.name = name } }

// SessionMaxAge sets how long a session stays valid (default 30 days).
func SessionMaxAge(d time.Duration) SessionOption {
	return func(s *SessionManager) { s.maxAge = d }
}

// SessionInsecure allows the cookie over plain HTTP (for local dev only). In
// production leave this off so the cookie is Secure.
func SessionInsecure() SessionOption { return func(s *SessionManager) { s.secure = false } }

// NewSessions creates a SessionManager signing with key (use the same key as the
// App so there is one secret). Prefer App.Sessions, which wires the app key.
func NewSessions(key []byte, opts ...SessionOption) *SessionManager {
	s := &SessionManager{key: key, name: "fa_session", maxAge: 30 * 24 * time.Hour, secure: true, sameSite: http.SameSiteLaxMode}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Sessions returns a SessionManager bound to this app's signing key.
func (a *App) Sessions(opts ...SessionOption) *SessionManager {
	return NewSessions(a.key, opts...)
}

// Save writes the values into a signed session cookie.
func (s *SessionManager) Save(w http.ResponseWriter, values map[string]string) {
	body, _ := json.Marshal(values)
	payload := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	value := payload + "." + hex.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{
		Name: s.name, Value: value, Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: s.sameSite,
		Expires: time.Now().Add(s.maxAge), MaxAge: int(s.maxAge.Seconds()),
	})
}

// Load reads and verifies the session, returning its values. A missing, malformed
// or tampered cookie yields an empty (non-nil) map.
func (s *SessionManager) Load(r *http.Request) map[string]string {
	out := map[string]string{}
	c, err := r.Cookie(s.name)
	if err != nil {
		return out
	}
	payload, sig, ok := cut(c.Value, '.')
	if !ok {
		return out
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want) {
		return out // forged or tampered → treat as no session
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(body, &out)
	if out == nil {
		out = map[string]string{}
	}
	return out
}

// Get returns one session value (e.g. the user id), or "".
func (s *SessionManager) Get(r *http.Request, key string) string {
	return s.Load(r)[key]
}

// Identity is an App.Identify resolver: it returns the "uid" session value, so a
// logged-in user is identified across tabs and the SSE delivery scopes (EmitTo)
// address them. Use: app.Identify(sessions.Identity).
func (s *SessionManager) Identity(r *http.Request) string {
	return s.Load(r)["uid"]
}

// Clear deletes the session cookie (logout).
func (s *SessionManager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.name, Value: "", Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: s.sameSite,
		Expires: time.Unix(0, 0), MaxAge: -1,
	})
}

// cut splits s on the first sep byte.
func cut(s string, sep byte) (before, after string, found bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
