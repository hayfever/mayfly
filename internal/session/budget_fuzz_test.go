package session

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"

	"github.com/hayfever/mayfly/internal/broker"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// FuzzTokenLedger drives the real ledger with random admit, charge, crash,
// refusal, and save-failure sequences and asserts the TokenBudget.tla
// invariants:
//
//   - BudgetBound: billed <= Budget - 1 + MaxCall, and the request count never
//     exceeds its cap.
//   - NoChargeLost: the in-memory ledger is never below billed, and the durable
//     record (spent + pending) is never below billed either while persistence
//     works.
//   - NoAdmitWhenExhausted: once the ledger reports exhaustion, and whenever the
//     live counters reach a limit, an admission is refused.
//
// billed is a ghost: the tokens the provider actually charged, or the
// reservation the ledger recovered after a crash mid-call. The harness always
// sets a token budget, so one call is in flight at a time.
func FuzzTokenLedger(f *testing.F) {
	// Op bytes: 0 admit, 1 charge usage, 2 charge a 2xx without usage, 3 charge
	// a 5xx, 4 charge a transport failure, 5 charge a forwarded 4xx, 6 crash and
	// recover, 7 toggle save failures, 8 charge while saves fail, 9 concurrent
	// admit, 10 admit while saves fail.
	f.Add(int64(10), int64(5), int64(0), []byte{0, 1})
	f.Add(int64(1), int64(1), int64(1), []byte{0, 1})
	f.Add(int64(5), int64(5), int64(0), []byte{0, 0})
	f.Add(int64(20), int64(30), int64(2), []byte{0, 6})
	f.Add(int64(50), int64(10), int64(3), []byte{0, 1, 0, 6, 0, 1, 9})
	f.Add(int64(5), int64(math.MaxInt64), int64(0), []byte{0, 6, 2})
	f.Add(int64(1000), int64(0), int64(0), []byte{})
	f.Add(int64(3), int64(2), int64(2), []byte{0, 1, 0, 1, 0, 1})
	f.Add(int64(2), int64(2), int64(1), []byte{9, 0, 1})
	f.Add(int64(40), int64(8), int64(2), []byte{0, 2, 0, 3, 0, 4})
	f.Add(int64(40), int64(8), int64(0), []byte{0, 5, 0, 4})
	f.Add(int64(60), int64(12), int64(3), []byte{0, 7, 0, 10, 0, 7, 0, 1})
	f.Add(int64(30), int64(6), int64(4), []byte{0, 8, 0, 1})

	f.Fuzz(func(t *testing.T, budgetSeed, maxOutSeed, requestSeed int64, ops []byte) {
		budget := int64(uint64(budgetSeed)%500) + 1
		maxOut := int64(uint64(maxOutSeed) % 200)
		if maxOutSeed == math.MaxInt64 {
			// The saturation seed: a reservation that cannot be represented.
			maxOut = math.MaxInt64
		}
		requests := int64(uint64(requestSeed) % 5)
		h := newLedgerHarness(t, budget, requests, maxOut)
		for _, op := range ops {
			h.run(t, op)
			h.check(t)
		}
	})
}

// ledgerHarness is one fuzzed session with a token budget and an optional
// request budget.
type ledgerHarness struct {
	t        *testing.T
	m        *Manager
	inst     *instance
	store    *failingRecords
	budget   int64
	maxReqs  int64
	req      int64
	maxOut   int64
	maxCall  int64 // largest token charge one call can make
	billed   int64
	admitted int64 // ghost count of admitted calls
	// inflight is the ghost count of admitted calls that still owe a charge.
	inflight int
	failSave bool
	// dirty marks a charge that reached memory but not the record store, so the
	// durable copy legitimately lags until the next successful save.
	dirty        bool
	sawExhausted bool
}

func newLedgerHarness(t *testing.T, budget, requests, maxOut int64) *ledgerHarness {
	t.Helper()
	store := &failingRecords{RecordStore: NewMemoryRecords()}
	m := &Manager{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		hub:      hub.New(1000),
		records:  store,
		sessions: map[string]*instance{},
	}
	m.hub.Create("fz")
	inst := &instance{sess: model.Session{ID: "fz", Status: model.StatusRunning}}
	inst.setLLMBudget(int(budget), int(requests))
	m.sessions["fz"] = inst
	return &ledgerHarness{
		t: t, m: m, inst: inst, store: store,
		budget: budget, maxReqs: requests, req: 400, maxOut: maxOut,
		maxCall: satAdd(byteEstimate(400), maxOut),
	}
}

