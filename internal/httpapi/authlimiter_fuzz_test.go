package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hayfever/mayfly/internal/config"
)

// FuzzAuthLimiterKey checks that arbitrary peer and proxy bytes produce a
// stable, bounded limiter key and never a panic.
func FuzzAuthLimiterKey(f *testing.F) {
	f.Add("", "", false)
	f.Add("192.0.2.1:4000", "", true)
	f.Add("192.0.2.1:4000", "203.0.113.5", true)
	f.Add("[2001:db8::1]:443", "2001:db8::2, 10.0.0.1", true)
	f.Add("garbage", "unknown", true)
	f.Add("192.0.2.1:4000", "not an address, another hop", false)

	f.Fuzz(func(t *testing.T, remote, forwarded string, trust bool) {
		cfg := config.Default()
		cfg.Server.TrustForwardedFor = trust
		req := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
		req.RemoteAddr = remote
		if forwarded != "" {
			req.Header.Set("X-Forwarded-For", forwarded)
		}
		s := &Server{cfg: cfg}

		key := s.authLimiterKey(req)
		if key == "" {
			t.Fatal("limiter key is empty")
		}
		if len(key) > maxAuthLimiterKeyLength {
			t.Fatalf("limiter key length = %d, want <= %d", len(key), maxAuthLimiterKeyLength)
		}
		if again := s.authLimiterKey(req); again != key {
			t.Fatalf("limiter key is not stable: %q then %q", key, again)
		}

		want := remoteHostKey(remote)
		if trust {
			if first, ok := forwardedLimiterKey(forwarded); ok {
				want = first
			}
		}
		if key != want {
			t.Fatalf("key = %q, want %q", key, want)
		}
	})
}
