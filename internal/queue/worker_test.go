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
	handler := func(_ context.Context, job Job) (Result, error) {
		mu.Lock()
		handled[job.Options.Prompt]++
		mu.Unlock()
		if job.Options.Prompt == "fail" {
			return Result{}, errors.New("boom")
		}
		return Result{SessionID: "sess-" + job.ID}, nil
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
	if err := store.Ack(context.Background(), lease, Result{}); err != nil {
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
	handler := func(_ context.Context, _ Job) (Result, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		return Result{}, nil
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
	handler := func(_ context.Context, _ Job) (Result, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			return Result{}, ErrRetryLater
		}
		return Result{}, nil
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
	if err := store.Ack(context.Background(), lease, Result{}); err != nil {
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
	worker := NewWorker(store, func(context.Context, Job) (Result, error) { return Result{}, nil }, 1, nil, 10*time.Millisecond, workerLogger())
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

func TestWorkerDoesNotClaimPastFreeSlots(t *testing.T) {
	store, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for range 10 {
		if err := store.Accept(context.Background(), testJob("")); err != nil {
			t.Fatal(err)
		}
	}
	const slots = 2
	var mu sync.Mutex
	running, peak, reserved, retries, done := 0, 0, 0, 0, 0
	handler := func(_ context.Context, _ Job) (Result, error) {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		defer func() { mu.Lock(); running--; mu.Unlock() }()
		time.Sleep(5 * time.Millisecond) // work done before the session is reserved
		mu.Lock()
		if reserved >= slots {
			retries++
			mu.Unlock()
			return Result{}, ErrRetryLater
		}
		reserved++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		reserved--
		done++
		mu.Unlock()
		return Result{}, nil
	}
	free := func() int { mu.Lock(); defer mu.Unlock(); return slots - reserved }
	cancel, stopped := startWorker(t, NewWorker(store, handler, 8, free, 5*time.Millisecond, workerLogger()))
	waitFor(t, 3*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return done == 10 })
	cancel()
	<-stopped
	mu.Lock()
	defer mu.Unlock()
	if peak > slots || retries != 0 {
		t.Fatalf("peak handlers = %d (want <= %d), capacity retries = %d (want 0)", peak, slots, retries)
	}
}

func TestWorkerRecordsSessionForLookup(t *testing.T) {
	cfg := testConfig(t)
	cfg.TombstoneTTL = config.Duration(time.Hour)
	store, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	job := testJob("correlate")
	if err := store.Accept(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	handler := func(context.Context, Job) (Result, error) {
		return Result{SessionID: "sess-42"}, nil
	}
	cancel, done := startWorker(t, NewWorker(store, handler, 1, nil, 10*time.Millisecond, workerLogger()))
	defer func() {
		cancel()
		<-done
	}()
	waitFor(t, time.Second, func() bool {
		record, ok, err := store.Get(context.Background(), job.ID)
		return err == nil && ok && record.State == StateDone && record.SessionID == "sess-42"
	})
}

func TestWorkerRenewsLeaseForSlowJob(t *testing.T) {
	for name, store := range backends(t) {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			runs := 0
			handler := func(context.Context, Job) (Result, error) {
				mu.Lock()
				runs++
				mu.Unlock()
				// Four visibility timeouts: without renewal a second claim
				// would start the job again.
				time.Sleep(200 * time.Millisecond)
				return Result{SessionID: "slow"}, nil
			}
			job := testJob("slow-delivery")
			if err := store.Accept(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			cancel, done := startWorker(t, NewWorker(store, handler, 2, nil, 5*time.Millisecond, workerLogger()))
			defer func() {
				cancel()
				<-done
			}()
			waitFor(t, 2*time.Second, func() bool {
				record, ok, err := store.Get(context.Background(), job.ID)
				return err == nil && ok && record.SessionID == "slow"
			})
			mu.Lock()
			defer mu.Unlock()
			if runs != 1 {
				t.Fatalf("handler ran %d times, want 1", runs)
			}
		})
	}
}

func TestWorkerSourceTimeout(t *testing.T) {
	store, err := New(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	worker := NewWorker(store, nil, 1, nil, time.Millisecond, workerLogger())
	worker.SetTimeout(KindSource, time.Hour)
	if got := worker.timeoutFor(Job{Kind: KindSource}); got != time.Hour {
		t.Fatalf("source timeout = %v", got)
	}
	if got := worker.timeoutFor(Job{}); got != DefaultJobTimeout {
		t.Fatalf("session timeout = %v", got)
	}
}
