// Package ratelimit implements a token bucket that bounds webhook traffic.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter refills tokens at a fixed rate and holds at most burst tokens. A nil
// Limiter allows every request, which is how an unlimited trigger is wired.
type Limiter struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	refill float64 // tokens per second
	last   time.Time
	now    func() time.Time
}

// New returns a limiter that admits limit requests per interval with room for
// burst requests at once. A non-positive limit or interval disables the limit
// and returns nil.
func New(limit int, interval time.Duration, burst int) *Limiter {
	if limit <= 0 || interval <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = limit
	}
	return &Limiter{
		tokens: float64(burst),
		burst:  float64(burst),
		refill: float64(limit) / interval.Seconds(),
		last:   time.Now(),
		now:    time.Now,
	}
}

// Allow takes one token and reports whether the request may proceed.
func (l *Limiter) Allow() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.advance()
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// RetryAfter reports how long the caller should wait for the next token. It
// returns at least one second so the response can carry a whole-second hint.
func (l *Limiter) RetryAfter() time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.advance()
	if l.tokens >= 1 {
		return 0
	}
	wait := time.Duration((1 - l.tokens) / l.refill * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// advance adds the tokens earned since the last call. The caller holds mu.
func (l *Limiter) advance() {
	now := l.now()
	elapsed := now.Sub(l.last).Seconds()
	if elapsed <= 0 {
		return
	}
	l.tokens = math.Min(l.burst, l.tokens+elapsed*l.refill)
	l.last = now
}
