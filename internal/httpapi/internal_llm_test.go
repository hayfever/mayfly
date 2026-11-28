package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/metrics"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/project"
	"github.com/hayfever/mayfly/internal/session"
)

// llmTestBody is one chat completion request. max_tokens fixes the reservation
// at 100 completion tokens, and the model field matches the session, so the
// broker forwards the body unchanged.
const llmTestBody = `{"model":"stub","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

const llmTestUsage = `{"usage":{"prompt_tokens":30,"completion_tokens":12},"choices":[]}`

// llmFixture is one API server over a real session manager and a stub
// provider, so the internal route exercises the real admit and charge path.
type llmFixture struct {
	t       *testing.T
	server  *Server
	mgr     *session.Manager
	runner  *recordingRunner
	metrics *metrics.Metrics
	records *flakyRecords
	id      string
	token   string

	mu      sync.Mutex
	handler func(w http.ResponseWriter, r *http.Request, body []byte)
	hits    int
}

// flakyRecords wraps the memory record store and refuses writes on demand, so
// a test can reach the budget's fail-closed reservation path.
type flakyRecords struct {
	session.RecordStore
	mu   sync.Mutex
	fail bool
}

func (r *flakyRecords) setFail(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = fail
}

func (r *flakyRecords) Save(ctx context.Context, rec session.Record) error {
	r.mu.Lock()
	fail := r.fail
	r.mu.Unlock()
	if fail {
		return errors.New("record store unavailable")
	}
	return r.RecordStore.Save(ctx, rec)
}

// newLLMFixture starts a server whose sessions carry the given model budget.
// Zero leaves a limit off.
func newLLMFixture(t *testing.T, tokens, requests int) *llmFixture {
	t.Helper()
	cfg := config.Default()
	cfg.AdminToken = "test-admin"
	cfg.Runtime.Backend = "local"
	cfg.Runtime.StateDir = t.TempDir()
	cfg.Runtime.Image = "example/agent:1"
	cfg.Server.InternalURL = "http://mayfly:8080"
	cfg.LLM.Model = "stub"
	cfg.Sessions.MaxLLMTokens = tokens
	cfg.Sessions.MaxLLMRequests = requests
	cfg.Sessions.MaxConcurrent = 0

	f := &llmFixture{t: t, metrics: metrics.New(metrics.Sources{}, metrics.BuildInfo{})}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		handler := f.handler
		f.hits++
		f.mu.Unlock()
		if handler != nil {
			handler(w, r, body)
			return
		}
		writeStubUsage(w, llmTestUsage)
	}))
	t.Cleanup(upstream.Close)
	cfg.LLM.BaseURL = upstream.URL + "/v1"

	bundle := &project.Bundle{Agents: []agents.Agent{
		{Definition: agents.Definition{Name: "helper"}, Source: "test"},
	}}
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.runner = &recordingRunner{}
	mgr, err := session.New(cfg, bundle, store, hub.New(64), journal.NewMemory(), f.runner, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	f.mgr = mgr
	f.records = &flakyRecords{RecordStore: session.NewMemoryRecords()}
	mgr.SetRecords(f.records)
	f.server = New(cfg, mgr, nil, testLogger(), WithMetrics(f.metrics))

	sess, err := mgr.Create(context.Background(), session.CreateOptions{Prompt: "llm probe", Agent: "helper"})
	if err != nil {
		t.Fatal(err)
	}
	f.id = sess.ID
	f.token = f.runner.token(sess.ID)
	if f.token == "" {
		t.Fatal("session started without a token")
	}
	return f
}

// writeStubUsage writes one JSON provider response.
func writeStubUsage(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, payload)
}

// setUpstream replaces the provider behavior.
func (f *llmFixture) setUpstream(handler func(w http.ResponseWriter, r *http.Request, body []byte)) {
	f.mu.Lock()
	f.handler = handler
	f.mu.Unlock()
}

func (f *llmFixture) upstreamHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// do serves one LLM request through the real handler.
func (f *llmFixture) do(w http.ResponseWriter, body string) {
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	f.server.Handler().ServeHTTP(w, req)
}

func (f *llmFixture) chat(body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.do(rec, body)
	return rec
}

func (f *llmFixture) session() model.Session {
	f.t.Helper()
	sess, ok := f.mgr.Get(f.id)
	if !ok {
		f.t.Fatal("session is gone")
	}
	return *sess
}

func (f *llmFixture) usage() model.LLMUsage {
	f.t.Helper()
	sess := f.session()
	if sess.LLMUsage == nil {
		return model.LLMUsage{}
	}
	return *sess.LLMUsage
}

// scrape returns the Prometheus exposition of the server's registry.
func (f *llmFixture) scrape() string {
	rec := httptest.NewRecorder()
	f.metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// TestInternalLLMChargesReportedUsage covers a non-streamed call: the ledger
// takes the provider's usage and the token counter follows it.
func TestInternalLLMChargesReportedUsage(t *testing.T) {
	f := newLLMFixture(t, 1_000_000, 0)
	rec := f.chat(llmTestBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	usage := f.usage()
	if usage.PromptTokens != 30 || usage.CompletionTokens != 12 || usage.Requests != 1 || usage.EstimatedCalls != 0 {
		t.Fatalf("usage = %+v", usage)
	}

	body := f.scrape()
	for _, want := range []string{
		`mayfly_llm_tokens_total{kind="completion",model="stub"} 12`,
		`mayfly_llm_tokens_total{kind="prompt",model="stub"} 30`,
		`mayfly_llm_requests_total{model="stub",result="ok"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics are missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `mayfly_llm_usage_estimated_total{`) {
		t.Fatalf("reported usage counted as estimated:\n%s", body)
	}

	// The session object carries usage and budget but not the reservation.
	sess := adminRequest(t, f.server.Handler(), http.MethodGet, "/api/v1/sessions/"+f.id, "")
	if sess.Code != http.StatusOK {
		t.Fatalf("session status = %d body=%s", sess.Code, sess.Body.String())
	}
	if !strings.Contains(sess.Body.String(), `"llm_usage"`) || !strings.Contains(sess.Body.String(), `"llm_budget"`) {
		t.Fatalf("session does not report the ledger: %s", sess.Body.String())
	}
	if strings.Contains(sess.Body.String(), `"llm_pending_tokens"`) {
		t.Fatalf("session leaks the reservation: %s", sess.Body.String())
	}
}

