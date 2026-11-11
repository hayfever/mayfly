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
func SessionHandler(create func(context.Context, session.CreateOptions) (*model.Session, error)) Handler {
	return func(ctx context.Context, job Job) error {
		if _, err := create(ctx, job.Options); err != nil {
			if errors.Is(err, session.ErrCapacity) {
				return fmt.Errorf("%w: %v", ErrRetryLater, err)
			}
			return err
		}
		return nil
	}
}
