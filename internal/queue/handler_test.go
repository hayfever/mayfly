package queue

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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
			result, err := SessionHandler(tc.create)(context.Background(), job)
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
			if result.SessionID != sess.ID {
				t.Fatalf("session id = %q, want %q", result.SessionID, sess.ID)
			}
		})
	}
}

func TestCompositeRoutesByKind(t *testing.T) {
	ctx := context.Background()
	var calls []Kind
	record := func(kind Kind) Handler {
		return func(context.Context, Job) (Result, error) {
			calls = append(calls, kind)
			return Result{SessionID: string(kind)}, nil
		}
	}
	handler := Composite(record(KindSession), record(KindSource))

	for _, job := range []Job{
		{ID: "legacy"},
		{ID: "session", Kind: KindSession},
		{ID: "source", Kind: KindSource},
	} {
		result, err := handler(ctx, job)
		if err != nil {
			t.Fatalf("%s: %v", job.ID, err)
		}
		if result.SessionID != string(job.JobKind()) {
			t.Fatalf("%s routed to %q", job.ID, result.SessionID)
		}
	}
	if len(calls) != 3 || calls[0] != KindSession || calls[2] != KindSource {
		t.Fatalf("calls = %v", calls)
	}

	onlySessions := Composite(record(KindSession), nil)
	if _, err := onlySessions(ctx, Job{Kind: KindSource}); err == nil {
		t.Fatal("nil source handler accepted a source job")
	}
}

func TestSourceJobDeliveryIsStable(t *testing.T) {
	now := time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC)
	src := SourceJob{Schedule: "nightly", Source: "pages", Slot: now}
	first := NewSourceJob("schedule:nightly:2026-01-01T07:00:00Z", src, now)
	again := NewSourceJob("schedule:nightly:2026-01-01T07:00:00Z", src, now)
	other := NewSourceJob("schedule:nightly:2026-01-02T07:00:00Z", src, now)

	if first.ID != again.ID {
		t.Fatalf("same delivery produced %q and %q", first.ID, again.ID)
	}
	if first.ID == other.ID {
		t.Fatalf("different deliveries produced the same id %q", first.ID)
	}
	if first.Kind != KindSource || first.Source == nil || first.Source.Schedule != "nightly" {
		t.Fatalf("job = %+v", first)
	}
	if first.JobKind() != KindSource {
		t.Fatalf("JobKind = %q", first.JobKind())
	}
}

// TestSessionHandlerUsesJobIDForIdempotency covers a worker that creates the
// session and stops before Ack: the retry must ask for the same session.
func TestSessionHandlerUsesJobIDForIdempotency(t *testing.T) {
	var got session.CreateOptions
	handler := SessionHandler(func(_ context.Context, opts session.CreateOptions) (*model.Session, error) {
		got = opts
		return &model.Session{ID: "sess-1"}, nil
	})
	if _, err := handler(context.Background(), Job{ID: "job-1", Options: session.CreateOptions{Prompt: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if got.IdempotencyKey != "job-1" {
		t.Fatalf("session idempotency key = %q, want the job id", got.IdempotencyKey)
	}

	// An options key set by a producer wins.
	handler = SessionHandler(func(_ context.Context, opts session.CreateOptions) (*model.Session, error) {
		got = opts
		return &model.Session{ID: "sess-2"}, nil
	})
	if _, err := handler(context.Background(), Job{ID: "job-2", Options: session.CreateOptions{
		Prompt:         "hi",
		IdempotencyKey: "explicit",
	}}); err != nil {
		t.Fatal(err)
	}
	if got.IdempotencyKey != "explicit" {
		t.Fatalf("session idempotency key = %q, want explicit", got.IdempotencyKey)
	}
}
