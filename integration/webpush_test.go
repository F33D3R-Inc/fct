package integration

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// facets/notify/webpush.fct, inside the product (f33d3r_com.fct), delivers
// the product's own notifications to a real (local) push service: the
// service verifies the RFC 8292 VAPID JWT (ES256 over header.claims, `k` the
// signer, aud the service's origin), decrypts the RFC 8291 aes128gcm body with
// the subscription's keys, and answers 201 / 410 / 503 per endpoint — so a
// delivery is counted, a 410 deactivates its subscription, and a 503 is queued
// for retry.
func TestWebPushDeliversToAPushService(t *testing.T) {
	b64 := base64.RawURLEncoding
	vapid, _ := ecdh.P256().GenerateKey(rand.Reader)
	t.Setenv("VAPID_PRIVATE_KEY", b64.EncodeToString(vapid.Bytes()))
	t.Setenv("VAPID_SUBJECT", "mailto:ops@f33d3r.example")
	t.Setenv("FACET_LOG_LEVEL", "error")

	type ua struct {
		priv *ecdh.PrivateKey
		auth []byte
	}
	uas := map[string]ua{}
	var mu sync.Mutex
	var payloads []string
	var problems []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fail := func(msg string) { problems = append(problems, r.URL.Path+": "+msg); w.WriteHeader(400) }
		// RFC 8292: vapid t=<jwt>, k=<key>
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "vapid t=") {
			fail("no vapid authorization: " + authz)
			return
		}
		tok, kpart, _ := strings.Cut(strings.TrimPrefix(authz, "vapid t="), ", k=")
		parts := strings.Split(tok, ".")
		kb, _ := b64.DecodeString(kpart)
		if len(parts) != 3 || len(kb) != 65 {
			fail("malformed vapid header")
			return
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(kb[1:33]), Y: new(big.Int).SetBytes(kb[33:])}
		sig, _ := b64.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if len(sig) != 64 || !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			fail("vapid signature does not verify")
			return
		}
		cb, _ := b64.DecodeString(parts[1])
		var claims map[string]any
		json.Unmarshal(cb, &claims)
		if claims["aud"] != "http://"+r.Host || claims["sub"] != "mailto:ops@f33d3r.example" {
			fail("claims " + string(cb))
			return
		}
		if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") == "" {
			fail("headers")
			return
		}
		// RFC 8291: decrypt with the user agent's keys.
		u := uas[r.URL.Path]
		body, _ := io.ReadAll(r.Body)
		salt, keyLen := body[:16], int(body[20])
		asPub, _ := ecdh.P256().NewPublicKey(body[21 : 21+keyLen])
		secret, _ := u.priv.ECDH(asPub)
		hk := func(salt, ikm, info []byte, n int) []byte {
			m := hmac.New(sha256.New, salt)
			m.Write(ikm)
			prk := m.Sum(nil)
			m = hmac.New(sha256.New, prk)
			m.Write(info)
			m.Write([]byte{1})
			return m.Sum(nil)[:n]
		}
		info := append(append([]byte("WebPush: info\x00"), u.priv.PublicKey().Bytes()...), asPub.Bytes()...)
		ikm := hk(u.auth, secret, info, 32)
		cek := hk(salt, ikm, []byte("Content-Encoding: aes128gcm\x00"), 16)
		nonce := hk(salt, ikm, []byte("Content-Encoding: nonce\x00"), 12)
		block, _ := aes.NewCipher(cek)
		g, _ := cipher.NewGCM(block)
		pt, err := g.Open(nil, nonce, body[21+keyLen:], nil)
		if err != nil || len(pt) == 0 || pt[len(pt)-1] != 2 {
			fail("does not decrypt")
			return
		}
		payloads = append(payloads, string(pt[:len(pt)-1]))
		switch {
		case strings.HasSuffix(r.URL.Path, "/gone"):
			w.WriteHeader(http.StatusGone)
		case strings.HasSuffix(r.URL.Path, "/busy"):
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer ts.Close()

	path, _ := filepath.Abs("../../facets/f33d3r_com.fct")
	g, err := compile.File(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	run := func(actor, role, action string, args ...any) any {
		t.Helper()
		v, err := srv.RunValue(actor, role, true, action, args)
		if err != nil {
			t.Fatalf("%s as %s: %v", action, actor, err)
		}
		return v
	}
	run("guest", "guest", "webSignup", "ada", "correct-horse")
	run("guest", "guest", "webSignup", "bob", "battery-staple")
	// ada's three devices register through the product's own push mutation.
	for _, dev := range []string{"ok", "gone", "busy"} {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		auth := make([]byte, 16)
		rand.Read(auth)
		uas["/push/"+dev] = ua{k, auth}
		run("ada", "member", "pushSubscriptionRegistered", map[string]any{"endpoint": ts.URL + "/push/" + dev,
			"keys": map[string]any{"p256dh": b64.EncodeToString(k.PublicKey().Bytes()), "auth": b64.EncodeToString(auth)}}, dev)
	}
	run("ada", "member", "post", `hello "world"`)
	run("bob", "member", "like", 1) // the product writes ada a like Notification
	run("system", "admin", "drainPushes")
	if len(problems) > 0 {
		t.Fatalf("the push service refused deliveries: %v", problems)
	}
	if len(payloads) != 3 {
		t.Fatalf("payloads = %d; want 3 attempted", len(payloads))
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payloads[0]), &p); err != nil || p["title"] != "bob liked your post" || p["body"] != `hello "world"` || p["tag"] != "f33d3r-like" || p["data"].(map[string]any)["url"] != "/post/1" {
		t.Errorf("payload = %s (%v)", payloads[0], err)
	}
	var logged map[string]any
	for _, r := range srv.EntityRows("PushLog") {
		logged = r.(map[string]any)
	}
	if logged["sent"] != 1 || logged["total"] != 3 {
		t.Errorf("push log = %v, want 1 of 3 delivered", logged)
	}
	devices := map[string]bool{}
	for _, r := range srv.EntityRows("PushSubscription") {
		devices[r.(map[string]any)["device_id"].(string)] = true
	}
	if devices["gone"] || !devices["ok"] || !devices["busy"] {
		t.Errorf("a 410 removes only its device's subscription: %v", devices)
	}
	queued := 0
	for _, r := range srv.EntityRows("PushRetry") {
		if r.(map[string]any)["status"] == "queued" {
			queued++
		}
	}
	if queued != 1 {
		t.Errorf("the 503 must be queued for retry once, got %d", queued)
	}
	// Drained once: a second pass pushes nothing new.
	run("system", "admin", "drainPushes")
	if len(payloads) != 3 {
		t.Errorf("a second drain re-pushed: %d payloads", len(payloads))
	}
	// Likes off in the product's preferences: the next like is not pushed.
	run("ada", "member", "patchNotificationPreferences", true, true, false)
	run("bob", "member", "like", 1)
	run("bob", "member", "like", 1)
	run("system", "admin", "drainPushes")
	if len(payloads) != 3 {
		t.Errorf("a like pushed with likes off: %d payloads", len(payloads))
	}
}
