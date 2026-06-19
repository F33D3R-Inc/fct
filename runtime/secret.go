package runtime

// Secrets management — one master secret (FACET_SECRET) drives every keyed
// operation the server needs: signing session cookies and CSRF tokens (so a
// client cannot forge identity), and encrypting `@secret` entity fields at rest
// (AES-256-GCM). Keys are *derived* from the master secret, never the master
// secret itself, so a leak of one derived key does not compromise the others.
//
// If FACET_SECRET is unset the server mints a random, process-ephemeral secret
// and warns once: cookies and CSRF still work within a run, but they (and any
// encrypted column) will not survive a restart. Production must set FACET_SECRET.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
)

// keyring holds the derived keys, initialized once from FACET_SECRET.
type keyring struct {
	signKey   []byte      // HMAC-SHA256 key for cookies/CSRF/token hashing
	gcm       cipher.AEAD // AES-256-GCM for @secret fields
	ephemeral bool        // true when FACET_SECRET was not set
}

var (
	keysOnce sync.Once
	keys     *keyring
)

// SecretConfigured reports whether FACET_SECRET was provided (vs. a generated
// ephemeral key) — used for the startup banner and to warn operators.
func SecretConfigured() bool { return !ring().ephemeral }

// SecurityDescription summarizes the active security posture for the startup
// banner: the secret source, and whether OIDC single sign-on is configured.
func SecurityDescription() string {
	parts := []string{"signed cookies", "CSRF", "rate-limit", "audit"}
	if SecretConfigured() {
		parts = append([]string{"FACET_SECRET set"}, parts...)
	} else {
		parts = append([]string{"ephemeral key — set FACET_SECRET"}, parts...)
	}
	if os.Getenv("FACET_OIDC_ISSUER") != "" {
		parts = append(parts, "OIDC SSO")
	}
	return strings.Join(parts, " · ")
}

func ring() *keyring {
	keysOnce.Do(func() {
		master := []byte(os.Getenv("FACET_SECRET"))
		ephemeral := false
		if len(master) == 0 {
			master = make([]byte, 32)
			if _, err := rand.Read(master); err != nil {
				panic("facet: cannot generate an ephemeral secret: " + err.Error())
			}
			ephemeral = true
			fmt.Fprintln(os.Stderr,
				"facet: FACET_SECRET is not set — using a random ephemeral key; "+
					"sessions, CSRF tokens, and @secret columns will not survive a restart. Set FACET_SECRET in production.")
		}
		k := &keyring{signKey: derive(master, "sign"), ephemeral: ephemeral}
		block, err := aes.NewCipher(derive(master, "encrypt")) // 32 bytes -> AES-256
		if err != nil {
			panic("facet: cipher init: " + err.Error())
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			panic("facet: gcm init: " + err.Error())
		}
		k.gcm = gcm
		keys = k
	})
	return keys
}

// derive produces a 32-byte subkey for a labeled purpose from the master secret,
// so distinct uses never share key material.
func derive(master []byte, label string) []byte {
	h := sha256.Sum256(append([]byte("facet:"+label+":"), master...))
	return h[:]
}

// ── HMAC signing (cookies, CSRF, token hashing) ──────────────────────────────

// sign returns the base64 HMAC-SHA256 of msg under the signing key.
func sign(msg string) string {
	mac := hmac.New(sha256.New, ring().signKey)
	mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// signValue tags a value with its signature: "value.signature".
func signValue(v string) string { return v + "." + sign(v) }

// verifySigned splits a signed value and checks the signature in constant time.
func verifySigned(s string) (string, bool) {
	dot := lastDot(s)
	if dot < 0 {
		return "", false
	}
	v, sig := s[:dot], s[dot+1:]
	if subtle.ConstantTimeCompare([]byte(sig), []byte(sign(v))) != 1 {
		return "", false
	}
	return v, true
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// hashToken is a keyed, non-reversible digest used to store reset/verification
// tokens — the database never holds a usable token, only its HMAC.
func hashToken(token string) string { return sign("token:" + token) }

// tokenEqual compares a presented token against a stored hash in constant time.
func tokenEqual(presented, storedHash string) bool {
	if storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashToken(presented)), []byte(storedHash)) == 1
}

// randomToken returns a URL-safe, unguessable token (n bytes of entropy).
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("facet: rng failure: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ── CSRF tokens ──────────────────────────────────────────────────────────────

// csrfToken binds an anti-forgery token to a session id; a forged request from
// another origin cannot produce it because it requires the signing key.
func csrfToken(sid string) string { return sign("csrf:" + sid) }

// csrfValid checks a presented CSRF token against the session in constant time.
func csrfValid(sid, presented string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(csrfToken(sid))) == 1
}

// ── field encryption at rest (AES-256-GCM) ───────────────────────────────────

const encPrefix = "fctenc:" // marks a value this server encrypted

// encryptSecret encrypts a plaintext field value for storage. An empty value
// stays empty (so an unset secret is not bloated and round-trips cleanly).
func encryptSecret(plaintext string) string {
	if plaintext == "" {
		return ""
	}
	nonce := make([]byte, ring().gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("facet: rng failure: " + err.Error())
	}
	ct := ring().gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.RawURLEncoding.EncodeToString(ct)
}

// decryptSecret reverses encryptSecret. A value that is empty, unprefixed
// (legacy plaintext), or undecipherable (encrypted under a different key) is
// returned unchanged, so the app keeps running across a key rotation rather than
// failing a whole table load.
func decryptSecret(stored string) string {
	if len(stored) < len(encPrefix) || stored[:len(encPrefix)] != encPrefix {
		return stored
	}
	raw, err := base64.RawURLEncoding.DecodeString(stored[len(encPrefix):])
	if err != nil {
		return stored
	}
	ns := ring().gcm.NonceSize()
	if len(raw) < ns {
		return stored
	}
	pt, err := ring().gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return stored
	}
	return string(pt)
}