// run performs one operation. Op bytes are folded to the supported set.
func (h *ledgerHarness) run(t *testing.T, op byte) {
	t.Helper()
	switch op % 11 {
	case 0:
		h.admit(t)
	case 1:
		h.settle(t, broker.CallResult{
			Status: 200, Forwarded: true, Reported: true,
			Usage: broker.Usage{Prompt: h.amount(op)},
		}, h.amount(op))
	case 2:
		respBytes := h.responseAmount(op) * 4
		h.settle(t, broker.CallResult{
			Status: 200, Forwarded: true, ReqBytes: h.req, RespBytes: respBytes,
		}, satAdd(byteEstimate(h.req), byteEstimate(respBytes)))
	case 3:
		respBytes := h.responseAmount(op) * 4
		h.settle(t, broker.CallResult{
			Status: 500 + int(op%2), Forwarded: true, ReqBytes: h.req, RespBytes: respBytes,
		}, satAdd(byteEstimate(h.req), byteEstimate(respBytes)))
	case 4:
		h.settle(t, broker.CallResult{
			Status: 502, Forwarded: true, TransportErr: true, ReqBytes: h.req, MaxOut: h.maxOut,
		}, h.maxCall)
	case 5:
		h.settle(t, broker.CallResult{
			Status: 429, Forwarded: true, ReqBytes: h.req, RespBytes: h.responseAmount(op) * 4,
		}, 0)
	case 6:
		h.crash(t)
	case 7:
		h.failSave = !h.failSave
		h.store.setFail(h.failSave)
	case 8:
		h.store.setFail(true)
		h.failSave = true
		h.settle(t, broker.CallResult{
			Status: 200, Forwarded: true, Reported: true,
			Usage: broker.Usage{Prompt: h.amount(op)},
		}, h.amount(op))
	case 9:
		h.concurrentAdmit(t)
	case 10:
		h.store.setFail(true)
		h.failSave = true
		h.admit(t)
	}
}

// admit makes one call. A refusal must be retryable busy or an exhausted
// budget, and it must not reserve anything.
func (h *ledgerHarness) admit(t *testing.T) {
	t.Helper()
	before := h.usage()
	beforeLedger := h.ledger()
	blocked := h.sawExhausted || h.atCapacity()
	err := h.m.AdmitLLM("fz", h.req, h.maxOut)
	switch {
	case err == nil:
		if h.inflight > 0 {
			t.Fatalf("admitted a second call while one is in flight")
		}
		if blocked {
			t.Fatalf("NoAdmitWhenExhausted: admitted with %+v at its limit", beforeLedger)
		}
		h.admitted++
		h.inflight = 1
		if !h.failSave {
			h.dirty = false
		}
		return
	case refusalCode(err) == CodeLLMBusy:
	case refusalCode(err) == CodeLLMBudgetExhausted:
		h.sawExhausted = true
	case refusalCode(err) == CodeLLMPersistFailed:
		if !h.failSave {
			t.Fatalf("admit failed to persist with a working store: %v", err)
		}
	default:
		t.Fatalf("admit: %v", err)
	}
	// A refused admission changes neither the counters nor the reservation.
	if after := h.usage(); after != before {
		t.Fatalf("refused admit changed usage: %+v -> %+v", before, after)
	}
	if ledger := h.ledger(); ledger.spent.Requests != beforeLedger.spent.Requests ||
		ledger.pending != beforeLedger.pending || ledger.inflight != h.inflight {
		t.Fatalf("refused admit changed the ledger: %+v -> %+v", beforeLedger, ledger)
	}
}

// concurrentAdmit races two callers. At most one may be admitted.
func (h *ledgerHarness) concurrentAdmit(t *testing.T) {
	t.Helper()
	if h.inflight > 0 {
		return
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	blocked := h.sawExhausted || h.atCapacity()
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- h.m.AdmitLLM("fz", h.req, h.maxOut)
		}()
	}
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		switch {
		case err == nil:
			admitted++
		case refusalCode(err) == CodeLLMBusy, refusalCode(err) == CodeLLMBudgetExhausted:
		case refusalCode(err) == CodeLLMPersistFailed:
			if !h.failSave {
				t.Fatalf("concurrent admit failed to persist with a working store: %v", err)
			}
		default:
			t.Fatalf("concurrent admit: %v", err)
		}
	}
	if admitted > 1 {
		t.Fatalf("%d callers were admitted at once", admitted)
	}
	if admitted == 1 && (blocked || h.failSave) {
		t.Fatalf("NoAdmitWhenExhausted: admitted at or past a limit with %+v", h.ledger())
	}
	if admitted == 1 {
		h.admitted++
		h.inflight = 1
	}
}

// settle calls ChargeLLM with one result and updates the ghost. A charge while
// nothing is in flight must change nothing.
func (h *ledgerHarness) settle(t *testing.T, res broker.CallResult, charged int64) {
	t.Helper()
	before := h.usage()
	_, _, err := h.m.ChargeLLM("fz", res)
	if h.inflight == 0 {
		if err != nil {
			t.Fatalf("stray charge: %v", err)
		}
		if after := h.usage(); after != before {
			t.Fatalf("stray charge changed usage: %+v -> %+v", before, after)
		}
		return
	}
	if h.failSave {
		if err == nil {
			t.Fatal("a charge that could not be persisted was not reported")
		}
	} else {
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		h.dirty = false
	}
	h.billed = satAdd(h.billed, charged)
	h.inflight = 0
}

