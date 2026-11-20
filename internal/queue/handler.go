package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
)

// SessionHandler turns a job into a session through create. A full session
// cap becomes ErrRetryLater, so the job waits without spending an attempt.
// The job id is the session idempotency key, so a job that runs twice between
// Create and Ack does not start a second session.
func SessionHandler(create func(context.Context, session.CreateOptions) (*model.Session, error)) Handler {
	return func(ctx context.Context, job Job) (Result, error) {
		options := job.Options
		if options.IdempotencyKey == "" {
			options.IdempotencyKey = job.ID
		}
		sess, err := create(ctx, options)
		if err != nil {
			if errors.Is(err, session.ErrCapacity) {
				return Result{}, fmt.Errorf("%w: %w", ErrRetryLater, err)
			}
			return Result{}, err
		}
		return Result{SessionID: sess.ID}, nil
	}
}

// Composite routes jobs to the handler for their kind.
func Composite(sessions, sources Handler) Handler {
	return func(ctx context.Context, job Job) (Result, error) {
		switch job.JobKind() {
		case KindSource:
			if sources == nil {
				return Result{}, errors.New("no handler for scheduled source jobs")
			}
			return sources(ctx, job)
		default:
			if sessions == nil {
				return Result{}, errors.New("no handler for session jobs")
			}
			return sessions(ctx, job)
		}
	}
}
