package runtime

// Edge hardening: a per-IP token-bucket rate limiter on state-changing
// endpoints, and a per-username brute-force lockout on login. Both are in-memory
// (one process); horizontal scale (Phase 3) moves them to a shared store.

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const defaultRatePerMin = 600 // generous default; tune with FACET_RATE_LIMIT

// rateLimitFromEnv reads FACET_RATE_LIMIT (requests per minute per IP).
func rateLimitFromEnv() int {
	if v := os.Getenv("FACET_RATE_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultRatePerMin
}

// rateLimiter is a token bucket per key (client IP): the bucket refills at a
// steady rate and a request costs one token, so bursts up to the bucket size are
// allowed but the sustained rate is capped.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64 // bucket capacity
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute <= 0 {
		perMinute = defaultRatePerMin
	}
	return &rateLimiter{
		buckets: map[string]*bucket{},
		rate:    float64(perMinute) / 60.0,
		burst:   float64(perMinute),
	}
}

// allow reports whether a request from key may proceed, consuming a token.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[key]
	if b == nil {
		// Bound memory: forget all buckets if the table grows pathologically large
		// (a flood of unique source IPs). Each forgotten bucket simply refills.
		if len(l.buckets) > 100000 {
			l.buckets = map[string]*bucket{}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// lockout tracks failed logins per username and locks an account after too many
// failures within a window, foiling password-guessing.
type lockout struct {
	mu     sync.Mutex
	fails  map[string]*failRecord
	max    int           // failures before lockout
	window time.Duration // failures must cluster within this window to count
	cool   time.Duration // how long a locked account stays locked
}

type failRecord struct {
	count int
	first time.Time
	until time.Time // locked until this time (zero = not locked)
}

func newLockout() *lockout {
	return &lockout{fails: map[string]*failRecord{}, max: 5, window: 15 * time.Minute, cool: 15 * time.Minute}
}

// locked reports whether key is currently locked out.
func (l *lockout) locked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.fails[key]
	return r != nil && !r.until.IsZero() && time.Now().Before(r.until)
}

// fail records a failed attempt, locking the account once it crosses the
// threshold within the window.
func (l *lockout) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	r := l.fails[key]
	if r == nil || now.Sub(r.first) > l.window {
		r = &failRecord{first: now}
		l.fails[key] = r
	}
	r.count++
	if r.count >= l.max {
		r.until = now.Add(l.cool)
	}
}

// reset clears a key's failure record after a successful login.
func (l *lockout) reset(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}

// clientIP extracts the caller's IP for rate-limiting: the first hop of
// X-Forwarded-For when present (behind a proxy), else the connection's address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := indexByteASCII(xff, ','); i >= 0 {
			return trimSpaceASCII(xff[:i])
		}
		return trimSpaceASCII(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func indexByteASCII(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimSpaceASCII(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
