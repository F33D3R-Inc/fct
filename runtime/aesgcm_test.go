package runtime

import (
	"crypto/aes"
	"crypto/cipher"
	"reflect"
	"strings"
	"testing"
)

func ints(b []byte) []any {
	out := make([]any, len(b))
	for i, x := range b {
		out[i] = int(x)
	}
	return out
}

func TestAesGcmBuiltins(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	nonce := []byte("twelve bytes")
	pt := []byte(strings.Repeat("a page of plaintext ", 50))
	block, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(block)
	want := ints(g.Seal(nil, nonce, pt, nil))

	sealed, err := aesGcmSeal(ints(key), ints(nonce), ints(pt))
	if err != nil || !reflect.DeepEqual(sealed, want) {
		t.Fatalf("aesGcmSeal = %v, %v; want Go's crypto/cipher output", err, sealed)
	}
	ok, err := aesGcmAuthentic(ints(key), ints(nonce), sealed)
	if err != nil || ok != true {
		t.Fatalf("aesGcmAuthentic(sealed) = %v, %v", ok, err)
	}
	back, err := aesGcmOpen(ints(key), ints(nonce), sealed)
	if err != nil || !reflect.DeepEqual(back, ints(pt)) {
		t.Fatalf("aesGcmOpen round trip: %v", err)
	}
	bad := append([]any{}, sealed.([]any)...)
	bad[3] = (bad[3].(int) + 1) % 256
	if ok, _ := aesGcmAuthentic(ints(key), ints(nonce), bad); ok != false {
		t.Fatal("a modified ciphertext authenticated")
	}
	if _, err := aesGcmOpen(ints(key), ints(nonce), bad); err == nil || !strings.Contains(err.Error(), "does not authenticate") {
		t.Fatalf("opening a modified ciphertext: %v", err)
	}
	for _, c := range []struct {
		key, nonce, pt []any
		msg            string
	}{
		{ints(key[:16]), ints(nonce), ints(pt), "key is 16 bytes"},
		{ints(key), ints(nonce[:8]), ints(pt), "nonce is 8 bytes"},
		{ints(key), ints(nonce), []any{1, 300}, "out of range"},
	} {
		if _, err := aesGcmSeal(c.key, c.nonce, c.pt); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Fatalf("want an error mentioning %q, got %v", c.msg, err)
		}
	}
	if _, err := aesGcmSeal("x", ints(nonce), ints(pt)); err == nil {
		t.Fatal("a non-buffer key was accepted")
	}
}
