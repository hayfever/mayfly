// Package queue accepts webhook work durably and hands it to workers. A
// webhook only writes a job and returns; a worker creates the session later, so
// a slow cluster cannot make a sender time out.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
)

var (
	// ErrDuplicate means a job with this idempotency key was already accepted.
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
	// ErrUnreadable means a stored job payload does not decode with this
	// build. The job stays in place so an operator can inspect or delete it.
	ErrUnreadable = errors.New("job payload is unreadable")
)

// UnreadableError reports one stored job whose payload does not decode. Its
// cause is the decoder error, so the message names the field or version that
// failed.
type UnreadableError struct {
	ID    string
	Cause error
}

func (e *UnreadableError) Error() string {
	return "job " + e.ID + " payload is unreadable: " + e.Cause.Error()
}

// Unwrap returns the decoder error.
func (e *UnreadableError) Unwrap() error { return e.Cause }

// Is reports ErrUnreadable, so callers can match without the decoder error.
func (e *UnreadableError) Is(target error) bool { return target == ErrUnreadable }

// clock reports the current time. Each store reads one clock for every lease,
// retry, and expiry decision, so a test can move time forward without waiting
// for the wall clock.
type clock func() time.Time

// systemClock is the clock a store built by New uses.
func systemClock() time.Time { return time.Now().UTC() }

// Kind tells a worker which handler owns a job.
type Kind string

const (
	// KindSession creates a session from the request in Options.
	KindSession Kind = "session.create"
	// KindSource runs a scheduled data source and creates a session only when
	// the run has changes the schedule triggers on.
	KindSource Kind = "source.run"
)

// Job is one accepted unit of work. Its JSON form is the versioned envelope in
// api/job.v1.schema.json; see MarshalJSON and UnmarshalJSON in envelope.go.
type Job struct {
	ID             string
	Kind           Kind
	IdempotencyKey string
	Trigger        string
	Options        session.CreateOptions
	Source         *SourceJob
	AcceptedAt     time.Time
	Attempts       int
	Metadata       map[string]string
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
	// Accept stores a job once. A repeated idempotency key returns ErrDuplicate.
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

// NewJob builds a job with a stable ID. The delivery argument is the job's
// idempotency key: the same key makes Accept idempotent, and without one every
// request is a new job.
func NewJob(trigger, delivery string, options session.CreateOptions, now time.Time) Job {
	id := deriveJobID(KindSession, trigger, delivery)
	options.Trigger.Delivery = delivery
	options.Trigger.JobID = id
	options.IdempotencyKey = id
	return Job{
		ID:             id,
		Kind:           KindSession,
		IdempotencyKey: delivery,
		Trigger:        trigger,
		Options:        options,
		AcceptedAt:     now.UTC(),
	}
}

// NewSourceJob builds a scheduled source run. The delivery key makes a slot
// idempotent: the same slot accepted twice returns ErrDuplicate.
func NewSourceJob(delivery string, src SourceJob, now time.Time) Job {
	id := deriveJobID(KindSource, "", delivery)
	return Job{
		ID:             id,
		Kind:           KindSource,
		IdempotencyKey: delivery,
		Trigger:        "schedule",
		Options: session.CreateOptions{
			Trigger: model.TriggerInfo{
				Kind:     "schedule",
				Name:     src.Schedule,
				Event:    "schedule",
				Delivery: delivery,
				JobID:    id,
			},
			IdempotencyKey: id,
		},
		Source:     &src,
		AcceptedAt: now.UTC(),
	}
}

// dedupeKey returns the key a store records and compares for a job, or "" when
// the job has no idempotency key. It is the producer's raw key, scoped by the
// trigger name the way deriveJobID scopes a derived id, so the same key on
// another trigger is separate work. A producer that supplies its own job id
// still dedupes on this key.
func dedupeKey(job Job) string {
	if job.IdempotencyKey == "" {
		return ""
	}
	scope := job.Trigger
	if name := job.Options.Trigger.Name; name != "" {
		scope = name
	}
	if job.JobKind() == KindSource {
		scope = "schedule"
	}
	return scope + "\x00" + job.IdempotencyKey
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
