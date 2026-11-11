package httpapi

import (
	"context"
	"encoding/json"
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
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/ratelimit"
	"github.com/hayfever/mayfly/internal/session"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testQueue(t *testing.T) queue.Store {
	t.Helper()
	store, err := queue.New(context.Background(), config.Default().Queue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestWebhookRateLimited(t *testing.T) {
	cfg := config.Default()
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	s := New(cfg, nil, testQueue(t), testLogger())
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
	s := New(cfg, nil, testQueue(t), testLogger())
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
	s := New(cfg, mgr, testQueue(t), testLogger())
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

func TestWebhookDeduplicatesByDeliveryKey(t *testing.T) {
	cfg := config.Default()
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	store := testQueue(t)
	s := New(cfg, nil, store, testLogger())
	handler := s.Handler()

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/hooks/generic", strings.NewReader(`{"prompt":"one run"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "delivery-42")
		handler.ServeHTTP(rec, req)
		return rec
	}

	first := post()
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}
	second := post()
	if second.Code != http.StatusAccepted {
		t.Fatalf("duplicate status = %d body=%s", second.Code, second.Body.String())
	}
	var out struct {
		Accepted  bool `json:"accepted"`
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Accepted || !out.Duplicate {
		t.Fatalf("duplicate response = %s", second.Body.String())
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Fatalf("pending jobs = %d, want 1", stats.Pending)
	}
}
