package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/metrics"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/queue"
)

func TestNewHubAppliesSessionLimits(t *testing.T) {
	cfg := config.Default()
	cfg.Sessions.EventBuffer = 100
	cfg.Sessions.EventBufferBytes = 4096
	h := newHub(cfg)
	if got := h.ByteBudget(); got != 4096 {
		t.Fatalf("hub budget = %d, want the configured 4096", got)
	}
	// The configured budget governs retention, not only the accessor: a hub
	// left on the default would keep all twenty events.
	for range 20 {
		h.Append("s", model.Event{Type: model.EventAssistantMessage, Text: strings.Repeat("x", 1000)})
	}
	if events := h.EventsAfter("s", 0); len(events) == 0 || len(events) > 4 {
		t.Fatalf("retained %d events under a 4096-byte budget", len(events))
	}

	cfg.Sessions.EventBufferBytes = 0
	if got := newHub(cfg).ByteBudget(); got != hub.DefaultByteBudget {
		t.Fatalf("unset budget = %d, want the %d-byte default", got, hub.DefaultByteBudget)
	}
}

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

func TestNewLoggerWarnsOnUnknownLevel(t *testing.T) {
	orig := os.Stderr
	t.Cleanup(func() { os.Stderr = orig })

	t.Run("unknown", func(t *testing.T) {
		t.Setenv("MAYFLY_LOG_LEVEL", "verbose")
		out := captureStderr(t, func() {
			logger := newLogger()
			logger.Debug("debug-should-be-silent")
			logger.Info("info-still-emitted")
		})
		if !strings.Contains(out, `"msg":"unknown MAYFLY_LOG_LEVEL; using info"`) {
			t.Fatalf("missing unknown-level warning:\n%s", out)
		}
		if !strings.Contains(out, `"value":"verbose"`) {
			t.Fatalf("warning does not name the value:\n%s", out)
		}
		if !strings.Contains(out, `"msg":"info-still-emitted"`) {
			t.Fatalf("info log at fallback level missing:\n%s", out)
		}
		if strings.Contains(out, "debug-should-be-silent") {
			t.Fatalf("debug log was emitted at the info fallback:\n%s", out)
		}
	})

	for _, level := range []string{"", "debug", "info", "warn", "error", "WARN"} {
		t.Run("known_"+level, func(t *testing.T) {
			t.Setenv("MAYFLY_LOG_LEVEL", level)
			out := captureStderr(t, func() { _ = newLogger() })
			if strings.Contains(out, "unknown MAYFLY_LOG_LEVEL") {
				t.Fatalf("unexpected warning for %q:\n%s", level, out)
			}
		})
	}
}

// captureStderr runs fn with os.Stderr pointing at a pipe and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return buf.String()
}
