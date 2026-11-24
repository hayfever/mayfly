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

// FuzzTokenLedger drives the real ledger with random admit, charge, crash, and
// concurrent-admit sequences and asserts the TokenBudget.tla invariants:
//
//   - BudgetBound: billed <= Budget - 1 + MaxCall.
//   - NoChargeLost: the durable record (spent + pending) is never below
//     billed, and a recovery never loses a charge.
//
// billed is a ghost: the tokens the provider actually charged, or the
// reservation the ledger recovered after a crash mid-call.
func FuzzTokenLedger(f *testing.F) {
	// Op bytes: 0/1 admit, 2 charge, 3 crash and recover, 4 concurrent admit,
	// 5 stray charge.
	f.Add(int64(10), int64(5), []byte{1, 2})
	f.Add(int64(1), int64(1), []byte{1, 2})
	f.Add(int64(5), int64(5), []byte{1, 1})
	f.Add(int64(20), int64(30), []byte{1, 3})
	f.Add(int64(50), int64(10), []byte{1, 2, 1, 3, 1, 2, 4})
	f.Add(int64(5), int64(math.MaxInt64), []byte{1, 3, 2})
	f.Add(int64(1000), int64(0), []byte{})
	f.Add(int64(3), int64(2), []byte{1, 2, 1, 2, 1, 2, 5})
	f.Add(int64(2), int64(2), []byte{4, 1, 2})

	f.Fuzz(func(t *testing.T, budgetSeed, maxOutSeed int64, ops []byte) {
		budget := int64(uint64(budgetSeed)%500) + 1
		maxOut := int64(uint64(maxOutSeed) % 200)
		if maxOutSeed == math.MaxInt64 {
			// The saturation seed: a reservation that cannot be represented.
			maxOut = math.MaxInt64
		}
		h := newLedgerHarness(t, budget, maxOut)
		for _, op := range ops {
			switch op % 6 {
			case 0, 1:
				h.admit(t, 400)
			case 2:
				h.charge(t, h.amount(op))
			case 3:
				h.crash(t)
			case 4:
				h.concurrentAdmit(t, 400)
			case 5:
				h.strayCharge(t, h.amount(op))
			}
			h.check(t)
		}
	})
}

// ledgerHarness is one fuzzed session with a token budget and no request
// budget.
type ledgerHarness struct {
	t        *testing.T
	m        *Manager
	inst     *instance
	budget   int64
	maxOut   int64
	maxCall  int64
	billed   int64
	admitted int
}

func newLedgerHarness(t *testing.T, budget, maxOut int64) *ledgerHarness {
	t.Helper()
	m := &Manager{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		hub:      hub.New(1000),
		records:  NewMemoryRecords(),
		sessions: map[string]*instance{},
	}
	m.hub.Create("fz")
	inst := &instance{sess: model.Session{ID: "fz", Status: model.StatusRunning}}
	inst.setLLMBudget(int(budget), 0)
	m.sessions["fz"] = inst
	return &ledgerHarness{
		t: t, m: m, inst: inst, budget: budget, maxOut: maxOut,
		maxCall: satAdd(100, maxOut),
	}
}

// admit makes one call. A refusal must be retryable busy or an exhausted
// budget, and it must not reserve anything.
func (h *ledgerHarness) admit(t *testing.T, reqBytes int64) {
	t.Helper()
	err := h.m.AdmitLLM("fz", reqBytes, h.maxOut)
	switch {
	case err == nil:
		h.admitted++
	case refusalCode(err) == CodeLLMBusy:
	case refusalCode(err) == CodeLLMBudgetExhausted:
	default:
		t.Fatalf("admit: %v", err)
	}
}

// charge settles one admitted call, or acts as a no-op when nothing is in
// flight.
func (h *ledgerHarness) charge(t *testing.T, tokens int64) {
	t.Helper()
	if tokens > h.maxCall {
		tokens = h.maxCall
	}
	res := broker.CallResult{
		Status:    200,
		Forwarded: true,
		Reported:  true,
		Usage:     broker.Usage{Prompt: tokens},
	}
	if err := h.m.ChargeLLM("fz", res); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if h.admitted > 0 {
		h.billed = satAdd(h.billed, tokens)
		h.admitted--
	}
}

// strayCharge calls ChargeLLM while nothing is in flight. The ledger must
// ignore it.
func (h *ledgerHarness) strayCharge(t *testing.T, tokens int64) {
	t.Helper()
	if h.admitted > 0 {
		return
	}
	before := h.usage()
	if err := h.m.ChargeLLM("fz", broker.CallResult{
		Status: 200, Forwarded: true, Reported: true,
		Usage: broker.Usage{Prompt: tokens},
	}); err != nil {
		t.Fatalf("stray charge: %v", err)
	}
	if after := h.usage(); after != before {
		t.Fatalf("stray charge changed usage: %+v -> %+v", before, after)
	}
}

// crash drops the in-memory ledger and reloads it from the durable record,
// the way adoption does after a control-plane restart. A call that was in
// flight is charged at its reservation, which is the largest amount the
// provider may have billed.
func (h *ledgerHarness) crash(t *testing.T) {
	t.Helper()
	if h.admitted > 0 {
		h.billed = satAdd(h.billed, h.inst.llm.pending)
		h.admitted = 0
	}
	h.inst.seedLLM(h.durable(t), sessioncfg.Config{MaxLLMTokens: int(h.budget)})
}

// concurrentAdmit races two callers. At most one may be admitted.
func (h *ledgerHarness) concurrentAdmit(t *testing.T, reqBytes int64) {
	t.Helper()
	if h.admitted > 0 {
		return
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- h.m.AdmitLLM("fz", reqBytes, h.maxOut)
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
		default:
			t.Fatalf("concurrent admit: %v", err)
		}
	}
	if admitted > 1 {
		t.Fatalf("%d callers were admitted at once", admitted)
	}
	h.admitted += admitted
}

// check asserts the TLA+ invariants at the current step.
func (h *ledgerHarness) check(t *testing.T) {
	t.Helper()
	spent := h.usage()
	durable := h.durable(t)
	durableTotal := satAdd(sessionTokens(durable), durable.LLMPendingTokens)

	if spent < h.billed {
		t.Fatalf("NoChargeLost: spent %d < billed %d", spent, h.billed)
	}
	if durableTotal < h.billed {
		t.Fatalf("NoChargeLost: durable %d < billed %d", durableTotal, h.billed)
	}
	if limit := satAdd(h.budget-1, h.maxCall); h.billed > limit {
		t.Fatalf("BudgetBound: billed %d > %d (budget %d, max call %d)", h.billed, limit, h.budget, h.maxCall)
	}
}

func (h *ledgerHarness) usage() int64 {
	h.inst.llmMu.Lock()
	defer h.inst.llmMu.Unlock()
	return h.inst.llm.spent.Total()
}

// durable returns the stored session record, or the zero session when the
// ledger has not written one yet.
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

// sessionTokens returns the charged tokens of a session snapshot.
func sessionTokens(s model.Session) int64 {
	if s.LLMUsage == nil {
		return 0
	}
	return s.LLMUsage.Total()
}

// amount derives a token amount from one fuzz byte, bounded by the largest
// single charge the harness allows.
func (h *ledgerHarness) amount(op byte) int64 {
	k := int64(op)
	if h.maxCall > 0 && h.maxCall < 1<<40 {
		k = k * (h.maxCall + 1) / 256
	}
	if k > h.maxCall {
		k = h.maxCall
	}
	return k
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
