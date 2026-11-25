package session

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/broker"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// budgetInstance registers a live session with a resolved budget.
func budgetInstance(m *Manager, id string, tokens, requests int) *instance {
	inst := &instance{
		sess: model.Session{ID: id, Status: model.StatusRunning, CreatedAt: time.Now().UTC()},
	}
	inst.setLLMBudget(tokens, requests)
	m.mu.Lock()
	m.sessions[id] = inst
	m.mu.Unlock()
	return inst
}

// liveUsage returns the charged usage of a live session.
func liveUsage(t *testing.T, m *Manager, id string) model.LLMUsage {
	t.Helper()
	inst, ok := m.instance(id)
	if !ok {
		t.Fatalf("session %s is not live", id)
	}
	inst.llmMu.Lock()
	defer inst.llmMu.Unlock()
	return inst.llm.spent
}

// refusalCode returns the Refusal code of err.
func refusalCode(err error) string {
	var refusal *broker.Refusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

func TestCreateResolvesLLMBudget(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	m.runner = nopRunner{}
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	m.cfg.Sessions.MaxLLMTokens = 1000
	m.cfg.Sessions.MaxLLMRequests = 5
	override := agents.Agent{Definition: agents.Definition{
		Name:           "frugal",
		MaxLLMTokens:   50,
		MaxLLMRequests: 2,
	}}
	m.agents = append(m.agents, override)
	m.agentByName[override.Name] = override

	global, err := m.Create(ctx, CreateOptions{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	inst, ok := m.instance(global.ID)
	if !ok {
		t.Fatal("session not registered")
	}
	if inst.runtimeCfg.MaxLLMTokens != 1000 || inst.runtimeCfg.MaxLLMRequests != 5 {
		t.Fatalf("runtime config budget = %d / %d", inst.runtimeCfg.MaxLLMTokens, inst.runtimeCfg.MaxLLMRequests)
	}
	if global.LLMBudget == nil || global.LLMBudget.MaxTokens != 1000 || global.LLMBudget.MaxRequests != 5 {
		t.Fatalf("session budget = %+v", global.LLMBudget)
	}

	limited, err := m.Create(ctx, CreateOptions{Prompt: "hello", Agent: "frugal"})
	if err != nil {
		t.Fatal(err)
	}
	inst, _ = m.instance(limited.ID)
	if inst.runtimeCfg.MaxLLMTokens != 50 || inst.runtimeCfg.MaxLLMRequests != 2 {
		t.Fatalf("override runtime config = %d / %d", inst.runtimeCfg.MaxLLMTokens, inst.runtimeCfg.MaxLLMRequests)
	}
	if limited.LLMBudget == nil || limited.LLMBudget.MaxTokens != 50 || limited.LLMBudget.MaxRequests != 2 {
		t.Fatalf("override session budget = %+v", limited.LLMBudget)
	}
	info := m.agentInfo(override)
	if info.MaxLLMTokens != 50 || info.MaxLLMRequests != 2 {
		t.Fatalf("agent info budget = %d / %d", info.MaxLLMTokens, info.MaxLLMRequests)
	}
}

func TestLLMAdmitCharge(t *testing.T) {
	m := testManager(t)
	inst := budgetInstance(m, "s1", 10000, 10)

	// 400 request bytes reserve 100 tokens, plus the 100 token output cap.
	if err := m.AdmitLLM("s1", 400, 100); err != nil {
		t.Fatal(err)
	}
	usage := liveUsage(t, m, "s1")
	if usage.Requests != 1 || inst.llm.pending != 200 || inst.llm.inflight != 1 {
		t.Fatalf("reservation = %+v pending=%d inflight=%d", usage, inst.llm.pending, inst.llm.inflight)
	}
	sess, _ := m.Get("s1")
	if sess.LLMPendingTokens != 200 || sess.LLMUsage == nil || sess.LLMUsage.Requests != 1 {
		t.Fatalf("session snapshot = %+v", sess)
	}
	rec, ok := storedRecord(t, m, "s1")
	if !ok || rec.LLMPendingTokens != 200 || rec.LLMUsage == nil || rec.LLMUsage.Requests != 1 {
		t.Fatalf("stored reservation = %+v", rec)
	}

	// A second call while one is in flight is busy and changes nothing.
	err := m.AdmitLLM("s1", 400, 100)
	if refusalCode(err) != CodeLLMBusy {
		t.Fatalf("second admit = %v", err)
	}
	if usage := liveUsage(t, m, "s1"); usage.Requests != 1 {
		t.Fatalf("busy refusal counted a request: %+v", usage)
	}

	if err := m.ChargeLLM("s1", broker.CallResult{
		Status: 200, Reported: true, Forwarded: true,
		Usage: broker.Usage{Prompt: 30, Completion: 12},
	}); err != nil {
		t.Fatal(err)
	}
	usage = liveUsage(t, m, "s1")
	if usage.PromptTokens != 30 || usage.CompletionTokens != 12 || usage.Requests != 1 || usage.EstimatedCalls != 0 {
		t.Fatalf("charged usage = %+v", usage)
	}
	if inst.llm.pending != 0 || inst.llm.inflight != 0 || inst.llm.exhausted {
		t.Fatalf("settled ledger = %+v", inst.llm)
	}
	rec, _ = storedRecord(t, m, "s1")
	if rec.LLMPendingTokens != 0 || rec.LLMUsage == nil || rec.LLMUsage.CompletionTokens != 12 {
		t.Fatalf("stored usage = %+v", rec)
	}

	// A charge without an admission is a no-op.
	if err := m.ChargeLLM("s1", broker.CallResult{Status: 200, Reported: true, Forwarded: true, Usage: broker.Usage{Prompt: 1000}}); err != nil {
		t.Fatal(err)
	}
	if usage := liveUsage(t, m, "s1"); usage.PromptTokens != 30 {
		t.Fatalf("stray charge applied: %+v", usage)
	}
}

func TestLLMChargeWithoutForwardedCostsNothing(t *testing.T) {
	m := testManager(t)
	inst := budgetInstance(m, "s1", 1000, 10)
	if err := m.AdmitLLM("s1", 400, 100); err != nil {
		t.Fatal(err)
	}
	if err := m.ChargeLLM("s1", broker.CallResult{Status: 200, Forwarded: false}); err != nil {
		t.Fatal(err)
	}
	usage := liveUsage(t, m, "s1")
	if usage.PromptTokens != 0 || usage.CompletionTokens != 0 || usage.EstimatedCalls != 0 || usage.Requests != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	if inst.llm.pending != 0 || inst.llm.inflight != 0 {
		t.Fatalf("ledger = %+v", inst.llm)
	}
}

func TestLLMChargeEstimate(t *testing.T) {
	cases := []struct {
		name       string
		res        broker.CallResult
		prompt     int64
		completion int64
		estimated  int64
	}{
		{
			name:   "reported usage",
			res:    broker.CallResult{Status: 200, Forwarded: true, Reported: true, Usage: broker.Usage{Prompt: 7, Completion: 11}},
			prompt: 7, completion: 11,
		},
		{
			name:   "2xx without usage",
			res:    broker.CallResult{Status: 200, Forwarded: true, ReqBytes: 400, RespBytes: 800},
			prompt: 100, completion: 200, estimated: 1,
		},
		{
			name:   "upstream 500 without usage",
			res:    broker.CallResult{Status: 500, Forwarded: true, ReqBytes: 800, RespBytes: 80},
			prompt: 200, completion: 20, estimated: 1,
		},
		{
			// A timeout after the upstream request surfaces as a 502, and the
			// provider may already have billed it.
			name:   "timeout behind a 502",
			res:    broker.CallResult{Status: 502, Forwarded: true, ReqBytes: 400, RespBytes: 0},
			prompt: 100, estimated: 1,
		},
		{
			name:      "forwarded 4xx without usage",
			res:       broker.CallResult{Status: 429, Forwarded: true, ReqBytes: 400, RespBytes: 120},
			estimated: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(t)
			budgetInstance(m, "s1", 1_000_000, 10)
			if err := m.AdmitLLM("s1", tc.res.ReqBytes, 100); err != nil {
				t.Fatal(err)
			}
			if err := m.ChargeLLM("s1", tc.res); err != nil {
				t.Fatal(err)
			}
			usage := liveUsage(t, m, "s1")
			if usage.PromptTokens != tc.prompt || usage.CompletionTokens != tc.completion ||
				usage.EstimatedCalls != tc.estimated || usage.Requests != 1 {
				t.Fatalf("usage = %+v, want prompt=%d completion=%d estimated=%d",
					usage, tc.prompt, tc.completion, tc.estimated)
			}
		})
	}
}

func TestLLMOvershootAtMostOneResponse(t *testing.T) {
	m := testManager(t)
	inst := budgetInstance(m, "s1", 100, 0)

	if err := m.AdmitLLM("s1", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.ChargeLLM("s1", reported(90)); err != nil {
		t.Fatal(err)
	}
	// 90 of 100 tokens used, so one more call may start.
	if err := m.AdmitLLM("s1", 0, 0); err != nil {
		t.Fatalf("admit below the cap: %v", err)
	}
	if err := m.ChargeLLM("s1", reported(50)); err != nil {
		t.Fatal(err)
	}
	usage := liveUsage(t, m, "s1")
	if usage.Total() != 140 {
		t.Fatalf("tokens = %d", usage.Total())
	}
	if overshoot := usage.Total() - inst.llm.maxTokens; overshoot > 50 {
		t.Fatalf("overshoot = %d, want at most one 50 token response", overshoot)
	}
	if err := m.AdmitLLM("s1", 0, 0); refusalCode(err) != CodeLLMBudgetExhausted {
		t.Fatalf("admit after the cap = %v", err)
	}
	if usage := liveUsage(t, m, "s1"); usage.Requests != 2 {
		t.Fatalf("requests = %d, want 2", usage.Requests)
	}
}

func TestLLMRequestCapOvershootsByZero(t *testing.T) {
	m := testManager(t)
	budgetInstance(m, "s1", 0, 1)
	if err := m.AdmitLLM("s1", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.ChargeLLM("s1", reported(1)); err != nil {
		t.Fatal(err)
	}
	err := m.AdmitLLM("s1", 0, 0)
	if refusalCode(err) != CodeLLMBudgetExhausted {
		t.Fatalf("second admit = %v", err)
	}
	if usage := liveUsage(t, m, "s1"); usage.Requests != 1 {
		t.Fatalf("requests = %d, want 1", usage.Requests)
	}
}

func TestLLMExhaustionClosesSession(t *testing.T) {
	m := testManager(t)
	budgetInstance(m, "s1", 10, 0)
	if err := m.AdmitLLM("s1", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.ChargeLLM("s1", reported(10)); err != nil {
		t.Fatal(err)
	}

	var errorText string
	var controlReason string
	for _, ev := range m.hub.EventsAfter("s1", 0) {
		switch ev.Type {
		case model.EventError:
			errorText = ev.Text
		case model.EventControl:
			if control, ok := ev.Data.(model.ControlData); ok {
				controlReason = control.Reason
			}
		}
	}
	if errorText != "LLM token budget exhausted: used 10 of 10 tokens" {
		t.Fatalf("error event = %q", errorText)
	}
	if controlReason != llmCloseReason {
		t.Fatalf("control reason = %q", controlReason)
	}
	inst, _ := m.instance("s1")
	inst.mu.Lock()
	status, sessErr, closeReason := inst.sess.Status, inst.sess.Error, inst.closeReason
	inst.mu.Unlock()
	if status != model.StatusClosing {
		t.Fatalf("status = %s", status)
	}
	if sessErr == "" || closeReason != llmCloseReason {
		t.Fatalf("error = %q close reason = %q", sessErr, closeReason)
	}

	// The next call is refused before any state changes.
	err := m.AdmitLLM("s1", 0, 0)
	if refusalCode(err) != CodeLLMBudgetExhausted {
		t.Fatalf("admit after close = %v", err)
	}
	if usage := liveUsage(t, m, "s1"); usage.Requests != 1 {
		t.Fatalf("requests = %d, want 1", usage.Requests)
	}
}

// failingRecords fails Save while fail is set and counts successful saves.
type failingRecords struct {
	RecordStore
	mu    sync.Mutex
	fail  bool
	saves int
}

func (s *failingRecords) Save(ctx context.Context, rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("record store unavailable")
	}
	s.saves++
	return s.RecordStore.Save(ctx, rec)
}

func (s *failingRecords) setFail(fail bool) {
	s.mu.Lock()
	s.fail = fail
	s.mu.Unlock()
}

func (s *failingRecords) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

func TestLLMAdmitFailsClosedWhenReservationIsNotDurable(t *testing.T) {
	m := testManager(t)
	store := &failingRecords{RecordStore: NewMemoryRecords(), fail: true}
	m.SetRecords(store)
	inst := budgetInstance(m, "s1", 1000, 10)

	err := m.AdmitLLM("s1", 400, 100)
	var refusal *broker.Refusal
	if !errors.As(err, &refusal) || refusal.Code != CodeLLMPersistFailed || refusal.Status != 503 {
		t.Fatalf("admit = %v", err)
	}
	inst.llmMu.Lock()
	ledger := inst.llm
	inst.llmMu.Unlock()
	if ledger.inflight != 0 || ledger.pending != 0 || ledger.spent.Requests != 0 {
		t.Fatalf("failed admission left state behind: %+v", ledger)
	}
	if sess, _ := m.Get("s1"); sess.LLMPendingTokens != 0 || sess.LLMUsage == nil || *sess.LLMUsage != (model.LLMUsage{}) {
		t.Fatalf("session snapshot = %+v", sess)
	}
	if _, ok := storedRecord(t, m, "s1"); ok {
		t.Fatal("failed admission wrote a record")
	}

	// Once the store recovers, the same session admits and charges again.
	store.setFail(false)
	if err := m.AdmitLLM("s1", 400, 100); err != nil {
		t.Fatal(err)
	}
	if err := m.ChargeLLM("s1", reported(5)); err != nil {
		t.Fatal(err)
	}
	if usage := liveUsage(t, m, "s1"); usage.PromptTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestLLMChargeKeepsMemoryWhenPersistFails(t *testing.T) {
	m := testManager(t)
	store := &failingRecords{RecordStore: NewMemoryRecords()}
	m.SetRecords(store)
	budgetInstance(m, "s1", 1000, 10)

	if err := m.AdmitLLM("s1", 400, 100); err != nil {
		t.Fatal(err)
	}
	store.setFail(true)
	if err := m.ChargeLLM("s1", reported(25)); err == nil {
		t.Fatal("a failed usage write was not reported")
	}
	if usage := liveUsage(t, m, "s1"); usage.PromptTokens != 25 {
		t.Fatalf("usage was lost: %+v", usage)
	}
	// The in-flight slot is free, so the session is not stuck busy.
	store.setFail(false)
	if err := m.AdmitLLM("s1", 400, 100); err != nil {
		t.Fatalf("admit after a failed charge: %v", err)
	}
}

func TestLLMNoBudgetSkipsPersist(t *testing.T) {
	m := testManager(t)
	store := &failingRecords{RecordStore: NewMemoryRecords(), fail: true}
	m.SetRecords(store)
	budgetInstance(m, "s1", 0, 0)

	if err := m.AdmitLLM("s1", 400, 100); err != nil {
		t.Fatal(err)
	}
	if err := m.ChargeLLM("s1", reported(9)); err != nil {
		t.Fatal(err)
	}
	if saves := store.saveCount(); saves != 0 {
		t.Fatalf("saves = %d, want none without a budget", saves)
	}
	if usage := liveUsage(t, m, "s1"); usage.PromptTokens != 9 || usage.Requests != 1 {
		t.Fatalf("in-memory usage = %+v", usage)
	}
	// A session without a budget admits concurrent calls.
	if err := m.AdmitLLM("s1", 400, 100); err != nil {
		t.Fatalf("second unbudgeted admit: %v", err)
	}
}

// TestLLMNoBudgetCountsOverlappingCharges is the regression for a settled call
// releasing every in-flight slot instead of its own. A session without a budget
// admits concurrent calls, so both charges must land.
func TestLLMNoBudgetCountsOverlappingCharges(t *testing.T) {
	m := testManager(t)
	inst := budgetInstance(m, "s1", 0, 0)

	if err := m.AdmitLLM("s1", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.AdmitLLM("s1", 0, 0); err != nil {
		t.Fatalf("second unbudgeted admit: %v", err)
	}
	if err := m.ChargeLLM("s1", reported(10)); err != nil {
		t.Fatal(err)
	}
	if err := m.ChargeLLM("s1", reported(20)); err != nil {
		t.Fatal(err)
	}

	usage := liveUsage(t, m, "s1")
	if usage.PromptTokens != 30 || usage.Requests != 2 {
		t.Fatalf("usage = %+v, want 30 tokens over 2 requests", usage)
	}
	inst.llmMu.Lock()
	inflight := inst.llm.inflight
	inst.llmMu.Unlock()
	if inflight != 0 {
		t.Fatalf("in-flight count = %d after both charges, want 0", inflight)
	}
}

// TestLLMExhaustionSurvivesEmptyFinalize is the regression for finalize
// overwriting the exhaustion error with the runtime's empty errMsg. The
// runtime reports no error for a graceful stop, so the budget message is the
// only record of why the session ended.
func TestLLMExhaustionSurvivesEmptyFinalize(t *testing.T) {
	m := testManager(t)
	inst := budgetInstance(m, "s1", 10, 0)
	const token = "runtime-token"
	inst.token = token
	inst.tokenHash = hashToken(token)
	m.mu.Lock()
	m.tokenIndex[inst.tokenHash] = "s1"
	m.mu.Unlock()

	if err := m.AdmitLLM("s1", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.ChargeLLM("s1", reported(10)); err != nil {
		t.Fatal(err)
	}
	const want = "LLM token budget exhausted: used 10 of 10 tokens"
	if err := m.Complete("s1", token, model.StatusCompleted, llmCloseReason, ""); err != nil {
		t.Fatal(err)
	}

	sess, ok := m.Get("s1")
	if !ok {
		t.Fatal("session disappeared")
	}
	if sess.Status != model.StatusCompleted {
		t.Fatalf("status = %s", sess.Status)
	}
	if sess.Error != want {
		t.Fatalf("session error = %q, want the exhaustion message", sess.Error)
	}
	rec, ok := storedRecord(t, m, "s1")
	if !ok || rec.Error != want {
		t.Fatalf("stored record error = %q", rec.Error)
	}
	var status model.StatusData
	var found bool
	for _, ev := range m.hub.EventsAfter("s1", 0) {
		if data, ok := ev.Data.(model.StatusData); ok && ev.Type == model.EventStatus && data.Status == model.StatusCompleted {
			status, found = data, true
		}
	}
	if !found || status.Reason != llmCloseReason || status.Error != want {
		t.Fatalf("final status event = %+v", status)
	}
}

func TestLLMCountersSaturate(t *testing.T) {
	l := &llmLedger{maxTokens: math.MaxInt64, maxRequests: 1}
	l.spent.PromptTokens = math.MaxInt64 - 1
	l.charge(broker.CallResult{Status: 200, Forwarded: true, Reported: true, Usage: broker.Usage{Prompt: 5, Completion: 5}})
	if l.spent.PromptTokens != math.MaxInt64 || l.spent.CompletionTokens != 5 {
		t.Fatalf("usage wrapped: %+v", l.spent)
	}
	if l.spent.Total() != math.MaxInt64 {
		t.Fatalf("total = %d", l.spent.Total())
	}
	l.charge(broker.CallResult{Status: 502, Forwarded: true, ReqBytes: math.MaxInt64, RespBytes: math.MaxInt64})
	if l.spent.PromptTokens != math.MaxInt64 || l.spent.CompletionTokens != satAdd(5, math.MaxInt64/4) {
		t.Fatalf("estimate wrapped: %+v", l.spent)
	}
	if l.spent.Total() != math.MaxInt64 {
		t.Fatalf("total after estimate = %d", l.spent.Total())
	}
	if got := satAdd(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("satAdd = %d", got)
	}
	if got := satAdd(0, 3); got != 3 {
		t.Fatalf("satAdd = %d", got)
	}
}

func reported(n int64) broker.CallResult {
	return broker.CallResult{Status: 200, Forwarded: true, Reported: true, Usage: broker.Usage{Prompt: n}}
}

// storedRecord returns the stored session record of id.
func storedRecord(t *testing.T, m *Manager, id string) (model.Session, bool) {
	t.Helper()
	if m.records == nil {
		return model.Session{}, false
	}
	records, err := m.records.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.Session.ID == id {
			return rec.Session, true
		}
	}
	return model.Session{}, false
}

// TestOldRecordDecodesEmptyUsage keeps the record layout additive: a record
// written before the model budget existed has no usage fields.
func TestOldRecordDecodesEmptyUsage(t *testing.T) {
	raw := []byte(`{"version":1,"session":{"id":"old1","title":"before the budget","status":"completed","prompt":"hi","agent":"default","model":"stub","message_count":2,"created_at":"2026-01-01T00:00:00Z"}}`)
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if err := checkRecordVersion(rec.Version); err != nil {
		t.Fatal(err)
	}
	sess := rec.Session
	if sess.LLMUsage != nil || sess.LLMBudget != nil || sess.LLMPendingTokens != 0 {
		t.Fatalf("old record gained budget fields: %+v", sess)
	}
	if sess.MessageCount != 2 || sess.Status != model.StatusCompleted {
		t.Fatalf("old fields = %+v", sess)
	}
}

// adoptBudgetSession starts a control plane over one fake cluster session and
// returns the manager. rec, when not nil, is already in the record store.
func adoptBudgetSession(t *testing.T, id string, cfg sessioncfg.Config, rec *Record) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := testManager(t)
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	m.cfg.Runtime.Backend = "kubernetes"
	m.cfg.Sessions.MaxRestarts = 2
	m.journal = journal.NewMemory()
	m.stateDir = t.TempDir()
	m.runner = &fakeRunner{}
	store := NewMemoryRecords()
	if rec != nil {
		if err := store.Save(ctx, *rec); err != nil {
			t.Fatal(err)
		}
	}
	m.SetRecords(store)
	m.kube = k8s.NewForClient(fake.NewSimpleClientset(), "mayfly")

	cfg.Version = sessioncfg.Version
	cfg.SessionID = id
	cfg.Agent = "default"
	cfg.Model = "stub"
	cfg.TTLSeconds = 600
	cfg.CreatedAt = time.Now().UTC()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec := &k8s.SessionSpec{
		Namespace:           "mayfly",
		SessionID:           id,
		PodName:             "mayfly-" + id,
		SecretName:          "mayfly-" + id,
		ConfigMapName:       "mayfly-" + id,
		Image:               "example/agent:1",
		PlatformNs:          "mayfly",
		PersistentWorkspace: true,
		SessionJSON:         raw,
		Token:               "token",
		TTL:                 time.Hour,
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := m.kube.CreateSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(context.Background()) })
	return m
}

// TestAdoptSeedsLLMUsage covers the restart gap: adoption rebuilds the session
// from the ConfigMap, which has no counters, so Start must read the record
// first and seed the ledger from it. An unsettled reservation is charged at
// its estimate.
func TestAdoptSeedsLLMUsage(t *testing.T) {
	rec := &Record{Version: RecordFormatVersion, Session: model.Session{
		ID:               "u1",
		Status:           model.StatusRunning,
		Prompt:           "hello",
		LLMUsage:         &model.LLMUsage{PromptTokens: 100, CompletionTokens: 20, Requests: 3},
		LLMPendingTokens: 50,
	}}
	m := adoptBudgetSession(t, "u1", sessioncfg.Config{MaxLLMTokens: 1000, MaxLLMRequests: 10}, rec)

	inst, ok := m.instance("u1")
	if !ok {
		t.Fatal("session was not adopted")
	}
	usage := liveUsage(t, m, "u1")
	if usage.PromptTokens != 150 || usage.CompletionTokens != 20 || usage.Requests != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	if inst.llm.pending != 0 || inst.llm.inflight != 0 || inst.llm.maxTokens != 1000 || inst.llm.maxRequests != 10 {
		t.Fatalf("ledger = %+v", inst.llm)
	}
	sess, _ := m.Get("u1")
	if sess.LLMPendingTokens != 0 || sess.LLMUsage == nil || sess.LLMUsage.PromptTokens != 150 {
		t.Fatalf("session = %+v", sess)
	}
	if sess.LLMBudget == nil || sess.LLMBudget.MaxTokens != 1000 {
		t.Fatalf("budget = %+v", sess.LLMBudget)
	}
	// The seed is durable, so the next restart starts from the same numbers.
	stored, ok := storedRecord(t, m, "u1")
	if !ok || stored.LLMPendingTokens != 0 || stored.LLMUsage == nil || stored.LLMUsage.PromptTokens != 150 {
		t.Fatalf("stored seed = %+v", stored)
	}
}

// TestAdoptWithoutBudgetFields keeps an old record working: adoption starts the
// counters at zero and the budget comes from the session config.
func TestAdoptWithoutBudgetFields(t *testing.T) {
	rec := &Record{Version: RecordFormatVersion, Session: model.Session{
		ID:     "u2",
		Status: model.StatusRunning,
		Prompt: "hello",
	}}
	m := adoptBudgetSession(t, "u2", sessioncfg.Config{MaxLLMTokens: 500}, rec)
	sess, ok := m.Get("u2")
	if !ok {
		t.Fatal("session was not adopted")
	}
	if sess.LLMUsage != nil && *sess.LLMUsage != (model.LLMUsage{}) {
		t.Fatalf("usage = %+v", sess.LLMUsage)
	}
	if sess.LLMPendingTokens != 0 {
		t.Fatalf("pending = %d", sess.LLMPendingTokens)
	}
	if sess.LLMBudget == nil || sess.LLMBudget.MaxTokens != 500 || sess.LLMBudget.MaxRequests != 0 {
		t.Fatalf("budget = %+v", sess.LLMBudget)
	}
}

// TestAdoptMemoryBackendStartsAtZero covers a memory record store: nothing
// survives the restart, so an adopted session starts without usage. That is
// acceptable only for the dev backend.
func TestAdoptMemoryBackendStartsAtZero(t *testing.T) {
	m := adoptBudgetSession(t, "u3", sessioncfg.Config{MaxLLMTokens: 500}, nil)
	sess, ok := m.Get("u3")
	if !ok {
		t.Fatal("session was not adopted")
	}
	if sess.LLMUsage != nil && *sess.LLMUsage != (model.LLMUsage{}) {
		t.Fatalf("usage = %+v", sess.LLMUsage)
	}
	if sess.LLMPendingTokens != 0 {
		t.Fatalf("pending = %d", sess.LLMPendingTokens)
	}
}

// TestLLMConcurrentAdmit is the race test: one budgeted session admits two
// callers at once. Exactly one wins, the other is busy, and one in-flight call
// never becomes two.
func TestLLMConcurrentAdmit(t *testing.T) {
	m := testManager(t)
	inst := budgetInstance(m, "s1", 1000, 10)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- m.AdmitLLM("s1", 400, 100)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	admitted, busy := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			admitted++
		case refusalCode(err) == CodeLLMBusy:
			busy++
		default:
			t.Fatalf("unexpected admit result: %v", err)
		}
	}
	if admitted != 1 || busy != 1 {
		t.Fatalf("admitted = %d busy = %d", admitted, busy)
	}
	if err := m.ChargeLLM("s1", reported(10)); err != nil {
		t.Fatal(err)
	}
	usage := liveUsage(t, m, "s1")
	if usage.PromptTokens != 10 || usage.Requests != 1 || inst.llm.inflight != 0 || inst.llm.pending != 0 {
		t.Fatalf("usage = %+v ledger = %+v", usage, inst.llm)
	}
}
