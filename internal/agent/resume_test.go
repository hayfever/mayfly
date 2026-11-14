package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/llm"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// toolLLM counts model requests and asks for one run_command, then replies.
type toolLLM struct {
	mu       sync.Mutex
	requests int
}

func (s *toolLLM) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
			Stream   bool          `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()

		executed := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				executed = true
			}
		}
		var calls []llm.ToolCall
		content := ""
		finish := "stop"
		if !executed {
			args, _ := json.Marshal(map[string]string{"command": "printf 'x\\n' >> calls.txt"})
			calls = []llm.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: llm.FunctionCall{Name: "run_command", Arguments: string(args)},
			}}
			finish = "tool_calls"
		} else {
			content = "finished"
		}
		if !req.Stream {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{
					"message":       llm.Message{Role: "assistant", Content: content, ToolCalls: calls},
					"finish_reason": finish,
				}},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		delta := map[string]any{"role": "assistant"}
		if len(calls) > 0 {
			delta["tool_calls"] = calls
		} else {
			delta["content"] = content
		}
		send(map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": nil}}})
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": finish}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	return mux
}

func (s *toolLLM) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func testRuntime(t *testing.T, dir, llmURL string, store journal.Store) *Runtime {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	conv, err := openConversation(filepath.Join(dir, "conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	trace, err := openTracer(filepath.Join(dir, "traces.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := newEmitter(nil, filepath.Join(dir, "events.jsonl"), logger)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	tools, err := newToolset(toolsetConfig{Workspace: workspace, AllowShell: true, ShellTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{
		cfg: sessioncfg.Config{
			Version:            sessioncfg.Version,
			SessionID:          "s1",
			Model:              "stub",
			MaxSteps:           8,
			MaxHistoryMessages: 20,
			WorkspaceDir:       workspace,
			ArtifactsDir:       dir,
			AllowShell:         true,
			ShellTimeoutSecs:   30,
			LLMTimeoutSeconds:  10,
		},
		logger:    logger,
		llm:       llm.New(llmURL, "", "stub"),
		tools:     tools,
		toolsList: tools.Tools(),
		conv:      conv,
		journal:   store,
		trace:     trace,
		events:    events,
		system:    "test system",
		fatalCh:   make(chan error, 1),
	}
	t.Cleanup(func() {
		_ = tools.Close()
		_ = events.Close()
		_ = trace.Close()
		_ = conv.Close()
	})
	return r
}

func TestHandleTurnJournalsEveryStep(t *testing.T) {
	dir := t.TempDir()
	llmStub := &toolLLM{}
	server := httptest.NewServer(llmStub.handler())
	defer server.Close()

	store := journal.NewMemory()
	r := testRuntime(t, dir, server.URL, store)
	ev := model.Event{Seq: 7, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"}
	r.handleTurn(context.Background(), ev)

	entries, err := store.Turn(context.Background(), "s1", 7)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"user", "llm", "tool:run_command", "assistant"}
	if len(entries) != len(wantNames) {
		t.Fatalf("entries = %d, want %d: %+v", len(entries), len(wantNames), entries)
	}
	lastLen := int64(0)
	for i, entry := range entries {
		if entry.Step != i || entry.Name != wantNames[i] {
			t.Fatalf("entry %d = step %d name %q, want step %d name %q", i, entry.Step, entry.Name, i, wantNames[i])
		}
		if entry.ConvLen <= lastLen {
			t.Fatalf("entry %d ConvLen = %d, want > %d", i, entry.ConvLen, lastLen)
		}
		lastLen = entry.ConvLen
		if len(entry.Output) == 0 {
			t.Fatalf("entry %d has no output", i)
		}
	}
	if got := r.conv.Size(); got != lastLen {
		t.Fatalf("conversation size = %d, want last ConvLen %d", got, lastLen)
	}
	if got := readLines(t, filepath.Join(dir, "workspace", "calls.txt")); got != 1 {
		t.Fatalf("tool executed %d times, want 1", got)
	}
	if got := llmStub.count(); got != 2 {
		t.Fatalf("model requests = %d, want 2", got)
	}
	if entries[0].Turn != 7 || string(entries[0].Input) == "" {
		t.Fatalf("user entry = %+v", entries[0])
	}
}

func TestResumeAppliesRecordedStepsWithoutReExecuting(t *testing.T) {
	dir := t.TempDir()
	llmStub := &toolLLM{}
	server := httptest.NewServer(llmStub.handler())
	defer server.Close()

	store := journal.NewMemory()
	first := testRuntime(t, dir, server.URL, store)
	ev := model.Event{Seq: 3, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"}
	first.handleTurn(context.Background(), ev)
	entries, err := store.Turn(context.Background(), "s1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %d", len(entries))
	}
	// Simulate a crash after the tool step was journaled but before its
	// conversation line was applied. The assistant step never completed.
	if err := first.conv.Truncate(entries[1].ConvLen); err != nil {
		t.Fatal(err)
	}
	_ = first.conv.Close()
	_ = first.trace.Close()
	_ = first.events.Close()
	_ = first.tools.Close()

	partial := journal.NewMemory()
	for _, entry := range entries[:3] {
		if err := partial.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	second := testRuntime(t, dir, server.URL, partial)
	after, resumed, err := second.prepareResume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after != 2 || resumed == nil || resumed.Seq != 3 {
		t.Fatalf("resume = after %d event %+v", after, resumed)
	}
	before := llmStub.count()
	second.handleTurn(context.Background(), *resumed)
	if got := readLines(t, filepath.Join(dir, "workspace", "calls.txt")); got != 1 {
		t.Fatalf("tool executed %d times, want 1", got)
	}
	if got := llmStub.count() - before; got != 1 {
		t.Fatalf("model requests during replay = %d, want 1 (assistant only)", got)
	}
	if got := second.conv.Size(); got != entries[3].ConvLen {
		t.Fatalf("conversation size = %d, want %d", got, entries[3].ConvLen)
	}
	got, err := partial.Turn(context.Background(), "s1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[3].Name != "assistant" {
		t.Fatalf("journal after replay = %+v", got)
	}
}

func TestResumeExecutesStepsMissingFromJournal(t *testing.T) {
	dir := t.TempDir()
	llmStub := &toolLLM{}
	server := httptest.NewServer(llmStub.handler())
	defer server.Close()

	full := journal.NewMemory()
	first := testRuntime(t, dir, server.URL, full)
	ev := model.Event{Seq: 5, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"}
	first.handleTurn(context.Background(), ev)
	entries, err := full.Turn(context.Background(), "s1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.conv.Truncate(entries[1].ConvLen); err != nil {
		t.Fatal(err)
	}
	_ = first.conv.Close()
	_ = first.trace.Close()
	_ = first.events.Close()
	_ = first.tools.Close()
	if err := os.Remove(filepath.Join(dir, "workspace", "calls.txt")); err != nil {
		t.Fatal(err)
	}

	// A crash before the tool step reached the journal leaves only user+llm.
	partial := journal.NewMemory()
	for _, entry := range entries[:2] {
		if err := partial.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	second := testRuntime(t, dir, server.URL, partial)
	after, resumed, err := second.prepareResume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after != 4 || resumed == nil || resumed.Seq != 5 {
		t.Fatalf("resume = after %d event %+v", after, resumed)
	}
	before := llmStub.count()
	second.handleTurn(context.Background(), *resumed)
	if got := readLines(t, filepath.Join(dir, "workspace", "calls.txt")); got != 1 {
		t.Fatalf("tool executed %d times, want 1", got)
	}
	if got := llmStub.count() - before; got != 1 {
		t.Fatalf("model requests during replay = %d, want 1", got)
	}
	got, err := partial.Turn(context.Background(), "s1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("journal now has %d entries, want 4", len(got))
	}
}

func TestResumeFullTurnDoesNotExecuteAnything(t *testing.T) {
	dir := t.TempDir()
	llmStub := &toolLLM{}
	server := httptest.NewServer(llmStub.handler())
	defer server.Close()

	store := journal.NewMemory()
	first := testRuntime(t, dir, server.URL, store)
	ev := model.Event{Seq: 9, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"}
	first.handleTurn(context.Background(), ev)
	entries, _ := store.Turn(context.Background(), "s1", 9)
	_ = first.conv.Close()
	_ = first.trace.Close()
	_ = first.events.Close()
	_ = first.tools.Close()

	second := testRuntime(t, dir, server.URL, store)
	after, resumed, err := second.prepareResume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := llmStub.count()
	second.handleTurn(context.Background(), *resumed)
	if after != 8 || resumed == nil {
		t.Fatalf("resume = after %d event %+v", after, resumed)
	}
	if got := llmStub.count() - before; got != 0 {
		t.Fatalf("fully journaled turn made %d model requests", got)
	}
	if got := readLines(t, filepath.Join(dir, "workspace", "calls.txt")); got != 1 {
		t.Fatalf("fully journaled turn executed the tool %d times", got)
	}
	if got := second.conv.Size(); got != entries[3].ConvLen {
		t.Fatalf("conversation size = %d, want %d", got, entries[3].ConvLen)
	}
}

func readLines(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(raw)), "\n"))
}

// eventTypes counts the local thread log by event type.
func eventTypes(t *testing.T, path string) map[model.EventType]int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[model.EventType]int{}
		}
		t.Fatal(err)
	}
	counts := map[model.EventType]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev model.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode local event: %v", err)
		}
		counts[ev.Type]++
	}
	return counts
}

// TestBundleEventsLogHasRepliesAndResults covers the artifact bundle's durable
// thread: the model reply and the tool result are journaled, so they must also
// reach events.jsonl even though the control plane is what publishes them live.
func TestBundleEventsLogHasRepliesAndResults(t *testing.T) {
	dir := t.TempDir()
	llmStub := &toolLLM{}
	server := httptest.NewServer(llmStub.handler())
	defer server.Close()

	store := journal.NewMemory()
	r := testRuntime(t, dir, server.URL, store)
	r.handleTurn(context.Background(), model.Event{Seq: 3, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"})
	if err := r.events.Close(); err != nil {
		t.Fatal(err)
	}

	counts := eventTypes(t, filepath.Join(dir, "events.jsonl"))
	for _, typ := range []model.EventType{model.EventAssistantMessage, model.EventToolCall, model.EventToolResult} {
		if counts[typ] != 1 {
			t.Fatalf("%s events = %d, want 1 (all: %+v)", typ, counts[typ], counts)
		}
	}
}

// TestReplayDoesNotDuplicateLocalEvents checks that a full replay neither
// re-applies steps nor writes their events to the local log a second time.
func TestReplayDoesNotDuplicateLocalEvents(t *testing.T) {
	dir := t.TempDir()
	llmStub := &toolLLM{}
	server := httptest.NewServer(llmStub.handler())
	defer server.Close()

	store := journal.NewMemory()
	first := testRuntime(t, dir, server.URL, store)
	first.handleTurn(context.Background(), model.Event{Seq: 9, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"})
	_ = first.conv.Close()
	_ = first.trace.Close()
	_ = first.events.Close()
	_ = first.tools.Close()
	before := eventTypes(t, filepath.Join(dir, "events.jsonl"))

	second := testRuntime(t, dir, server.URL, store)
	_, resumed, err := second.prepareResume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second.handleTurn(context.Background(), *resumed)
	if err := second.events.Close(); err != nil {
		t.Fatal(err)
	}
	after := eventTypes(t, filepath.Join(dir, "events.jsonl"))
	if len(after) != len(before) {
		t.Fatalf("event types after replay = %+v, want %+v", after, before)
	}
	for typ, n := range before {
		if after[typ] != n {
			t.Fatalf("%s events after replay = %d, want %d (all: %+v)", typ, after[typ], n, after)
		}
	}
}

// TestReplayRecordsLocalEventsLostInACrash covers a process that applied a
// step to the conversation and stopped before logging its events. The replay
// finds nothing to repair in the conversation, yet the bundle's thread must
// still get the reply and the result exactly once.
func TestReplayRecordsLocalEventsLostInACrash(t *testing.T) {
	dir := t.TempDir()
	llmStub := &toolLLM{}
	server := httptest.NewServer(llmStub.handler())
	defer server.Close()

	store := journal.NewMemory()
	first := testRuntime(t, dir, server.URL, store)
	first.handleTurn(context.Background(), model.Event{Seq: 9, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"})
	_ = first.conv.Close()
	_ = first.trace.Close()
	_ = first.events.Close()
	_ = first.tools.Close()

	// Drop the journaled steps' local records, as if the process died right
	// after each apply.
	path := filepath.Join(dir, "events.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev model.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == model.EventAssistantMessage || ev.Type == model.EventToolResult {
			continue
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	second := testRuntime(t, dir, server.URL, store)
	_, resumed, err := second.prepareResume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second.handleTurn(context.Background(), *resumed)
	if err := second.events.Close(); err != nil {
		t.Fatal(err)
	}
	counts := eventTypes(t, path)
	for _, typ := range []model.EventType{model.EventAssistantMessage, model.EventToolCall, model.EventToolResult} {
		if counts[typ] != 1 {
			t.Fatalf("%s events after replay = %d, want 1 (all: %+v)", typ, counts[typ], counts)
		}
	}
	if got := llmStub.count(); got != 2 {
		t.Fatalf("model requests = %d, want 2 (the replay must not call the model)", got)
	}
}

// TestHandleTurnRejectsReusedTurnKey covers a control-plane restart that hands
// out a sequence a previous lifetime already journaled. The runtime must not
// replay the old turn over the new message.
func TestHandleTurnRejectsReusedTurnKey(t *testing.T) {
	dir := t.TempDir()
	llmStub := &toolLLM{}
	server := httptest.NewServer(llmStub.handler())
	defer server.Close()

	store := journal.NewMemory()
	old, _ := json.Marshal(model.Event{Seq: 7, Type: model.EventUserMessage, Actor: "trigger", Text: "old", Time: time.Now().UTC()})
	if err := store.Append(context.Background(), journal.Entry{
		SessionID: "s1", Turn: 7, Step: 0, Name: "user",
		Input: old, Output: json.RawMessage(`{"message":{"role":"user","content":"old"}}`), ConvLen: 40,
	}); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(t, dir, server.URL, store)
	r.handleTurn(context.Background(), model.Event{Seq: 7, Type: model.EventUserMessage, Actor: "trigger", Text: "new"})

	select {
	case err := <-r.fatalCh:
		if !strings.Contains(err.Error(), "different message") {
			t.Fatalf("fatal error = %v", err)
		}
	default:
		t.Fatal("a reused turn key was not fatal")
	}
	if got := llmStub.count(); got != 0 {
		t.Fatalf("model requests = %d, want 0", got)
	}
	if got := r.conv.Size(); got != 0 {
		t.Fatalf("conversation size = %d, want 0", got)
	}
}

func TestResumeSkipUsesEventIdentity(t *testing.T) {
	r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t0 := time.Now().UTC()
	resumed := model.Event{Seq: 12, Type: model.EventUserMessage, Actor: "trigger", Text: "resume me", Time: t0}
	reused := model.Event{Seq: 12, Type: model.EventUserMessage, Actor: "trigger", Text: "a new message", Time: t0.Add(time.Second)}

	r.armResume(resumed)
	if r.skipResumeEvent(reused) {
		t.Fatal("a different message with the same sequence was skipped")
	}
	if !r.skipResumeEvent(resumed) {
		t.Fatal("the redelivered resume event was not skipped")
	}
	// The skip is consumed once.
	if r.skipResumeEvent(resumed) {
		t.Fatal("the resume event was skipped twice")
	}

	r.armResume(resumed)
	r.clearResumeEvent()
	if r.skipResumeEvent(resumed) {
		t.Fatal("clearResumeEvent left the skip armed")
	}
}

// TestModelRetryBudgetAbortsTurn checks that a connection failure cannot keep
// retrying until the session's idle or TTL timer ends it.
func TestModelRetryBudgetAbortsTurn(t *testing.T) {
	old := modelRetryBudget
	modelRetryBudget = 50 * time.Millisecond
	defer func() { modelRetryBudget = old }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	r := testRuntime(t, t.TempDir(), server.URL, journal.NewMemory())
	r.handleTurn(context.Background(), model.Event{Seq: 4, Type: model.EventUserMessage, Actor: "trigger", Text: "do it"})

	select {
	case err := <-r.fatalCh:
		if !strings.Contains(err.Error(), "retry budget") {
			t.Fatalf("fatal error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the model retry budget did not end the turn")
	}
}

// TestPrepareResumeUsesMostRecentTurn covers a journal that holds a higher
// turn key written before a newer, lower one. Resuming the highest key would
// truncate the newer conversation history away.
func TestPrepareResumeUsesMostRecentTurn(t *testing.T) {
	dir := t.TempDir()
	store := journal.NewMemory()
	t0 := time.Now().UTC().Truncate(time.Second)
	oldEvent, _ := json.Marshal(model.Event{Seq: 40, Type: model.EventUserMessage, Actor: "trigger", Text: "old", Time: t0})
	newEvent, _ := json.Marshal(model.Event{Seq: 4, Type: model.EventUserMessage, Actor: "trigger", Text: "new", Time: t0.Add(time.Minute)})
	for _, entry := range []journal.Entry{
		{SessionID: "s1", Turn: 40, Step: 0, Name: "user", Input: oldEvent, ConvLen: 0, Time: t0},
		{SessionID: "s1", Turn: 4, Step: 0, Name: "user", Input: newEvent, ConvLen: 0, Time: t0.Add(time.Minute)},
	} {
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	r := testRuntime(t, dir, "http://127.0.0.1:1", store)
	after, resumed, err := r.prepareResume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resumed == nil || resumed.Seq != 4 || after != 3 {
		t.Fatalf("resume = after %d event %+v, want turn 4", after, resumed)
	}
}

// TestReplayLengthMismatchIsFatal covers a partial conversation write: the
// journal says the step produced a different size, so the process must exit and
// let a replacement repair the file.
func TestReplayLengthMismatchIsFatal(t *testing.T) {
	r := testRuntime(t, t.TempDir(), "http://127.0.0.1:1", journal.NewMemory())
	output, err := json.Marshal(modelStepOutput{Message: llm.Message{Role: "assistant", Content: "done"}})
	if err != nil {
		t.Fatal(err)
	}
	entry := journal.Entry{SessionID: "s1", Turn: 3, Step: 1, Name: "assistant", Output: output, ConvLen: 1000}
	input, _ := json.Marshal(modelStepInput{Model: "stub", Tools: len(r.toolsList)})
	_, replayed, err := r.runOrReplay(context.Background(), 3, 1, input, &entry,
		func() (stepRun, error) {
			t.Fatal("a recorded step was executed")
			return stepRun{}, nil
		}, r.applyModel)
	if err == nil || !isFatal(err) {
		t.Fatalf("mismatch error = %v, want a fatal error", err)
	}
	if !replayed {
		t.Fatal("a recorded step was not reported as replayed")
	}
}

// TestHandleTurnRejectsStepGap checks the ordering invariant the replay relies
// on: a missing step would make a later step run again and overwrite its entry.
func TestHandleTurnRejectsStepGap(t *testing.T) {
	store := journal.NewMemory()
	ev := model.Event{Seq: 6, Type: model.EventUserMessage, Actor: "trigger", Text: "go", Time: time.Now().UTC()}
	input, _ := json.Marshal(ev)
	entries := []journal.Entry{
		{SessionID: "s1", Turn: 6, Step: 0, Name: "user", Input: input},
		{SessionID: "s1", Turn: 6, Step: 2, Name: "assistant", Output: json.RawMessage(`{}`)},
	}
	for _, entry := range entries {
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	r := testRuntime(t, t.TempDir(), "http://127.0.0.1:1", store)
	r.handleTurn(context.Background(), ev)
	select {
	case err := <-r.fatalCh:
		if !strings.Contains(err.Error(), "step 2") {
			t.Fatalf("fatal error = %v", err)
		}
	default:
		t.Fatal("a step gap was not fatal")
	}
}
