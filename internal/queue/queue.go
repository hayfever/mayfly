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
	// ErrNotFound means the job is unknown, or its record has expired.
	ErrNotFound = errors.New("job not found")
)

// Kind tells a worker which handler owns a job.
type Kind string

const (
	// KindSession creates a session from the request in Options.
	KindSession Kind = "session"
	// KindSource runs a scheduled data source and creates a session only when
	// the run has changes the schedule triggers on.
	KindSource Kind = "source"
)

// Job is one accepted unit of work.
type Job struct {
	ID         string                `json:"id"`
	Kind       Kind                  `json:"kind,omitempty"`
	Delivery   string                `json:"delivery,omitempty"`
	Trigger    string                `json:"trigger"`
	Options    session.CreateOptions `json:"options"`
	Source     *SourceJob            `json:"source,omitempty"`
	AcceptedAt time.Time             `json:"accepted_at"`
	Attempts   int                   `json:"attempts"`
}

// SourceJob identifies one scheduled run of one data source.
type SourceJob struct {
	RunID    string    `json:"run_id"`
	Schedule string    `json:"schedule"`
	Source   string    `json:"source"`
	Slot     time.Time `json:"slot"`
}

// JobKind returns KindSession for jobs written before kinds existed.
func (j Job) JobKind() Kind {
	if j.Kind == "" {
		return KindSession
	}
	return j.Kind
}

// Result records what a completed job produced. The session ID is kept in the
// job record after acknowledgement so a sender can find its session.
type Result struct {
	SessionID string `json:"session_id,omitempty"`
}

// State is the lifecycle state of an accepted job.
type State string

const (
	// StateQueued means the job is claimable.
	StateQueued State = "queued"
	// StateLeased means a worker holds the job or it waits out a retry delay.
	StateLeased State = "leased"
	// StateDead means the job exhausted max_attempts.
	StateDead State = "dead"
	// StateDone means the job created a session and was acknowledged.
	StateDone State = "done"
)

// Record is one job as seen by lookup and dead-letter tooling.
type Record struct {
	Job        Job       `json:"job"`
	State      State     `json:"state"`
	Error      string    `json:"error,omitempty"`
	FailedAt   time.Time `json:"failed_at,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
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
	// Ack completes a job and keeps a short-lived record of its session.
	Ack(ctx context.Context, lease Lease, result Result) error
	// Nack records a failed attempt, requeues with backoff, or buries the job
	// when max_attempts is reached.
	Nack(ctx context.Context, lease Lease, cause error) error
	// Release requeues a job without counting an attempt.
	Release(ctx context.Context, lease Lease, after time.Duration) error
	// Extend moves a held lease's deadline to now plus ttl and returns the
	// renewed lease. Later Ack, Nack, Release, and Extend calls must use the
	// returned lease. It returns ErrLeaseLost when the lease already expired
	// and another claim took the job.
	Extend(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error)
	// Get returns a job record, including recently completed jobs.
	Get(ctx context.Context, id string) (Record, bool, error)
	// Dead returns buried jobs, newest first.
	Dead(ctx context.Context, limit int) ([]Record, error)
	// Retry moves a buried job back to the queue with its attempt count reset.
	Retry(ctx context.Context, id string) error
	// Delete removes a buried job.
	Delete(ctx context.Context, id string) error
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
	options.Trigger.JobID = id
	return Job{
		ID:         id,
		Kind:       KindSession,
		Delivery:   delivery,
		Trigger:    trigger,
		Options:    options,
		AcceptedAt: now.UTC(),
	}
}

// NewSourceJob builds a scheduled source run. The delivery key makes a slot
// idempotent: the same slot accepted twice returns ErrDuplicate.
func NewSourceJob(delivery string, src SourceJob, now time.Time) Job {
	id := randomID()
	if delivery != "" {
		sum := sha256.Sum256([]byte("schedule\x00" + delivery))
		id = "s-" + hex.EncodeToString(sum[:16])
	}
	return Job{
		ID:         id,
		Kind:       KindSource,
		Delivery:   delivery,
		Trigger:    "schedule",
		Source:     &src,
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
