//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestControlPlaneAuth checks the probes and that each API surface accepts
// only its own credential.
func TestControlPlaneAuth(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		expectStatus(t, env.request(t, http.MethodGet, path, nil, nil), http.StatusOK, path)
	}

	r := env.request(t, http.MethodGet, "/api/v1/sessions", nil, nil)
	expectStatus(t, r, http.StatusUnauthorized, "admin API without a token")
	r = env.request(t, http.MethodGet, "/api/v1/sessions", nil, map[string]string{"Authorization": "Bearer wrong"})
	expectStatus(t, r, http.StatusUnauthorized, "admin API with a wrong token")

	// The admin token must not work as a session token: the LLM proxy and the
	// session runtime endpoints are for session pods only.
	body := []byte(`{"model":"stub","messages":[{"role":"user","content":"hi"}]}`)
	r = env.request(t, http.MethodPost, "/internal/v1/llm/chat/completions", body, map[string]string{
		"Authorization": "Bearer " + env.token,
		"Content-Type":  "application/json",
	})
	expectStatus(t, r, http.StatusUnauthorized, "LLM proxy with the admin token")

	r = env.admin(t, http.MethodGet, "/api/v1/config", nil)
	expectStatus(t, r, http.StatusOK, "config")
	for _, want := range []string{`"e2e"`, `"echo"`, `"generic"`} {
		if !strings.Contains(string(r.Body), want) {
			t.Errorf("config does not list %s: %s", want, r.Body)
		}
	}
}

// TestGenericWebhookSignatures checks that the generic trigger accepts an HMAC
// signature or the shared token, and nothing else. A request that passes
// verification is given a model the release does not allow, so it is rejected
// with 400 after verification and nothing is queued.
func TestGenericWebhookSignatures(t *testing.T) {
	body := []byte(`{"prompt":"signature probe","model":"not-allowed"}`)
	cases := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"unsigned", map[string]string{}, http.StatusUnauthorized},
		{"garbage signature", map[string]string{"X-Mayfly-Signature-256": "sha256=zz"}, http.StatusUnauthorized},
		{"wrong secret", map[string]string{"X-Mayfly-Signature-256": signature("wrong", body)}, http.StatusUnauthorized},
		{"signature over another body", map[string]string{"X-Mayfly-Signature-256": signature(env.webhookSecret, []byte("{}"))}, http.StatusUnauthorized},
		{"wrong token", map[string]string{"X-Mayfly-Token": "wrong"}, http.StatusUnauthorized},
		{"valid signature", map[string]string{"X-Mayfly-Signature-256": signature(env.webhookSecret, body)}, http.StatusBadRequest},
		{"GitHub-style signature header", map[string]string{"X-Hub-Signature-256": signature(env.webhookSecret, body)}, http.StatusBadRequest},
		{"valid token", map[string]string{"X-Mayfly-Token": env.webhookSecret}, http.StatusBadRequest},
	}
	before := queueStats(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.header["Content-Type"] = "application/json"
			r := env.request(t, http.MethodPost, "/hooks/generic", body, tc.header)
			expectStatus(t, r, tc.want, tc.name)
			if tc.want == http.StatusBadRequest && !strings.Contains(string(r.Body), "not allowed") {
				t.Fatalf("verified request was rejected for the wrong reason: %s", r)
			}
		})
	}
	if after := queueStats(t); after.Pending+after.Leased != before.Pending+before.Leased {
		t.Fatalf("rejected webhooks changed the queue: before %+v after %+v", before, after)
	}
	expectStatus(t, env.request(t, http.MethodPost, "/hooks/nope", body, nil), http.StatusNotFound, "unknown trigger")
}

type stats struct {
	Pending int `json:"pending"`
	Leased  int `json:"leased"`
	Dead    int `json:"dead"`
}

func queueStats(t *testing.T) stats {
	t.Helper()
	r := env.admin(t, http.MethodGet, "/api/v1/queue", nil)
	expectStatus(t, r, http.StatusOK, "queue stats")
	return decode[struct {
		Queue stats `json:"queue"`
	}](t, r).Queue
}

