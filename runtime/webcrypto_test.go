package runtime

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RFC 8291 Appendix A: the user agent's and application server's keys, the
// shared secret they agree on, and AES-128-GCM under the derived key.
func TestWebPushPrimitivesRFC8291(t *testing.T) {
	d := func(s string) bytesVal {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return bytesVal(b)
	}
	uaPriv := d("q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94")
	asPriv := d("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	asPub := d("BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	uaPub := d("BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	shared := d("kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs")

	if got, err := p256PublicKey(asPriv); err != nil || !bytes.Equal(got.(bytesVal), asPub) {
		t.Fatalf("p256PublicKey(as_private) = %v, %v", got, err)
	}
	if got, _ := p256PublicKey(uaPriv); !bytes.Equal(got.(bytesVal), uaPub) {
		t.Fatalf("p256PublicKey(ua_private) mismatch")
	}
	for _, pair := range [][2]bytesVal{{asPriv, uaPub}, {uaPriv, asPub}} {
		if got, err := p256Ecdh(pair[0], pair[1]); err != nil || !bytes.Equal(got.(bytesVal), shared) {
			t.Fatalf("p256Ecdh = %v, %v; want the RFC's ecdh_secret", got, err)
		}
	}
	cek := d("oIhVW04MRdy2XN9CiKLxTg")
	nonce := d("4h_95klXJ5E_qnoN")
	body := d("DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")
	pt := append([]byte("When I grow up, I want to be a watermelon"), 2)
	sealed, err := aesGcmSeal(cek, nonce, bytesVal(pt))
	if err != nil || !bytes.Equal(sealed.(bytesVal), body[86:]) {
		t.Fatalf("AES-128-GCM seal = %v, %v; want the RFC's ciphertext", sealed, err)
	}
	if got, _ := fromBase64UrlRaw("BTBZMqHH6r4Tts7J_aSIgg=="); len(got.(bytesVal)) != 16 {
		t.Errorf("fromBase64UrlRaw must tolerate padding")
	}
	if _, err := p256Ecdh(asPriv, bytesVal([]byte{4, 1, 2})); err == nil {
		t.Errorf("a malformed point must be refused")
	}
}

// es256Sign answers a JWS r‖s signature any ES256 verifier accepts.
func TestES256SignVerifies(t *testing.T) {
	priv, _ := p256PrivateKey()
	msg := bytesVal([]byte("header.claims"))
	sig, err := es256Sign(priv, msg)
	if err != nil || len(sig.(bytesVal)) != 64 {
		t.Fatalf("es256Sign = %v, %v", sig, err)
	}
	pubB, _ := p256PublicKey(priv)
	pb := pubB.(bytesVal)
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(pb[1:33]), Y: new(big.Int).SetBytes(pb[33:])}
	sum := sha256.Sum256(msg)
	sb := sig.(bytesVal)
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sb[:32]), new(big.Int).SetBytes(sb[32:])) {
		t.Fatalf("the signature does not verify")
	}
}

// httpSend carries the method, the headers and the binary body, and answers
// the status (0 when the request never completed).
func TestHTTPSend(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody = make([]byte, r.ContentLength)
		r.Body.Read(gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()
	s := &Server{}
	st, err := s.ioHTTPSend("post", ts.URL+"/push/abc", []any{"TTL: 60", "Content-Encoding: aes128gcm"}, bytesVal([]byte{0, 1, 2, 255}))
	if err != nil || st != http.StatusCreated {
		t.Fatalf("httpSend = %v, %v", st, err)
	}
	if got.Method != http.MethodPost || got.Header.Get("TTL") != "60" || got.Header.Get("Content-Encoding") != "aes128gcm" || !bytes.Equal(gotBody, []byte{0, 1, 2, 255}) {
		t.Errorf("request = %s %v %v", got.Method, got.Header, gotBody)
	}
	if _, err := s.ioHTTPSend("POST", ts.URL, []any{"no colon"}, bytesVal(nil)); err == nil || !strings.Contains(err.Error(), "Name: value") {
		t.Errorf("a malformed header must be refused: %v", err)
	}
	if st, _ := s.ioHTTPSend("POST", "http://127.0.0.1:1/", []any{}, bytesVal(nil)); st != 0 {
		t.Errorf("an unreachable endpoint answers 0, got %v", st)
	}
}
