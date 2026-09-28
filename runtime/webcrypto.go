package runtime

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
)

// The primitives Web Push is built from (RFC 8291 message encryption, RFC
// 8292 VAPID), exposed as proc builtins so the protocol itself — HKDF, the
// record layout, the JWT — is authored in .fct (facets/notify/webpush.fct).
// Each is a thin, total wrapper over Go's standard library:
//
//	p256PrivateKey() -> bytes           a fresh P-256 private scalar (32 bytes)
//	p256PublicKey(priv) -> bytes        its uncompressed public point (65 bytes)
//	p256Ecdh(priv, pub) -> bytes        the ECDH shared secret (32 bytes)
//	es256Sign(priv, msg) -> bytes       ECDSA P-256/SHA-256, JWS form r‖s (64 bytes)
//	fromBase64UrlRaw(text) -> bytes     base64url decode (padding tolerated)
//	httpSend(method, url, headers, body) -> int
//	                                    one HTTP request; headers are
//	                                    "Name: value" texts; answers the status,
//	                                    or 0 when the request never completed

func p256PrivateKey() (any, error) {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("p256PrivateKey: %v", err)
	}
	return byteBuf(k.Bytes()), nil
}

func p256Priv(name string, v any) (*ecdh.PrivateKey, error) {
	b, err := byteArg(name, "private key", v)
	if err != nil {
		return nil, err
	}
	k, err := ecdh.P256().NewPrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("%s: not a P-256 private key (%d bytes): %v", name, len(b), err)
	}
	return k, nil
}

func p256PublicKey(privV any) (any, error) {
	k, err := p256Priv("p256PublicKey", privV)
	if err != nil {
		return nil, err
	}
	return byteBuf(k.PublicKey().Bytes()), nil
}

func p256Ecdh(privV, pubV any) (any, error) {
	k, err := p256Priv("p256Ecdh", privV)
	if err != nil {
		return nil, err
	}
	pb, err := byteArg("p256Ecdh", "public key", pubV)
	if err != nil {
		return nil, err
	}
	pub, err := ecdh.P256().NewPublicKey(pb)
	if err != nil {
		return nil, fmt.Errorf("p256Ecdh: not an uncompressed P-256 point (%d bytes): %v", len(pb), err)
	}
	s, err := k.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("p256Ecdh: %v", err)
	}
	return byteBuf(s), nil
}

func es256Sign(privV, msgV any) (any, error) {
	b, err := byteArg("es256Sign", "private key", privV)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("es256Sign: private key is %d bytes; P-256 takes 32", len(b))
	}
	msg, err := byteArg("es256Sign", "message", msgV)
	if err != nil {
		return nil, err
	}
	curve := elliptic.P256()
	d := new(big.Int).SetBytes(b)
	x, y := curve.ScalarBaseMult(b)
	key := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d}
	sum := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return nil, fmt.Errorf("es256Sign: %v", err)
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return byteBuf(out), nil
}

func fromBase64UrlRaw(v any) (any, error) {
	s := strings.TrimRight(strings.TrimSpace(toStr(v)), "=")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("fromBase64UrlRaw: %v", err)
	}
	return byteBuf(b), nil
}

func (s *Server) ioHTTPSend(method, rawURL string, headersV, bodyV any) (any, error) {
	if err := validHTTPURL(rawURL); err != nil {
		return nil, fmt.Errorf("httpSend: %v", err)
	}
	method = strings.ToUpper(method)
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, fmt.Errorf("httpSend: method %q is not GET, POST, PUT, PATCH or DELETE", method)
	}
	body, err := byteArg("httpSend", "body", bodyV)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("httpSend: %v", err)
	}
	hs, _ := listElems(headersV)
	for _, h := range hs {
		name, value, ok := strings.Cut(toStr(h), ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("httpSend: header %q is not \"Name: value\"", toStr(h))
		}
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	resp, err := ioHTTPClient.Do(req)
	if err != nil {
		return 0, nil // never completed: the caller's to retry, not a proc failure
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return resp.StatusCode, nil
}
