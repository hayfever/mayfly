//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type triggerInfo struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Event    string `json:"event"`
	Delivery string `json:"delivery"`
	JobID    string `json:"job_id"`
}

type artifactInfo struct {
	Size    int64 `json:"size"`
	Partial bool  `json:"partial"`
}

// llmUsage mirrors model.LLMUsage in the session response.
type llmUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	Requests         int64 `json:"requests"`
	EstimatedCalls   int64 `json:"estimated_calls"`
}

// llmBudget mirrors model.LLMBudget in the session response.
type llmBudget struct {
	MaxTokens   int64 `json:"max_tokens"`
	MaxRequests int64 `json:"max_requests"`
}

// Total returns the charged prompt plus completion tokens.
func (u *llmUsage) Total() int64 {
	if u == nil {
		return 0
	}
	return u.PromptTokens + u.CompletionTokens
}

type session struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	Status    string        `json:"status"`
	Agent     string        `json:"agent"`
	Harness   string        `json:"harness"`
	Trigger   triggerInfo   `json:"trigger"`
	Artifact  *artifactInfo `json:"artifact"`
	LLMUsage  *llmUsage     `json:"llm_usage"`
	LLMBudget *llmBudget    `json:"llm_budget"`
	Error     string        `json:"error"`
}

type event struct {
	Seq   int64          `json:"seq"`
	Type  string         `json:"type"`
	Actor string         `json:"actor"`
	Text  string         `json:"text"`
	Data  map[string]any `json:"data"`
}

