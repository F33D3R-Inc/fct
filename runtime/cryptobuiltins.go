package runtime

// Keyed digests and password hashing as proc builtins, over the same byte
// buffers aesGcmSeal/aesGcmOpen take (a `bytes` value: a list of ints 0-255,
// see aesgcm.go's byteArg/byteBuf).
//
// These exist so a runtime written in fct can do what THIS runtime does with
// secret.go — sign a session cookie (HMAC-SHA256 under a key derived by
// SHA-256 from FACET_SECRET, spelled base64url without padding) and store a
// password as its bcrypt hash — through the same primitives, rather than
// re-implementing SHA-256 and bcrypt in fct. bcryptMatches is verifyPassword
// for a hash held in a proc-local text: the compiler admits only a @password
// field as verifyPassword's first argument, which a runtime that keeps its
// user table as plain rows cannot offer.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// sha256Bytes(b) -> bytes: the SHA-256 digest of a byte buffer.
func sha256Bytes(v any) (any, error) {
	b, err := byteArg("sha256Bytes", "data", v)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return byteBuf(sum[:]), nil
}

// hmacSha256(key, msg) -> bytes: HMAC-SHA256 of msg under key.
func hmacSha256(keyV, msgV any) (any, error) {
	key, err := byteArg("hmacSha256", "key", keyV)
	if err != nil {
		return nil, err
	}
	msg, err := byteArg("hmacSha256", "message", msgV)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return byteBuf(mac.Sum(nil)), nil
}

// base64UrlRaw(b) -> text: the URL-safe base64 of a byte buffer, no padding —
// the spelling secret.go's sign() and randomToken() use.
func base64UrlRaw(v any) (any, error) {
	b, err := byteArg("base64UrlRaw", "data", v)
	if err != nil {
		return nil, err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// bcryptHash(plain) -> text: hashPassword as a builtin. An empty text hashes
// to "" (no credential) and an over-long one is an error, exactly as a
// @password write behaves.
func bcryptHashBuiltin(v any) (any, error) {
	h, err := hashPassword(toStr(v))
	if err != nil {
		return nil, err
	}
	return h, nil
}
