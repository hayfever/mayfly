package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	opSessionCreate = "session create"
	opWebhook       = "webhook delivery"
)

// loadConfig is the resolved load to generate.
type loadConfig struct {
	mode           string
	concurrency    int
	duration       time.Duration
	iterations     int
	prompt         string
	ttl            string
	sessionTimeout time.Duration
	pollInterval   time.Duration
	sampleInterval time.Duration
	webhookTrigger string
	metricsURL     string
	runID          string
}

// apiClient talks to the control plane with one API token.
type apiClient struct {
	base          string
	token         string
	webhookSecret string
	http          *http.Client
}

func newAPIClient(base, token, webhookSecret string) *apiClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 512
	transport.MaxIdleConnsPerHost = 256
	return &apiClient{
		base:          base,
		token:         token,
		webhookSecret: webhookSecret,
		http:          &http.Client{Transport: transport, Timeout: 2 * time.Minute},
	}
}

// runLoad drives the workers for the window and returns the merged report.
// A lost port-forward cancels the run and is returned as an error.
func runLoad(parent context.Context, cfg loadConfig, client *apiClient, lost <-chan error) (*Summary, error) {
	start := time.Now()
	if cfg.runID == "" {
		cfg.runID = fmt.Sprintf("%d", start.UnixNano())
	}
	ctx, cancel := context.WithTimeout(parent, cfg.duration)
	defer cancel()

	var lostMu sync.Mutex
	var lostErr error
	if lost != nil {
		go func() {
			select {
			case err := <-lost:
				lostMu.Lock()
				lostErr = err
				lostMu.Unlock()
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	samples := make(chan []RuntimeSample, 1)
	go func() {
		samples <- collectRuntime(ctx, client, cfg.metricsURL, cfg.sampleInterval)
	}()

	result := runWorkers(ctx, cfg, client)
	cancel()
	runtimeSamples := <-samples

	summary := &Summary{
		Mode:              cfg.mode,
		Workers:           cfg.concurrency,
		Window:            time.Since(start),
		Ops:               map[string]*opStats{},
		SessionsCreated:   result.sessionsCreated,
		SessionsFinalized: result.completed + result.failed + result.expired,
		SessionCompleted:  result.completed,
		SessionFailed:     result.failed,
		SessionExpired:    result.expired,
		Lifecycle:         result.lifecycle,
		WaitFailures:      result.waitFailures,
		JobFailures:       result.jobFailures,
		CleanupFailures:   result.cleanupFailures,
		Runtime:           runtimeSamples,
	}
	for op, latencies := range result.ops {
		summary.Ops[op] = &opStats{Latencies: latencies}
	}
	for _, f := range result.failures {
		stats := summary.Ops[f.operation]
		if stats == nil {
			stats = &opStats{}
			summary.Ops[f.operation] = stats
		}
		stats.Failures = append(stats.Failures, f)
	}

	lostMu.Lock()
	err := lostErr
	lostMu.Unlock()
	return summary, err
}

// workerResult is one worker's contribution. It is merged after all workers
// stop so no lock is needed on the hot path.
type workerResult struct {
	ops             map[string][]time.Duration
	failures        []opFailure
	sessionsCreated int
	completed       int
	failed          int
	expired         int
	lifecycle       []time.Duration
	waitFailures    []string
	jobFailures     []string
	cleanupFailures []string
}

type opFailure struct {
	operation string
	code      int
	reason    string
}

func (r *workerResult) record(op string, d time.Duration) {
	if r.ops == nil {
		r.ops = map[string][]time.Duration{}
	}
	r.ops[op] = append(r.ops[op], d)
}

func (r *workerResult) fail(op string, code int, reason string) {
	r.failures = append(r.failures, opFailure{operation: op, code: code, reason: reason})
}

func runWorkers(ctx context.Context, cfg loadConfig, client *apiClient) workerResult {
	results := make(chan workerResult, cfg.concurrency)
	for i := range cfg.concurrency {
		go func(id int) {
			results <- runWorker(ctx, cfg, client, id)
		}(i)
	}
	var merged workerResult
	for range cfg.concurrency {
		mergeWorker(&merged, <-results)
	}
	return merged
}

func mergeWorker(dst *workerResult, src workerResult) {
	for op, latencies := range src.ops {
		if dst.ops == nil {
			dst.ops = map[string][]time.Duration{}
		}
		dst.ops[op] = append(dst.ops[op], latencies...)
	}
	dst.failures = append(dst.failures, src.failures...)
	dst.sessionsCreated += src.sessionsCreated
	dst.completed += src.completed
	dst.failed += src.failed
	dst.expired += src.expired
	dst.lifecycle = append(dst.lifecycle, src.lifecycle...)
	dst.waitFailures = append(dst.waitFailures, src.waitFailures...)
	dst.jobFailures = append(dst.jobFailures, src.jobFailures...)
	dst.cleanupFailures = append(dst.cleanupFailures, src.cleanupFailures...)
}

type worker struct {
	cfg    loadConfig
	client *apiClient
	id     int
	result workerResult
}

func runWorker(ctx context.Context, cfg loadConfig, client *apiClient, id int) workerResult {
	mode := cfg.mode
	if mode == "both" {
		if id%2 == 0 {
			mode = "sessions"
		} else {
			mode = "webhooks"
		}
	}
	w := &worker{cfg: cfg, client: client, id: id}
	for n := 0; cfg.iterations == 0 || n < cfg.iterations; n++ {
		if ctx.Err() != nil {
			break
		}
		if mode == "sessions" {
			w.session(ctx)
		} else {
			w.webhook(ctx, n)
		}
	}
	return w.result
}

// session creates, waits for, and deletes one session, so the number of
// in-flight sessions stays at or below the worker count.
func (w *worker) session(ctx context.Context) {
	start := time.Now()
	id, code, latency, err := w.client.createSession(ctx, w.cfg.prompt, w.cfg.ttl)
	if err != nil {
		if ctx.Err() == nil {
			w.result.fail(opSessionCreate, code, err.Error())
		}
		return
	}
	w.result.record(opSessionCreate, latency)
	w.result.sessionsCreated++

	status, waitErr := w.client.waitSession(ctx, id, w.cfg.pollInterval, w.cfg.sessionTimeout)
	cleanup(ctx, w, id, waitErr)
	if waitErr != nil {
		if ctx.Err() == nil {
			w.result.waitFailures = append(w.result.waitFailures, waitErr.Error())
		}
		return
	}
	w.result.lifecycle = append(w.result.lifecycle, time.Since(start))
	switch status {
	case "completed":
		w.result.completed++
	case "failed":
		w.result.failed++
	case "expired":
		w.result.expired++
	}
}

// webhook delivers one signed trigger, waits for the job to create a session,
// waits for that session, and deletes it.
func (w *worker) webhook(ctx context.Context, n int) {
	start := time.Now()
	key := fmt.Sprintf("loadtest-%s-%d-%d", w.cfg.runID, w.id, n)
	jobID, code, latency, err := w.client.deliver(ctx, w.cfg.webhookTrigger, key, w.cfg.prompt)
	if err != nil {
		if ctx.Err() == nil {
			w.result.fail(opWebhook, code, err.Error())
		}
		return
	}
	w.result.record(opWebhook, latency)

	id, err := w.client.waitJob(ctx, jobID, w.cfg.pollInterval, w.cfg.sessionTimeout)
	if err != nil {
		if ctx.Err() == nil {
			w.result.jobFailures = append(w.result.jobFailures, err.Error())
		}
		return
	}
	w.result.sessionsCreated++
	status, waitErr := w.client.waitSession(ctx, id, w.cfg.pollInterval, w.cfg.sessionTimeout)
	cleanup(ctx, w, id, waitErr)
	if waitErr != nil {
		if ctx.Err() == nil {
			w.result.waitFailures = append(w.result.waitFailures, waitErr.Error())
		}
		return
	}
	w.result.lifecycle = append(w.result.lifecycle, time.Since(start))
	switch status {
	case "completed":
		w.result.completed++
	case "failed":
		w.result.failed++
	case "expired":
		w.result.expired++
	}
}

// cleanup deletes a session after a terminal state or a failed wait. It uses
// its own context so the end of the load window does not skip the delete.
func cleanup(ctx context.Context, w *worker, id string, waitErr error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.client.deleteSession(cleanupCtx, id); err != nil {
		if waitErr != nil && ctx.Err() != nil {
			return
		}
		w.result.cleanupFailures = append(w.result.cleanupFailures, err.Error())
	}
}

func (c *apiClient) request(ctx context.Context, method, path string, body []byte, header http.Header) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

func (c *apiClient) createSession(ctx context.Context, prompt, ttl string) (string, int, time.Duration, error) {
	body, err := json.Marshal(map[string]string{"prompt": prompt, "ttl": ttl})
	if err != nil {
		return "", 0, 0, err
	}
	header := http.Header{"Content-Type": []string{"application/json"}}
	start := time.Now()
	code, data, err := c.request(ctx, http.MethodPost, "/api/v1/sessions", body, header)
	latency := time.Since(start)
	if err != nil {
		return "", 0, latency, err
	}
	if code != http.StatusCreated {
		return "", code, latency, errors.New(errorText(code, data))
	}
	var resp struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", code, latency, fmt.Errorf("decode create response: %w", err)
	}
	if resp.Session.ID == "" {
		return "", code, latency, errors.New("create response has no session id")
	}
	return resp.Session.ID, code, latency, nil
}

func (c *apiClient) deliver(ctx context.Context, trigger, idempotencyKey, prompt string) (string, int, time.Duration, error) {
	body, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		return "", 0, 0, err
	}
	header := http.Header{
		"Content-Type":    []string{"application/json"},
		"Idempotency-Key": []string{idempotencyKey},
	}
	if c.webhookSecret != "" {
		header.Set("X-Mayfly-Signature-256", signBody(c.webhookSecret, body))
	}
	start := time.Now()
	code, data, err := c.request(ctx, http.MethodPost, "/hooks/"+url.PathEscape(trigger), body, header)
	latency := time.Since(start)
	if err != nil {
		return "", 0, latency, err
	}
	if code != http.StatusAccepted {
		return "", code, latency, errors.New(errorText(code, data))
	}
	var resp struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", code, latency, fmt.Errorf("decode webhook response: %w", err)
	}
	if resp.JobID == "" {
		return "", code, latency, errors.New("webhook response has no job id")
	}
	return resp.JobID, code, latency, nil
}

