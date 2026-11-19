package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/broker"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/project"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/ratelimit"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/session"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testManager(t *testing.T, cfg *config.Config) *session.Manager {
	t.Helper()
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.New(cfg, nil, store, hub.New(10), journal.NewMemory(), nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func testQueue(t *testing.T) queue.Store {
	t.Helper()
	return testQueueWith(t, config.Default().Queue)
}

func testQueueWith(t *testing.T, cfg config.QueueConfig) queue.Store {
	t.Helper()
	store, err := queue.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestWebhookRateLimited(t *testing.T) {
	cfg := config.Default()
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	s := New(cfg, nil, testQueue(t), testLogger())
	s.limiters["generic"] = ratelimit.New(1, time.Hour, 1)
	if !s.limiters["generic"].Allow() {
		t.Fatal("burst token was not available")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hooks/generic", strings.NewReader(`{"prompt":"x"}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestUnsignedWebhooksDoNotSpendRateLimit(t *testing.T) {
	cfg := config.Default()
	cfg.Triggers = []config.TriggerConfig{{Name: "github", Type: "github", Secret: "s3cret"}}
	s := New(cfg, nil, testQueue(t), testLogger())
	s.limiters["github"] = ratelimit.New(1, time.Hour, 1)

	for i := range 10 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(`{"action":"opened"}`))
		req.Header.Set("X-GitHub-Event", "push")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d status = %d, want 401", i, rec.Code)
		}
	}
	if !s.limiters["github"].Allow() {
		t.Fatal("unsigned traffic consumed the trigger rate limit")
	}
}

func TestReadyReflectsManagerState(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.Image = "example/agent:1"
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.New(cfg, nil, store, hub.New(10), journal.NewMemory(), nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, mgr, testQueue(t), testLogger())
	handler := s.Handler()

	check := func(want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != want {
			t.Fatalf("readyz = %d body=%s", rec.Code, rec.Body.String())
		}
	}

	check(http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	check(http.StatusOK)
	mgr.Shutdown(context.Background())
	check(http.StatusServiceUnavailable)
}

// TestWithBrokerSharesOneClient keeps the HTTP API and the tool gateway on the
// same broker instead of building a second upstream client.
func TestWithBrokerSharesOneClient(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.Image = "example/agent:1"
	shared := broker.New(cfg.LLM, testLogger())
	s := New(cfg, testManager(t, cfg), testQueue(t), testLogger(), WithBroker(shared))
	if s.broker != shared {
		t.Fatal("the server built its own broker")
	}
	if s.broker.HTTPClient() != shared.HTTPClient() {
		t.Fatal("the brokers do not share one HTTP client")
	}
}

func TestWebhookDeduplicatesByDeliveryKey(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.Image = "example/agent:1"
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	store := testQueue(t)
	s := New(cfg, testManager(t, cfg), store, testLogger())
	handler := s.Handler()

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/hooks/generic", strings.NewReader(`{"prompt":"one run"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "delivery-42")
		handler.ServeHTTP(rec, req)
		return rec
	}

	first := post()
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}
	second := post()
	if second.Code != http.StatusAccepted {
		t.Fatalf("duplicate status = %d body=%s", second.Code, second.Body.String())
	}
	var out struct {
		Accepted  bool `json:"accepted"`
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Accepted || !out.Duplicate {
		t.Fatalf("duplicate response = %s", second.Body.String())
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Fatalf("pending jobs = %d, want 1", stats.Pending)
	}
}

func TestWebhookRejectsInvalidSessionOptions(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.Image = "example/agent:1"
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	store := testQueue(t)
	s := New(cfg, testManager(t, cfg), store, testLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hooks/generic", strings.NewReader(`{"prompt":"x","agent":"no-such-agent"}`))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no-such-agent") {
		t.Fatalf("body does not name the agent: %s", rec.Body.String())
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 {
		t.Fatalf("pending jobs = %d, want 0", stats.Pending)
	}
}

func TestQueueAdminEndpoints(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "test-admin"
	qcfg := config.Default().Queue
	qcfg.Backend = "memory"
	qcfg.MaxAttempts = 1
	store := testQueueWith(t, qcfg)
	s := New(cfg, nil, store, testLogger())
	handler := s.Handler()
	ctx := context.Background()

	do := func(method, target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Authorization", "Bearer test-admin")
		handler.ServeHTTP(rec, req)
		return rec
	}

	job := queue.NewJob("generic", "", session.CreateOptions{Prompt: "will fail"}, time.Now())
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

	rec := do(http.MethodGet, "/api/v1/queue/dead")
	if rec.Code != http.StatusOK {
		t.Fatalf("dead list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []queueRecord `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != job.ID || list.Items[0].State != queue.StateDead {
		t.Fatalf("dead list = %+v", list.Items)
	}
	if !strings.Contains(list.Items[0].Error, "provider said no") {
		t.Fatalf("dead error = %q", list.Items[0].Error)
	}

	if rec = do(http.MethodPost, "/api/v1/queue/dead/"+job.ID+"/retry"); rec.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec = do(http.MethodGet, "/api/v1/queue/dead"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("dead list after retry = %d %s", rec.Code, rec.Body.String())
	}

	leases, err = store.Claim(ctx, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim retried job = %+v, %v", leases, err)
	}
	if err := store.Nack(ctx, leases[0], errors.New("still failing")); err != nil {
		t.Fatal(err)
	}
	if rec = do(http.MethodDelete, "/api/v1/queue/dead/"+job.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec = do(http.MethodDelete, "/api/v1/queue/dead/"+job.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d", rec.Code)
	}

	done := queue.NewJob("generic", "", session.CreateOptions{Prompt: "works"}, time.Now())
	if err := store.Accept(ctx, done); err != nil {
		t.Fatal(err)
	}
	leases, err = store.Claim(ctx, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim done job = %+v, %v", leases, err)
	}
	if err := store.Ack(ctx, leases[0], queue.Result{SessionID: "session-7"}); err != nil {
		t.Fatal(err)
	}
	rec = do(http.MethodGet, "/api/v1/queue/jobs/"+done.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("job lookup status = %d body=%s", rec.Code, rec.Body.String())
	}
	var lookup struct {
		Job queueRecord `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lookup); err != nil {
		t.Fatal(err)
	}
	if lookup.Job.State != queue.StateDone || lookup.Job.SessionID != "session-7" || lookup.Job.Prompt != "works" {
		t.Fatalf("job lookup = %+v", lookup.Job)
	}
	if rec = do(http.MethodGet, "/api/v1/queue/jobs/missing"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job status = %d", rec.Code)
	}
}

// recordingRunner stands in for a runtime process and keeps the session token
// the manager handed it, so a test can authenticate like a real runtime.
type recordingRunner struct {
	mu     sync.Mutex
	tokens map[string]string
}

func (r *recordingRunner) Start(_ context.Context, spec runner.Spec) (runner.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tokens == nil {
		r.tokens = map[string]string{}
	}
	r.tokens[spec.SessionID] = spec.Token
	return runner.Handle{Backend: spec.Backend, SessionID: spec.SessionID}, nil
}

func (r *recordingRunner) Stop(context.Context, runner.Handle, bool) error { return nil }

func (r *recordingRunner) Logs(context.Context, runner.Handle, int64) (string, error) {
	return "", nil
}

func (r *recordingRunner) token(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens[id]
}

// toolFixture is one API server whose manager holds a real session per
// harness, so the tool routes authenticate exactly as they do in production.
type toolFixture struct {
	handler   http.Handler
	gateway   *gateway.Gateway
	runner    *recordingRunner
	helper    model.Session
	alternate model.Session
}

func newToolFixture(t *testing.T, manifests ...plugin.Manifest) *toolFixture {
	t.Helper()
	cfg := config.Default()
	cfg.Runtime.Backend = "local"
	cfg.Runtime.StateDir = t.TempDir()
	cfg.Runtime.Image = "example/agent:1"
	cfg.LLM.Model = "stub"
	cfg.Server.InternalURL = "http://mayfly:8080"
	bundle := &project.Bundle{
		Agents: []agents.Agent{
			{Definition: agents.Definition{Name: "helper"}, Source: "test"},
			{Definition: agents.Definition{Name: "alternate", Harness: "pi"}, Source: "test"},
		},
	}
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	mgr, err := session.New(cfg, bundle, store, hub.New(10), journal.NewMemory(), runner, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(gateway.Options{
		Ledger:    journal.NewMemory(),
		Manifests: func() []plugin.Manifest { return manifests },
		Logger:    testLogger(),
	})
	server := New(cfg, mgr, nil, testLogger(), WithGateway(gw))

	ctx := context.Background()
	helper, err := mgr.Create(ctx, session.CreateOptions{Prompt: "one", Agent: "helper"})
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := mgr.Create(ctx, session.CreateOptions{Prompt: "two", Agent: "alternate"})
	if err != nil {
		t.Fatal(err)
	}
	grant := gateway.Grant{
		SessionID: helper.ID,
		Agent:     "helper",
		MaxSteps:  4,
		Tools: map[string]gateway.GrantTool{
			"plugin_alpha_get": {
				Name:       "plugin_alpha_get",
				Owner:      "alpha",
				Remote:     "get",
				Placement:  gateway.Inline,
				Idempotent: true,
				Timeout:    30 * time.Second,
				MaxOutput:  4096,
			},
		},
	}
	gw.Register(grant)
	// The alternate harness gets a grant too, so its 404 can only come from
	// the harness rule and not from a missing session.
	alternateGrant := grant
	alternateGrant.SessionID = alternate.ID
	alternateGrant.Agent = "alternate"
	gw.Register(alternateGrant)

	return &toolFixture{
		handler:   server.Handler(),
		gateway:   gw,
		runner:    runner,
		helper:    *helper,
		alternate: *alternate,
	}
}

func (f *toolFixture) do(method, id, suffix, token, body string) *httptest.ResponseRecorder {
	target := "/internal/v1/sessions/" + id + suffix
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestInternalToolsAuth(t *testing.T) {
	f := newToolFixture(t)
	token := f.runner.token(f.helper.ID)
	if token == "" {
		t.Fatal("manager did not hand the runtime a token")
	}

	rec := f.do(http.MethodGet, f.helper.ID, "/tools", "wrong-token", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodGet, f.helper.ID, "/tools", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", rec.Code)
	}

	rec = f.do(http.MethodGet, f.alternate.ID, "/tools", f.runner.token(f.alternate.ID), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("alternate harness status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = f.do(http.MethodPost, f.helper.ID, "/tools/calls", token, `{"turn":1,"step":1,"tool":"plugin_beta_query"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted tool status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = f.do(http.MethodGet, f.helper.ID, "/tools", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tools status = %d body=%s", rec.Code, rec.Body.String())
	}
	var discovery gateway.Discovery
	if err := json.Unmarshal(rec.Body.Bytes(), &discovery); err != nil {
		t.Fatal(err)
	}
	if len(discovery.Tools) != 1 || discovery.Tools[0].Name != "plugin_alpha_get" || discovery.Tools[0].Owner != "alpha" {
		t.Fatalf("discovery = %+v", discovery.Tools)
	}
	if discovery.Tools[0].TimeoutMS != 30000 || discovery.Tools[0].Placement != gateway.Inline {
		t.Fatalf("grant fields = %+v", discovery.Tools[0])
	}
}

func TestInternalToolCallRoundTrip(t *testing.T) {
	f := newToolFixture(t)
	token := f.runner.token(f.helper.ID)
	calls := "/tools/calls"

	rec := f.do(http.MethodPost, f.helper.ID, calls, token, `{"turn":0,"step":1,"tool":"plugin_alpha_get"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad turn status = %d body=%s", rec.Code, rec.Body.String())
	}

	begin := `{"turn":1,"step":1,"call_id":"call_ab","tool":"plugin_alpha_get","arguments":{"b":2,"a":1}}`
	rec = f.do(http.MethodPost, f.helper.ID, calls, token, begin)
	if rec.Code != http.StatusOK {
		t.Fatalf("begin status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp gateway.BeginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Dispatch == nil || resp.Result != nil {
		t.Fatalf("begin response = %+v", resp)
	}
	wantKey := f.helper.ID + "/1/1"
	if resp.Dispatch.Key != wantKey || resp.Dispatch.TimeoutMS != 30000 || resp.Dispatch.Token != "" {
		t.Fatalf("dispatch = %+v", resp.Dispatch)
	}

	rec = f.do(http.MethodPost, f.helper.ID, calls+"/complete", token, `{"key":"`+wantKey+`","content":"done"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("complete status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodPost, f.helper.ID, calls+"/complete", token, `{"key":"`+wantKey+`","content":"done"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("repeated complete status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodPost, f.helper.ID, calls+"/complete", token, `{"key":"`+wantKey+`","content":"other"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("differing complete status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodPost, f.helper.ID, calls+"/complete", token, `{"key":"missing","content":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = f.do(http.MethodPost, f.helper.ID, calls, token, begin)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d body=%s", rec.Code, rec.Body.String())
	}
	resp = gateway.BeginResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result == nil || resp.Dispatch != nil {
		t.Fatalf("replay response = %+v", resp)
	}
	if resp.Result.Content != "done" || !resp.Result.Replayed || resp.Result.IsError {
		t.Fatalf("replay result = %+v", resp.Result)
	}

	other := `{"turn":1,"step":1,"tool":"plugin_alpha_get","arguments":{"a":2}}`
	rec = f.do(http.MethodPost, f.helper.ID, calls, token, other)
	if rec.Code != http.StatusConflict {
		t.Fatalf("changed arguments status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestInternalToolCallRateLimited(t *testing.T) {
	f := newToolFixture(t, plugin.Manifest{
		Name:   "alpha",
		Limits: plugin.Limits{Rate: &plugin.Rate{Requests: 1, Interval: "1h", Burst: 1}},
	})
	token := f.runner.token(f.helper.ID)

	begin := func(step int) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"turn":1,"step":%d,"tool":"plugin_alpha_get"}`, step)
		return f.do(http.MethodPost, f.helper.ID, "/tools/calls", token, body)
	}
	if rec := begin(1); rec.Code != http.StatusOK {
		t.Fatalf("first call status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := begin(2)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d body=%s", rec.Code, rec.Body.String())
	}
	retry := rec.Header().Get("Retry-After")
	if seconds, err := strconv.Atoi(retry); err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q", retry)
	}
}

// adminGET sends one authorized admin request.
func adminGET(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer test-admin")
	handler.ServeHTTP(rec, req)
	return rec
}

// decodeEnvelope asserts a JSON error envelope and returns its body.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q body=%s", got, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Code == "" || env.Error.Message == "" {
		t.Fatalf("incomplete envelope: %s", rec.Body.String())
	}
	return env.Error
}

// sessionServer builds a server whose manager holds real, started sessions so
// the list routes have records to page over.
func sessionServer(t *testing.T, count int) (*Server, *session.Manager) {
	t.Helper()
	cfg := config.Default()
	cfg.AdminToken = "test-admin"
	cfg.Runtime.Backend = "local"
	cfg.Runtime.StateDir = t.TempDir()
	cfg.Runtime.Image = "example/agent:1"
	cfg.Server.InternalURL = "http://mayfly:8080"
	cfg.LLM.Model = "stub"
	bundle := &project.Bundle{Agents: []agents.Agent{
		{Definition: agents.Definition{Name: "helper"}, Source: "test"},
	}}
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.New(cfg, bundle, store, hub.New(10), journal.NewMemory(), &recordingRunner{}, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i := range count {
		if _, err := mgr.Create(context.Background(), session.CreateOptions{
			Prompt: fmt.Sprintf("page probe %d", i),
			Agent:  "helper",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return New(cfg, mgr, nil, testLogger()), mgr
}

func TestErrorEnvelope(t *testing.T) {
	server, _ := sessionServer(t, 1)
	handler := server.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != codeUnauthorized {
		t.Fatalf("missing token code = %q", body.Code)
	}

	rec = adminGET(t, handler, "/api/v1/sessions/missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != codeNotFound {
		t.Fatalf("unknown session code = %q", body.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader("{"))
	req.Header.Set("Authorization", "Bearer test-admin")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != codeInvalidRequest {
		t.Fatalf("bad body code = %q", body.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/missing", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown trigger status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != codeNotFound {
		t.Fatalf("unknown trigger code = %q", body.Code)
	}
}

func TestSessionErrorMapping(t *testing.T) {
	s := New(config.Default(), nil, nil, testLogger())
	cases := []struct {
		err    error
		status int
		code   string
		retry  bool
	}{
		{fmt.Errorf("lookup: %w", session.ErrNotFound), http.StatusNotFound, codeNotFound, false},
		{fmt.Errorf("post: %w", session.ErrClosed), http.StatusConflict, codeConflict, false},
		{fmt.Errorf("auth: %w", session.ErrUnauthorized), http.StatusUnauthorized, codeUnauthorized, false},
		{session.ErrCapacity, http.StatusTooManyRequests, codeRateLimited, true},
		{session.ErrEmptyMessage, http.StatusBadRequest, codeInvalidRequest, false},
		{fmt.Errorf("%w: 40000 bytes (limit 32768)", session.ErrMessageTooLong), http.StatusBadRequest, codeInvalidRequest, false},
		{errors.New("storage exploded"), http.StatusInternalServerError, codeInternal, false},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.writeSessionError(rec, tc.err)
		if rec.Code != tc.status {
			t.Fatalf("%v status = %d, want %d", tc.err, rec.Code, tc.status)
		}
		if body := decodeEnvelope(t, rec); body.Code != tc.code {
			t.Fatalf("%v code = %q, want %q", tc.err, body.Code, tc.code)
		}
		if got := rec.Header().Get("Retry-After") != ""; got != tc.retry {
			t.Fatalf("%v Retry-After set = %v, want %v", tc.err, got, tc.retry)
		}
		if strings.Contains(rec.Body.String(), "storage exploded") {
			t.Fatalf("unknown error leaked its text: %s", rec.Body.String())
		}
	}
}

func TestPostMessageRejectsBadText(t *testing.T) {
	server, mgr := sessionServer(t, 1)
	sessions := mgr.List()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d", len(sessions))
	}
	id := sessions[0].ID
	handler := server.Handler()

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+id+"/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin")
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := post(`{"text":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty message status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeEnvelope(t, rec); body.Code != codeInvalidRequest {
		t.Fatalf("empty message code = %q", body.Code)
	}

	rec = post(`{"text":"` + strings.Repeat("x", 32<<10+1) + `"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long message status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeEnvelope(t, rec); body.Code != codeInvalidRequest {
		t.Fatalf("long message code = %q", body.Code)
	}
}

func TestPanicRecoveryUsesEnvelope(t *testing.T) {
	s := New(config.Default(), nil, nil, testLogger())
	handler := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != codeInternal {
		t.Fatalf("panic code = %q", body.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/x/tools", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("internal panic status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "internal server error\n" {
		t.Fatalf("internal panic body = %q", got)
	}
}

func TestUnknownRouteUsesEnvelope(t *testing.T) {
	server, _ := sessionServer(t, 0)
	handler := server.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != codeNotFound {
		t.Fatalf("unknown route code = %q", body.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/sessions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != codeMethodNotAllowed {
		t.Fatalf("wrong method code = %q", body.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
		t.Fatalf("Allow = %q", allow)
	}
}

func TestSessionListPagination(t *testing.T) {
	server, mgr := sessionServer(t, 5)
	handler := server.Handler()

	want := mgr.List()
	sort.Slice(want, func(i, j int) bool {
		return newerKey(sessionKey(want[i]), sessionKey(want[j]))
	})

	var got []model.Session
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination did not finish")
		}
		target := "/api/v1/sessions?limit=2"
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := adminGET(t, handler, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("page status = %d body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Items      []model.Session `json:"items"`
			NextCursor string          `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) > 2 {
			t.Fatalf("page holds %d items", len(out.Items))
		}
		got = append(got, out.Items...)
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	if len(got) != len(want) {
		t.Fatalf("paged over %d sessions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("position %d = %s, want %s", i, got[i].ID, want[i].ID)
		}
	}
}

func TestListPaginationRejectsBadInput(t *testing.T) {
	server, _ := sessionServer(t, 0)
	handler := server.Handler()
	for _, target := range []string{
		"/api/v1/sessions?limit=0",
		"/api/v1/sessions?limit=501",
		"/api/v1/sessions?limit=two",
		"/api/v1/sessions?cursor=not-base64!",
	} {
		rec := adminGET(t, handler, target)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d body=%s", target, rec.Code, rec.Body.String())
		}
		if body := decodeEnvelope(t, rec); body.Code != codeInvalidRequest {
			t.Fatalf("%s code = %q", target, body.Code)
		}
	}
}

func TestDeadLetterPagination(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "test-admin"
	qcfg := config.Default().Queue
	qcfg.Backend = "memory"
	qcfg.MaxAttempts = 1
	store := testQueueWith(t, qcfg)
	ctx := context.Background()

	for i := range 3 {
		job := queue.NewJob("generic", "", session.CreateOptions{Prompt: fmt.Sprintf("dead %d", i)}, time.Now())
		if err := store.Accept(ctx, job); err != nil {
			t.Fatal(err)
		}
		leases, err := store.Claim(ctx, 1)
		if err != nil || len(leases) != 1 {
			t.Fatalf("claim = %v, %v", leases, err)
		}
		if err := store.Nack(ctx, leases[0], fmt.Errorf("reason %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	s := New(cfg, nil, store, testLogger())
	handler := s.Handler()

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination did not finish")
		}
		target := "/api/v1/queue/dead?limit=2"
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := adminGET(t, handler, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("page status = %d body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Items      []queueRecord `json:"items"`
			NextCursor string        `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, item := range out.Items {
			if seen[item.ID] {
				t.Fatalf("job %s appeared twice", item.ID)
			}
			seen[item.ID] = true
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("paged over %d dead jobs, want 3", len(seen))
	}
}

func TestScheduleListPagination(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "test-admin"
	s := New(cfg, nil, nil, testLogger(), WithSchedules(ScheduleService{List: []ScheduleInfo{
		{Name: "charlie"},
		{Name: "alpha"},
		{Name: "bravo"},
	}}))
	handler := s.Handler()

	rec := adminGET(t, handler, "/api/v1/schedules?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d body=%s", rec.Code, rec.Body.String())
	}
	var first struct {
		Items      []ScheduleInfo `json:"items"`
		NextCursor string         `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.Items[0].Name != "alpha" || first.Items[1].Name != "bravo" {
		t.Fatalf("first page = %+v", first.Items)
	}
	if first.NextCursor == "" {
		t.Fatal("first page has no cursor")
	}

	rec = adminGET(t, handler, "/api/v1/schedules?limit=2&cursor="+url.QueryEscape(first.NextCursor))
	if rec.Code != http.StatusOK {
		t.Fatalf("second page status = %d body=%s", rec.Code, rec.Body.String())
	}
	var second struct {
		Items      []ScheduleInfo `json:"items"`
		NextCursor string         `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Name != "charlie" {
		t.Fatalf("second page = %+v", second.Items)
	}
	if second.NextCursor != "" {
		t.Fatalf("last page cursor = %q", second.NextCursor)
	}
}

// failingRunner rejects the runtime start, so Create fails after the session
// record exists.
type failingRunner struct{}

func (failingRunner) Start(context.Context, runner.Spec) (runner.Handle, error) {
	return runner.Handle{}, errors.New("admission webhook rejected the pod")
}

func (failingRunner) Stop(context.Context, runner.Handle, bool) error { return nil }

func (failingRunner) Logs(context.Context, runner.Handle, int64) (string, error) {
	return "", nil
}

func TestCreateFailureReportsSessionID(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "test-admin"
	cfg.Runtime.Backend = "local"
	cfg.Runtime.StateDir = t.TempDir()
	cfg.Runtime.Image = "example/agent:1"
	cfg.Server.InternalURL = "http://mayfly:8080"
	cfg.LLM.Model = "stub"
	bundle := &project.Bundle{Agents: []agents.Agent{
		{Definition: agents.Definition{Name: "helper"}, Source: "test"},
	}}
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.New(cfg, bundle, store, hub.New(10), journal.NewMemory(), failingRunner{}, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, mgr, nil, testLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"prompt":"x","agent":"helper"}`))
	req.Header.Set("Authorization", "Bearer test-admin")
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeEnvelope(t, rec)
	if body.Code != codeInternal {
		t.Fatalf("code = %q", body.Code)
	}
	id, _ := body.Details["session_id"].(string)
	if id == "" {
		t.Fatalf("details do not carry the session id: %+v", body.Details)
	}
	if _, ok := mgr.Get(id); !ok {
		t.Fatalf("session %s is not readable", id)
	}
	if strings.Contains(rec.Body.String(), "admission webhook") {
		t.Fatalf("internal error text leaked: %s", rec.Body.String())
	}
}
