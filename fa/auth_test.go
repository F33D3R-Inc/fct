package fa

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Keep PBKDF2 cheap in tests; the encoded hash records the iteration count, so
// correctness is unaffected by the work factor.
func init() { pbkdf2Iter = 1000 }

func TestHashPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$") {
		t.Errorf("hash not versioned: %q", h)
	}
	if strings.Contains(h, "correct horse") {
		t.Error("hash leaks the plaintext")
	}
	if !VerifyPassword(h, "correct horse battery staple") {
		t.Error("correct password rejected")
	}
	if VerifyPassword(h, "wrong") {
		t.Error("wrong password accepted")
	}
	// Two hashes of the same password differ (random salt) but both verify.
	h2, _ := HashPassword("correct horse battery staple")
	if h == h2 {
		t.Error("expected distinct salts")
	}
	if !VerifyPassword(h2, "correct horse battery staple") {
		t.Error("second hash failed to verify")
	}
}

func TestVerifyPasswordFailsClosed(t *testing.T) {
	for _, bad := range []string{"", "plain", "bcrypt$x$y$z", "pbkdf2-sha256$nope$a$b", "pbkdf2-sha256$1000$@@@$@@@"} {
		if VerifyPassword(bad, "x") {
			t.Errorf("malformed hash %q verified", bad)
		}
	}
}

func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	app := New([]byte(`{}`), WithSigningKey([]byte("test-key")))
	return app.Auth(nil, SessionInsecure())
}

func TestSignupAndAuthenticate(t *testing.T) {
	au := newTestAuth(t)
	ctx := context.Background()

	if _, err := au.Signup(ctx, "ada", "short"); err != ErrWeakPassword {
		t.Errorf("want ErrWeakPassword, got %v", err)
	}
	if _, err := au.Signup(ctx, "  ", "longenough"); err != ErrNoLogin {
		t.Errorf("want ErrNoLogin, got %v", err)
	}

	acc, err := au.Signup(ctx, "Ada", "longenough", "admin")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	if acc.ID == "" || acc.Hash == "" || !acc.HasRole("admin") {
		t.Errorf("bad account: %+v", acc)
	}

	// Duplicate login (case-insensitive) is rejected.
	if _, err := au.Signup(ctx, "ada", "longenough"); err != ErrLoginTaken {
		t.Errorf("want ErrLoginTaken, got %v", err)
	}

	// Authenticate: right password by either case; wrong password and unknown
	// login both give ErrBadCredentials.
	if got, err := au.Authenticate(ctx, "ADA", "longenough"); err != nil || got.ID != acc.ID {
		t.Errorf("authenticate good: got %v, %v", got, err)
	}
	if _, err := au.Authenticate(ctx, "ada", "nope"); err != ErrBadCredentials {
		t.Errorf("wrong password: want ErrBadCredentials, got %v", err)
	}
	if _, err := au.Authenticate(ctx, "ghost", "whatever"); err != ErrBadCredentials {
		t.Errorf("unknown login: want ErrBadCredentials, got %v", err)
	}
}

