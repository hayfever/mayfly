package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/session"
)

// acceptedKeys is the exact field set of an accepted-job response.
var acceptedKeys = []string{"accepted", "delivery", "duplicate", "job_id", "status"}

func checkAccepted(t *testing.T, rec *httptest.ResponseRecorder, jobID, delivery string, duplicate bool) {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode accepted body: %v (%s)", err, rec.Body.String())
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(acceptedKeys, ",") {
		t.Fatalf("fields = %v, want %v", keys, acceptedKeys)
	}
	var body acceptedJob
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode into the accepted shape: %v", err)
	}
	if !body.Accepted || body.Duplicate != duplicate || body.JobID != jobID || body.Delivery != delivery || body.Status != string(queue.StateQueued) {
		t.Fatalf("accepted body = %+v", body)
	}
}

// TestAcceptedJobShapeOnEveryEnqueuePath checks that the webhook, the manual
// schedule run, and the dead-job retry all answer with one shape.
func TestAcceptedJobShapeOnEveryEnqueuePath(t *testing.T) {
	ctx := context.Background()

	t.Run("webhook", func(t *testing.T) {
		cfg := config.Default()
		cfg.AdminToken = "test-admin"
		cfg.Runtime.Image = "example/agent:1"
		cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
		store := testQueue(t)
		handler := New(cfg, testManager(t, cfg), store, testLogger()).Handler()

		post := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/hooks/generic", strings.NewReader(`{"prompt":"one run"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "delivery-1")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec
		}
		first := post()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(first.Body.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		var jobID string
		if err := json.Unmarshal(fields["job_id"], &jobID); err != nil || jobID == "" {
			t.Fatalf("job_id = %q", jobID)
		}
		checkAccepted(t, first, jobID, "delivery-1", false)
		checkAccepted(t, post(), jobID, "delivery-1", true)
	})

	t.Run("schedule run", func(t *testing.T) {
		cfg := config.Default()
		cfg.AdminToken = "test-admin"
		store := testQueue(t)
		delivery := "manual:page-watch:test"
		job := queue.NewSourceJob(delivery, queue.SourceJob{Schedule: "page-watch"}, time.Now())
		if err := store.Accept(ctx, job); err != nil {
			t.Fatal(err)
		}
		server := New(cfg, nil, store, testLogger(), WithSchedules(ScheduleService{
			Run: func(context.Context, string) (string, error) { return job.ID, nil },
		}))
		checkAccepted(t, adminRequest(t, server.Handler(), http.MethodPost, "/api/v1/schedules/page-watch/run", ""), job.ID, delivery, false)
	})

	t.Run("dead job retry", func(t *testing.T) {
		cfg := config.Default()
		cfg.AdminToken = "test-admin"
		qcfg := config.Default().Queue
		qcfg.Backend = "memory"
		qcfg.MaxAttempts = 1
		store := testQueueWith(t, qcfg)
		job := queue.NewJob("generic", "delivery-r", session.CreateOptions{Prompt: "will fail"}, time.Now())
		if err := store.Accept(ctx, job); err != nil {
			t.Fatal(err)
		}
		leases, err := store.Claim(ctx, 1)
		if err != nil || len(leases) != 1 {
			t.Fatalf("claim = %v, %v", leases, err)
		}
		if err := store.Nack(ctx, leases[0], errors.New("provider said no")); err != nil {
			t.Fatal(err)
		}
		server := New(cfg, nil, store, testLogger())
		checkAccepted(t, adminRequest(t, server.Handler(), http.MethodPost, "/api/v1/queue/dead/"+job.ID+"/retry", ""), job.ID, "delivery-r", false)
	})
}
