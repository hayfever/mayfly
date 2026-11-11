// Package queue accepts webhook work durably and hands it to workers. A
// webhook only writes a job and returns; a worker creates the session later, so
// a slow cluster cannot make a sender time out.
package queue

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/session"
)

var (
	// ErrDuplicate means a job with this delivery key was already accepted.
	ErrDuplicate = errors.New("job already accepted")
	// ErrFull means the queue holds max_jobs undelivered jobs.
	ErrFull = errors.New("queue is full")
	// ErrRetryLater tells the worker to requeue without counting an attempt.
	ErrRetryLater = errors.New("retry later")
	// ErrLeaseLost means the lease expired and the job was claimed again, so
	// this holder may no longer reschedule it.
	ErrLeaseLost = errors.New("queue lease lost")
)

// Job is one accepted session request.
type Job struct {
	ID         string                `json:"id"`
	Delivery   string                `json:"delivery,omitempty"`
	Trigger    string                `json:"trigger"`
	Options    session.CreateOptions `json:"options"`
	AcceptedAt time.Time             `json:"accepted_at"`
	Attempts   int                   `json:"attempts"`
}

// Lease is a claim on a job. It remains invisible to other workers until Ack,
// Nack, Release, or the visibility timeout.
type Lease struct {
	ID       string
	Job      Job
	Deadline time.Time
}

// Stats describes the queue contents.
type Stats struct {
	Pending int       `json:"pending"`
	Leased  int       `json:"leased"`
	Dead    int       `json:"dead"`
	Oldest  time.Time `json:"oldest,omitempty"`
}

// Store accepts jobs and hands them to workers.
type Store interface {
	// Accept stores a job once. A repeated Delivery returns ErrDuplicate.
	Accept(ctx context.Context, job Job) error
	// Claim leases up to max due jobs.
	Claim(ctx context.Context, max int) ([]Lease, error)
	// Ack removes a completed job.
	Ack(ctx context.Context, lease Lease) error
	// Nack records a failed attempt, requeues with backoff, or buries the job
	// when max_attempts is reached.
	Nack(ctx context.Context, lease Lease, cause error) error
	// Release requeues a job without counting an attempt.
	Release(ctx context.Context, lease Lease, after time.Duration) error
	Stats(ctx context.Context) (Stats, error)
	Close() error
}

// New builds the configured store. An empty backend selects memory.
func New(ctx context.Context, cfg config.QueueConfig) (Store, error) {
	switch cfg.Backend {
	case "", "memory":
		return newMemory(cfg), nil
	case "bbolt":
		return newBolt(cfg)
	case "redis":
		return newRedis(ctx, cfg)
	default:
		return nil, errors.New("queue.backend must be memory, bbolt, or redis, got " + cfg.Backend)
	}
}

// NewJob builds a job with a stable ID. A delivery key makes Accept
// idempotent; without one every request is a new job.
func NewJob(trigger, delivery string, options session.CreateOptions, now time.Time) Job {
	id := randomID()
	if delivery != "" {
		sum := sha256.Sum256([]byte(trigger + "\x00" + delivery))
		id = "d-" + hex.EncodeToString(sum[:16])
	}
	options.Trigger.Delivery = delivery
	return Job{
		ID:         id,
		Delivery:   delivery,
		Trigger:    trigger,
		Options:    options,
		AcceptedAt: now.UTC(),
	}
}

// Backoff returns the delay before the next attempt: base doubling per attempt,
// capped at ten minutes.
func Backoff(attempts int, base time.Duration) time.Duration {
	if attempts < 1 {
		return 0
	}
	if base <= 0 {
		base = 5 * time.Second
	}
	delay := base << uint(attempts-1)
	if delay > 10*time.Minute || delay <= 0 {
		delay = 10 * time.Minute
	}
	return delay
}

// due reports whether a job with this ready time can be claimed.
func due(readyAt, now time.Time) bool {
	return !readyAt.After(now)
}

func randomID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}
