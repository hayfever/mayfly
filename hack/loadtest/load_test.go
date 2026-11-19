package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeControlPlane implements the endpoints the harness touches. It is not a
// Mayfly server: it records the load and returns the same shapes.
type fakeControlPlane struct {
	token  string
	secret string

	mu             sync.Mutex
	creates        int
	hooks          int
	deletes        int
	badSignatures  int
	sessions       map[string]int
	jobs           map[string]string
	jobPolls       map[string]int
	jobDelay       int
	failCreates    bool
	metricsScrapes atomic.Int64
}

func newFakeControlPlane(t *testing.T, token, secret string) *fakeControlPlane {
	t.Helper()
	return &fakeControlPlane{
		token:    token,
		secret:   secret,
		sessions: map[string]int{},
		jobs:     map[string]string{},
		jobPolls: map[string]int{},
	}
}

func (f *fakeControlPlane) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !f.authorized(r) {
			writeJSONTest(w, http.StatusUnauthorized, errBody("unauthenticated", "missing token"))
			return
		}
		var req struct {
			Prompt string `json:"prompt"`
			TTL    string `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
			writeJSONTest(w, http.StatusBadRequest, errBody("invalid_request", "prompt is required"))
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failCreates {
			writeJSONTest(w, http.StatusInternalServerError, errBody("internal", "forced failure"))
			return
		}
		f.creates++
		id := fmt.Sprintf("s%d", f.creates)
		f.sessions[id] = 0
		writeJSONTest(w, http.StatusCreated, map[string]any{
			"session": map[string]string{"id": id, "status": "pending"},
			"url":     "http://example.test/#/sessions/" + id,
		})
	})
	mux.HandleFunc("GET /api/v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		polls, ok := f.sessions[id]
		if !ok {
			writeJSONTest(w, http.StatusNotFound, errBody("not_found", "session not found"))
			return
		}
		f.sessions[id] = polls + 1
		status := "running"
		if polls >= 1 {
			status = "completed"
		}
		writeJSONTest(w, http.StatusOK, map[string]any{
			"session": map[string]string{"id": id, "status": status},
		})
	})
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.sessions[id]; !ok {
			writeJSONTest(w, http.StatusNotFound, errBody("not_found", "session not found"))
			return
		}
		f.deletes++
		delete(f.sessions, id)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /hooks/{trigger}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSONTest(w, http.StatusBadRequest, errBody("invalid_request", "body"))
			return
		}
		mac := hmac.New(sha256.New, []byte(f.secret))
		mac.Write(body)
		expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if r.Header.Get("X-Mayfly-Signature-256") != expected {
			f.mu.Lock()
			f.badSignatures++
			f.mu.Unlock()
			writeJSONTest(w, http.StatusUnauthorized, errBody("unauthenticated", "webhook verification failed"))
			return
		}
		if r.Header.Get("Idempotency-Key") == "" {
			writeJSONTest(w, http.StatusBadRequest, errBody("invalid_request", "delivery key"))
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hooks++
		jobID := fmt.Sprintf("j%d", f.hooks)
		sessionID := fmt.Sprintf("w%d", f.hooks)
		f.jobs[jobID] = sessionID
		if f.jobDelay == 0 {
			f.sessions[sessionID] = 0
		}
		writeJSONTest(w, http.StatusAccepted, map[string]any{
			"accepted": true, "duplicate": false, "job_id": jobID,
			"delivery": r.Header.Get("Idempotency-Key"), "status": "queued",
		})
	})
	mux.HandleFunc("GET /api/v1/queue/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		jobID := r.PathValue("id")
		sessionID, ok := f.jobs[jobID]
		if !ok {
			writeJSONTest(w, http.StatusNotFound, errBody("not_found", "job not found"))
			return
		}
		f.jobPolls[jobID]++
		if f.jobDelay > 0 && f.jobPolls[jobID] <= f.jobDelay {
			writeJSONTest(w, http.StatusOK, map[string]any{
				"job": map[string]any{"state": "queued"},
			})
			return
		}
		if _, ok := f.sessions[sessionID]; !ok {
			f.sessions[sessionID] = 0
		}
		writeJSONTest(w, http.StatusOK, map[string]any{
			"job": map[string]any{"state": "done", "session_id": sessionID},
		})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if !f.authorized(r) {
			writeJSONTest(w, http.StatusUnauthorized, errBody("unauthenticated", "missing token"))
			return
		}
		n := f.metricsScrapes.Add(1)
		fmt.Fprintf(w, "# HELP go_goroutines Number of goroutines.\n")
		fmt.Fprintf(w, "# TYPE go_goroutines gauge\n")
		fmt.Fprintf(w, "go_goroutines %d\n", 100+int(n))
		fmt.Fprintf(w, "process_resident_memory_bytes %d\n", 1_000_000+int(n)*4096)
	})
	return mux
}

func (f *fakeControlPlane) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+f.token
}

func writeJSONTest(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func errBody(code, message string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code, "message": message}}
}

func TestRunAgainstFakeControlPlane(t *testing.T) {
	fake := newFakeControlPlane(t, "test-token", "test-secret")
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := newAPIClient(server.URL, "test-token", "test-secret")
	summary, err := runLoad(context.Background(), loadConfig{
		mode:           "both",
		concurrency:    4,
		duration:       30 * time.Second,
		iterations:     2,
		prompt:         "loadtest",
		ttl:            "1m",
		sessionTimeout: 2 * time.Second,
		pollInterval:   time.Millisecond,
		sampleInterval: 5 * time.Millisecond,
		webhookTrigger: "generic",
		metricsURL:     server.URL + "/metrics",
	}, client, nil)
	if err != nil {
		t.Fatalf("runLoad: %v", err)
	}

	if got, want := summary.Requests(), 8; got != want {
		t.Errorf("requests = %d, want %d", got, want)
	}
	if got := summary.Errors(); got != 0 {
		t.Errorf("errors = %d, want 0", got)
	}
	if got, want := summary.SessionsCreated, 8; got != want {
		t.Errorf("sessions created = %d, want %d", got, want)
	}
	if got, want := summary.SessionsFinalized, 8; got != want {
		t.Errorf("sessions finalized = %d, want %d", got, want)
	}
	if got, want := summary.SessionCompleted, 8; got != want {
		t.Errorf("sessions completed = %d, want %d", got, want)
	}
	if summary.Throughput() <= 0 {
		t.Errorf("throughput = %v, want positive", summary.Throughput())
	}
	for _, op := range []string{opSessionCreate, opWebhook} {
		stats := summary.Ops[op]
		if stats == nil || len(stats.Latencies) != 4 {
			t.Fatalf("%s latencies = %v, want 4", op, stats)
		}
		p50 := Percentile(stats.Latencies, 0.50)
		p95 := Percentile(stats.Latencies, 0.95)
		p99 := Percentile(stats.Latencies, 0.99)
		if p50 > p95 || p95 > p99 {
			t.Errorf("%s percentiles are not ordered: %s %s %s", op, p50, p95, p99)
		}
	}

	growth := summary.growth()
	if growth.GoroutineSamples < 2 {
		t.Fatalf("goroutine samples = %d, want at least 2", growth.GoroutineSamples)
	}
	if growth.GoroutinesEnd <= growth.GoroutinesStart {
		t.Errorf("goroutines start %d end %d, want growth", growth.GoroutinesStart, growth.GoroutinesEnd)
	}
	if growth.RSSSamples < 2 {
		t.Errorf("RSS samples = %d, want at least 2", growth.RSSSamples)
	}

	fake.mu.Lock()
	deletes, bad := fake.deletes, fake.badSignatures
	fake.mu.Unlock()
	if deletes != 8 {
		t.Errorf("deleted sessions = %d, want 8", deletes)
	}
	if bad != 0 {
		t.Errorf("bad signatures = %d, want 0", bad)
	}

	var out bytes.Buffer
	printSummary(&out, summary)
	for _, want := range []string{"error rate: 0.00%", "sessions", "throughput", "go_goroutines", "resident memory"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestRunCountsPrimaryErrors(t *testing.T) {
	fake := newFakeControlPlane(t, "test-token", "test-secret")
	fake.failCreates = true
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := newAPIClient(server.URL, "test-token", "test-secret")
	summary, err := runLoad(context.Background(), loadConfig{
		mode:           "sessions",
		concurrency:    2,
		duration:       30 * time.Second,
		iterations:     3,
		prompt:         "loadtest",
		ttl:            "1m",
		sessionTimeout: 2 * time.Second,
		pollInterval:   time.Millisecond,
		sampleInterval: 10 * time.Millisecond,
		webhookTrigger: "generic",
		metricsURL:     server.URL + "/metrics",
	}, client, nil)
	if err != nil {
		t.Fatalf("runLoad: %v", err)
	}
	if got, want := summary.Requests(), 6; got != want {
		t.Errorf("requests = %d, want %d", got, want)
	}
	if got, want := summary.Errors(), 6; got != want {
		t.Errorf("errors = %d, want %d", got, want)
	}
	if got := summary.ErrorRate(); got != 1 {
		t.Errorf("error rate = %v, want 1", got)
	}
	if summary.SessionsFinalized != 0 {
		t.Errorf("finalized = %d, want 0", summary.SessionsFinalized)
	}
	if labels := failureLabels(summary); len(labels) != 1 || !strings.Contains(labels[0], "500") {
		t.Errorf("failure labels = %v, want one 500 label", labels)
	}
}

func TestRunStopsAtDuration(t *testing.T) {
	fake := newFakeControlPlane(t, "test-token", "test-secret")
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := newAPIClient(server.URL, "test-token", "test-secret")
	start := time.Now()
	summary, err := runLoad(context.Background(), loadConfig{
		mode:           "sessions",
		concurrency:    1,
		duration:       150 * time.Millisecond,
		prompt:         "loadtest",
		ttl:            "1m",
		sessionTimeout: 2 * time.Second,
		pollInterval:   10 * time.Millisecond,
		sampleInterval: 20 * time.Millisecond,
		webhookTrigger: "generic",
		metricsURL:     server.URL + "/metrics",
	}, client, nil)
	if err != nil {
		t.Fatalf("runLoad: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("run took %s, want the 150ms window", elapsed)
	}
	if summary.Requests() == 0 {
		t.Errorf("requests = 0, want load during the window")
	}
	if summary.Window <= 0 {
		t.Errorf("window = %v, want positive", summary.Window)
	}
}

// TestCleanupUsesBackgroundContext proves the end of the load window still
// deletes an in-flight session. The fake session never finishes, so the worker
// is waiting when the window closes.
func TestCleanupUsesBackgroundContext(t *testing.T) {
	fake := newFakeControlPlane(t, "test-token", "test-secret")
	fake.failCreates = false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions":
			writeJSONTest(w, http.StatusCreated, map[string]any{"session": map[string]string{"id": "slow"}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/sessions/"):
			writeJSONTest(w, http.StatusOK, map[string]any{"session": map[string]string{"id": "slow", "status": "running"}})
		case r.Method == http.MethodDelete:
			fake.mu.Lock()
			fake.deletes++
			fake.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAPIClient(server.URL, "test-token", "test-secret")
	summary, err := runLoad(context.Background(), loadConfig{
		mode:           "sessions",
		concurrency:    1,
		duration:       100 * time.Millisecond,
		prompt:         "loadtest",
		ttl:            "1m",
		sessionTimeout: 30 * time.Second,
		pollInterval:   10 * time.Millisecond,
		sampleInterval: 20 * time.Millisecond,
		webhookTrigger: "generic",
		metricsURL:     server.URL + "/metrics",
	}, client, nil)
	if err != nil {
		t.Fatalf("runLoad: %v", err)
	}
	if summary.Requests() != 1 {
		t.Errorf("requests = %d, want 1", summary.Requests())
	}
	fake.mu.Lock()
	deletes := fake.deletes
	fake.mu.Unlock()
	if deletes != 1 {
		t.Errorf("deleted sessions = %d, want 1 after the window closed", deletes)
	}
}

// TestLateWebhookSessionIsCleanedUp covers a job that creates its session
// after the harness stops waiting for that job. The fake reports no session
// for the first 20 job polls, more than a 10ms wait can make, and then the
// final pass finds the session and deletes it.
func TestLateWebhookSessionIsCleanedUp(t *testing.T) {
	fake := newFakeControlPlane(t, "test-token", "test-secret")
	fake.jobDelay = 20
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := newAPIClient(server.URL, "test-token", "test-secret")
	summary, err := runLoad(context.Background(), loadConfig{
		mode:           "webhooks",
		concurrency:    1,
		iterations:     1,
		duration:       30 * time.Second,
		prompt:         "loadtest",
		ttl:            "1m",
		sessionTimeout: 10 * time.Millisecond,
		pollInterval:   2 * time.Millisecond,
		sampleInterval: 10 * time.Millisecond,
		webhookTrigger: "generic",
		metricsURL:     server.URL + "/metrics",
	}, client, nil)
	if err != nil {
		t.Fatalf("runLoad: %v", err)
	}

	if got, want := len(summary.JobFailures), 1; got != want {
		t.Errorf("job failures = %d, want %d", got, want)
	}
	if got, want := summary.LateSessionsCleaned, 1; got != want {
		t.Errorf("late sessions cleaned up = %d, want %d", got, want)
	}
	if got, want := summary.LateSessionsLeft, 0; got != want {
		t.Errorf("late sessions left behind = %d, want %d", got, want)
	}
	if got, want := summary.SessionsCreated, 0; got != want {
		t.Errorf("sessions created = %d, want %d (the wait never saw the session)", got, want)
	}

	fake.mu.Lock()
	deletes := fake.deletes
	_, stillThere := fake.sessions["w1"]
	fake.mu.Unlock()
	if deletes != 1 {
		t.Errorf("deleted sessions = %d, want 1", deletes)
	}
	if stillThere {
		t.Error("late session w1 is still present after the run")
	}

	var out bytes.Buffer
	printSummary(&out, summary)
	if want := "late sessions cleaned up: 1, left behind: 0"; !strings.Contains(out.String(), want) {
		t.Errorf("report does not contain %q:\n%s", want, out.String())
	}
}

// TestCleanLateJobsDoesNotStallOnOneJob covers a final pass where the first
// job stays queued. The pass must still read the later jobs, delete the
// session one of them created, and skip the dead one.
func TestCleanLateJobsDoesNotStallOnOneJob(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/queue/jobs/stuck":
			io.WriteString(w, `{"job":{"state":"queued"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/queue/jobs/late":
			io.WriteString(w, `{"job":{"state":"done","session_id":"s-late"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/queue/jobs/dead":
			io.WriteString(w, `{"job":{"state":"dead","error":"boom"}}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/sessions/"):
			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAPIClient(server.URL, "test-token", "test-secret")
	cleaned, left := cleanLateJobs(client, []string{"stuck", "late", "dead"}, 5*time.Millisecond, 200*time.Millisecond)
	if cleaned != 1 || left != 0 {
		t.Fatalf("cleaned, left = %d, %d; want 1, 0", cleaned, left)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "s-late" {
		t.Fatalf("deleted = %v, want [s-late]", deleted)
	}
}
