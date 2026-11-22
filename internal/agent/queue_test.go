package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/model"
)

// TestEnqueueUserWaitsForRoom covers the rule that a full queue applies
// backpressure. The control plane already made the message durable and
// acknowledged it, so the runtime must not discard it while it lives.
func TestEnqueueUserWaitsForRoom(t *testing.T) {
	ch := make(chan model.Event, 1)
	ctx := context.Background()
	if !enqueueUser(ctx, ch, model.Event{Seq: 1}) {
		t.Fatal("enqueue into an empty queue was refused")
	}

	done := make(chan bool, 1)
	go func() { done <- enqueueUser(ctx, ch, model.Event{Seq: 2}) }()
	select {
	case <-done:
		t.Fatal("enqueue did not wait for a full queue")
	case <-time.After(50 * time.Millisecond):
	}
	if got := <-ch; got.Seq != 1 {
		t.Fatalf("first dequeued event = %+v", got)
	}
	if ok := <-done; !ok {
		t.Fatal("enqueue was refused after room appeared")
	}
	if got := <-ch; got.Seq != 2 {
		t.Fatalf("second dequeued event = %+v", got)
	}
}

// TestEnqueueUserStopsOnShutdown covers the only allowed drop: the runtime is
// shutting down and the control plane will redeliver the message.
func TestEnqueueUserStopsOnShutdown(t *testing.T) {
	ch := make(chan model.Event, 1)
	ch <- model.Event{Seq: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- enqueueUser(ctx, ch, model.Event{Seq: 2}) }()
	select {
	case <-done:
		t.Fatal("enqueue did not wait for a full queue")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("enqueue succeeded after shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue did not stop on shutdown")
	}
}
