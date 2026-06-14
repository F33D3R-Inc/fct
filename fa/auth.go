package fa

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth is FA's batteries-included authentication: password hashing, an account
// store, sessions, and a ready-to-mount login flow — wired so a logged-in user
// IS the FA identity that scopes SSE delivery (EmitTo), event guards (App.Guard),
// and who: policies (RenderFor). It is the built-in alternative to hand-rolling
// a login handler:
//
//	auth := app.Auth(nil)                 // nil store → in-memory (dev only)
//	app.Identify(auth.Identity)           // session uid → FA identity
//	auth.Signup(ctx, "ada", "s3cret…")    // create the first user (e.g. on boot)
//	auth.MountLogin(app, mux, fa.LoginOptions{})   // GET/POST /login, GET /logout
//
// To gate the admin panel (Django-style), pass an authorizer:
//
//	adm.Authorize(auth.Authorize("admin"))
//
// Bring a real database in production by implementing AuthStore; MemoryStore is a
// development default that does not persist across restarts.
type Auth struct {
	store    AuthStore
	sessions *SessionManager
	minPass  int
}

// Auth creates an authenticator bound to this app's session (same signing key).
// A nil store uses an in-memory store — fine for development, but it forgets
// every account on restart, so supply your own AuthStore in production.
func (a *App) Auth(store AuthStore, opts ...SessionOption) *Auth {
	if store == nil {
		store = NewMemoryStore()
	}
	return &Auth{store: store, sessions: a.Sessions(opts...), minPass: 8}
}

// Store exposes the underlying account store.
func (au *Auth) Store() AuthStore { return au.store }

// MinPasswordLength sets the minimum accepted password length (default 8).
func (au *Auth) MinPasswordLength(n int) *Auth { au.minPass = n; return au }

var (
	// ErrLoginTaken is returned by Signup when the login already exists.
	ErrLoginTaken = errors.New("fa: login already taken")
	// ErrBadCredentials is returned by Authenticate/Login for an unknown login OR
	// a wrong password — deliberately the same error, so callers cannot tell which
	// (no account enumeration).
	ErrBadCredentials = errors.New("fa: invalid login or password")
	// ErrWeakPassword is returned by Signup when the password is too short.
	ErrWeakPassword = errors.New("fa: password too short")
	// ErrNoLogin is returned by Signup when the login is empty.
	ErrNoLogin = errors.New("fa: login required")
)

// Signup creates an account with a hashed password and returns it. Roles are
// optional (e.g. "admin" for the first superuser). The login is unique.
func (au *Auth) Signup(ctx context.Context, login, password string, roles ...string) (*Account, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return nil, ErrNoLogin
	}
	if len(password) < au.minPass {
		return nil, ErrWeakPassword
	}
	existing, err := au.store.ByLogin(ctx, login)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrLoginTaken
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	acc := &Account{ID: newID(), Login: login, Hash: hash, Roles: roles, Created: time.Now().UTC()}
	if err := au.store.Create(ctx, acc); err != nil {
		return nil, err
	}
	return acc, nil
}

// Authenticate verifies credentials WITHOUT touching the session (use Login to
// also set the cookie). It runs the password hash even when the login is unknown,
// so the response time does not reveal whether an account exists.
func (au *Auth) Authenticate(ctx context.Context, login, password string) (*Account, error) {
	acc, err := au.store.ByLogin(ctx, strings.TrimSpace(login))
	if err != nil {
		return nil, err
	}
	if acc == nil {
		VerifyPassword(decoyHash(), password) // equalize timing; result ignored
		return nil, ErrBadCredentials
	}
	if !VerifyPassword(acc.Hash, password) {
		return nil, ErrBadCredentials
	}
	return acc, nil
}

// Login authenticates and, on success, writes the session cookie identifying the
// account. The account's ID becomes the FA identity (see App.Identify).
func (au *Auth) Login(w http.ResponseWriter, r *http.Request, login, password string) (*Account, error) {
	acc, err := au.Authenticate(r.Context(), login, password)
	if err != nil {
		return nil, err
	}
	au.sessions.Save(w, map[string]string{"uid": acc.ID})
	return acc, nil
}

// Logout clears the session cookie.
func (au *Auth) Logout(w http.ResponseWriter) { au.sessions.Clear(w) }

// Identity is an App.Identify resolver: the signed-in account ID, or "" if not
// logged in. Use: app.Identify(auth.Identity).
func (au *Auth) Identity(r *http.Request) string { return au.sessions.Get(r, "uid") }

