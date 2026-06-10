package fa

import (
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// sameOrigin rejects cross-origin POSTs as defense-in-depth CSRF (audit H1, on
// top of the connection-id requirement). A missing Origin — same-origin
// navigation or a non-browser client — is allowed; a present Origin must match
// the request host.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a per-key token bucket with lazy refill, used to throttle
// /events per client IP (audit H2). Safe for concurrent use.
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity
	buckets map[string]*bucket
}

type bucket struct {
	n    float64
	last time.Time
}

func newRateLimiter(ratePerSec, burst float64) *rateLimiter {
	return &rateLimiter{rate: ratePerSec, burst: burst, buckets: make(map[string]*bucket)}
}

// allow consumes one token for key, returning false when the bucket is empty.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[key]
	if b == nil {
		// Opportunistic cap on map growth under a flood of distinct keys.
		if len(l.buckets) > 100000 {
			l.buckets = make(map[string]*bucket)
		}
		b = &bucket{n: l.burst, last: now}
		l.buckets[key] = b
	}
	b.n += now.Sub(b.last).Seconds() * l.rate
	if b.n > l.burst {
		b.n = l.burst
	}
	b.last = now
	if b.n < 1 {
		return false
	}
	b.n--
	return true
}
