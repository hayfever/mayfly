package ratelimit

import (
	"testing"
	"time"
)

func TestLimiterDrainsBurstThenRefills(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	l := New(2, time.Minute, 3)
	l.now = func() time.Time { return clock }
	l.last = clock

	for i := range 3 {
		if !l.Allow() {
			t.Fatalf("burst request %d was rejected", i)
		}
	}
	if l.Allow() {
		t.Fatal("request above the burst was allowed")
	}
	if got := l.RetryAfter(); got != 30*time.Second {
		t.Fatalf("retry after = %s, want 30s", got)
	}

	clock = clock.Add(30 * time.Second)
	if !l.Allow() {
		t.Fatal("one token must refill after 30s at 2 per minute")
	}
	if l.Allow() {
		t.Fatal("a second request must wait for the next refill")
	}

	clock = clock.Add(time.Hour)
	for i := range 3 {
		if !l.Allow() {
			t.Fatalf("refilled request %d was rejected", i)
		}
	}
	if l.Allow() {
		t.Fatal("bucket must not refill above the burst")
	}
}

func TestNilLimiterAllowsEverything(t *testing.T) {
	var l *Limiter
	if !l.Allow() || l.RetryAfter() != 0 {
		t.Fatal("a nil limiter must not rate limit")
	}
	if New(0, time.Minute, 10) != nil {
		t.Fatal("limit 0 must disable the limiter")
	}
	if New(10, 0, 10) != nil {
		t.Fatal("interval 0 must disable the limiter")
	}
}
