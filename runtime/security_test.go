package runtime

import (
	"encoding/base32"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"facet/internal/ir"
)

// The security primitives are pure and DB-free; these tests pin them directly.
// Integration tests in server_test.go exercise the flows end-to-end when
// FACET_DATABASE_URL is set.

func TestCookieSigning(t *testing.T) {
	signed := signValue("s7")
	if signed == "s7" || !strings.HasPrefix(signed, "s7.") {
		t.Fatalf("signValue should append a signature, got %q", signed)
	}
	v, ok := verifySigned(signed)
	if !ok || v != "s7" {
		t.Fatalf("verifySigned round-trip failed: v=%q ok=%v", v, ok)
	}
	// a tampered value (or signature) is rejected.
	if _, ok := verifySigned("s8." + strings.SplitN(signed, ".", 2)[1]); ok {
		t.Error("a tampered cookie value should not verify")
	}
	if _, ok := verifySigned("garbage"); ok {
		t.Error("an unsigned value should not verify")
	}
}

func TestCSRFToken(t *testing.T) {
	tok := csrfToken("sX")
	if !csrfValid("sX", tok) {
		t.Error("a fresh CSRF token should validate for its session")
	}
	if csrfValid("sY", tok) {
		t.Error("a CSRF token must not validate for another session")
	}
	if csrfValid("sX", "") {
		t.Error("an empty CSRF token must not validate")
	}
}

func TestFieldEncryptionRoundTrip(t *testing.T) {
	enc := encryptSecret("ssn-123-45-6789")
	if !strings.HasPrefix(enc, encPrefix) {
		t.Fatalf("encrypted value should carry the marker prefix, got %q", enc)
	}
	if strings.Contains(enc, "123-45-6789") {
		t.Error("ciphertext must not contain the plaintext")
	}
	if got := decryptSecret(enc); got != "ssn-123-45-6789" {
		t.Errorf("decrypt round-trip = %q, want the plaintext", got)
	}
	// empty stays empty; legacy plaintext passes through unchanged.
	if encryptSecret("") != "" || decryptSecret("") != "" {
		t.Error("empty secret should round-trip as empty")
	}
	if decryptSecret("not-encrypted") != "not-encrypted" {
		t.Error("an unprefixed (legacy plaintext) value should pass through")
	}
}

// A @secret column is encrypted on the way to storage (colValue) and decrypted on
// the way back (normalize), so the database only ever holds ciphertext.
func TestSecretColumnStorage(t *testing.T) {
	f := ir.Field{Name: "ssn", Type: "text", Secret: true}
	stored := colValue(f, "top-secret").(string)
	if !strings.HasPrefix(stored, encPrefix) {
		t.Fatalf("a @secret column should store ciphertext, got %q", stored)
	}
	if got := normalize([]byte(stored), f); got != "top-secret" {
		t.Errorf("normalize of a @secret column = %v, want the plaintext", got)
	}
}

// TOTP matches the RFC 6238 reference vector (SHA1, T=59 -> 287082).
func TestTOTPReferenceVector(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	if got := totpCode(secret, time.Unix(59, 0)); got != "287082" {
		t.Errorf("RFC 6238 vector: totpCode = %q, want 287082", got)
	}
	// a freshly-computed current code validates (and a wrong one does not).
	now := time.Now()
	if !totpValid(secret, totpCode(secret, now), now) {
		t.Error("a current TOTP code should validate")
	}
	if totpValid(secret, "000000", now.Add(10*time.Minute)) {
		t.Error("a stale/wrong code should not validate")
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(60) // burst 60, refill 1/sec
	ok := 0
	for i := 0; i < 60; i++ {
		if l.allow("1.2.3.4") {
			ok++
		}
	}
	if ok != 60 {
		t.Fatalf("burst should allow 60 requests, allowed %d", ok)
	}
	if l.allow("1.2.3.4") {
		t.Error("the 61st request in an instant should be throttled")
	}
	// a different client has its own bucket.
	if !l.allow("5.6.7.8") {
		t.Error("a different IP should not be throttled")
	}
}

func TestLockout(t *testing.T) {
	l := newLockout() // max 5
	if l.locked("ada") {
		t.Fatal("a fresh account is not locked")
	}
	for i := 0; i < 4; i++ {
		l.fail("ada")
	}
	if l.locked("ada") {
		t.Error("4 failures should not lock yet")
	}
	l.fail("ada")
	if !l.locked("ada") {
		t.Error("the 5th failure should lock the account")
	}
	l.reset("ada")
	if l.locked("ada") {
		t.Error("reset should clear the lockout")
	}
}

func TestTokenHashing(t *testing.T) {
	tok := randomToken(24)
	h := hashToken(tok)
	if h == tok || strings.Contains(h, tok) {
		t.Error("a stored token must be a non-reversible hash, not the token")
	}
	if !tokenEqual(tok, h) {
		t.Error("the original token should match its stored hash")
	}
	if tokenEqual("wrong", h) || tokenEqual(tok, "") {
		t.Error("a wrong token (or empty hash) must not match")
	}
}

// PKCE and the OIDC authorization URL are well-formed and bound to the request.
func TestOIDCAuthURL(t *testing.T) {
	p := &oidcProvider{
		clientID:     "cid",
		redirect:     "https://app/cb",
		authEndpoint: "https://idp/authorize",
	}
	verifier, challenge := pkce()
	if verifier == "" || challenge == "" || verifier == challenge {
		t.Fatalf("pkce should produce a distinct verifier and challenge")
	}
	u := p.authURL("state123", challenge)
	for _, sub := range []string{
		"https://idp/authorize?",
		"client_id=cid",
		"code_challenge_method=S256",
		"code_challenge=" + challenge,
		"state=state123",
		"response_type=code",
		"scope=openid+email+profile",
	} {
		if !strings.Contains(u, sub) {
			t.Errorf("auth URL missing %q in:\n%s", sub, u)
		}
	}
}

func TestParseIDClaims(t *testing.T) {
	// header.payload.signature with a base64url JSON payload (signature ignored).
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"abc","email":"ada@x.io","email_verified":true,"preferred_username":"ada"}`))
	tok := "h." + payload + ".s"
	c, err := parseIDClaims(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sub != "abc" || c.Email != "ada@x.io" || !c.EmailVerified || c.PreferredUsername != "ada" {
		t.Errorf("claims parsed wrong: %+v", c)
	}
	if oidcUsername(c) != "ada" {
		t.Errorf("oidcUsername should prefer preferred_username, got %q", oidcUsername(c))
	}
	if _, err := parseIDClaims("not-a-jwt"); err == nil {
		t.Error("a malformed id_token should error")
	}
}
