package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"facet/internal/compile"
)

// The crypto builtins a self-hosted runtime signs and stores credentials with
// must agree with secret.go: sign(msg) restated in fct over sha256Bytes /
// hmacSha256 / base64UrlRaw is byte-identical to sign(), and bcryptHash /
// bcryptMatches are hashPassword / passwordMatches.
const cryptoBuiltinApp = `app C:
    proc signLike(master: text, msg: text) -> text:
        let key = sha256Bytes(textToBytes("facet:sign:" + master))
        let mac = hmacSha256(key, textToBytes(msg))
        return base64UrlRaw(mac)
    proc hashAndCheck(plain: text, candidate: text) -> bool:
        let h = bcryptHash(plain)
        return bcryptMatches(h, candidate)
    proc emptyHash() -> text:
        return bcryptHash("")
`

func TestCryptoBuiltinsMatchSecretGo(t *testing.T) {
	g, err := compile.String(cryptoBuiltinApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	got, err := srv.runProcLocked(srv.byProc["signLike"], []any{"master-secret", "csrf:abc"})
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte("facet:sign:master-secret"))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("csrf:abc"))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if toStr(got) != want {
		t.Fatalf("signLike = %q, want %q", toStr(got), want)
	}
	for _, c := range []struct {
		plain, cand string
		want        bool
	}{{"hunter2", "hunter2", true}, {"hunter2", "hunter3", false}, {"", "", false}} {
		v, err := srv.runProcLocked(srv.byProc["hashAndCheck"], []any{c.plain, c.cand})
		if err != nil {
			t.Fatal(err)
		}
		if truthy(v) != c.want {
			t.Errorf("hashAndCheck(%q, %q) = %v, want %v", c.plain, c.cand, v, c.want)
		}
	}
	if v, err := srv.runProcLocked(srv.byProc["emptyHash"], nil); err != nil || toStr(v) != "" {
		t.Errorf("bcryptHash(\"\") = %q, %v; want \"\" (no credential)", toStr(v), err)
	}
}