type jobRecord struct {
	ID        string `json:"id"`
	Trigger   string `json:"trigger"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	SessionID string `json:"session_id"`
	Error     string `json:"error"`
	FailedAt  string `json:"failed_at"`
}

type accepted struct {
	Accepted  bool   `json:"accepted"`
	Duplicate bool   `json:"duplicate"`
	JobID     string `json:"job_id"`
	Delivery  string `json:"delivery"`
	Status    string `json:"status"`
}

type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r response) String() string {
	return fmt.Sprintf("%d %s", r.Status, strings.TrimSpace(string(r.Body)))
}

// request sends one HTTP request to the control plane.
func (c *cluster) request(t *testing.T, method, path string, body []byte, header map[string]string) response {
	t.Helper()
	req, err := http.NewRequest(method, c.url(path), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: data}
}

// admin calls the admin API with the release's API token.
func (c *cluster) admin(t *testing.T, method, path string, payload any) response {
	t.Helper()
	header := map[string]string{"Authorization": "Bearer " + c.token}
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		header["Content-Type"] = "application/json"
	}
	return c.request(t, method, path, body, header)
}

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("decode %T from %s: %v", v, r, err)
	}
	return v
}

func expectStatus(t *testing.T, r response, want int, what string) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: got %s, want %d", what, r, want)
	}
}

// eventually polls check until it reports done or the timeout passes. check
// returns a description of the current state for the failure message.
func eventually(t *testing.T, timeout time.Duration, what string, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var state string
	for {
		var done bool
		done, state = check()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached within %s; last state: %s", what, timeout, state)
		}
		time.Sleep(time.Second)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func signature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func slackSignature(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// deliveryJobID is the job id the queue derives from a delivery key, so a
// sender that never sees the job id (Slack) can still be followed.
func deliveryJobID(trigger, delivery string) string {
	sum := sha256.Sum256([]byte(trigger + "\x00" + delivery))
	return "d-" + hex.EncodeToString(sum[:16])
}

// webhook posts a JSON body to the signed generic trigger.
func (c *cluster) webhook(t *testing.T, payload any, idempotencyKey string) response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	header := map[string]string{
		"Content-Type":           "application/json",
		"X-Mayfly-Signature-256": signature(c.webhookSecret, body),
	}
	if idempotencyKey != "" {
		header["Idempotency-Key"] = idempotencyKey
	}
	return c.request(t, http.MethodPost, "/hooks/generic", body, header)
}

func (c *cluster) getSession(t *testing.T, id string) (session, response) {
	t.Helper()
	r := c.admin(t, http.MethodGet, "/api/v1/sessions/"+id, nil)
	if r.Status != http.StatusOK {
		return session{}, r
	}
	return decode[struct {
		Session session `json:"session"`
	}](t, r).Session, r
}

// createSession starts a session through the admin API and deletes it when
// the test ends.
func (c *cluster) createSession(t *testing.T, req map[string]any) session {
	t.Helper()
	r := c.admin(t, http.MethodPost, "/api/v1/sessions", req)
	expectStatus(t, r, http.StatusCreated, "create session")
	s := decode[struct {
		Session session `json:"session"`
	}](t, r).Session
	c.cleanupSession(t, s.ID)
	return s
}

// cleanupSession force-deletes a session at the end of the test unless it has
// already finished.
func (c *cluster) cleanupSession(t *testing.T, id string) {
	t.Cleanup(func() {
		s, r := c.getSession(t, id)
		if r.Status != http.StatusOK || terminal(s.Status) {
			return
		}
		c.admin(t, http.MethodDelete, "/api/v1/sessions/"+id, nil)
	})
}

func terminal(status string) bool {
	return status == "completed" || status == "failed" || status == "expired"
}

// waitStatus waits for a session to reach want. Reaching a different terminal
// status first fails the test immediately.
func (c *cluster) waitStatus(t *testing.T, id, want string, timeout time.Duration) session {
	t.Helper()
	var s session
	eventually(t, timeout, "session "+id+" status "+want, func() (bool, string) {
		var r response
		s, r = c.getSession(t, id)
		if r.Status != http.StatusOK {
			return false, r.String()
		}
		if s.Status != want && terminal(s.Status) {
			t.Fatalf("session %s ended %s (error %q), want %s", id, s.Status, s.Error, want)
		}
		return s.Status == want, s.Status
	})
	return s
}

func (c *cluster) job(t *testing.T, id string) (jobRecord, response) {
	t.Helper()
	r := c.admin(t, http.MethodGet, "/api/v1/queue/jobs/"+id, nil)
	if r.Status != http.StatusOK {
		return jobRecord{}, r
	}
	return decode[struct {
		Job jobRecord `json:"job"`
	}](t, r).Job, r
}

func (c *cluster) waitJob(t *testing.T, id, state string, timeout time.Duration) jobRecord {
	t.Helper()
	var j jobRecord
	eventually(t, timeout, "job "+id+" state "+state, func() (bool, string) {
		var r response
		j, r = c.job(t, id)
		if r.Status != http.StatusOK {
			return false, r.String()
		}
		return j.State == state, fmt.Sprintf("%s attempts=%d error=%q", j.State, j.Attempts, j.Error)
	})
	return j
}

func (c *cluster) events(t *testing.T, id string) []event {
	t.Helper()
	r := c.admin(t, http.MethodGet, "/api/v1/sessions/"+id+"/events", nil)
	expectStatus(t, r, http.StatusOK, "events")
	return decode[struct {
		Events []event `json:"events"`
	}](t, r).Events
}

func (c *cluster) waitEvent(t *testing.T, id, what string, match func(event) bool) event {
	t.Helper()
	var found event
	eventually(t, 2*time.Minute, "session "+id+": "+what, func() (bool, string) {
		evs := c.events(t, id)
		for _, ev := range evs {
			if match(ev) {
				found = ev
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d events", len(evs))
	})
	return found
}

// waitObjectsGone waits until the pod, Secret, ConfigMap, and NetworkPolicy
// of a session are deleted.
func (c *cluster) waitObjectsGone(t *testing.T, id string) {
	t.Helper()
	eventually(t, 90*time.Second, "session "+id+" objects deleted", func() (bool, string) {
		objs, err := c.sessionObjects(id)
		if err != nil {
			return false, err.Error()
		}
		return len(objs) == 0, strings.Join(objs, ", ")
	})
}

// artifactFiles downloads a session bundle and returns its files by name.
func (c *cluster) artifactFiles(t *testing.T, id string) map[string][]byte {
	t.Helper()
	r := c.admin(t, http.MethodGet, "/api/v1/sessions/"+id+"/artifact", nil)
	expectStatus(t, r, http.StatusOK, "download artifact")
	gz, err := gzip.NewReader(bytes.NewReader(r.Body))
	if err != nil {
		t.Fatalf("artifact is not gzip: %v", err)
	}
	files := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read artifact tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = data
	}
	return files
}

func manifestOf(t *testing.T, files map[string][]byte) map[string]any {
	t.Helper()
	raw, ok := files["manifest.json"]
	if !ok {
		t.Fatalf("artifact has no manifest.json; files: %v", keys(files))
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	return m
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
