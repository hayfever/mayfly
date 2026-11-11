package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/ratelimit"
	"github.com/hayfever/mayfly/internal/session"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWebhookRateLimited(t *testing.T) {
	cfg := config.Default()
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	s := New(cfg, nil, testLogger())
	s.limiters["generic"] = ratelimit.New(1, time.Hour, 1)
	if !s.limiters["generic"].Allow() {
		t.Fatal("burst token was not available")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hooks/generic", strings.NewReader(`{"prompt":"x"}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestUnsignedWebhooksDoNotSpendRateLimit(t *testing.T) {
	cfg := config.Default()
	cfg.Triggers = []config.TriggerConfig{{Name: "github", Type: "github", Secret: "s3cret"}}
	s := New(cfg, nil, testLogger())
	s.limiters["github"] = ratelimit.New(1, time.Hour, 1)

	for i := range 10 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(`{"action":"opened"}`))
		req.Header.Set("X-GitHub-Event", "push")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d status = %d, want 401", i, rec.Code)
		}
	}
	if !s.limiters["github"].Allow() {
		t.Fatal("unsigned traffic consumed the trigger rate limit")
	}
}

func TestReadyReflectsManagerState(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.Image = "example/agent:1"
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.New(cfg, store, hub.New(10), nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, mgr, testLogger())
	handler := s.Handler()

	check := func(want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != want {
			t.Fatalf("readyz = %d body=%s", rec.Code, rec.Body.String())
		}
	}

	check(http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	check(http.StatusOK)
	mgr.Shutdown(context.Background())
	check(http.StatusServiceUnavailable)
}
