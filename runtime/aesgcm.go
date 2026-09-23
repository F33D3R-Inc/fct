package runtime

// AES-256-GCM as standard builtins, the way every real language's standard
// library ships its cryptographic primitives: a byte-buffer program (the
// self-hosted storage engine seals every 16 KiB page it writes) should not
// run a block cipher through the interpreter one table lookup at a time.
//
//	aesGcmSeal(key, nonce, plaintext) -> bytes   ciphertext || 16-byte tag
//	aesGcmAuthentic(key, nonce, sealed) -> bool  the tag verifies
//	aesGcmOpen(key, nonce, sealed) -> bytes      the plaintext; a sealed
//	                                             buffer that does not verify
//	                                             is a runtime error, so a
//	                                             caller that can meet a bad
//	                                             one asks aesGcmAuthentic first
//
// The key is 32 bytes and the nonce 12 (AES-256, the 96-bit nonce GCM is
// specified around); anything else is a runtime error. selfhost/aes_gcm.fct
// keeps the cipher written in fct as the conformance reference these are
// tested against.

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
)

func byteArg(name, what string, v any) ([]byte, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s is not a byte buffer", name, what)
	}
	out := make([]byte, len(arr))
	for i, x := range arr {
		n := toInt(x)
		if n < 0 || n > 255 {
			return nil, fmt.Errorf("%s: %s byte %d is %d, out of range (must be 0-255)", name, what, i, n)
		}
		out[i] = byte(n)
	}
	return out, nil
}

func byteBuf(b []byte) []any {
	out := make([]any, len(b))
	for i, x := range b {
		out[i] = boxedInts[x]
	}
	return out
}

func aesGcmFor(name string, keyV, nonceV any) (cipher.AEAD, []byte, error) {
	key, err := byteArg(name, "key", keyV)
	if err != nil {
		return nil, nil, err
	}
	if len(key) != 32 {
		return nil, nil, fmt.Errorf("%s: key is %d bytes; AES-256 takes 32", name, len(key))
	}
	nonce, err := byteArg(name, "nonce", nonceV)
	if err != nil {
		return nil, nil, err
	}
	if len(nonce) != 12 {
		return nil, nil, fmt.Errorf("%s: nonce is %d bytes; GCM takes 12", name, len(nonce))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %v", name, err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %v", name, err)
	}
	return g, nonce, nil
}

func aesGcmSeal(key, nonce, plaintext any) (any, error) {
	g, n, err := aesGcmFor("aesGcmSeal", key, nonce)
	if err != nil {
		return nil, err
	}
	pt, err := byteArg("aesGcmSeal", "plaintext", plaintext)
	if err != nil {
		return nil, err
	}
	return byteBuf(g.Seal(nil, n, pt, nil)), nil
}

func aesGcmOpenBytes(name string, key, nonce, sealed any) ([]byte, bool, error) {
	g, n, err := aesGcmFor(name, key, nonce)
	if err != nil {
		return nil, false, err
	}
	ct, err := byteArg(name, "sealed buffer", sealed)
	if err != nil {
		return nil, false, err
	}
	pt, err := g.Open(nil, n, ct, nil)
	if err != nil {
		return nil, false, nil
	}
	return pt, true, nil
}

func aesGcmAuthentic(key, nonce, sealed any) (any, error) {
	_, ok, err := aesGcmOpenBytes("aesGcmAuthentic", key, nonce, sealed)
	if err != nil {
		return nil, err
	}
	return ok, nil
}

func aesGcmOpen(key, nonce, sealed any) (any, error) {
	pt, ok, err := aesGcmOpenBytes("aesGcmOpen", key, nonce, sealed)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("aesGcmOpen: the sealed buffer does not authenticate under this key and nonce — check it with aesGcmAuthentic first")
	}
	return byteBuf(pt), nil
}
