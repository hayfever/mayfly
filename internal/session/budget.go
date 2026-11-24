package session

import (
	"fmt"
	"math"
	"net/http"

	"github.com/hayfever/mayfly/internal/broker"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// Refusal codes the model budget returns. The broker writes them in the
// response body so a harness can tell a retryable busy session from an
// exhausted budget.
const (
	// CodeLLMBusy refuses a second concurrent call of the same session. The
	// runtime retries it.
	CodeLLMBusy = "llm_busy"
	// CodeLLMBudgetExhausted refuses every call after the session reached its
	// token or request limit.
	CodeLLMBudgetExhausted = "llm_budget_exhausted"
	// CodeLLMPersistFailed refuses a call when its reservation could not be
	// recorded. The budget fails closed instead of running untracked.
	CodeLLMPersistFailed = "llm_persist_failed"
)

// llmCloseReason is the close reason of a session whose model budget ran out.
const llmCloseReason = CodeLLMBudgetExhausted

// llmLedger is one session's model budget and usage. spent and pending are
// durable on the session record; inflight and exhausted are in memory.
//
// Admit reserves before the upstream call and Charge settles after it, so a
// crash between the two loses no charge: the reservation is already recorded
// and recovery bills it. Only one call may be in flight while a budget is
// set, which bounds the spend to one response past the limit.
type llmLedger struct {
	maxTokens   int64
	maxRequests int64
	spent       model.LLMUsage
	pending     int64
	inflight    int
	exhausted   bool
}

// budgeted reports whether any limit is set. Without one the ledger still
// counts usage for the session view, but it neither caps calls nor persists
// them.
func (l *llmLedger) budgeted() bool {
	return l.maxTokens > 0 || l.maxRequests > 0
}

// tokens returns the charged tokens, saturated at MaxInt64.
func (l *llmLedger) tokens() int64 { return l.spent.Total() }

// exhaustedMessage returns why the budget refuses a call, or "" while it has
// room. The token limit wins when both limits are reached.
func (l *llmLedger) exhaustedMessage() string {
	if l.maxTokens > 0 && l.tokens() >= l.maxTokens {
		return fmt.Sprintf("LLM token budget exhausted: used %d of %d tokens", l.tokens(), l.maxTokens)
	}
	if l.maxRequests > 0 && l.spent.Requests >= l.maxRequests {
		return fmt.Sprintf("LLM request budget exhausted: used %d of %d requests", l.spent.Requests, l.maxRequests)
	}
	return ""
}

// charge adds one call to the ledger. A call that never reached a provider
// costs nothing. Reported usage wins. Otherwise a 2xx or a 5xx is charged the
// size-based estimate, because the provider may already have billed the call
// (a 502 also covers a timeout after the upstream request was made). Any other
// forwarded status is charged no tokens: the request already counted at
// admit.
func (l *llmLedger) charge(res broker.CallResult) {
	switch {
	case !res.Forwarded:
	case res.Reported:
		l.spent.PromptTokens = satAdd(l.spent.PromptTokens, res.Usage.Prompt)
		l.spent.CompletionTokens = satAdd(l.spent.CompletionTokens, res.Usage.Completion)
	case res.Status >= 500 || (res.Status >= 200 && res.Status < 300):
		l.spent.PromptTokens = satAdd(l.spent.PromptTokens, byteEstimate(res.ReqBytes))
		l.spent.CompletionTokens = satAdd(l.spent.CompletionTokens, byteEstimate(res.RespBytes))
		l.spent.EstimatedCalls = satAdd(l.spent.EstimatedCalls, 1)
	}
}

// Admission and charging.

// AdmitLLM reserves one model call for a session. Pass it as the broker's
// admit hook. A nil return means the reservation is recorded; ChargeLLM must
// then settle the call.
//
// A session without a budget always admits the call. A budgeted session admits
// one call at a time while it is below both limits. Without a record store the
// reservation stays in memory, which is the memory backend's behaviour.
func (m *Manager) AdmitLLM(id string, reqBytes, maxOut int64) error {
	inst, ok := m.instance(id)
	if !ok {
		return fmt.Errorf("admit llm call: session %s is not running", id)
	}
	return m.admitLLM(inst, reqBytes, maxOut)
}

func (m *Manager) admitLLM(inst *instance, reqBytes, maxOut int64) error {
	inst.llmMu.Lock()
	if !inst.llm.budgeted() {
		// No cap: count the call for the session view and let it run. The
		// in-flight count is tracked so ChargeLLM can settle it.
		inst.llm.inflight++
		inst.llm.spent.Requests = satAdd(inst.llm.spent.Requests, 1)
		inst.writeLLMLocked()
		inst.llmMu.Unlock()
		return nil
	}
	if inst.llm.inflight >= 1 {
		inst.llmMu.Unlock()
		return &broker.Refusal{
			Status:  http.StatusTooManyRequests,
			Code:    CodeLLMBusy,
			Message: "another model call is in flight",
		}
	}
	// exhausted is sticky once a charge reaches a limit; the live counters are
	// checked too, so a seeded session refuses without waiting for a charge.
	message := inst.llm.exhaustedMessage()
	if inst.llm.exhausted || message != "" {
		inst.llmMu.Unlock()
		return &broker.Refusal{
			Status:  http.StatusTooManyRequests,
			Code:    CodeLLMBudgetExhausted,
			Message: message,
		}
	}

	previousRequests := inst.llm.spent.Requests
	previousPending := inst.llm.pending
	inst.llm.inflight = 1
	inst.llm.spent.Requests = satAdd(previousRequests, 1)
	inst.llm.pending = satAdd(byteEstimate(reqBytes), nonNegative(maxOut))
	inst.writeLLMLocked()
	inst.llmMu.Unlock()

	// The reservation is durable before the upstream call, so a crash in the
	// middle of it cannot lose the charge.
	if err := m.persistInstanceErr(inst); err != nil {
		inst.llmMu.Lock()
		inst.llm.inflight = 0
		inst.llm.spent.Requests = previousRequests
		inst.llm.pending = previousPending
		inst.writeLLMLocked()
		inst.llmMu.Unlock()
		m.logger.Warn("reserve model call", "session", inst.sess.ID, "error", err)
		return &broker.Refusal{
			Status:  http.StatusServiceUnavailable,
			Code:    CodeLLMPersistFailed,
			Message: "cannot record the model call",
		}
	}
	return nil
}

// ChargeLLM settles a call that AdmitLLM admitted. It releases the in-flight
// slot, charges the usage or its estimate, and persists the result. When the
// charge reaches a limit it emits an error event and closes the session with
// reason llm_budget_exhausted. A session without a budget keeps its usage in
// memory and writes it on the next ordinary transition.
//
// It is safe to call after a failed call and when no call is in flight: with
// no admission the method does nothing.
func (m *Manager) ChargeLLM(id string, res broker.CallResult) error {
	inst, ok := m.instance(id)
	if !ok {
		return ErrNotFound
	}
	return m.chargeLLM(inst, res)
}

func (m *Manager) chargeLLM(inst *instance, res broker.CallResult) error {
	exhausted, message, budgeted := inst.settleLLM(res)
	var err error
	if budgeted {
		if err = m.persistInstanceErr(inst); err != nil {
			// The charge stays in memory, so the next admission sees it and
			// the in-flight slot is already free. Only durability is at risk.
			m.logger.Warn("persist model usage", "session", inst.sess.ID, "error", err)
		}
	}
	if exhausted {
		m.exhaustLLM(inst, message)
	}
	return err
}

// settleLLM applies one call result and releases the in-flight slot. It
// reports whether the budget is now exhausted and whether the ledger is
// budgeted (and therefore needs a durable write).
func (inst *instance) settleLLM(res broker.CallResult) (exhausted bool, message string, budgeted bool) {
	inst.llmMu.Lock()
	if inst.llm.inflight == 0 {
		inst.llmMu.Unlock()
		return false, "", false
	}
	// Release the slot even if the charge itself panics, so a stuck inflight
	// counter cannot refuse every later call as llm_busy. The snapshot below
	// already carries the released state.
	defer func() {
		inst.llm.inflight = 0
		inst.llm.pending = 0
		inst.llmMu.Unlock()
	}()
	budgeted = inst.llm.budgeted()
	inst.llm.charge(res)
	inst.llm.inflight--
	inst.llm.pending = 0
	inst.llm.exhausted = budgeted && inst.llm.exhaustedMessage() != ""
	inst.writeLLMLocked()
	return inst.llm.exhausted, inst.llm.exhaustedMessage(), budgeted
}

// exhaustLLM records the exhausted budget and asks the runtime to finalize.
// Close goes through the runtime's graceful path, so the artifact export still
// happens.
func (m *Manager) exhaustLLM(inst *instance, message string) {
	id := inst.sess.ID
	inst.mu.Lock()
	if inst.finalized || inst.sess.Status == model.StatusClosing ||
		inst.sess.Status == model.StatusCompleted || inst.sess.Status == model.StatusFailed ||
		inst.sess.Status == model.StatusExpired {
		inst.mu.Unlock()
		return
	}
	inst.sess.Error = message
	inst.mu.Unlock()
	m.hub.Append(id, model.Event{Type: model.EventError, Actor: "mayfly", Text: message})
	if err := m.Close(id, llmCloseReason); err != nil {
		m.logger.Warn("close session after model budget", "session", id, "error", err)
	}
}

// Ledger state.

// writeLLMLocked copies the ledger onto the session snapshot. The caller holds
// inst.llmMu.
func (inst *instance) writeLLMLocked() {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.writeLLMSessionLocked()
}

// writeLLMSessionLocked copies the ledger onto the session snapshot. The
// caller holds inst.llmMu and inst.mu.
func (inst *instance) writeLLMSessionLocked() {
	if inst.llm.budgeted() {
		budget := model.LLMBudget{MaxTokens: inst.llm.maxTokens, MaxRequests: inst.llm.maxRequests}
		inst.sess.LLMBudget = &budget
	}
	usage := inst.llm.spent
	if usage != (model.LLMUsage{}) || inst.llm.budgeted() {
		inst.sess.LLMUsage = &usage
	}
	inst.sess.LLMPendingTokens = inst.llm.pending
}

// setLLMBudget resolves the caps of a new session.
func (inst *instance) setLLMBudget(tokens, requests int) {
	inst.llmMu.Lock()
	defer inst.llmMu.Unlock()
	inst.llm.maxTokens = int64(tokens)
	inst.llm.maxRequests = int64(requests)
	inst.writeLLMSessionLocked()
}

// seedLLM restores an adopted session's ledger. The session config carries the
// resolved caps. The record carries the counters, including the reservation of
// a call that was in flight when the previous control plane stopped. That
// reservation is charged at its estimate, so a crash can over-charge but never
// lose a charge.
func (inst *instance) seedLLM(rec model.Session, cfg sessioncfg.Config) {
	tokens := int64(cfg.MaxLLMTokens)
	requests := int64(cfg.MaxLLMRequests)
	if tokens <= 0 && rec.LLMBudget != nil {
		tokens = rec.LLMBudget.MaxTokens
	}
	if requests <= 0 && rec.LLMBudget != nil {
		requests = rec.LLMBudget.MaxRequests
	}
	spent := model.LLMUsage{}
	if rec.LLMUsage != nil {
		spent = model.LLMUsage{
			PromptTokens:     nonNegative(rec.LLMUsage.PromptTokens),
			CompletionTokens: nonNegative(rec.LLMUsage.CompletionTokens),
			Requests:         nonNegative(rec.LLMUsage.Requests),
			EstimatedCalls:   nonNegative(rec.LLMUsage.EstimatedCalls),
		}
	}
	// The record keeps one reservation total, not its prompt and completion
	// split, so recovery charges it as prompt tokens.
	spent.PromptTokens = satAdd(spent.PromptTokens, rec.LLMPendingTokens)

	inst.llmMu.Lock()
	defer inst.llmMu.Unlock()
	inst.llm.maxTokens = tokens
	inst.llm.maxRequests = requests
	inst.llm.spent = spent
	inst.llm.pending = 0
	inst.llm.inflight = 0
	inst.llm.exhausted = inst.llm.budgeted() && inst.llm.exhaustedMessage() != ""
	inst.writeLLMSessionLocked()
}

// adoptRecord returns the record Start read for id. An absent record is the
// zero session, which seeds empty usage.
func (m *Manager) adoptRecord(id string) model.Session {
	return m.adoptRecords[id]
}

// satAdd adds two non-negative counters and saturates at MaxInt64, so a
// hostile or broken provider cannot wrap a counter.
func satAdd(a, b int64) int64 {
	a = nonNegative(a)
	b = nonNegative(b)
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// nonNegative reports b, or zero when it is negative.
func nonNegative(b int64) int64 {
	if b < 0 {
		return 0
	}
	return b
}

// byteEstimate is the token estimate of b request or response bytes. Four
// bytes per token over-estimates a stream, which is the safe direction for a
// budget.
func byteEstimate(b int64) int64 { return nonNegative(b) / 4 }
