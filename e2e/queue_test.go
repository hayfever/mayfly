//go:build e2e

package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDeadLetter checks the failure path of accepted work. The "denied"
// agent's pods are rejected by an admission policy installed by hack/e2e.sh,
// so session creation fails after the webhook was accepted. The job must be
// retried, buried after queue.max_attempts (2 in e2e/values.yaml), leave no
// half-created objects behind, and be retryable and deletable by an operator.
func TestDeadLetter(t *testing.T) {
	if err := env.waitNoSessionObjects(90 * time.Second); err != nil {
		t.Fatal(err)
	}
	r := env.webhook(t, map[string]any{"prompt": "dead letter probe", "agent": "denied"}, "")
	expectStatus(t, r, http.StatusAccepted, "webhook for an agent whose pods are denied")
	id := decode[accepted](t, r).JobID

	job := env.waitJob(t, id, "dead", 2*time.Minute)
	if job.Attempts != 2 {
		t.Errorf("buried after %d attempts, want queue.max_attempts=2", job.Attempts)
	}
	if !strings.Contains(job.Error, "pod denied by the dead-letter fixture") {
		t.Errorf("dead job error does not carry the cause: %q", job.Error)
	}
	if !slices.Contains(deadIDs(t), id) {
		t.Fatal("dead job is not listed under /api/v1/queue/dead")
	}
	if left, err := env.sessionObjects(""); err != nil || len(left) != 0 {
		t.Fatalf("failed session starts left objects behind: %v (%v)", left, err)
	}
	firstFailure := job.FailedAt

	r = env.admin(t, http.MethodPost, "/api/v1/queue/dead/"+id+"/retry", nil)
	expectStatus(t, r, http.StatusAccepted, "retry dead job")
	eventually(t, 2*time.Minute, "retried job buried again", func() (bool, string) {
		j, _ := env.job(t, id)
		return j.State == "dead" && j.FailedAt != firstFailure, j.State
	})
	if j, _ := env.job(t, id); j.Attempts != 2 {
		t.Errorf("retry did not reset attempts: buried again after %d", j.Attempts)
	}

	expectStatus(t, env.admin(t, http.MethodDelete, "/api/v1/queue/dead/"+id, nil), http.StatusNoContent, "delete dead job")
	if slices.Contains(deadIDs(t), id) {
		t.Fatal("deleted job is still listed as dead")
	}
	expectStatus(t, env.admin(t, http.MethodPost, "/api/v1/queue/dead/"+id+"/retry", nil), http.StatusNotFound, "retry a deleted job")
	expectStatus(t, env.admin(t, http.MethodDelete, "/api/v1/queue/dead/nope", nil), http.StatusNotFound, "delete an unknown job")
}

func deadIDs(t *testing.T) []string {
	t.Helper()
	r := env.admin(t, http.MethodGet, "/api/v1/queue/dead?limit=500", nil)
	expectStatus(t, r, http.StatusOK, "list dead jobs")
	var ids []string
	for _, j := range decode[struct {
		Dead []jobRecord `json:"dead"`
	}](t, r).Dead {
		ids = append(ids, j.ID)
	}
	return ids
}

// TestQueueSurvivesRestart fills sessions.max_concurrent (3 in
// e2e/values.yaml) so an accepted webhook has to wait, then replaces the
// control plane pod. The waiting job, the record of a finished job, and the
// running sessions must all survive, and the job must run once a slot frees.
func TestQueueSurvivesRestart(t *testing.T) {
	// A finished job whose record must outlive the restart.
	r := env.webhook(t, map[string]any{"prompt": "restart probe: finished before", "agent": "e2e"}, "")
	expectStatus(t, r, http.StatusAccepted, "webhook before restart")
	doneID := decode[accepted](t, r).JobID
	done := env.waitJob(t, doneID, "done", 2*time.Minute)
	env.cleanupSession(t, done.SessionID)
	env.admin(t, http.MethodDelete, "/api/v1/sessions/"+done.SessionID, nil)
	env.waitObjectsGone(t, done.SessionID)

	var blockers []string
	for range 3 {
		s := env.createSession(t, map[string]any{"prompt": "restart probe: holding a slot", "agent": "e2e", "ttl": "20m"})
		blockers = append(blockers, s.ID)
	}
	for _, id := range blockers {
		env.waitStatus(t, id, "running", 3*time.Minute)
	}
	r = env.admin(t, http.MethodPost, "/api/v1/sessions", map[string]any{"prompt": "over capacity", "agent": "e2e"})
	expectStatus(t, r, http.StatusTooManyRequests, "admin create over sessions.max_concurrent")

	r = env.webhook(t, map[string]any{"prompt": "restart probe: waiting for a slot", "agent": "e2e"}, "")
	expectStatus(t, r, http.StatusAccepted, "webhook while at capacity")
	waitingID := decode[accepted](t, r).JobID
	time.Sleep(8 * time.Second) // several worker polls
	if j, _ := env.job(t, waitingID); j.State == "done" || j.SessionID != "" || j.Attempts != 0 {
		t.Fatalf("job ran or spent attempts while the session cap was full: %+v", j)
	}

	env.restartControlPlane(t)

	if j, r := env.job(t, doneID); r.Status != http.StatusOK || j.State != "done" || j.SessionID != done.SessionID {
		t.Errorf("finished job record lost in restart: %s", r)
	}
	if j, r := env.job(t, waitingID); r.Status != http.StatusOK || (j.State != "queued" && j.State != "leased") {
		t.Fatalf("waiting job lost in restart: %s", r)
	}
	for _, id := range blockers {
		s, r := env.getSession(t, id)
		if r.Status != http.StatusOK {
			t.Fatalf("running session %s lost in restart: %s", id, r)
		}
		if s.Trigger.Kind != "adopted" {
			t.Errorf("session %s trigger kind after restart = %q, want adopted", id, s.Trigger.Kind)
		}
		env.waitStatus(t, id, "running", 2*time.Minute)
	}

	expectStatus(t, env.admin(t, http.MethodDelete, "/api/v1/sessions/"+blockers[0], nil), http.StatusNoContent, "free a slot")
	j := env.waitJob(t, waitingID, "done", 2*time.Minute)
	env.cleanupSession(t, j.SessionID)
	s := env.waitStatus(t, j.SessionID, "running", 3*time.Minute)
	if s.Trigger.JobID != waitingID {
		t.Fatalf("session trigger job id = %q, want %q", s.Trigger.JobID, waitingID)
	}
	for _, id := range append(blockers, s.ID) {
		env.admin(t, http.MethodDelete, "/api/v1/sessions/"+id, nil)
		env.waitObjectsGone(t, id)
	}
}
