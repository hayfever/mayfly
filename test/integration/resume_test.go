package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/httpapi"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/llm"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/stub"
)

// TestLocalResumeAfterKill starts a local session, waits until its first tool
// call is journaled, kills the child with SIGKILL, and checks that the
// replacement resumes the turn without repeating that tool call.
func TestLocalResumeAfterKill(t *testing.T) {
	resumeAfterSignal(t, syscall.SIGKILL)
}

// TestLocalResumeAfterTerminate sends SIGTERM, which is how a drain, eviction,
// or pod deletion ends a runtime. The runtime must not finalize the session as
// completed: the turn has to resume in a replacement.
func TestLocalResumeAfterTerminate(t *testing.T) {
	resumeAfterSignal(t, syscall.SIGTERM)
}

func resumeAfterSignal(t *testing.T, sig syscall.Signal) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	agentBin := buildAgent(t, root)

	var mcpCalls atomic.Int64
	mcpHandler := countingMCP(stub.MCPHandler(), &mcpCalls)
	mcpServer := httptest.NewServer(mcpHandler)
	defer mcpServer.Close()

	llmServer := httptest.NewServer(resumeLLM())
	defer llmServer.Close()

	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	cfg := testConfig(t, root, agentBin, addr, llmServer.URL, mcpServer.URL)
	cfg.Journal = config.JournalConfig{Backend: "memory"}
	cfg.Sessions.MaxRestarts = 2

	store, err := artifact.New(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	local, err := runner.NewLocal(cfg.Runtime.AgentBinary, cfg.Runtime.StateDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	journalStore := journal.NewMemory()
	mgr, err := session.New(cfg, testBundle(t, cfg), store, hub.New(1000), journalStore, local, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	// The runtime sends every granted tool through the gateway, so a session
	// without one cannot call its tools.
	toolGateway := gateway.New(gateway.Options{Ledger: journalStore, Logger: logger})
	mgr.SetGateway(toolGateway)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}

	ts.Config.Handler = httpapi.New(cfg, mgr, nil, logger, httpapi.WithGateway(toolGateway)).Handler()
	ts.Start()
	defer ts.Close()
	// Registered after Close so it runs first and stops the agent's SSE
	// connection before httptest waits for active requests.
	defer mgr.Shutdown(context.Background())
	base := ts.URL

	prompt := "Local resume probe " + strconv.FormatInt(time.Now().UnixNano(), 36)
	resp, err := admin(http.MethodPost, base+"/api/v1/sessions", map[string]any{
		"prompt": prompt,
		"agent":  "triage",
	})
	if err != nil {
		t.Fatal(err)
	}
	var created sessionResponse
	decode(t, resp, &created)
	id := created.Session.ID
	if id == "" {
		t.Fatal("session id is empty")
	}

	// Wait until the completed tool call is durable in the journal.
	var toolEntry journal.Entry
	eventually(t, 30*time.Second, "journaled tool step", func() (bool, string) {
		last, err := journalStore.LastTurn(context.Background(), id)
		if err != nil || last == 0 {
			return false, "no turn yet"
		}
		entries, err := journalStore.Turn(context.Background(), id, last)
		if err != nil {
			return false, err.Error()
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name, "tool:") {
				toolEntry = entry
				return true, entry.Name
			}
		}
		return false, fmt.Sprintf("%d steps", len(entries))
	})
	if toolEntry.Step != 2 {
		t.Fatalf("tool step = %d, want 2", toolEntry.Step)
	}

	cfgPath := filepath.Join(cfg.Runtime.StateDir, "sessions", id, "session.json")
	killChildren(t, cfgPath, sig)

	// The replacement resumes at the missing assistant step.
	var entries []journal.Entry
	eventually(t, 60*time.Second, "resumed turn", func() (bool, string) {
		last, err := journalStore.LastTurn(context.Background(), id)
		if err != nil || last == 0 {
			return false, "no turn"
		}
		entries, err = journalStore.Turn(context.Background(), id, last)
		if err != nil {
			return false, err.Error()
		}
		if len(entries) < 4 {
			return false, fmt.Sprintf("%d steps", len(entries))
		}
		return entries[3].Name == "assistant", fmt.Sprintf("last step %q", entries[3].Name)
	})
	wantNames := []string{"user", "llm", "tool:mcp_echo_echo", "assistant"}
	if len(entries) != len(wantNames) {
		t.Fatalf("journal has %d entries, want %d: %+v", len(entries), len(wantNames), entries)
	}
	for i, entry := range entries {
		if entry.Step != i || entry.Name != wantNames[i] {
			t.Fatalf("entry %d = step %d %q, want %d %q", i, entry.Step, entry.Name, i, wantNames[i])
		}
	}
	if got := mcpCalls.Load(); got != 1 {
		t.Fatalf("MCP tool executed %d times, want 1", got)
	}

	events := waitForSessionEvents(t, base, id, func(events []model.Event) bool {
		for _, ev := range events {
			if ev.Type == model.EventAssistantMessage && strings.Contains(ev.Text, "finished") {
				return true
			}
		}
		return false
	})
	users := 0
	for _, ev := range events {
		if ev.Type == model.EventUserMessage && ev.Text == prompt {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("user message appears %d times, want 1", users)
	}

	// The control plane publishes the reply when its step is journaled, and
	// the runtime applies the step to the conversation right after that.
	convPath := filepath.Join(cfg.Runtime.StateDir, "sessions", id, "artifacts", "conversation.jsonl")
	wantConv := entries[len(entries)-1].ConvLen
	eventually(t, 10*time.Second, "conversation applied", func() (bool, string) {
		info, err := os.Stat(convPath)
		if err != nil {
			return false, err.Error()
		}
		return info.Size() == wantConv, fmt.Sprintf("conversation size = %d, want %d", info.Size(), wantConv)
	})

	// A signal must not finalize the session. The replacement keeps it running
	// until a real close, TTL, or idle timeout.
	resp, err = admin(http.MethodGet, base+"/api/v1/sessions/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	var final sessionResponse
	decode(t, resp, &final)
	if final.Session.Status != model.StatusRunning {
		t.Fatalf("session status after %s = %s, want running", sig, final.Session.Status)
	}
}

func countingMCP(next http.Handler, calls *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(body, &req) == nil && req.Method == "tools/call" {
				calls.Add(1)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// resumeLLM calls the echo tool once, then waits before its final reply so the
// test can kill the child while the assistant step is still in flight.
func resumeLLM() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
			Stream   bool          `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		executed := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				executed = true
			}
		}
		var calls []llm.ToolCall
		content := ""
		finish := "stop"
		if executed {
			time.Sleep(2 * time.Second)
			content = "finished"
		} else {
			args, _ := json.Marshal(map[string]string{"text": "hello"})
			calls = []llm.ToolCall{{
				ID:       "call_echo",
				Type:     "function",
				Function: llm.FunctionCall{Name: "mcp_echo_echo", Arguments: string(args)},
			}}
			finish = "tool_calls"
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

func killChildren(t *testing.T, match string, sig syscall.Signal) {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", match).Output()
	if err != nil && len(out) == 0 {
		t.Fatalf("no child matched %s: %v", match, err)
	}
	killed := 0
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid == os.Getpid() {
			continue
		}
		if err := syscall.Kill(pid, sig); err == nil {
			killed++
		}
	}
	if killed == 0 {
		t.Fatalf("killed no child matching %s", match)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, detail := check()
		if ok {
			return
		}
		last = detail
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %s", what, last)
}

func waitForSessionEvents(t *testing.T, base, id string, match func([]model.Event) bool) []model.Event {
	t.Helper()
	var events []model.Event
	eventually(t, 60*time.Second, "session events", func() (bool, string) {
		resp, err := admin(http.MethodGet, base+"/api/v1/sessions/"+id+"/events", nil)
		if err != nil {
			return false, err.Error()
		}
		var out eventsResponse
		decode(t, resp, &out)
		events = out.Events
		return match(events), fmt.Sprintf("%d events", len(events))
	})
	return events
}