// TestGitHubWebhook checks signature-only verification, delivery dedupe, and
// that the delivery id reaches the session.
func TestGitHubWebhook(t *testing.T) {
	payload := map[string]any{
		"ref":        "refs/heads/main",
		"compare":    "https://github.com/hayfever/mayfly/compare/a...b",
		"repository": map[string]any{"full_name": "hayfever/mayfly", "html_url": "https://github.com/hayfever/mayfly"},
		"sender":     map[string]any{"login": "e2e-bot"},
		"commits":    []any{map[string]any{"message": "e2e: push event"}},
	}
	body, _ := json.Marshal(payload)
	delivery := "e2e-" + randomHex(8)
	header := func(extra map[string]string) map[string]string {
		h := map[string]string{
			"Content-Type":      "application/json",
			"X-GitHub-Event":    "push",
			"X-GitHub-Delivery": delivery,
		}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	r := env.request(t, http.MethodPost, "/hooks/github", body, header(map[string]string{"X-Mayfly-Token": env.webhookSecret}))
	expectStatus(t, r, http.StatusUnauthorized, "GitHub trigger with a static token")
	r = env.request(t, http.MethodPost, "/hooks/github", body, header(map[string]string{"X-Hub-Signature-256": signature("wrong", body)}))
	expectStatus(t, r, http.StatusUnauthorized, "GitHub trigger with a bad signature")

	signed := header(map[string]string{"X-Hub-Signature-256": signature(env.webhookSecret, body)})
	r = env.request(t, http.MethodPost, "/hooks/github", body, signed)
	expectStatus(t, r, http.StatusAccepted, "signed GitHub delivery")
	first := decode[accepted](t, r)
	if first.Duplicate || first.JobID != deliveryJobID("github", delivery) {
		t.Fatalf("unexpected accept: %+v", first)
	}
	r = env.request(t, http.MethodPost, "/hooks/github", body, signed)
	expectStatus(t, r, http.StatusAccepted, "redelivery")
	again := decode[accepted](t, r)
	if !again.Duplicate || again.JobID != first.JobID {
		t.Fatalf("redelivery was not deduplicated: %+v", again)
	}

	job := env.waitJob(t, first.JobID, "done", 2*time.Minute)
	env.cleanupSession(t, job.SessionID)
	s, _ := env.getSession(t, job.SessionID)
	if s.Trigger.Kind != "github" || s.Trigger.Event != "push" || s.Trigger.Delivery != delivery || s.Trigger.JobID != first.JobID {
		t.Fatalf("session trigger = %+v", s.Trigger)
	}
	if s.Title != "GitHub push: hayfever/mayfly" {
		t.Fatalf("session title = %q", s.Title)
	}
	sessions := env.admin(t, http.MethodGet, "/api/v1/sessions", nil)
	if n := strings.Count(string(sessions.Body), delivery); n != 1 {
		t.Fatalf("delivery %s appears in %d sessions, want 1", delivery, n)
	}
	env.admin(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil)
	env.waitObjectsGone(t, s.ID)
}

// TestSlackWebhook checks Slack's signing scheme, the URL verification
// handshake, replay protection, and a slash command that starts a session.
func TestSlackWebhook(t *testing.T) {
	post := func(body []byte, ts string, sig string, contentType string) response {
		h := map[string]string{"Content-Type": contentType}
		if ts != "" {
			h["X-Slack-Request-Timestamp"] = ts
		}
		if sig != "" {
			h["X-Slack-Signature"] = sig
		}
		return env.request(t, http.MethodPost, "/hooks/slack", body, h)
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)

	challenge := []byte(`{"type":"url_verification","challenge":"e2e-challenge"}`)
	r := post(challenge, now, slackSignature(env.slackSecret, now, challenge), "application/json")
	expectStatus(t, r, http.StatusOK, "signed URL verification")
	if got := decode[map[string]string](t, r)["challenge"]; got != "e2e-challenge" {
		t.Fatalf("challenge = %q", got)
	}
	expectStatus(t, post(challenge, "", "", "application/json"), http.StatusUnauthorized, "unsigned URL verification")
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	expectStatus(t, post(challenge, stale, slackSignature(env.slackSecret, stale, challenge), "application/json"),
		http.StatusUnauthorized, "replayed request outside the 5 minute window")

	triggerID := "e2e." + randomHex(6)
	form := url.Values{
		"command":      {"/mayfly"},
		"text":         {"check the e2e deploy"},
		"user_name":    {"e2e"},
		"channel_name": {"ops"},
		"trigger_id":   {triggerID},
	}
	body := []byte(form.Encode())
	r = post(body, now, slackSignature(env.slackSecret, now, body), "application/x-www-form-urlencoded")
	expectStatus(t, r, http.StatusOK, "signed slash command")
	if !strings.Contains(string(r.Body), "Mayfly session queued.") {
		t.Fatalf("slash command reply = %s", r)
	}

	job := env.waitJob(t, deliveryJobID("slack", triggerID), "done", 2*time.Minute)
	env.cleanupSession(t, job.SessionID)
	s, _ := env.getSession(t, job.SessionID)
	if s.Trigger.Kind != "slack" || s.Title != "Slack: check the e2e deploy" {
		t.Fatalf("session = %+v", s)
	}
	env.admin(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil)
	env.waitObjectsGone(t, s.ID)
}

// TestDatadogWebhook checks that a signed Datadog alert starts a session.
func TestDatadogWebhook(t *testing.T) {
	body := []byte(`{"title":"e2e: high error rate","text":"5xx above 5%","alert_type":"error","url":"https://app.datadoghq.com/monitors/1"}`)
	r := env.request(t, http.MethodPost, "/hooks/datadog", body, map[string]string{"Content-Type": "application/json"})
	expectStatus(t, r, http.StatusUnauthorized, "unsigned Datadog alert")
	r = env.request(t, http.MethodPost, "/hooks/datadog", body, map[string]string{
		"Content-Type":           "application/json",
		"X-Mayfly-Signature-256": signature(env.webhookSecret, body),
	})
	expectStatus(t, r, http.StatusAccepted, "signed Datadog alert")
	job := env.waitJob(t, decode[accepted](t, r).JobID, "done", 2*time.Minute)
	env.cleanupSession(t, job.SessionID)
	s, _ := env.getSession(t, job.SessionID)
	if s.Trigger.Kind != "datadog" || s.Trigger.Event != "error" || s.Title != "e2e: high error rate" {
		t.Fatalf("session = %+v", s)
	}
	env.admin(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil)
	env.waitObjectsGone(t, s.ID)
}

// TestWebhookRateLimit checks that a trigger's budget is spent only by
// verified requests and that the limit answers 429 with Retry-After. The
// "limited" trigger allows a burst of 2 per hour.
func TestWebhookRateLimit(t *testing.T) {
	// Verified but invalid: spends a token, then fails validation, so no
	// session is created.
	body := []byte(`{"prompt":"rate limit probe","model":"not-allowed"}`)
	signed := map[string]string{"Content-Type": "application/json", "X-Mayfly-Signature-256": signature(env.webhookSecret, body)}
	for range 5 {
		r := env.request(t, http.MethodPost, "/hooks/limited", body, map[string]string{"Content-Type": "application/json"})
		expectStatus(t, r, http.StatusUnauthorized, "unsigned request to the limited trigger")
	}
	for i := range 2 {
		r := env.request(t, http.MethodPost, "/hooks/limited", body, signed)
		expectStatus(t, r, http.StatusBadRequest, "verified request "+strconv.Itoa(i+1)+" within the burst")
	}
	r := env.request(t, http.MethodPost, "/hooks/limited", body, signed)
	expectStatus(t, r, http.StatusTooManyRequests, "verified request over the burst")
	if after, err := strconv.Atoi(r.Header.Get("Retry-After")); err != nil || after < 1 {
		t.Fatalf("Retry-After = %q", r.Header.Get("Retry-After"))
	}
}