// TestInternalLLMChargesStreamedUsage covers a streamed call: the usage chunk
// at the end of the stream reaches the ledger.
func TestInternalLLMChargesStreamedUsage(t *testing.T) {
	f := newLLMFixture(t, 0, 0)
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":7}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rec := f.chat(`{"model":"stub","max_tokens":100,"stream":true,"messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	usage := f.usage()
	if usage.PromptTokens != 21 || usage.CompletionTokens != 7 || usage.Requests != 1 || usage.EstimatedCalls != 0 {
		t.Fatalf("usage = %+v", usage)
	}
	if body := f.scrape(); !strings.Contains(body, `mayfly_llm_tokens_total{kind="prompt",model="stub"} 21`) {
		t.Fatalf("streamed tokens are not counted:\n%s", body)
	}
}

// TestInternalLLMExhaustedBudget covers the call after the request budget is
// spent: a 429 with the exhausted code, no upstream call, no charge, and the
// session closed with reason llm_budget_exhausted.
func TestInternalLLMExhaustedBudget(t *testing.T) {
	f := newLLMFixture(t, 0, 1)
	if rec := f.chat(llmTestBody); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", rec.Code, rec.Body.String())
	}
	before := f.usage()
	hits := f.upstreamHits()

	rec := f.chat(llmTestBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("exhausted refusal has Retry-After %q", got)
	}
	var refused struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
		t.Fatalf("refusal body: %v (%s)", err, rec.Body.String())
	}
	if refused.Code != session.CodeLLMBudgetExhausted {
		t.Fatalf("refusal code = %q body=%s", refused.Code, rec.Body.String())
	}
	if after := f.usage(); after != before {
		t.Fatalf("refused call charged: %+v -> %+v", before, after)
	}
	if f.upstreamHits() != hits {
		t.Fatalf("refused call reached the provider")
	}

	sess := f.session()
	if sess.Status != model.StatusClosing || sess.Error == "" {
		t.Fatalf("session = status %s error %q", sess.Status, sess.Error)
	}

	rec = adminRequest(t, f.server.Handler(), http.MethodGet, "/api/v1/sessions/"+f.id+"/events", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Events []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Data struct {
				Reason string `json:"reason"`
			} `json:"data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("events body: %v (%s)", err, rec.Body.String())
	}
	sawError, sawClose := false, false
	for _, ev := range page.Events {
		switch ev.Type {
		case string(model.EventError):
			sawError = strings.Contains(ev.Text, "budget exhausted")
		case string(model.EventControl):
			sawClose = ev.Data.Reason == session.CodeLLMBudgetExhausted
		}
	}
	if !sawError || !sawClose {
		t.Fatalf("error event = %v close reason = %v events=%s", sawError, sawClose, rec.Body.String())
	}

	body := f.scrape()
	if !strings.Contains(body, `mayfly_llm_requests_total{model="stub",result="over_budget"} 1`) {
		t.Fatalf("exhausted refusal is not labeled over_budget:\n%s", body)
	}
	if !strings.Contains(body, `mayfly_llm_tokens_total{kind="prompt",model="stub"} 30`) {
		t.Fatalf("refused call moved the token counter:\n%s", body)
	}
}

// TestInternalLLMPersistFailureRefusesAndLabels covers a reservation that the
// record store cannot keep: the budget fails closed with a 503, the call never
// reaches the provider, nothing is charged, and the metric names the refusal.
func TestInternalLLMPersistFailureRefusesAndLabels(t *testing.T) {
	f := newLLMFixture(t, 1000, 10)
	before := f.usage()
	hits := f.upstreamHits()
	f.records.setFail(true)

	rec := f.chat(llmTestBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("persist refusal has Retry-After %q", got)
	}
	var refused struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
		t.Fatalf("refusal body: %v (%s)", err, rec.Body.String())
	}
	if refused.Code != session.CodeLLMPersistFailed {
		t.Fatalf("refusal code = %q body=%s", refused.Code, rec.Body.String())
	}
	if after := f.usage(); after != before {
		t.Fatalf("refused call charged: %+v -> %+v", before, after)
	}
	if f.upstreamHits() != hits {
		t.Fatalf("refused call reached the provider")
	}

	// The slot was released: the next call is admitted once the store
	// recovers, not refused as llm_busy.
	f.records.setFail(false)
	if rec := f.chat(llmTestBody); rec.Code != http.StatusOK {
		t.Fatalf("call after the refusal = %d body=%s", rec.Code, rec.Body.String())
	}
	body := f.scrape()
	if !strings.Contains(body, `mayfly_llm_requests_total{model="stub",result="persist_failed"} 1`) {
		t.Fatalf("persist refusal is not labeled persist_failed:\n%s", body)
	}
}

// TestInternalLLMBusyDoesNotCharge covers two overlapping calls: the second is
// refused with Retry-After, does not touch the running call's ledger, and the
// first still settles its usage.
func TestInternalLLMBusyDoesNotCharge(t *testing.T) {
	f := newLLMFixture(t, 1_000_000, 0)
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	// Release the fake upstream on any exit, so an assertion failure cannot
	// leave the blocked call waiting until the test timeout.
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		writeStubUsage(w, llmTestUsage)
	})

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- f.chat(llmTestBody) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked call never reached the provider")
	}

	before := f.usage()
	rec := f.chat(llmTestBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d body=%s", rec.Code, rec.Body.String())
	}
	retry := rec.Header().Get("Retry-After")
	if retry == "" || retry == "0" {
		t.Fatalf("busy refusal Retry-After = %q", retry)
	}
	var refused struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
		t.Fatalf("refusal body: %v", err)
	}
	if refused.Code != session.CodeLLMBusy {
		t.Fatalf("refusal code = %q", refused.Code)
	}
	if after := f.usage(); after != before {
		t.Fatalf("busy refusal changed the running call's ledger: %+v -> %+v", before, after)
	}
	if before.Requests != 1 || before.PromptTokens != 0 {
		t.Fatalf("running call ledger = %+v", before)
	}

	releaseUpstream()
	rec = <-first
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", rec.Code, rec.Body.String())
	}
	if usage := f.usage(); usage.PromptTokens != 30 || usage.CompletionTokens != 12 || usage.Requests != 1 {
		t.Fatalf("first call was not charged: %+v", usage)
	}

	// The slot was released: the next call is admitted, not busy.
	if rec := f.chat(llmTestBody); rec.Code != http.StatusOK {
		t.Fatalf("third status = %d body=%s", rec.Code, rec.Body.String())
	}
	if usage := f.usage(); usage.Requests != 2 {
		t.Fatalf("after the retry requests = %d, want 2", usage.Requests)
	}
	body := f.scrape()
	if !strings.Contains(body, `mayfly_llm_requests_total{model="stub",result="busy"} 1`) {
		t.Fatalf("busy refusal is not labeled busy:\n%s", body)
	}
}

// TestInternalLLMBusyUntilUpstreamEOF covers a streamed call whose reply the
// client has already read to "data: [DONE]" while the broker still waits for
// upstream EOF. The agent's client stops at [DONE] (internal/llm/client.go),
// and the handler charges the call only after ServeLLM returns, which needs
// EOF (internal/httpapi/internal.go). The call therefore holds the session's
// only in-flight slot after the reply is complete on the wire: a second call
// is refused busy and the ledger keeps no token until the charge, and the
// retry after EOF is admitted.
func TestInternalLLMBusyUntilUpstreamEOF(t *testing.T) {
	f := newLLMFixture(t, 1_000_000, 0)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)
	f.setUpstream(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":7}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Hold the connection open: the reply is complete, but the broker has
		// not reached EOF and the charge has not run.
		<-release
	})

	// A real server, not a recorder, so the test reads the streamed bytes while
	// the handler is still running.
	api := httptest.NewServer(f.server.Handler())
	t.Cleanup(api.Close)
	const streamBody = `{"model":"stub","max_tokens":100,"stream":true,"messages":[]}`
	sawDone := make(chan error, 1)
	finished := make(chan string, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, api.URL+"/internal/v1/llm/chat/completions", strings.NewReader(streamBody))
		if err != nil {
			sawDone <- err
			return
		}
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := api.Client().Do(req)
		if err != nil {
			sawDone <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			sawDone <- fmt.Errorf("status %d", resp.StatusCode)
			return
		}
		seen := false
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if scanner.Text() == "data: [DONE]" && !seen {
				seen = true
				sawDone <- nil
			}
		}
		switch {
		case scanner.Err() != nil:
			finished <- scanner.Err().Error()
		case !seen:
			finished <- "the reply ended without data: [DONE]"
		default:
			finished <- ""
		}
	}()

	select {
	case err := <-sawDone:
		if err != nil {
			t.Fatalf("streamed call: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client never read data: [DONE]")
	}

	inFlight := f.usage()
	if inFlight.Requests != 1 || inFlight.Total() != 0 {
		t.Fatalf("usage while the stream is open = %+v, want one request and no token", inFlight)
	}
	rec := f.chat(llmTestBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("call after the client read [DONE] = %d body=%s", rec.Code, rec.Body.String())
	}
	if retry := rec.Header().Get("Retry-After"); retry == "" || retry == "0" {
		t.Fatalf("busy refusal Retry-After = %q", retry)
	}
	if code := refusalCodeOf(t, rec.Body.String()); code != session.CodeLLMBusy {
		t.Fatalf("refusal code = %q", code)
	}
	if after := f.usage(); after != inFlight {
		t.Fatalf("busy refusal changed the open call's ledger: %+v -> %+v", inFlight, after)
	}
	if hits := f.upstreamHits(); hits != 1 {
		t.Fatalf("upstream hits = %d, want the single open call", hits)
	}

	releaseUpstream()
	select {
	case msg := <-finished:
		if msg != "" {
			t.Fatalf("streamed call: %s", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the streamed call did not reach EOF")
	}

	// EOF runs the charge. The client's read may return before the deferred
	// charge does, so wait for the ledger rather than reading it once.
	deadline := time.Now().Add(10 * time.Second)
	for {
		usage := f.usage()
		if usage.PromptTokens == 21 && usage.CompletionTokens == 7 {
			if usage.Requests != 1 || usage.EstimatedCalls != 0 {
				t.Fatalf("settled usage = %+v", usage)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the call was not charged after EOF: %+v", usage)
		}
		time.Sleep(time.Millisecond)
	}

	// The slot is free: the retry is admitted and charged.
	f.setUpstream(nil)
	if rec := f.chat(llmTestBody); rec.Code != http.StatusOK {
		t.Fatalf("retry after EOF = %d body=%s", rec.Code, rec.Body.String())
	}
	if usage := f.usage(); usage.Requests != 2 || usage.PromptTokens != 51 || usage.CompletionTokens != 19 {
		t.Fatalf("usage after the retry = %+v, want requests 2 prompt 51 completion 19", usage)
	}
}

// panicWriter fails on the first response write, which is how the test lands a
// panic inside the proxy after the admit hook reserved the call.
type panicWriter struct {
	header http.Header
}

func (w *panicWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *panicWriter) WriteHeader(int) { panic("response writer panic") }

func (w *panicWriter) Write([]byte) (int, error) { panic("response writer panic") }

// TestInternalLLMPanicChargesReservation covers a panic after admit: the
// reservation is billed as a transport failure and the in-flight slot is not
// leaked.
func TestInternalLLMPanicChargesReservation(t *testing.T) {
	f := newLLMFixture(t, 1_000_000, 0)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the response-writer panic did not propagate")
			}
		}()
		f.do(&panicWriter{}, llmTestBody)
	}()

	usage := f.usage()
	wantPrompt := int64(len(llmTestBody)) / 4
	if usage.PromptTokens != wantPrompt || usage.CompletionTokens != 100 ||
		usage.EstimatedCalls != 1 || usage.Requests != 1 {
		t.Fatalf("reservation charge = %+v, want prompt=%d completion=100 estimated=1", usage, wantPrompt)
	}

	// A later call must not be stuck at llm_busy.
	f.setUpstream(nil)
	if rec := f.chat(llmTestBody); rec.Code != http.StatusOK {
		t.Fatalf("call after the panic = %d body=%s", rec.Code, rec.Body.String())
	}
	body := f.scrape()
	if !strings.Contains(body, `mayfly_llm_usage_estimated_total{model="stub"} 1`) {
		t.Fatalf("panic reservation is not counted as estimated:\n%s", body)
	}
	// The panic unwinds the proxy, so the request metric must be recorded by
	// the deferred charge. A forwarded call that panics is a transport
	// failure, not a refusal. The later call adds the second observation.
	if !strings.Contains(body, `mayfly_llm_requests_total{model="stub",result="error"} 1`) {
		t.Fatalf("panic call is not counted as an error:\n%s", body)
	}
	if !strings.Contains(body, `mayfly_llm_request_duration_seconds_count{model="stub"} 2`) {
		t.Fatalf("panic call has no duration observation:\n%s", body)
	}
}