// Current loads the signed-in account, or nil if not logged in (or the account
// no longer exists).
func (au *Auth) Current(r *http.Request) *Account {
	id := au.Identity(r)
	if id == "" {
		return nil
	}
	acc, _ := au.store.ByID(r.Context(), id)
	return acc
}

// Guard returns an App.Guard predicate admitting only signed-in users — or, when
// a role is given, only users holding it. Use: app.Guard("post.delete", auth.Guard("admin")).
func (au *Auth) Guard(role ...string) func(Ctx) bool {
	return func(c Ctx) bool {
		if c.Identity == "" {
			return false
		}
		if len(role) == 0 || role[0] == "" {
			return true
		}
		acc, _ := au.store.ByID(c.R.Context(), c.Identity)
		return acc != nil && acc.HasRole(role[0])
	}
}

// Authorize returns an http predicate (for Admin.Authorize / route middleware)
// admitting only signed-in users holding role — or any signed-in user when role
// is "". Use: adm.Authorize(auth.Authorize("admin")).
func (au *Auth) Authorize(role string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		if role == "" {
			return au.Identity(r) != ""
		}
		acc := au.Current(r)
		return acc != nil && acc.HasRole(role)
	}
}

// ── the built-in login flow ──────────────────────────────────────────────────

// LoginOptions configures MountLogin. The zero value serves /login and /logout
// and redirects to "/" after each.
type LoginOptions struct {
	Prefix   string // path prefix for the routes (e.g. "/admin"); default "" → /login, /logout
	Redirect string // where to send the browser after login/logout; default "/"
	Title    string // login page heading/title; default "Sign in"
}

// MountLogin registers a ready-to-use login flow on mux: GET <prefix>/login
// serves a minimal sign-in form, POST <prefix>/login authenticates and sets the
// session, GET <prefix>/logout clears it. Same-origin is required on POST
// (defense-in-depth CSRF, matching /events). This is what makes auth built-in
// rather than something every app re-writes.
func (au *Auth) MountLogin(app *App, mux *http.ServeMux, opts LoginOptions) {
	prefix := strings.TrimRight(opts.Prefix, "/")
	redirect := opts.Redirect
	if redirect == "" {
		redirect = "/"
	}
	loginPath := prefix + "/login"

	mux.HandleFunc("GET "+loginPath, func(w http.ResponseWriter, r *http.Request) {
		if au.Identity(r) != "" { // already signed in
			http.Redirect(w, r, redirect, http.StatusFound)
			return
		}
		writeHTML(w, http.StatusOK, au.loginPage(app, loginPath, "", opts.Title))
	})

	mux.HandleFunc("POST "+loginPath, func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if _, err := au.Login(w, r, r.FormValue("login"), r.FormValue("password")); err != nil {
			writeHTML(w, http.StatusUnauthorized, au.loginPage(app, loginPath, "Invalid login or password.", opts.Title))
			return
		}
		http.Redirect(w, r, redirect, http.StatusFound)
	})

	mux.HandleFunc("GET "+prefix+"/logout", func(w http.ResponseWriter, r *http.Request) {
		au.Logout(w)
		http.Redirect(w, r, redirect, http.StatusFound)
	})
}

func writeHTML(w http.ResponseWriter, status int, html string) {
	secureHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(html))
}

func (au *Auth) loginPage(app *App, action, errMsg, title string) string {
	if title == "" {
		title = "Sign in"
	}
	var b strings.Builder
	b.WriteString(`<form class="fa-auth" method="post" action="` + template.HTMLEscapeString(action) + `">`)
	b.WriteString(`<h1>` + template.HTMLEscapeString(title) + `</h1>`)
	if errMsg != "" {
		b.WriteString(`<p class="fa-auth__err" role="alert">` + template.HTMLEscapeString(errMsg) + `</p>`)
	}
	b.WriteString(`<label>Login<input name="login" autocomplete="username" autofocus required></label>`)
	b.WriteString(`<label>Password<input name="password" type="password" autocomplete="current-password" required></label>`)
	b.WriteString(`<button type="submit">Sign in</button>`)
	b.WriteString(`</form>`)
	return string(app.Page(template.HTML(b.String()), ShellOptions{Title: title, CSS: authCSS}))
}

