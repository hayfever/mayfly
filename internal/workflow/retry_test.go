package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/sources"
	"github.com/hayfever/mayfly/internal/state"
)

// capacitySessions reports a full session cap for the first full calls, then
// creates sessions. Like the session manager, a repeated idempotency key
// returns the session it already created.
type capacitySessions struct {
	mu    sync.Mutex
	full  int
	calls []session.CreateOptions
	byKey map[string]*model.Session
}

func (c *capacitySessions) Create(_ context.Context, opts session.CreateOptions) (*model.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, opts)
	if c.full > 0 {
		c.full--
		return nil, fmt.Errorf("%w: full", session.ErrCapacity)
	}
	if existing, ok := c.byKey[opts.IdempotencyKey]; ok && opts.IdempotencyKey != "" {
		return existing, nil
	}
	if c.byKey == nil {
		c.byKey = map[string]*model.Session{}
	}
	sess := &model.Session{ID: fmt.Sprintf("session-%d", len(c.byKey)+1)}
	c.byKey[opts.IdempotencyKey] = sess
	return sess, nil
}

func (c *capacitySessions) created() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byKey)
}

func newTestRunner(t *testing.T, providerURL string, store *memArtifacts, sessions SessionCreator, queueStore queue.Store) *Runner {
	t.Helper()
	runner, err := New(testBundle(t, providerURL, providerURL), Options{
		Store:    store,
		Queue:    queueStore,
		State:    state.NewMemory(),
		Sessions: sessions,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func sourceJob(delivery string) queue.Job {
	slot := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	return queue.NewSourceJob(delivery, queue.SourceJob{Schedule: "watch", Source: "pages", Slot: slot}, slot)
}

// TestCapacityRetryDoesNotRerunPlan runs a changed source job through the
// real queue worker while the session cap is full. The retry must resume from
// the stored run artifact (the provider would now report "same"), create the
// session, and a repeated Create for the job must not start a second one.
func TestCapacityRetryDoesNotRerunPlan(t *testing.T) {
	providers := &providerStub{}
	server := httptest.NewServer(providers.handler(t))
	defer server.Close()
	ctx := context.Background()
	queueStore, err := queue.New(ctx, config.QueueConfig{
		Backend:           "memory",
		MaxAttempts:       5,
		RetryBase:         config.Duration(time.Millisecond),
		VisibilityTimeout: config.Duration(5 * time.Minute),
		DedupeTTL:         config.Duration(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer queueStore.Close()
	store := &memArtifacts{}
	sessions := &capacitySessions{}
	runner := newTestRunner(t, server.URL, store, sessions, queueStore)

	// The baseline run marks both pages new and creates no session.
	if _, err := runner.Handle(ctx, sourceJob("manual:watch:baseline")); err != nil {
		t.Fatal(err)
	}
	providers.mu.Lock()
	baseline := providers.scrapes
	providers.mu.Unlock()

	// The changed run meets a full session cap twice.
	sessions.full = 2
	job := sourceJob("manual:watch:changed")
	if err := queueStore.Accept(ctx, job); err != nil {
		t.Fatal(err)
	}
	worker := queue.NewWorker(queueStore, runner.Handle, 1, nil, 2*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(workerCtx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, ok, err := queueStore.Get(ctx, job.ID)
		if err == nil && ok && record.SessionID != "" {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("retried job never created a session: %+v", record)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	providers.mu.Lock()
	scrapes := providers.scrapes - baseline
	providers.mu.Unlock()
	if scrapes != 2 {
		t.Fatalf("changed job scraped %d pages across its attempts, want 2 (one plan run)", scrapes)
	}
	sessions.mu.Lock()
	calls := append([]session.CreateOptions(nil), sessions.calls...)
	sessions.mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("create calls = %d, want 3 (two at capacity, one success)", len(calls))
	}
	for _, call := range calls {
		if call.IdempotencyKey != job.ID {
			t.Fatalf("idempotency key = %q, want job ID %q", call.IdempotencyKey, job.ID)
		}
		if !strings.Contains(call.Prompt, "-Pro $10") {
			t.Fatalf("retry lost the change:\n%s", call.Prompt)
		}
	}

	// A crash between Create and Ack repeats the whole handler.
	if _, err := runner.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if n := sessions.created(); n != 1 {
		t.Fatalf("sessions created = %d, want 1", n)
	}
	providers.mu.Lock()
	if providers.scrapes-baseline != 2 {
		t.Fatalf("repeated handler ran the plan again")
	}
	providers.mu.Unlock()
}

func TestStoredArtifactMustDecode(t *testing.T) {
	providers := &providerStub{}
	server := httptest.NewServer(providers.handler(t))
	defer server.Close()
	ctx := context.Background()
	queueStore, _ := queue.New(ctx, config.QueueConfig{Backend: "memory", VisibilityTimeout: config.Duration(5 * time.Minute)})
	defer queueStore.Close()
	store := &memArtifacts{}
	runner := newTestRunner(t, server.URL, store, &capacitySessions{}, queueStore)
	job := sourceJob("manual:watch:torn")
	key := runKey("watch", job.ID)
	if _, err := store.Put(ctx, key, strings.NewReader(`{"run_id":`), 10, "", "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Handle(ctx, job); err == nil || !strings.Contains(err.Error(), "does not decode") {
		t.Fatalf("torn artifact: err = %v", err)
	}
	if providers.scrapes != 0 {
		t.Fatal("a torn artifact made the job run the plan again")
	}
	other := `{"run_id":"other","schedule":"watch","source":"pages","slot":"2026-11-13T07:00:00Z","generated_at":"2026-11-13T07:00:00Z","totals":{"pages":0,"changed":0,"new":0,"same":0,"failed":0},"documents":[]}`
	if _, err := store.Put(ctx, key, strings.NewReader(other), int64(len(other)), "", "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Handle(ctx, job); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("foreign artifact: err = %v", err)
	}
}

// failingProviders serves a working Notion query and a scrape endpoint that
// always answers with status.
func failingProviders(t *testing.T, status int) *httptest.Server {
	t.Helper()
	ok := (&providerStub{}).handler(t)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/databases/db/query", ok)
	mux.HandleFunc("POST /v2/scrape", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"success":false,"error":"nope"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestRunFailsWhenEveryPageFails(t *testing.T) {
	ctx := context.Background()
	queueStore, _ := queue.New(ctx, config.QueueConfig{Backend: "memory", VisibilityTimeout: config.Duration(5 * time.Minute)})
	defer queueStore.Close()
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusBadRequest, ErrAllFailed},
		{http.StatusUnauthorized, sources.ErrAuth},
		{http.StatusForbidden, sources.ErrAuth},
	} {
		server := failingProviders(t, tc.status)
		store := &memArtifacts{}
		sessions := &capacitySessions{}
		runner := newTestRunner(t, server.URL, store, sessions, queueStore)
		job := sourceJob(fmt.Sprintf("manual:watch:fail-%d", tc.status))
		_, err := runner.Handle(ctx, job)
		if !errors.Is(err, tc.want) {
			t.Fatalf("HTTP %d: err = %v, want %v", tc.status, err, tc.want)
		}
		if _, ok := store.values[runKey("watch", job.ID)]; ok {
			t.Fatalf("HTTP %d: a failed run wrote its artifact", tc.status)
		}
		if len(sessions.calls) != 0 {
			t.Fatalf("HTTP %d: a failed run created a session", tc.status)
		}
	}
}

func TestTickSlotUsesScheduledTime(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 13, 7, 0, 59, 0, time.UTC),
		time.Date(2026, 11, 13, 7, 3, 10, 0, time.UTC),
	} {
		if got := tickSlot("0 7 * * *", now, time.UTC); !got.Equal(time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)) {
			t.Fatalf("tick at %s: slot %s", now, got)
		}
	}
	if got := tickSlot("*/5 * * * *", time.Date(2026, 11, 13, 7, 6, 30, 0, time.UTC), time.UTC); !got.Equal(time.Date(2026, 11, 13, 7, 5, 0, 0, time.UTC)) {
		t.Fatalf("five-minute tick slot = %s", got)
	}
}
