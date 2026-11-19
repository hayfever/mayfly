package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/metrics"
	"github.com/hayfever/mayfly/internal/queue"
)

func TestInstrumentQueueCountsAttemptResults(t *testing.T) {
	registry := metrics.New(metrics.Sources{}, metrics.BuildInfo{})
	attempts := []error{
		nil,
		fmt.Errorf("deferred: %w", queue.ErrRetryLater),
		errors.New("handler failed"),
	}
	for _, attempt := range attempts {
		handler := instrumentQueue(registry, func(context.Context, queue.Job) (queue.Result, error) {
			return queue.Result{SessionID: "s1"}, attempt
		})
		result, err := handler(context.Background(), queue.Job{})
		if !errors.Is(err, attempt) {
			t.Fatalf("error = %v, want %v", err, attempt)
		}
		if result.SessionID != "s1" {
			t.Fatalf("result = %+v", result)
		}
	}

	rec := httptest.NewRecorder()
	registry.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, line := range []string{
		`mayfly_queue_job_attempts_total{result="succeeded"} 1`,
		`mayfly_queue_job_attempts_total{result="retry_later"} 1`,
		`mayfly_queue_job_attempts_total{result="failed"} 1`,
	} {
		if !strings.Contains(body, line) {
			t.Fatalf("missing %q:\n%s", line, body)
		}
	}
}
