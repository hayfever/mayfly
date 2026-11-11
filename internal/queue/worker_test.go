package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

func workerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startWorker(t *testing.T, worker *Worker) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	return cancel, done
}

func TestWorkerRunsAndSettlesJobs(t *testing.T) {
	// A long retry delay keeps the worker from re-running the failed job
	// while the test inspects the settled state.
	cfg := testConfig(t)
	cfg.RetryBase = config.Duration(300 * time.Millisecond)
	store, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var mu sync.Mutex
	handled := map[string]int{}
	handler := func(_ context.Context, job Job) error {
		mu.Lock()
		handled[job.Options.Prompt]++
		mu.Unlock()
		if job.Options.Prompt == "fail" {
			return errors.New("boom")
		}
		return nil
	}
	ok := testJob("ok-delivery")
	ok.Options.Prompt = "ok"
	bad := testJob("bad-delivery")
	bad.Options.Prompt = "fail"
	if err := store.Accept(context.Background(), ok); err != nil {
		t.Fatal(err)
	}
	if err := store.Accept(context.Background(), bad); err != nil {
		t.Fatal(err)
	}

	cancel, done := startWorker(t, NewWorker(store, handler, 2, nil, 10*time.Millisecond, workerLogger()))
	defer cancel()

	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return handled["ok"] == 1 && handled["fail"] == 1
	})

	// Stop the worker so it cannot re-claim the failed job while the
	// settled state is inspected.
	cancel()
	<-done

	mu.Lock()
	failCount := handled["fail"]
	mu.Unlock()
	if failCount != 1 {
		t.Fatalf("failed job ran %d times, want 1", failCount)
	}

	// The successful job is gone; the failed one waits for its retry.
	waitFor(t, 2*time.Second, func() bool {
		stats, err := store.Stats(context.Background())
		return err == nil && stats.Pending == 1 && stats.Dead == 0
	})
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Dead != 0 {
		t.Fatalf("job buried before its attempts ran out: %+v", stats)
	}
	lease := claimOne(t, store)
	if lease.Job.Attempts != 1 {
		t.Fatalf("attempts after one failure = %d, want 1", lease.Job.Attempts)
	}
	if err := store.Ack(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerHonorsCapacity(t *testing.T) {
	store, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Accept(context.Background(), testJob("waiting")); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	slots := 0
	runs := 0
	handler := func(_ context.Context, _ Job) error {
		mu.Lock()
		runs++
		mu.Unlock()
		return nil
	}
	worker := NewWorker(store, handler, 2, func() int {
		mu.Lock()
		defer mu.Unlock()
		return slots
	}, 10*time.Millisecond, workerLogger())
	cancel, done := startWorker(t, worker)
	defer func() {
		cancel()
		<-done
	}()

	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	if runs != 0 {
		mu.Unlock()
		t.Fatal("worker ran a job with no free slot")
	}
	slots = 1
	mu.Unlock()
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return runs == 1
	})
}

func TestWorkerReleaseDoesNotCountAttempt(t *testing.T) {
	store, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Accept(context.Background(), testJob("deferred")); err != nil {
		t.Fatal(err)
	}

	var once sync.Once
	handler := func(_ context.Context, _ Job) error {
		first := false
		once.Do(func() { first = true })
		if first {
			return ErrRetryLater
		}
		return nil
	}
	cancel, done := startWorker(t, NewWorker(store, handler, 1, nil, 10*time.Millisecond, workerLogger()))
	waitFor(t, 2*time.Second, func() bool {
		stats, err := store.Stats(context.Background())
		return err == nil && stats.Pending == 1 && stats.Dead == 0
	})
	lease := claimOne(t, store)
	if lease.Job.Attempts != 0 {
		t.Fatalf("release counted an attempt: %d", lease.Job.Attempts)
	}
	if err := store.Ack(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
}

func TestWorkerStopsOnCancel(t *testing.T) {
	store, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	worker := NewWorker(store, func(context.Context, Job) error { return nil }, 1, nil, 10*time.Millisecond, workerLogger())
	cancel, done := startWorker(t, worker)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
