package httpapi

import (
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Failed authentication is limited per client. A token bucket refills at
// authFailureRate per authFailureInterval up to authFailureBurst, so a burst
// of guesses is refused while ordinary retries still work.
const (
	authFailureRate     = 10
	authFailureInterval = time.Minute
	authFailureBurst    = 20
	// authFailureMaxKeys bounds memory. A public listener must not let an
	// attacker grow one bucket per source address without limit.
	authFailureMaxKeys = 4096
	// authFailureIdleTTL forgets a source that has not failed recently.
	authFailureIdleTTL = 10 * time.Minute
	// maxAuthLimiterKeyLength bounds a key copied from RemoteAddr or a header.
	maxAuthLimiterKeyLength = 128
)

type authFailureBucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

// authFailureLimiter holds one bucket per client key. Only failed
// authentication reaches it; a valid credential never asks for a token.
type authFailureLimiter struct {
	mu      sync.Mutex
	buckets map[string]*authFailureBucket
	now     func() time.Time
	max     int
	idle    time.Duration
	rate    float64
	burst   float64
}

func newAuthFailureLimiter() *authFailureLimiter {
	return &authFailureLimiter{
		buckets: map[string]*authFailureBucket{},
		now:     time.Now,
		max:     authFailureMaxKeys,
		idle:    authFailureIdleTTL,
		rate:    float64(authFailureRate) / authFailureInterval.Seconds(),
		burst:   authFailureBurst,
	}
}

// fail records one failed authentication for key. It reports whether the
// attempt may proceed and, when it may not, how long the caller should wait.
func (l *authFailureLimiter) fail(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	bucket := l.buckets[key]
	if bucket != nil {
		if now.Sub(bucket.seen) > l.idle {
			delete(l.buckets, key)
			bucket = nil
		} else {
			elapsed := now.Sub(bucket.last).Seconds()
			if elapsed > 0 {
				bucket.tokens = math.Min(l.burst, bucket.tokens+elapsed*l.rate)
				bucket.last = now
			}
		}
	}
	if bucket == nil {
		if len(l.buckets) >= l.max {
			l.evict(now)
		}
		bucket = &authFailureBucket{tokens: l.burst, last: now}
		l.buckets[key] = bucket
	}
	bucket.seen = now
	if bucket.tokens < 1 {
		wait := time.Duration((1 - bucket.tokens) / l.rate * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait
	}
	bucket.tokens--
	return true, 0
}

// evict drops idle buckets first, then the least recently seen one, so the map
// never grows past max. The caller holds mu.
func (l *authFailureLimiter) evict(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for key, bucket := range l.buckets {
		if now.Sub(bucket.seen) > l.idle {
			delete(l.buckets, key)
			continue
		}
		if oldestKey == "" || bucket.seen.Before(oldest) {
			oldestKey, oldest = key, bucket.seen
		}
	}
	if len(l.buckets) >= l.max && oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

// authLimiterKey is the bucket key for one failed authentication. By default
// it is the RemoteAddr host; X-Forwarded-For is used only when the operator
// states that a trusted proxy sets it.
func (s *Server) authLimiterKey(r *http.Request) string {
	if s.cfg.Server.TrustForwardedFor {
		if key, ok := forwardedLimiterKey(r.Header.Get("X-Forwarded-For")); ok {
			return key
		}
	}
	return remoteHostKey(r.RemoteAddr)
}

// forwardedLimiterKey takes the first X-Forwarded-For hop. It accepts any
// non-empty value so a malformed proxy header cannot crash a request; the key
// is bounded.
func forwardedLimiterKey(header string) (string, bool) {
	first, _, _ := strings.Cut(header, ",")
	first = strings.TrimSpace(first)
	if first == "" {
		return "", false
	}
	if len(first) > maxAuthLimiterKeyLength {
		first = first[:maxAuthLimiterKeyLength]
	}
	return first, true
}

// remoteHostKey drops the port from RemoteAddr. A value without a port, as in
// a direct handler test, is used as is.
func remoteHostKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		if len(host) > maxAuthLimiterKeyLength {
			return host[:maxAuthLimiterKeyLength]
		}
		return host
	}
	if remoteAddr == "" {
		return "unknown"
	}
	if len(remoteAddr) > maxAuthLimiterKeyLength {
		return remoteAddr[:maxAuthLimiterKeyLength]
	}
	return remoteAddr
}
