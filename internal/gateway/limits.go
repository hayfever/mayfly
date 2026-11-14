package gateway

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/ratelimit"
)

// limits holds the per-session plugin rate limiters and the in-flight call
// table. Both are in-memory: a control-plane restart loses a partial token
// bucket, which only makes the next call more likely to be admitted, and
// drops in-flight slots for calls a dead runtime will never complete. The
// ledger, not this table, is the durable record.
type limits struct {
	mu        sync.Mutex
	max       int
	logger    *slog.Logger
	bySession map[string]map[string]*ratelimit.Limiter
	inflight  map[string]time.Time // ledger key -> deadline
}

// newLimits bounds in-flight calls across every session.
func newLimits(max int, logger *slog.Logger) *limits {
	if max <= 0 {
		max = defaultMaxInflight
	}
	return &limits{
		max:       max,
		logger:    logger,
		bySession: map[string]map[string]*ratelimit.Limiter{},
		inflight:  map[string]time.Time{},
	}
}

// limiter returns the limiter for one plugin in one session, building it on
// first use. A nil rate means the plugin is unlimited, and the nil limiter is
// cached too so the manifest is read once per session.
func (l *limits) limiter(session, name string, rate *plugin.Rate) *ratelimit.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	sessionLimiters, ok := l.bySession[session]
	if !ok {
		sessionLimiters = map[string]*ratelimit.Limiter{}
		l.bySession[session] = sessionLimiters
	}
	if lim, ok := sessionLimiters[name]; ok {
		return lim
	}
	lim := buildLimiter(name, rate, l.logger)
	sessionLimiters[name] = lim
	return lim
}

// buildLimiter converts one manifest rate into a token bucket. A rate the
// manifest parser would have rejected is treated as unlimited and reported
// once, so a bad bundle cannot block every call.
func buildLimiter(name string, rate *plugin.Rate, logger *slog.Logger) *ratelimit.Limiter {
	if rate == nil || rate.Requests <= 0 {
		return nil
	}
	interval, err := time.ParseDuration(rate.Interval)
	if err != nil {
		if logger != nil {
			logger.Warn("plugin rate limit ignored", "plugin", name, "interval", rate.Interval, "error", err)
		}
		return nil
	}
	burst := rate.Burst
	if burst <= 0 {
		burst = rate.Requests
	}
	return ratelimit.New(rate.Requests, interval, burst)
}

// reserve takes one in-flight slot for key until deadline. It prunes slots
// whose dispatch outlived its timeout, which is what a runtime that died
// mid-call leaves behind.
func (l *limits) reserve(key string, now, deadline time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, until := range l.inflight {
		if now.After(until) {
			delete(l.inflight, k)
		}
	}
	if len(l.inflight) >= l.max {
		return false
	}
	l.inflight[key] = deadline
	return true
}

// release frees the slot of a completed or abandoned dispatch.
func (l *limits) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.inflight, key)
}

// dropSession forgets one session's limiters and in-flight slots. The ledger
// keeps the records until the journal is deleted, so a slot released here is
// released only because the session is finalized.
func (l *limits) dropSession(session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.bySession, session)
	prefix := session + "/"
	for key := range l.inflight {
		if strings.HasPrefix(key, prefix) {
			delete(l.inflight, key)
		}
	}
}
