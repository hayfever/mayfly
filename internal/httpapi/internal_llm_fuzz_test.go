package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
)

// FuzzServeLLMBudget drives the internal LLM route with random budgets,
// responses, transport failures, busy overlaps, and panics after admit. It
// asserts the token-ledger properties of the httpapi wiring:
//
//   - No charge after a refusal: a refused call changes neither the usage nor
//     the in-flight call's settlement. A busy refusal must not release the
//     running call's slot, and that call's reported usage must land after it
//     finishes.
//   - No leaked in-flight slot: once no call is in flight, a session below its
//     limits admits the next call, and one at its limits is refused as
//     over_budget rather than busy.
//   - BudgetBound: billed tokens stay at or below Budget-1+MaxCall, and the
//     request count never exceeds its cap.
//
// MaxCall is the largest charge one call can make: the request estimate plus
// the completion cap. Reported usage and estimated response bytes stay within
// that bound, so the budget can overshoot by at most one response.
func FuzzServeLLMBudget(f *testing.F) {
	// Op bytes: 0 reported usage, 1 upstream 5xx, 2 forwarded 4xx, 3 transport
	// failure, 4 two overlapping calls, 5 panic after admit, 6 probe.
	f.Add(int64(10), int64(0), int64(5), []byte{0, 1, 2, 3})
	f.Add(int64(1), int64(1), int64(1), []byte{0, 0})
	f.Add(int64(100), int64(2), int64(20), []byte{0, 6, 0, 6})
	f.Add(int64(5), int64(0), int64(3), []byte{3, 1, 3})
	f.Add(int64(50), int64(3), int64(10), []byte{4, 0, 6})
	f.Add(int64(20), int64(0), int64(8), []byte{5, 6, 0})
	f.Add(int64(300), int64(0), int64(99), []byte{2, 2, 1, 1, 0})

	f.Fuzz(func(t *testing.T, budgetSeed, requestSeed, maxOutSeed int64, ops []byte) {
		budget := int64(uint64(budgetSeed)%500) + 1
		maxRequests := int64(uint64(requestSeed) % 4)
		maxOut := int64(uint64(maxOutSeed)%100) + 1

		fixture := newLLMFixture(t, int(budget), int(maxRequests))
		h := &llmFuzzHarness{
			t:           t,
			f:           fixture,
			budget:      budget,
			maxRequests: maxRequests,
			maxOut:      maxOut,
			body:        fmt.Sprintf(`{"model":"stub","max_tokens":%d,"messages":[]}`, maxOut),
		}
		h.maxCall = int64(len(h.body))/4 + maxOut
		h.last = h.usage()

		for i, op := range ops {
			if i >= 24 {
				break
			}
			h.run(op)
			h.check()
		}
	})
}

// llmFuzzHarness is one fuzzed session and its budget.
type llmFuzzHarness struct {
	t           *testing.T
	f           *llmFixture
	budget      int64
	maxRequests int64
	maxOut      int64
	maxCall     int64
	body        string
	last        model.LLMUsage
}

func (h *llmFuzzHarness) run(op byte) {
	switch op % 7 {
	case 0:
		h.reported(op)
	case 1:
		h.upstream5xx(op)
	case 2:
		h.forwarded4xx()
	case 3:
		h.transportFailure()
	case 4:
		h.busy()
	case 5:
		h.panicAfterAdmit()
	case 6:
		h.probe()
	}
}

// atCapacity reports whether the session refuses a new call for its budget.
func (h *llmFuzzHarness) atCapacity() bool {
	usage := h.usage()
	if usage.Total() >= h.budget {
		return true
	}
	return h.maxRequests > 0 && usage.Requests >= h.maxRequests
}

func (h *llmFuzzHarness) usage() model.LLMUsage {
	sess, ok := h.f.mgr.Get(h.f.id)
	if !ok {
		h.t.Fatal("session is gone")
	}
	if sess.LLMUsage == nil {
		return model.LLMUsage{}
	}
	return *sess.LLMUsage
}

