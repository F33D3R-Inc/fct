package runtime

// Edge hardening: a per-IP token-bucket rate limiter on state-changing
// endpoints, and a per-username brute-force lockout on login. Both are in-memory
// (one process); horizontal scale (Phase 3) moves them to a shared store.

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultRatePerMin = 600 // generous default; tune with FACET_RATE_LIMIT

// rateLimitFromEnv reads FACET_RATE_LIMIT (requests per minute per IP).
// rateLimitFromEnvClass reads FACET_RATE_LIMIT_<CLASS> (READ/WRITE/AUTH), the
// per-minute budget of one declared-api rate class, falling back to the
// global FACET_RATE_LIMIT.
func rateLimitFromEnvClass(class string) int {
	if v := os.Getenv("FACET_RATE_LIMIT_" + strings.ToUpper(class)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if v := os.Getenv("FACET_RATE_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if n, ok := rateClassDefaults[class]; ok {
		return n
	}
	return defaultRatePerMin
}

// rateClassDefaults are a declared route's per-minute budget by rate class
// when the deployment names none: reads are cheap, writes change state, and
// sign-in, sign-up and password reset are credential guesses.
var rateClassDefaults = map[string]int{"read": 300, "write": 60, "auth": 40}

// rateBurst is a class limiter's bucket: a quarter of the minute's budget, so
// a quiet client's short spike passes and a flood does not.
func rateBurst(perMinute int) int {
	if b := perMinute / 4; b > 0 {
		return b
	}
	return 1
}

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
	mu        sync.Mutex
	buckets   map[string]*bucket
	rate      float64 // tokens per second
	burst     float64 // bucket capacity
	perMinute int     // the budget as configured; X-RateLimit-Limit
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
		buckets:   map[string]*bucket{},
		rate:      float64(perMinute) / 60.0,
		burst:     float64(perMinute),
		perMinute: perMinute,
	}
}

// newClassLimiter is a declared rate class's limiter: perMinute sustained,
// with a burst of rateBurst(perMinute).
func newClassLimiter(perMinute int) *rateLimiter {
	l := newRateLimiter(perMinute)
	l.perMinute = perMinute
	l.burst = float64(rateBurst(perMinute))
	return l
}

// rateDecision is one metered request's outcome and the budget headers that
// report it: X-RateLimit-Limit (the per-minute budget), -Remaining (whole
// requests left in the bucket now) and -Reset (unix seconds when the bucket
// is full again); a refusal adds Retry-After (seconds until one request has
// refilled, at least 1).
type rateDecision struct {
	allowed    bool
	limit      int
	remaining  int
	reset      int64
	retryAfter int
}

func (d rateDecision) headers(h http.Header) {
	h.Set("X-RateLimit-Limit", strconv.Itoa(d.limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(d.remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(d.reset, 10))
	if !d.allowed {
		h.Set("Retry-After", strconv.Itoa(d.retryAfter))
	}
}

// take meters one request from key and reports the decision.
func (l *rateLimiter) take(key string) rateDecision {
	allowed := l.allow(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	d := rateDecision{allowed: allowed, limit: l.perMinute}
	now := time.Now()
	tokens := l.burst
	if b := l.buckets[key]; b != nil {
		tokens = b.tokens
	}
	d.remaining = int(tokens)
	d.reset = now.Add(time.Duration((l.burst - tokens) / l.rate * float64(time.Second))).Unix()
	if !allowed {
		wait := (1 - tokens) / l.rate
		d.retryAfter = int(wait)
		if float64(d.retryAfter) < wait {
			d.retryAfter++
		}
		if d.retryAfter < 1 {
			d.retryAfter = 1
		}
	}
	return d
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
