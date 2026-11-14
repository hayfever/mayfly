package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Handler turns one job into a session. The result is stored with the job so a
// sender can find the session. Returning ErrRetryLater requeues the job without
// counting an attempt, which is how a worker backs off when the session cap is
// full.
type Handler func(ctx context.Context, job Job) (Result, error)

// Worker claims jobs and runs them with bounded concurrency. Run blocks until
// the context is cancelled and every in-flight job has finished.
type Worker struct {
	store       Store
	handler     Handler
	logger      *slog.Logger
	concurrency int
	free        func() int
	poll        time.Duration
	timeout     time.Duration
	timeouts    map[Kind]time.Duration
}

// DefaultJobTimeout bounds one handler call when no per-kind timeout is set.
const DefaultJobTimeout = 5 * time.Minute

// SetTimeout bounds handler calls for jobs of one kind. Source jobs fetch and
// diff external pages before they create a session, so they usually need
// longer than session jobs. A non-positive d restores the default.
func (w *Worker) SetTimeout(kind Kind, d time.Duration) {
	if d <= 0 {
		delete(w.timeouts, kind)
		return
	}
	w.timeouts[kind] = d
}

func (w *Worker) timeoutFor(job Job) time.Duration {
	if d, ok := w.timeouts[job.JobKind()]; ok {
		return d
	}
	return w.timeout
}

// NewWorker builds a worker. free reports how many sessions may start now; nil
// means only concurrency limits the worker.
func NewWorker(store Store, handler Handler, concurrency int, free func() int, poll time.Duration, logger *slog.Logger) *Worker {
	if concurrency <= 0 {
		concurrency = 1
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	return &Worker{
		store:       store,
		handler:     handler,
		logger:      logger,
		concurrency: concurrency,
		free:        free,
		poll:        poll,
		timeout:     DefaultJobTimeout,
		timeouts:    map[Kind]time.Duration{},
	}
}

// Run claims and processes jobs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	sem := make(chan struct{}, w.concurrency)
	var wg sync.WaitGroup
	for {
		if ctx.Err() != nil {
			wg.Wait()
			return
		}
		// Handlers still running may not have reserved their session yet,
		// so count them against both the worker and the session cap.
		inflight := len(sem)
		limit := w.concurrency - inflight
		if w.free != nil {
			if available := w.free() - inflight; available < limit {
				limit = available
			}
		}
		if limit <= 0 {
			if !sleep(ctx, w.poll) {
				wg.Wait()
				return
			}
			continue
		}
		leases, err := w.store.Claim(ctx, limit)
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return
			}
			w.logger.Warn("queue claim failed", "error", err)
			if !sleep(ctx, w.poll) {
				wg.Wait()
				return
			}
			continue
		}
		if len(leases) == 0 {
			if !sleep(ctx, w.poll) {
				wg.Wait()
				return
			}
			continue
		}
		for i, lease := range leases {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				// Hand back every lease that was claimed but never started.
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				for _, unstarted := range leases[i:] {
					if err := w.store.Release(releaseCtx, unstarted, 0); err != nil {
						w.logger.Warn("queue release failed", "job", unstarted.ID, "error", err)
					}
				}
				cancel()
				wg.Wait()
				return
			}
			wg.Add(1)
			go func(lease Lease) {
				defer wg.Done()
				defer func() { <-sem }()
				w.handle(ctx, lease)
			}(lease)
		}
	}
}

func (w *Worker) handle(ctx context.Context, lease Lease) {
	// Keep working on shutdown: a half-created session is worse than a slow one.
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.timeoutFor(lease.Job))
	defer cancel()
	stopHeartbeat := w.heartbeat(jobCtx, lease)
	result, err := w.handler(jobCtx, lease.Job)
	lease = stopHeartbeat()

	settleCtx, settleCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer settleCancel()
	switch {
	case err == nil:
		if ackErr := w.store.Ack(settleCtx, lease, result); ackErr != nil {
			w.logger.Warn("queue ack failed", "job", lease.ID, "error", ackErr)
		}
		w.logger.Info("job completed", "job", lease.ID, "trigger", lease.Job.Trigger)
	case errors.Is(err, ErrRetryLater):
		if releaseErr := w.store.Release(settleCtx, lease, w.poll*5); releaseErr != nil {
			w.logger.Warn("queue release failed", "job", lease.ID, "error", releaseErr)
		}
	default:
		w.logger.Warn("job failed", "job", lease.ID, "trigger", lease.Job.Trigger, "attempt", lease.Job.Attempts+1, "error", err)
		if nackErr := w.store.Nack(settleCtx, lease, err); nackErr != nil {
			w.logger.Warn("queue nack failed", "job", lease.ID, "error", nackErr)
		}
	}
}

// heartbeat renews the lease while the handler runs, so a job that takes
// longer than the visibility timeout is not claimed again mid-flight. The
// returned stop function ends renewal and hands back the latest lease, which
// the caller must use to settle the job.
func (w *Worker) heartbeat(ctx context.Context, lease Lease) func() Lease {
	ttl := time.Until(lease.Deadline)
	if ttl <= 0 {
		return func() Lease { return lease }
	}
	interval := ttl / 3
	var mu sync.Mutex
	current := lease
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			mu.Lock()
			held := current
			mu.Unlock()
			extendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interval)
			renewed, err := w.store.Extend(extendCtx, held, ttl)
			cancel()
			if err != nil {
				w.logger.Warn("queue lease renewal failed", "job", lease.ID, "error", err)
				if errors.Is(err, ErrLeaseLost) {
					return
				}
				continue
			}
			mu.Lock()
			current = renewed
			mu.Unlock()
		}
	}()
	return func() Lease {
		close(stop)
		<-done
		mu.Lock()
		defer mu.Unlock()
		return current
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
