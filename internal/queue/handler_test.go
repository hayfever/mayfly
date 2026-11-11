package queue

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
)

func TestSessionHandler(t *testing.T) {
	job := Job{ID: "job-1", Options: session.CreateOptions{Prompt: "hello"}}
	sess := &model.Session{ID: "sess-1"}

	cases := []struct {
		name   string
		create func(context.Context, session.CreateOptions) (*model.Session, error)
		want   error
	}{
		{
			name:   "success",
			create: func(context.Context, session.CreateOptions) (*model.Session, error) { return sess, nil },
			want:   nil,
		},
		{
			name: "capacity becomes retry later",
			create: func(context.Context, session.CreateOptions) (*model.Session, error) {
				return nil, fmt.Errorf("%w: 4 sessions are already active", session.ErrCapacity)
			},
			want: ErrRetryLater,
		},
		{
			name: "other errors pass through",
			create: func(context.Context, session.CreateOptions) (*model.Session, error) {
				return nil, errors.New("prompt is required")
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SessionHandler(tc.create)(context.Background(), job)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v, want %v", err, tc.want)
				}
				return
			}
			if tc.name == "other errors pass through" {
				if err == nil {
					t.Fatal("error = nil, want the create error")
				}
				if errors.Is(err, ErrRetryLater) {
					t.Fatalf("error = %v, must not be ErrRetryLater", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
		})
	}
}