const authCSS template.CSS = `
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:#f7f9f9;font:15px/1.5 system-ui,sans-serif;color:#0f1419}
.fa-auth{display:flex;flex-direction:column;gap:12px;width:320px;max-width:90vw;background:#fff;border:1px solid #cfd9de;border-radius:16px;padding:24px}
.fa-auth h1{margin:0 0 4px;font-size:22px}
.fa-auth label{display:flex;flex-direction:column;gap:4px;font-size:13px;color:#536471}
.fa-auth input{font:inherit;color:#0f1419;padding:10px 12px;border:1px solid #cfd9de;border-radius:8px}
.fa-auth button{margin-top:4px;padding:10px;border:0;border-radius:999px;background:#1d9bf0;color:#fff;font-weight:700;cursor:pointer}
.fa-auth__err{margin:0;color:#f4212e;font-size:13px}
`

// ── accounts + store ─────────────────────────────────────────────────────────

// Account is one authenticatable user. Roles drive role checks (Guard/Authorize);
// "admin" is the conventional role for the admin panel.
type Account struct {
	ID      string
	Login   string // unique username/email used to sign in
	Hash    string // encoded password hash (see HashPassword); never the plaintext
	Roles   []string
	Created time.Time
}

// HasRole reports whether the account holds role.
func (a *Account) HasRole(role string) bool {
	for _, r := range a.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// AuthStore persists accounts. Implement it over your database; the lookups
// return (nil, nil) when no account matches (not an error). NewMemoryStore is a
// built-in development default.
type AuthStore interface {
	ByLogin(ctx context.Context, login string) (*Account, error)
	ByID(ctx context.Context, id string) (*Account, error)
	Create(ctx context.Context, a *Account) error
}

// MemoryStore is an in-memory AuthStore for development and tests. It is safe for
// concurrent use but does not persist — use a database-backed AuthStore in
// production. Logins are matched case-insensitively.
type MemoryStore struct {
	mu      sync.RWMutex
	byID    map[string]*Account
	byLogin map[string]*Account
}

// NewMemoryStore returns an empty in-memory account store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: map[string]*Account{}, byLogin: map[string]*Account{}}
}

func (m *MemoryStore) ByLogin(_ context.Context, login string) (*Account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneAccount(m.byLogin[loginKey(login)]), nil
}

func (m *MemoryStore) ByID(_ context.Context, id string) (*Account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneAccount(m.byID[id]), nil
}

func (m *MemoryStore) Create(_ context.Context, a *Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := loginKey(a.Login)
	if _, ok := m.byLogin[key]; ok {
		return ErrLoginTaken
	}
	stored := cloneAccount(a)
	m.byID[a.ID] = stored
	m.byLogin[key] = stored
	return nil
}

func loginKey(login string) string { return strings.ToLower(strings.TrimSpace(login)) }

// cloneAccount returns a deep-enough copy so callers can't mutate stored state.
func cloneAccount(a *Account) *Account {
	if a == nil {
		return nil
	}
	c := *a
	c.Roles = append([]string(nil), a.Roles...)
	return &c
}

// ── password hashing (stdlib PBKDF2-HMAC-SHA256, versioned) ──────────────────

// pbkdf2Iter is the PBKDF2-HMAC-SHA256 work factor (OWASP minimum). It is a var
// so tests can lower it; the encoded hash records the value used, so raising it
// later does not break existing hashes.
var pbkdf2Iter = 600_000

const (
	pbkdf2SaltLen = 16
	pbkdf2KeyLen  = 32
	pbkdf2Scheme  = "pbkdf2-sha256"
)

// HashPassword returns a self-describing hash string
// "pbkdf2-sha256$<iter>$<base64 salt>$<base64 key>". The scheme and parameters
// live in the string, so the cost can be raised — or the algorithm swapped (e.g.
// argon2id) — without invalidating hashes already stored: VerifyPassword reads
// the parameters from each record. Dependency-free (crypto/pbkdf2, stdlib).
func HashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Scheme, pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the encoded hash, comparing in
// constant time. Any unrecognized or malformed encoding fails closed.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Scheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// decoyHash is a valid-format hash of a random password, computed once, used to
// equalize Authenticate's timing when a login is unknown.
var (
	decoyOnce sync.Once
	decoyVal  string
)

func decoyHash() string {
	decoyOnce.Do(func() {
		decoyVal, _ = HashPassword(newID())
	})
	return decoyVal
}

// newID returns a random 128-bit identifier, base64url-encoded.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