// expectRefusal covers a call that must be refused for the budget. A busy
// refusal here would mean a slot leaked from an earlier call.
func (h *llmFuzzHarness) expectRefusal() {
	before := h.usage()
	rec := h.f.chat(h.body)
	if rec.Code != http.StatusTooManyRequests {
		h.t.Fatalf("call at capacity = %d body=%s", rec.Code, rec.Body.String())
	}
	if code := refusalCodeOf(h.t, rec.Body.String()); code != session.CodeLLMBudgetExhausted {
		h.t.Fatalf("call at capacity code = %q, want %q", code, session.CodeLLMBudgetExhausted)
	}
	if rec.Header().Get("Retry-After") != "" {
		h.t.Fatalf("exhausted refusal carries Retry-After")
	}
	if after := h.usage(); after != before {
		h.t.Fatalf("refused call changed usage: %+v -> %+v", before, after)
	}
}

// reported charges a provider response with bounded usage.
func (h *llmFuzzHarness) reported(op byte) {
	if h.atCapacity() {
		h.expectRefusal()
		return
	}
	total := h.amount(op)
	prompt := total / 2
	completion := total - prompt
	h.f.setUpstream(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeStubUsage(w, fmt.Sprintf(`{"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, prompt, completion))
	})
	rec := h.f.chat(h.body)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("reported call = %d body=%s", rec.Code, rec.Body.String())
	}
	h.expectDelta(model.LLMUsage{PromptTokens: prompt, CompletionTokens: completion, EstimatedCalls: 0})
}

// upstream5xx charges the size estimate of a forwarded 5xx.
func (h *llmFuzzHarness) upstream5xx(op byte) {
	if h.atCapacity() {
		h.expectRefusal()
		return
	}
	tokens := h.amount(op)
	body := strings.Repeat("x", int(tokens*4))
	h.f.setUpstream(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	})
	rec := h.f.chat(h.body)
	if rec.Code != http.StatusInternalServerError {
		h.t.Fatalf("5xx call = %d body=%s", rec.Code, rec.Body.String())
	}
	h.expectDelta(model.LLMUsage{
		PromptTokens:     int64(len(h.body)) / 4,
		CompletionTokens: tokens,
		EstimatedCalls:   1,
	})
}

// forwarded4xx costs no tokens but still counts one request.
func (h *llmFuzzHarness) forwarded4xx() {
	if h.atCapacity() {
		h.expectRefusal()
		return
	}
	h.f.setUpstream(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	})
	rec := h.f.chat(h.body)
	if rec.Code != http.StatusTooManyRequests {
		h.t.Fatalf("4xx call = %d body=%s", rec.Code, rec.Body.String())
	}
	h.expectDelta(model.LLMUsage{})
}

// transportFailure bills the full reservation.
func (h *llmFuzzHarness) transportFailure() {
	if h.atCapacity() {
		h.expectRefusal()
		return
	}
	h.f.setUpstream(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			panic("test server does not hijack")
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			panic(err)
		}
		_ = conn.Close()
	})
	rec := h.f.chat(h.body)
	if rec.Code != http.StatusBadGateway {
		h.t.Fatalf("transport call = %d body=%s", rec.Code, rec.Body.String())
	}
	h.expectDelta(model.LLMUsage{
		PromptTokens:     int64(len(h.body)) / 4,
		CompletionTokens: h.maxOut,
		EstimatedCalls:   1,
	})
}

// busy overlaps two calls. The second is refused and must not settle the
// first; the first still lands its reported usage after it finishes.
func (h *llmFuzzHarness) busy() {
	if h.atCapacity() {
		h.expectRefusal()
		return
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.f.setUpstream(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		once.Do(func() { close(entered) })
		<-release
		// At least one reported token, so a slot released too early loses a
		// visible charge.
		writeStubUsage(w, `{"usage":{"prompt_tokens":1,"completion_tokens":0}}`)
	})
	first := make(chan int, 1)
	go func() { first <- h.f.chat(h.body).Code }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		h.t.Fatal("the blocked call never reached the provider")
	}

	before := h.usage()
	rec := h.f.chat(h.body)
	if rec.Code != http.StatusTooManyRequests {
		h.t.Fatalf("overlapping call = %d body=%s", rec.Code, rec.Body.String())
	}
	if code := refusalCodeOf(h.t, rec.Body.String()); code != session.CodeLLMBusy {
		h.t.Fatalf("overlapping code = %q, want %q", code, session.CodeLLMBusy)
	}
	if rec.Header().Get("Retry-After") == "" {
		h.t.Fatal("busy refusal has no Retry-After")
	}
	if after := h.usage(); after != before {
		h.t.Fatalf("busy refusal settled the running call: %+v -> %+v", before, after)
	}

	close(release)
	if code := <-first; code != http.StatusOK {
		h.t.Fatalf("blocked call = %d", code)
	}
	h.f.setUpstream(nil)
	// The first call's usage must be the delta: a refusal that charged would
	// have released its slot and lost this charge.
	h.expectDelta(model.LLMUsage{PromptTokens: 1})
}

// panicAfterAdmit bills the reservation and must not leak the slot.
func (h *llmFuzzHarness) panicAfterAdmit() {
	if h.atCapacity() {
		h.expectRefusal()
		return
	}
	h.f.setUpstream(nil)
	func() {
		defer func() {
			if recover() == nil {
				h.t.Fatal("response-writer panic did not propagate")
			}
		}()
		h.f.do(&panicWriter{}, h.body)
	}()
	h.expectDelta(model.LLMUsage{
		PromptTokens:     int64(len(h.body)) / 4,
		CompletionTokens: h.maxOut,
		EstimatedCalls:   1,
	})
}

// probe asserts that a session with room admits and one at its limit is
// refused for the budget, never as busy.
func (h *llmFuzzHarness) probe() {
	if h.atCapacity() {
		h.expectRefusal()
		return
	}
	total := h.maxOut
	if total > 30 {
		total = 30
	}
	h.f.setUpstream(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeStubUsage(w, fmt.Sprintf(`{"usage":{"prompt_tokens":%d,"completion_tokens":0}}`, total))
	})
	rec := h.f.chat(h.body)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("probe call = %d body=%s", rec.Code, rec.Body.String())
	}
	h.expectDelta(model.LLMUsage{PromptTokens: total})
}

// expectDelta asserts the usage moved by exactly want since the last check,
// apart from the request count, which admission increments first.
func (h *llmFuzzHarness) expectDelta(want model.LLMUsage) {
	now := h.usage()
	got := model.LLMUsage{
		PromptTokens:     now.PromptTokens - h.last.PromptTokens,
		CompletionTokens: now.CompletionTokens - h.last.CompletionTokens,
		Requests:         now.Requests - h.last.Requests,
		EstimatedCalls:   now.EstimatedCalls - h.last.EstimatedCalls,
	}
	want.Requests = 1
	if got != want {
		h.t.Fatalf("charge delta = %+v, want %+v (before %+v, after %+v)", got, want, h.last, now)
	}
	h.last = now
}

// check asserts the ledger invariants after one operation.
func (h *llmFuzzHarness) check() {
	now := h.usage()
	if now.PromptTokens < h.last.PromptTokens || now.CompletionTokens < h.last.CompletionTokens ||
		now.Requests < h.last.Requests || now.EstimatedCalls < h.last.EstimatedCalls {
		h.t.Fatalf("usage went backwards: %+v -> %+v", h.last, now)
	}
	limit := h.budget - 1 + h.maxCall
	if now.Total() > limit {
		h.t.Fatalf("BudgetBound: billed %d > %d (budget %d, max call %d)", now.Total(), limit, h.budget, h.maxCall)
	}
	if h.maxRequests > 0 && now.Requests > h.maxRequests {
		h.t.Fatalf("BudgetBound: %d requests > cap %d", now.Requests, h.maxRequests)
	}
}

// amount derives a token amount from one fuzz byte, bounded by the completion
// cap so a reported charge stays inside one response.
func (h *llmFuzzHarness) amount(op byte) int64 {
	k := int64(op)
	if h.maxOut < 1<<40 {
		k = k * (h.maxOut + 1) / 256
	}
	if k > h.maxOut {
		k = h.maxOut
	}
	return k
}

// refusalCodeOf reads the code field of a refusal body.
func refusalCodeOf(t *testing.T, raw string) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("refusal body: %v", err)
	}
	return body.Code
}
