package runtime

// The process's own configuration and entropy, as builtins:
//
//	envVar(name) -> text     the environment variable, "" when unset
//	                         (`uses io.env`: a program run as a service is
//	                         configured through its environment)
//	envSet(name) -> bool     whether it is set at all, even to "" (io.env)
//	randomBytes(n) -> bytes  n bytes from the OS's cryptographic RNG
//	                         (effectful like rand: forces server placement)

import (
	"crypto/rand"
	"fmt"
)

// maxRandomBytes bounds one randomBytes call: key and nonce material, not a
// bulk stream.
const maxRandomBytes = 1 << 16

func randomBytes(n int) (any, error) {
	if n < 0 || n > maxRandomBytes {
		return nil, fmt.Errorf("randomBytes: %d bytes asked for; it gives 0-%d", n, maxRandomBytes)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("randomBytes: %v", err)
	}
	return byteBuf(b), nil
}
