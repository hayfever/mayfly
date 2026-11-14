package httpapi

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/ratelimit"
	"github.com/hayfever/mayfly/internal/session"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testManager(t *testing.T, cfg *config.Config) *session.Manager {
	t.Helper()
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.New(cfg, nil, store, hub.New(10), journal.NewMemory(), nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func testQueue(t *testing.T) queue.Store {
	t.Helper()
	return testQueueWith(t, config.Default().Queue)
}

func testQueueWith(t *testing.T, cfg config.QueueConfig) queue.Store {
	t.Helper()
	store, err := queue.New(context.Background(), cfg)
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
	mgr, err := session.New(cfg, nil, store, hub.New(10), journal.NewMemory(), nil, nil, testLogger())
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
	cfg.Runtime.Image = "example/agent:1"
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	store := testQueue(t)
	s := New(cfg, testManager(t, cfg), store, testLogger())
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

func TestWebhookRejectsInvalidSessionOptions(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.Image = "example/agent:1"
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	store := testQueue(t)
	s := New(cfg, testManager(t, cfg), store, testLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hooks/generic", strings.NewReader(`{"prompt":"x","agent":"no-such-agent"}`))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no-such-agent") {
		t.Fatalf("body does not name the agent: %s", rec.Body.String())
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 {
		t.Fatalf("pending jobs = %d, want 0", stats.Pending)
	}
}

func TestQueueAdminEndpoints(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "test-admin"
	qcfg := config.Default().Queue
	qcfg.Backend = "memory"
	qcfg.MaxAttempts = 1
	store := testQueueWith(t, qcfg)
	s := New(cfg, nil, store, testLogger())
	handler := s.Handler()
	ctx := context.Background()

	do := func(method, target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Authorization", "Bearer test-admin")
		handler.ServeHTTP(rec, req)
		return rec
	}

	job := queue.NewJob("generic", "", session.CreateOptions{Prompt: "will fail"}, time.Now())
	if err := store.Accept(ctx, job); err != nil {
		t.Fatal(err)
	}
	leases, err := store.Claim(ctx, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim = %v, %v", leases, err)
	}
	if err := store.Nack(ctx, leases[0], errors.New("provider said no")); err != nil {
		t.Fatal(err)
	}

	rec := do(http.MethodGet, "/api/v1/queue/dead")
	if rec.Code != http.StatusOK {
		t.Fatalf("dead list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Dead []queueRecord `json:"dead"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Dead) != 1 || list.Dead[0].ID != job.ID || list.Dead[0].State != queue.StateDead {
		t.Fatalf("dead list = %+v", list.Dead)
	}
	if !strings.Contains(list.Dead[0].Error, "provider said no") {
		t.Fatalf("dead error = %q", list.Dead[0].Error)
	}

	if rec = do(http.MethodPost, "/api/v1/queue/dead/"+job.ID+"/retry"); rec.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec = do(http.MethodGet, "/api/v1/queue/dead"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dead":[]`) {
		t.Fatalf("dead list after retry = %d %s", rec.Code, rec.Body.String())
	}

	leases, err = store.Claim(ctx, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim retried job = %+v, %v", leases, err)
	}
	if err := store.Nack(ctx, leases[0], errors.New("still failing")); err != nil {
		t.Fatal(err)
	}
	if rec = do(http.MethodDelete, "/api/v1/queue/dead/"+job.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec = do(http.MethodDelete, "/api/v1/queue/dead/"+job.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d", rec.Code)
	}

	done := queue.NewJob("generic", "", session.CreateOptions{Prompt: "works"}, time.Now())
	if err := store.Accept(ctx, done); err != nil {
		t.Fatal(err)
	}
	leases, err = store.Claim(ctx, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim done job = %+v, %v", leases, err)
	}
	if err := store.Ack(ctx, leases[0], queue.Result{SessionID: "session-7"}); err != nil {
		t.Fatal(err)
	}
	rec = do(http.MethodGet, "/api/v1/queue/jobs/"+done.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("job lookup status = %d body=%s", rec.Code, rec.Body.String())
	}
	var lookup struct {
		Job queueRecord `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lookup); err != nil {
		t.Fatal(err)
	}
	if lookup.Job.State != queue.StateDone || lookup.Job.SessionID != "session-7" || lookup.Job.Prompt != "works" {
		t.Fatalf("job lookup = %+v", lookup.Job)
	}
	if rec = do(http.MethodGet, "/api/v1/queue/jobs/missing"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job status = %d", rec.Code)
	}
}
