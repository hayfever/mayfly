package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/hayfever/mayfly/internal/model"
)

type sample struct {
	labels map[string]string
	value  float64
}

func gather(t *testing.T, m *Metrics) map[string][]sample {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make(map[string][]sample, len(families))
	for _, family := range families {
		for _, metric := range family.Metric {
			out[family.GetName()] = append(out[family.GetName()], sample{
				labels: labelsOf(metric),
				value:  valueOf(metric),
			})
		}
	}
	return out
}

func labelsOf(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.Label))
	for _, pair := range metric.Label {
		labels[pair.GetName()] = pair.GetValue()
	}
	return labels
}

func valueOf(metric *dto.Metric) float64 {
	switch {
	case metric.Gauge != nil:
		return metric.Gauge.GetValue()
	case metric.Counter != nil:
		return metric.Counter.GetValue()
	case metric.Histogram != nil:
		return float64(metric.Histogram.GetSampleCount())
	default:
		return 0
	}
}

// find returns the sample that carries every label in want.
func find(t *testing.T, samples []sample, want map[string]string) float64 {
	t.Helper()
	for _, s := range samples {
		match := true
		for key, value := range want {
			if s.labels[key] != value {
				match = false
				break
			}
		}
		if match {
			return s.value
		}
	}
	t.Fatalf("no sample with labels %v", want)
	return 0
}

func sourceCounts() map[model.SessionStatus]int {
	return map[model.SessionStatus]int{
		model.StatusRunning: 3,
		model.StatusFailed:  1,
	}
}

func TestNewRegistersEveryMetric(t *testing.T) {
	m := New(Sources{
		Sessions: sourceCounts,
		Queue: func(context.Context) (QueueDepth, error) {
			return QueueDepth{Pending: 2, Leased: 1, Dead: 4}, nil
		},
	}, BuildInfo{Version: "1.2.3", Commit: "abc", Date: "today"})
	m.SessionCreated()
	m.SessionFinished(model.StatusCompleted)
	m.QueueJobAttempt(AttemptFailed)
	m.HTTPRequest("/api/v1/sessions", "GET", 200, 5*time.Millisecond)
	m.LLMRequest("gpt-test", "ok", time.Second)
	m.ToolCall("plugin__tool", "ok")

	got := gather(t, m)
	for _, name := range []string{
		"mayfly_sessions",
		"mayfly_sessions_created_total",
		"mayfly_sessions_finished_total",
		"mayfly_queue_jobs",
		"mayfly_queue_job_attempts_total",
		"mayfly_http_requests_total",
		"mayfly_http_request_duration_seconds",
		"mayfly_llm_requests_total",
		"mayfly_llm_request_duration_seconds",
		"mayfly_gateway_calls_total",
		"mayfly_build_info",
	} {
		if len(got[name]) == 0 {
			t.Errorf("%s is not registered", name)
		}
	}
	if value := find(t, got["mayfly_build_info"], map[string]string{"version": "1.2.3", "commit": "abc", "date": "today"}); value != 1 {
		t.Fatalf("build info = %v", value)
	}
}

func TestQueueGaugeFollowsStats(t *testing.T) {
	depth := QueueDepth{Pending: 2, Leased: 1, Dead: 4}
	m := New(Sources{Queue: func(context.Context) (QueueDepth, error) {
		return depth, nil
	}}, BuildInfo{})
	jobs := gather(t, m)["mayfly_queue_jobs"]
	for state, want := range map[string]float64{"pending": 2, "leased": 1, "dead": 4} {
		if value := find(t, jobs, map[string]string{"state": state}); value != want {
			t.Fatalf("%s = %v, want %v", state, value, want)
		}
	}
}

func TestQueueGaugeSkipsAFailedRead(t *testing.T) {
	m := New(Sources{Queue: func(context.Context) (QueueDepth, error) {
		return QueueDepth{Pending: 9}, errors.New("queue unavailable")
	}}, BuildInfo{})
	if jobs := gather(t, m)["mayfly_queue_jobs"]; len(jobs) != 0 {
		t.Fatalf("a failed read reported %v", jobs)
	}
}

func TestSessionGaugeReportsZeroStates(t *testing.T) {
	m := New(Sources{Sessions: sourceCounts}, BuildInfo{})
	sessions := gather(t, m)["mayfly_sessions"]
	if value := find(t, sessions, map[string]string{"state": string(model.StatusRunning)}); value != 3 {
		t.Fatalf("running = %v", value)
	}
	if value := find(t, sessions, map[string]string{"state": string(model.StatusPending)}); value != 0 {
		t.Fatalf("pending = %v, want 0", value)
	}
	if value := find(t, sessions, map[string]string{"state": string(model.StatusFailed)}); value != 1 {
		t.Fatalf("failed = %v", value)
	}
}

func TestCountersMoveOnEveryCall(t *testing.T) {
	m := New(Sources{}, BuildInfo{})
	m.SessionCreated()
	m.SessionCreated()
	m.SessionFinished(model.StatusCompleted)
	m.SessionFinished(model.StatusExpired)
	m.QueueJobAttempt(AttemptSucceeded)
	m.QueueJobAttempt(AttemptRetryLater)
	m.HTTPRequest("/api/v1/sessions", "GET", 201, time.Millisecond)
	m.LLMRequest("gpt-test", "error", time.Millisecond)
	m.ToolCall("plugin__tool", "error")

	got := gather(t, m)
	if value := got["mayfly_sessions_created_total"][0].value; value != 2 {
		t.Fatalf("created = %v", value)
	}
	if value := find(t, got["mayfly_sessions_finished_total"], map[string]string{"result": "completed"}); value != 1 {
		t.Fatalf("finished completed = %v", value)
	}
	if value := find(t, got["mayfly_sessions_finished_total"], map[string]string{"result": "expired"}); value != 1 {
		t.Fatalf("finished expired = %v", value)
	}
	if value := find(t, got["mayfly_queue_job_attempts_total"], map[string]string{"result": AttemptRetryLater}); value != 1 {
		t.Fatalf("retry attempt = %v", value)
	}
	if value := find(t, got["mayfly_http_requests_total"], map[string]string{"route": "/api/v1/sessions", "method": "GET", "code": "201"}); value != 1 {
		t.Fatalf("http request = %v", value)
	}
	if value := find(t, got["mayfly_http_request_duration_seconds"], map[string]string{"route": "/api/v1/sessions"}); value != 1 {
		t.Fatalf("http duration count = %v", value)
	}
	if value := find(t, got["mayfly_llm_requests_total"], map[string]string{"model": "gpt-test", "result": "error"}); value != 1 {
		t.Fatalf("llm request = %v", value)
	}
	if value := find(t, got["mayfly_gateway_calls_total"], map[string]string{"tool": "plugin__tool", "result": "error"}); value != 1 {
		t.Fatalf("gateway call = %v", value)
	}
}

func TestHandlerServesTextFormat(t *testing.T) {
	m := New(Sources{}, BuildInfo{Version: "test"})
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "mayfly_build_info{") || !strings.Contains(body, `version="test"`) {
		t.Fatalf("body does not expose the registry:\n%s", body)
	}
	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics status = %d", rec.Code)
	}
}

func TestNilMetricsAcceptsEveryCall(t *testing.T) {
	var m *Metrics
	m.SessionCreated()
	m.SessionFinished(model.StatusFailed)
	m.QueueJobAttempt(AttemptFailed)
	m.HTTPRequest("/", "GET", 500, time.Second)
	m.LLMRequest("model", "error", time.Second)
	m.ToolCall("tool", "error")
}
