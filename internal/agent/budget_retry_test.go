package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/llm"
	"github.com/hayfever/mayfly/internal/model"
)

func TestRetryableModelErrorClassifiesBudgetRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"budget exhausted", &llm.StatusError{Code: 429, Body: `{"error":"LLM token budget exhausted","code":"llm_budget_exhausted"}`}, false},
		{"busy", &llm.StatusError{Code: 429, Body: `{"error":"another model call is in flight","code":"llm_busy"}`}, true},
		{"plain 429", &llm.StatusError{Code: 429, Body: "slow down"}, true},
		{"overloaded", &llm.StatusError{Code: 503, Body: "unavailable"}, true},
		{"timeout", &llm.StatusError{Code: 408, Body: "timeout"}, true},
		{"bad request", &llm.StatusError{Code: 400, Body: `{"error":"bad","code":"llm_budget_exhausted"}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableModelError(tc.err); got != tc.want {
				t.Fatalf("retryableModelError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestTurnStopsOnBudgetExhausted checks that a budget refusal ends the turn
// after one request instead of entering the five-minute retry loop.
func TestTurnStopsOnBudgetExhausted(t *testing.T) {
	dir := t.TempDir()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"LLM token budget exhausted","code":"llm_budget_exhausted"}`)
	}))
	defer server.Close()

	r := testRuntime(t, dir, server.URL, journal.NewMemory())
	// The deadline only bounds a regression: with a retry the turn keeps
	// running, and the extra request is visible before the context expires.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.handleTurn(ctx, model.Event{Seq: 1, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"})

	if got := requests.Load(); got != 1 {
		t.Fatalf("model requests = %d, want 1: a budget refusal must not be retried", got)
	}
	if err := r.events.Close(); err != nil {
		t.Fatal(err)
	}
	if got := eventTypes(t, filepath.Join(dir, "events.jsonl"))[model.EventError]; got != 1 {
		t.Fatalf("error events = %d, want 1", got)
	}
}

// TestTurnRetriesBusyModelError checks that llm_busy keeps the retry path.
func TestTurnRetriesBusyModelError(t *testing.T) {
	dir := t.TempDir()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"another model call is in flight","code":"llm_busy"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	store := journal.NewMemory()
	r := testRuntime(t, dir, server.URL, store)
	r.handleTurn(context.Background(), model.Event{Seq: 1, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"})

	if got := requests.Load(); got != 2 {
		t.Fatalf("model requests = %d, want 2", got)
	}
	entries, err := store.Turn(context.Background(), "s1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Name != "assistant" {
		t.Fatalf("journal entries = %+v, want user, assistant", entries)
	}
	if err := r.events.Close(); err != nil {
		t.Fatal(err)
	}
	if got := eventTypes(t, filepath.Join(dir, "events.jsonl"))[model.EventError]; got != 0 {
		t.Fatalf("error events = %d, want 0", got)
	}
}