func (c *apiClient) waitSession(ctx context.Context, id string, poll, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		code, data, err := c.request(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(id), nil, nil)
		if err != nil {
			return "", err
		}
		switch code {
		case http.StatusNotFound:
			return "", fmt.Errorf("session %s disappeared before a terminal state", id)
		case http.StatusOK:
		default:
			return "", fmt.Errorf("read session %s: %s", id, errorText(code, data))
		}
		var resp struct {
			Session struct {
				Status string `json:"status"`
			} `json:"session"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return "", fmt.Errorf("decode session %s: %w", id, err)
		}
		switch resp.Session.Status {
		case "completed", "failed", "expired":
			return resp.Session.Status, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("session %s did not finish within %s (last status %q)", id, timeout, resp.Session.Status)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *apiClient) waitJob(ctx context.Context, id string, poll, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		code, data, err := c.request(ctx, http.MethodGet, "/api/v1/queue/jobs/"+url.PathEscape(id), nil, nil)
		if err != nil {
			return "", err
		}
		if code != http.StatusOK {
			return "", fmt.Errorf("read job %s: %s", id, errorText(code, data))
		}
		var resp struct {
			Job struct {
				State     string `json:"state"`
				SessionID string `json:"session_id"`
				Error     string `json:"error"`
			} `json:"job"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return "", fmt.Errorf("decode job %s: %w", id, err)
		}
		if resp.Job.SessionID != "" {
			return resp.Job.SessionID, nil
		}
		if resp.Job.State == "dead" {
			reason := resp.Job.Error
			if reason == "" {
				reason = "job is dead"
			}
			return "", fmt.Errorf("job %s did not create a session: %s", id, reason)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("job %s created no session within %s (last state %q)", id, timeout, resp.Job.State)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *apiClient) deleteSession(ctx context.Context, id string) error {
	code, data, err := c.request(ctx, http.MethodDelete, "/api/v1/sessions/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return err
	}
	if code == http.StatusNoContent || code == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("delete session %s: %s", id, errorText(code, data))
}

// signBody returns the generic-trigger HMAC header value.
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// errorText prefers the stable error code and the human message.
func errorText(code int, body []byte) string {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Code != "" {
		return envelope.Error.Code + ": " + envelope.Error.Message
	}
	return fmt.Sprintf("HTTP %d", code)
}
