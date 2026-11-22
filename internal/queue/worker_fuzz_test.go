package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

// FuzzWorkerHandlerPanic feeds arbitrary bytes into a handler that panics. The
// worker must recover every panic, count each attempt, bury the poison job
// after max_attempts, and still run a healthy job.
func FuzzWorkerHandlerPanic(f *testing.F) {
	f.Add([]byte("boom"))
	f.Add([]byte(""))
	f.Add([]byte{0xff, 0xfe, 0x00, 'x'})
	f.Fuzz(func(t *testing.T, prompt []byte) {
		if len(prompt) > 256 {
			prompt = prompt[:256]
		}
		cfg := testConfig(t)
		cfg.MaxAttempts = 2
		cfg.RetryBase = config.Duration(time.Nanosecond)
		store, err := New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		poison := testJob("fuzz-poison")
		poison.Trigger = "poison"
		if err := store.Accept(context.Background(), poison); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		panics := 0
		handler := func(_ context.Context, job Job) (Result, error) {
			mu.Lock()
			defer mu.Unlock()
			if job.Trigger == "poison" {
				panics++
				panic(string(prompt))
			}
			return Result{SessionID: "healthy"}, nil
		}
		var counted int
		worker := NewWorker(store, handler, 1, nil, time.Millisecond, workerLogger())
		worker.SetPanicObserver(func(string) {
			mu.Lock()
			defer mu.Unlock()
			counted++
		})
		cancel, done := startWorker(t, worker)
		defer func() {
			cancel()
			<-done
		}()

		waitFor(t, 2*time.Second, func() bool {
			record, ok, err := store.Get(context.Background(), poison.ID)
			return err == nil && ok && record.State == StateDead
		})
		record, ok, err := store.Get(context.Background(), poison.ID)
		if err != nil || !ok {
			t.Fatalf("poison record = %+v, %v, %v", record, ok, err)
		}
		if record.Job.Attempts != cfg.MaxAttempts {
			t.Fatalf("attempts = %d, want %d", record.Job.Attempts, cfg.MaxAttempts)
		}

		healthy := testJob("fuzz-healthy")
		healthy.Trigger = "healthy"
		if err := store.Accept(context.Background(), healthy); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 2*time.Second, func() bool {
			record, ok, err := store.Get(context.Background(), healthy.ID)
			return err == nil && ok && record.State == StateDone
		})
		mu.Lock()
		defer mu.Unlock()
		if panics != cfg.MaxAttempts || counted != cfg.MaxAttempts {
			t.Fatalf("panics = %d, counted = %d, want %d each", panics, counted, cfg.MaxAttempts)
		}
	})
}