// crash drops the in-memory ledger and reloads it from the durable record, the
// way adoption does after a control-plane restart. A call that was in flight is
// charged at its reservation, which is the largest amount the provider may have
// billed. A charge that never reached the record store cannot be recovered, so
// the crash is skipped while the store lags.
func (h *ledgerHarness) crash(t *testing.T) {
	t.Helper()
	if h.dirty {
		return
	}
	if h.inflight > 0 {
		h.billed = satAdd(h.billed, h.ledger().pending)
		h.inflight = 0
	}
	h.inst.seedLLM(h.durable(t), sessioncfg.Config{
		MaxLLMTokens:   int(h.budget),
		MaxLLMRequests: int(h.maxReqs),
	})
}

// check asserts the TLA+ invariants at the current step.
func (h *ledgerHarness) check(t *testing.T) {
	t.Helper()
	ledger := h.ledger()
	spent := ledger.spent.Total()
	durable := h.durable(t)
	durableTotal := satAdd(sessionTokens(durable), durable.LLMPendingTokens)

	if spent < h.billed {
		t.Fatalf("NoChargeLost: memory spent %d < billed %d", spent, h.billed)
	}
	if !h.dirty && durableTotal < h.billed {
		t.Fatalf("NoChargeLost: durable %d < billed %d", durableTotal, h.billed)
	}
	if limit := satAdd(h.budget-1, h.maxCall); h.billed > limit {
		t.Fatalf("BudgetBound: billed %d > %d (budget %d, max call %d)", h.billed, limit, h.budget, h.maxCall)
	}
	if ledger.spent.Requests != h.admitted {
		t.Fatalf("request count = %d, want %d", ledger.spent.Requests, h.admitted)
	}
	if h.maxReqs > 0 && ledger.spent.Requests > h.maxReqs {
		t.Fatalf("BudgetBound: %d requests > cap %d", ledger.spent.Requests, h.maxReqs)
	}
	if h.inflight == 0 && h.sawExhausted && !h.atCapacity() {
		t.Fatalf("exhausted flag set while the ledger has room: %+v", ledger)
	}
}

// atCapacity reports whether the ledger refuses a new call for its budget.
func (h *ledgerHarness) atCapacity() bool {
	ledger := h.ledger()
	return ledger.exhausted || ledger.exhaustedMessage() != ""
}

func (h *ledgerHarness) ledger() llmLedger {
	h.inst.llmMu.Lock()
	defer h.inst.llmMu.Unlock()
	return h.inst.llm
}

func (h *ledgerHarness) usage() model.LLMUsage { return h.ledger().spent }

// durable returns the stored session record, or the zero session when the
// ledger has not written one yet. The store still lists while Save fails.
func (h *ledgerHarness) durable(t *testing.T) model.Session {
	t.Helper()
	records, err := h.m.records.List(context.Background())
	if err != nil {
		t.Fatalf("list records: %v", err)
	}
	for _, rec := range records {
		if rec.Session.ID == "fz" {
			return rec.Session
		}
	}
	return model.Session{}
}

// amount derives a token amount from one fuzz byte, bounded by the largest
// single charge the harness allows.
func (h *ledgerHarness) amount(op byte) int64 {
	k := int64(op)
	if h.maxCall < 1<<40 {
		k = k * (h.maxCall + 1) / 256
	}
	if k > h.maxCall {
		k = h.maxCall
	}
	return k
}

// responseAmount derives a response byte count that estimates to at most the
// completion reserve.
func (h *ledgerHarness) responseAmount(op byte) int64 {
	if h.maxOut <= 0 {
		return 0
	}
	k := int64(op)
	if h.maxOut < 1<<40 {
		k = k * (h.maxOut + 1) / 256
	}
	if k > h.maxOut {
		k = h.maxOut
	}
	return k
}

// sessionTokens returns the charged tokens of a session snapshot.
func sessionTokens(s model.Session) int64 {
	if s.LLMUsage == nil {
		return 0
	}
	return s.LLMUsage.Total()
}

// TestTokenLedgerRecordRoundTrip keeps the record fields additive and
// recoverable through JSON.
func TestTokenLedgerRecordRoundTrip(t *testing.T) {
	sess := model.Session{
		ID:               "r1",
		Status:           model.StatusRunning,
		LLMUsage:         &model.LLMUsage{PromptTokens: 3, CompletionTokens: 4, Requests: 2, EstimatedCalls: 1},
		LLMBudget:        &model.LLMBudget{MaxTokens: 100, MaxRequests: 5},
		LLMPendingTokens: 7,
	}
	raw, err := json.Marshal(Record{Version: RecordFormatVersion, Session: sess})
	if err != nil {
		t.Fatal(err)
	}
	var decoded Record
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded.Session
	if got.LLMUsage == nil || *got.LLMUsage != *sess.LLMUsage {
		t.Fatalf("usage = %+v", got.LLMUsage)
	}
	if got.LLMBudget == nil || *got.LLMBudget != *sess.LLMBudget {
		t.Fatalf("budget = %+v", got.LLMBudget)
	}
	if got.LLMPendingTokens != 7 {
		t.Fatalf("pending = %d", got.LLMPendingTokens)
	}
}