func TestLoginSetsSessionAndCurrent(t *testing.T) {
	au := newTestAuth(t)
	ctx := context.Background()
	acc, err := au.Signup(ctx, "ada", "longenough")
	if err != nil {
		t.Fatal(err)
	}

	// Wrong creds set no cookie.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/login", nil)
	if _, err := au.Login(w, r, "ada", "wrong"); err != ErrBadCredentials {
		t.Fatalf("want ErrBadCredentials, got %v", err)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("failed login should not set a cookie")
	}

	// Correct creds set the session; Current/Identity read it back.
	w = httptest.NewRecorder()
	if _, err := au.Login(w, r, "ada", "longenough"); err != nil {
		t.Fatalf("login: %v", err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login set no cookie")
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	for _, c := range cookies {
		r2.AddCookie(c)
	}
	if got := au.Identity(r2); got != acc.ID {
		t.Errorf("Identity = %q, want %q", got, acc.ID)
	}
	cur := au.Current(r2)
	if cur == nil || cur.Login != "ada" {
		t.Errorf("Current = %+v", cur)
	}

	// Logout clears it.
	w3 := httptest.NewRecorder()
	au.Logout(w3)
	r3 := httptest.NewRequest("GET", "/", nil)
	for _, c := range w3.Result().Cookies() {
		r3.AddCookie(c)
	}
	if au.Current(r3) != nil {
		t.Error("Current after logout should be nil")
	}
}

func TestGuardAndAuthorize(t *testing.T) {
	au := newTestAuth(t)
	ctx := context.Background()
	admin, _ := au.Signup(ctx, "root", "longenough", "admin")
	user, _ := au.Signup(ctx, "bob", "longenough")

	anyGuard, adminGuard := au.Guard(), au.Guard("admin")
	if anyGuard(Ctx{Identity: ""}) {
		t.Error("anon should fail the signed-in guard")
	}
	if !anyGuard(Ctx{Identity: user.ID, R: httptest.NewRequest("GET", "/", nil)}) {
		t.Error("signed-in user should pass the signed-in guard")
	}
	if adminGuard(Ctx{Identity: user.ID, R: httptest.NewRequest("GET", "/", nil)}) {
		t.Error("non-admin should fail the admin guard")
	}
	if !adminGuard(Ctx{Identity: admin.ID, R: httptest.NewRequest("GET", "/", nil)}) {
		t.Error("admin should pass the admin guard")
	}

	// Authorize (http predicate) mirrors the same rules off the session cookie.
	authz := au.Authorize("admin")
	if authz(httptest.NewRequest("GET", "/admin", nil)) {
		t.Error("anon should be denied admin")
	}
	if !authz(signedInReq(t, au, "root", "longenough")) {
		t.Error("admin should be allowed")
	}
	if authz(signedInReq(t, au, "bob", "longenough")) {
		t.Error("non-admin should be denied admin")
	}
}

// signedInReq returns a GET request carrying a valid session cookie for login.
func signedInReq(t *testing.T, au *Auth, login, pass string) *http.Request {
	t.Helper()
	w := httptest.NewRecorder()
	if _, err := au.Login(w, httptest.NewRequest("POST", "/login", nil), login, pass); err != nil {
		t.Fatalf("login %s: %v", login, err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	return r
}

func TestMountLoginFlow(t *testing.T) {
	app := New([]byte(`{}`), WithSigningKey([]byte("test-key")))
	au := app.Auth(nil, SessionInsecure())
	if _, err := au.Signup(context.Background(), "ada", "longenough"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	au.MountLogin(app, mux, LoginOptions{})

	// GET /login serves the form.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/login", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `name="password"`) {
		t.Fatalf("GET /login: code %d, body lacks form:\n%s", w.Code, w.Body.String())
	}

	// POST with bad creds → 401, no cookie.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, loginPost("ada", "wrong"))
	if w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Errorf("bad POST: code %d, cookies %d", w.Code, len(w.Result().Cookies()))
	}

	// POST with good creds → redirect + session cookie.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, loginPost("ada", "longenough"))
	if w.Code != http.StatusFound {
		t.Errorf("good POST: code %d, want 302", w.Code)
	}
	if len(w.Result().Cookies()) == 0 {
		t.Error("good POST set no session cookie")
	}

	// Cross-origin POST is rejected (CSRF).
	w = httptest.NewRecorder()
	xo := loginPost("ada", "longenough")
	xo.Header.Set("Origin", "https://evil.example")
	mux.ServeHTTP(w, xo)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST: code %d, want 403", w.Code)
	}
}

func loginPost(login, pass string) *http.Request {
	r := httptest.NewRequest("POST", "/login", strings.NewReader("login="+login+"&password="+pass))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}
