package runtime

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"hash/fnv"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the IANA zone database, embedded: zones never depend on the host
)

// The formatting builtins a feed reads by: `ago(ts)`, `compact(n)`, `commas(n)`.
// Each is mirrored character for character in assets/facet.js, and
// runtime/format_test.go runs the shipped client's copy over the same cases —
// a count that renders "1.3K" on first paint and "1.2K" after hydration is a
// page that changes under the reader, so the arithmetic here is integer-only:
// no float formatting either side could round differently.

// clock is the render clock `ago` reads. It is a variable so a test can pin it;
// nothing else may set it.
var clock = time.Now

// iso renders a unix timestamp as an RFC 3339 UTC instant ("2026-09-21T21:40:00Z")
// — the wire form a `datetime`-typed field/parameter carries (runtime/
// contract.go's wireSchema emits `format: date-time` for it), the same way
// `money(n)` renders a `money` int as its own decimal text. `now()` is the
// int this formats: `iso(now())` is how an action populates a `datetime`
// field with the current instant.
//
// 0 is the language's unset instant (an entity's `published_at: 0` before it
// is published), so iso(0) is "" — no instant — which an optional wire field
// then leaves out entirely rather than claiming 1970.
func iso(ts int) string {
	if ts == 0 {
		return ""
	}
	return time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
}

// fromIso is iso's inverse: an RFC 3339 instant (as a client sends one) as
// unix seconds, 0 — the unset instant — for "" or anything unparseable.
func fromIso(s string) int {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return int(t.Unix())
}

// ago renders a unix timestamp relative to now the way a timeline does: "now"
// under a minute, then "5m", "20h", then a date — "Jun 3" inside the current
// year, "Jun 3, 2025" outside it. Days are not counted ("3d") because past a day
// a reader wants the date, and a 30-day boundary is a fact about a calendar
// that two clocks can disagree on at midnight.
//
// It is pure in the sense the placement calculus cares about — it writes
// nothing, it reads no state — but it reads the clock, so the server's first
// paint and the client's re-render can differ by however long the page took to
// arrive. That is what "20h" means; the client's refresh recomputes it.
func ago(ts, now int) string {
	d := now - ts
	switch {
	case d < 60:
		return "now"
	case d < 3600:
		return strconv.Itoa(d/60) + "m"
	case d < 86400:
		return strconv.Itoa(d/3600) + "h"
	}
	t := time.Unix(int64(ts), 0).UTC()
	n := time.Unix(int64(now), 0).UTC()
	s := t.Month().String()[:3] + " " + strconv.Itoa(t.Day())
	if t.Year() != n.Year() {
		s += ", " + strconv.Itoa(t.Year())
	}
	return s
}

// compact renders a count the way an engagement bar does: plain under a
// thousand, then one decimal and a unit — "1.3K", "79.8K", "604K", "240.1M",
// "2B". The decimal is dropped when it is zero, never rounded: 1999 is "1.9K",
// because the integer division both interpreters perform truncates, and a
// truncation is the same on every platform where a rounding might not be.
func compact(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	unit, suffix := 1, ""
	switch {
	case n >= 1_000_000_000:
		unit, suffix = 1_000_000_000, "B"
	case n >= 1_000_000:
		unit, suffix = 1_000_000, "M"
	case n >= 1000:
		unit, suffix = 1000, "K"
	}
	var s string
	if unit == 1 {
		s = strconv.Itoa(n)
	} else {
		tenths := n / (unit / 10)
		s = strconv.Itoa(tenths / 10)
		if tenths%10 != 0 {
			s += "." + strconv.Itoa(tenths%10)
		}
		s += suffix
	}
	if neg {
		return "-" + s
	}
	return s
}

// commas renders an integer with thousands separators: 1352 → "1,352".
func commas(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	digits := strconv.Itoa(n)
	var out []byte
	for i, c := range []byte(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// ed25519Verify reports whether sigB64 (standard base64) is a valid Ed25519
// signature by pubB64 (standard base64, 32 bytes) over message's UTF-8 bytes.
// A key or signature that does not decode is simply not valid.
func ed25519Verify(pubB64, message, sigB64 string) bool {
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubB64))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), []byte(message), sig)
}

// canonicalJSON is a value's canonical JSON text: object keys sorted, no HTML
// escaping, no trailing newline — the bytes a content id is hashed from, the
// same JSON.stringify of a key-sorted object gives.
func canonicalJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalizeRecords(v)); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

// normalizeRecords turns the runtime's record values into plain maps, whose
// keys encoding/json writes sorted.
func normalizeRecords(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = normalizeRecords(x)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalizeRecords(x)
		}
		return out
	}
	return v
}

// shuffleOrder is a seeded permutation of 0..n-1: a Fisher–Yates shuffle
// driven by math/rand seeded from the FNV-1a 64-bit hash of seed. It is a
// pure function of (seed, n) — the same seed always yields the same order,
// on every process and release — and it is exactly the order the legacy
// F33D3R ballot laid shuffled options out in, so a viewer keeps theirs.
func shuffleOrder(seed string, n int) []int {
	if n < 0 {
		n = 0
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if seed == "" || n < 2 {
		return order
	}
	h := fnv.New64a()
	h.Write([]byte(seed))
	rng := rand.New(rand.NewSource(int64(h.Sum64())))
	for i := n - 1; i > 0; i-- {
		j := rng.Intn(i + 1)
		order[i], order[j] = order[j], order[i]
	}
	return order
}

// zone is an IANA time zone by name ("" is UTC), from the zone database
// embedded in this binary (time/tzdata) — the same answer on every host,
// whatever its own /usr/share/zoneinfo holds.
func zone(name string) (*time.Location, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "UTC" {
		return time.UTC, true
	}
	loc, err := time.LoadLocation(name)
	if err != nil || name == "Local" {
		return nil, false
	}
	return loc, true
}

// fromLocal reads a wall-clock time — "2006-01-02T15:04", with optional
// ":05" seconds, or a space for the "T" — in an IANA zone, as unix seconds;
// 0 (the unset instant) when either does not read.
func fromLocal(wall, zoneName string) int {
	loc, ok := zone(zoneName)
	if !ok {
		return 0
	}
	wall = strings.TrimSpace(wall)
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, wall, loc); err == nil {
			return int(t.Unix())
		}
	}
	return 0
}

// formatIn renders unix seconds in an IANA zone with a Go reference layout
// ("Mon 3:04 PM MST" -> "Sun 1:00 PM EDT"); "" for the unset instant or an
// unknown zone.
func formatIn(ts int, zoneName, layout string) string {
	loc, ok := zone(zoneName)
	if !ok || ts == 0 {
		return ""
	}
	return time.Unix(int64(ts), 0).In(loc).Format(layout)
}

// ecdsaP256Verify reports whether sigB64URL — a WebCrypto/CryptoKit
// IEEE-P1363 signature (r||s, 64 bytes, unpadded base64url) — is a valid
// ECDSA P-256 / SHA-256 signature over message's UTF-8 bytes by the public
// key spkiB64 (a DER SubjectPublicKeyInfo, standard base64, padded or not).
func ecdsaP256Verify(spkiB64, message, sigB64URL string) bool {
	spki, err := base64.StdEncoding.DecodeString(strings.TrimSpace(spkiB64))
	if err != nil {
		if spki, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(spkiB64)); err != nil {
			return false
		}
	}
	pub, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return false
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(sigB64URL))
	if err != nil || len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(message))
	return ecdsa.Verify(ec, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}
