package runtime

// Multi-factor authentication — time-based one-time passwords (TOTP, RFC 6238),
// the algorithm every authenticator app implements. Enrollment mints a random
// shared secret (stored @secret, encrypted at rest); login then demands a
// 6-digit code derived from that secret and the current 30-second window. No
// external service is involved — it is pure, offline cryptography.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const totpPeriod = 30 // seconds per code window

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret mints a fresh 160-bit base32 shared secret.
func newTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("facet: rng failure: " + err.Error())
	}
	return base32NoPad.EncodeToString(b)
}

// totpCode is the 6-digit code for a secret at a given time.
func totpCode(secret string, t time.Time) string {
	key, err := base32NoPad.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return ""
	}
	return hotp(key, uint64(t.Unix())/totpPeriod)
}

// hotp is the HMAC-based one-time password (RFC 4226) that TOTP counts on.
func hotp(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])) % 1_000_000
	return fmt.Sprintf("%06d", code)
}

// totpValid checks a presented code against the secret, allowing one window of
// clock skew on either side, in constant time.
func totpValid(secret, code string, t time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	for _, skew := range []time.Duration{0, -totpPeriod * time.Second, totpPeriod * time.Second} {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, t.Add(skew))), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// otpauthURI is the standard provisioning URI an authenticator app reads (from a
// QR code) to add the account.
func otpauthURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", fmt.Sprintf("%d", totpPeriod))
	return "otpauth://totp/" + label + "?" + v.Encode()
}
