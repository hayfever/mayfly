package integration

import (
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
)

// interruptedMessage is the gateway's stored result for a non-idempotent call
// a restart cut short. The runtime shows it to the model unchanged.
const interruptedMessage = "ERROR: tool plugin_fixture_sleep was interrupted by a container restart and may already have taken effect. " +
	"Mayfly did not run it again because the tool is not idempotent. Check the target before retrying."

// fixtureSleepMS keeps a fixture call in flight while a test kills the runtime.
const fixtureSleepMS = 5000

// gatewaySession is one control plane with a gateway, plus the session a test
// created through it.
type gatewaySession struct {
	base    string
	id      string
	cfg     *config.Config
	journal *journal.Memory
	audit   string
	calls   string
}

// sessionSetup describes the session a gateway test starts.
type sessionSetup struct {
	// llm answers model requests. Nil calls one fixture echo tool.
	llm http.Handler
	// mcpServers replace the configuration's MCP servers.
	mcpServers []config.MCPServerConfig
	// fixturePlugin writes the fixture plugin into the project and selects the
	// agent that uses it.
	fixturePlugin bool
	// sleepMS is how long the fixture's record and sleep tools wait, which is
	// how a test keeps a call in flight long enough to kill the runtime.
	sleepMS int
	// agent selects the agent manifest to create the session from.
	agent string
	// extraGrant adds granted tools no manifest declares, so a test can
	// exercise the runtime's local dispatch of a placement whose grant a later
	// work package produces.
	extraGrant []gateway.GrantTool
	// holdComplete delays the gateway's answer to CompleteCall after the
	// record is written.
	holdComplete time.Duration
}

// TestPluginSessionCallsTool runs an inline plugin tool end to end: the
// gateway audits the begin and the done, the journal holds the step, and the
// transcript holds the result.
func TestPluginSessionCallsTool(t *testing.T) {
	llmServer := pluginLLM("plugin_fixture_echo", `{"text":"hi"}`)
	gs := startGatewaySession(t, sessionSetup{
		llm:           llmServer,
		fixturePlugin: true,
		agent:         "fixture",
	})

	entries := waitForTurn(t, gs, func(entries []journal.Entry) bool {
		return len(entries) > 2 && entries[2].Name == "tool:plugin_fixture_echo"
	})
	step := decodeStep(t, entries[2])
	if !step.Executed || step.IsError || step.Replayed || step.Interrupted {
		t.Fatalf("step = %+v", step)
	}
	if step.Message.Content != "echo: hi" {
		t.Fatalf("result = %q", step.Message.Content)
	}

	records := waitForAudit(t, gs.audit, "plugin_fixture_echo", "call.done")
	if !hasAuditEvent(records, "call.begin") || !hasAuditEvent(records, "call.done") {
		t.Fatalf("audit = %+v, want begin and done", records)
	}

	waitForSessionEvents(t, gs.base, gs.id, func(events []model.Event) bool {
		for _, ev := range events {
			if ev.Type == model.EventToolResult && strings.Contains(ev.Text, "echo: hi") {
				return true
			}
		}
		return false
	})
}

// TestPluginResumeDoesNotRepeatEffect kills the runtime while a
// non-idempotent plugin tool is in flight. The replacement must not call it
// again, and must tell the model why.
func TestPluginResumeDoesNotRepeatEffect(t *testing.T) {
	llmServer := pluginLLM("plugin_fixture_sleep", `{}`)
	gs := startGatewaySession(t, sessionSetup{
		llm:           llmServer,
		fixturePlugin: true,
		sleepMS:       fixtureSleepMS,
		agent:         "fixture",
	})

	waitForFixtureCall(t, gs.calls, "sleep")
	killSession(t, gs)
	entries := waitForTurn(t, gs, func(entries []journal.Entry) bool {
		return len(entries) > 3 && entries[3].Name == "assistant"
	})
	step := decodeStep(t, entries[2])
	if !step.Interrupted || step.Executed || !step.IsError {
		t.Fatalf("step = %+v", step)
	}
	if step.Message.Content != interruptedMessage {
		t.Fatalf("result = %q", step.Message.Content)
	}
	if calls := fixtureCalls(t, gs.calls); len(calls) != 1 {
		t.Fatalf("fixture calls = %d, want 1 (the tool must not run again)", len(calls))
	}

	waitForSessionEvents(t, gs.base, gs.id, func(events []model.Event) bool {
		var interrupted, system bool
		for _, ev := range events {
			if ev.Type == model.EventToolResult {
				if value, _ := ev.Data["interrupted"].(bool); value {
					interrupted = true
				}
			}
			if ev.Type == model.EventSystem && ev.Text == interruptedMessage {
				system = true
			}
		}
		return interrupted && system
	})
	assertSessionStatus(t, gs, model.StatusRunning)
}

// TestPluginResumeRepeatsIdempotent kills the runtime while an idempotent
// plugin tool is in flight. The replacement calls it again with the same key.
func TestPluginResumeRepeatsIdempotent(t *testing.T) {
	llmServer := pluginLLM("plugin_fixture_record", `{}`)
	gs := startGatewaySession(t, sessionSetup{
		llm:           llmServer,
		fixturePlugin: true,
		sleepMS:       fixtureSleepMS,
		agent:         "fixture",
	})

	waitForFixtureCall(t, gs.calls, "record")
	killSession(t, gs)
	entries := waitForTurn(t, gs, func(entries []journal.Entry) bool {
		return len(entries) > 3 && entries[3].Name == "assistant"
	})
	step := decodeStep(t, entries[2])
	if !step.Executed || step.Interrupted {
		t.Fatalf("step = %+v", step)
	}
	calls := fixtureCalls(t, gs.calls)
	if len(calls) != 2 {
		t.Fatalf("fixture calls = %d, want 2 (the idempotent tool is retried)", len(calls))
	}
	if calls[0].Key == "" || calls[0].Key != calls[1].Key {
		t.Fatalf("call keys = %q and %q, want the same idempotency key", calls[0].Key, calls[1].Key)
	}
	if !strings.HasPrefix(step.Message.Content, "recorded ") {
		t.Fatalf("result = %q", step.Message.Content)
	}
}

// TestPluginReplaysCompletedCall kills the runtime after the gateway recorded
// the result and before the journal holds the step. The replacement replays
// the recorded result instead of calling the tool again.
func TestPluginReplaysCompletedCall(t *testing.T) {
	llmServer := pluginLLM("plugin_fixture_record", `{}`)
	gs := startGatewaySession(t, sessionSetup{
		llm:           llmServer,
		fixturePlugin: true,
		agent:         "fixture",
		holdComplete:  5 * time.Second,
	})

	waitForLedgerDone(t, gs, "plugin_fixture_record")
	killSession(t, gs)
	entries := waitForTurn(t, gs, func(entries []journal.Entry) bool {
		return len(entries) > 3 && entries[3].Name == "assistant"
	})
	step := decodeStep(t, entries[2])
	if !step.Replayed || step.Executed || step.Interrupted {
		t.Fatalf("step = %+v", step)
	}
	if calls := fixtureCalls(t, gs.calls); len(calls) != 1 {
		t.Fatalf("fixture calls = %d, want 1 (the recorded result is replayed)", len(calls))
	}
	records := waitForAudit(t, gs.audit, "plugin_fixture_record", "call.done")
	var replayed bool
	for _, record := range records {
		if record.Event == "call.done" && record.Replayed {
			replayed = true
		}
	}
	if !replayed {
		t.Fatalf("audit = %+v, want a replayed done", records)
	}
}

// TestStdioMCPThroughGateway proves a stdio mcp.servers tool is authorized and
// recorded by the gateway while the runtime performs the call.
func TestStdioMCPThroughGateway(t *testing.T) {
	root := repoRoot(t)
	fixture := buildFixture(t, root)
	llmServer := pluginLLM("mcp_echostd_echo", `{"text":"hi"}`)
	gs := startGatewaySession(t, sessionSetup{
		llm: llmServer,
		mcpServers: []config.MCPServerConfig{{
			Name:      "echostd",
			Transport: "stdio",
			Command:   fixture,
		}},
		agent: "plain",
		extraGrant: []gateway.GrantTool{{
			Name:       "mcp_echostd_echo",
			Owner:      "echostd",
			Remote:     "echo",
			Placement:  gateway.MCPStdio,
			Idempotent: true,
			Timeout:    30 * time.Second,
		}},
	})

	entries := waitForTurn(t, gs, func(entries []journal.Entry) bool {
		return len(entries) > 2 && entries[2].Name == "tool:mcp_echostd_echo"
	})
	step := decodeStep(t, entries[2])
	if !step.Executed || step.IsError || step.Replayed || step.Interrupted {
		t.Fatalf("step = %+v", step)
	}
	if step.Message.Content != "echo: hi" {
		t.Fatalf("result = %q", step.Message.Content)
	}
	records := waitForAudit(t, gs.audit, "mcp_echostd_echo", "call.done")
	if !hasAuditEvent(records, "call.begin") || !hasAuditEvent(records, "call.done") {
		t.Fatalf("audit = %+v, want begin and done", records)
	}
	retried := waitForLedgerDone(t, gs, "mcp_echostd_echo")
	if retried.State != journal.CallDone || !strings.Contains(retried.Content, "echo: hi") {
		t.Fatalf("ledger record = %+v", retried)
	}
	assertSessionStatus(t, gs, model.StatusRunning)
}

// startGatewaySession starts a control plane whose sessions may call gateway
// tools, creates one session, and returns it.
func startGatewaySession(t *testing.T, setup sessionSetup) *gatewaySession {
	t.Helper()
	root := repoRoot(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agentBin := buildAgent(t, root)

	project := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls.log")
	var fixture string
	if setup.fixturePlugin {
		fixture = buildFixture(t, root)
	}
	writeTestProject(t, project, fixture, calls, setup.sleepMS)

	llmHandler := setup.llm
	if llmHandler == nil {
		llmHandler = pluginLLM("plugin_fixture_echo", `{"text":"hi"}`)
	}
	llmServer := httptest.NewServer(llmHandler)
	t.Cleanup(llmServer.Close)

	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	cfg := testConfig(t, root, agentBin, addr, llmServer.URL, llmServer.URL)
	cfg.Journal = config.JournalConfig{Backend: "memory"}
	cfg.Sessions.MaxRestarts = 2
	cfg.Agents.Dirs = append(cfg.Agents.Dirs, project)
	cfg.MCP.Servers = setup.mcpServers

	store, err := artifact.New(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatal(err)
	}
	local, err := runner.NewLocal(cfg.Runtime.AgentBinary, cfg.Runtime.StateDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	memory := journal.NewMemory()
	mgr, err := session.New(cfg, testBundle(t, cfg), store, hub.New(1000), memory, local, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := gateway.NewAudit(auditPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	gw := gateway.New(gateway.Options{
		Ledger:    &delayedLedger{Ledger: memory, hold: setup.holdComplete},
		Sequencer: memory,
		Audit:     audit,
		Logger:    logger,
	})
	mgr.SetGateway(extraGrants{gateway: gw, tools: setup.extraGrant})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = httpapi.New(cfg, mgr, nil, logger, httpapi.WithGateway(gw)).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	base := ts.URL

	agent := setup.agent
	if agent == "" {
		agent = "fixture"
	}
	resp, err := admin(http.MethodPost, base+"/api/v1/sessions", map[string]any{
		"prompt": "plugin gateway probe " + strconv.FormatInt(time.Now().UnixNano(), 36),
		"agent":  agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	var created sessionResponse
	decode(t, resp, &created)
	if created.Session.ID == "" {
		t.Fatal("session id is empty")
	}
	return &gatewaySession{
		base:    base,
		id:      created.Session.ID,
		cfg:     cfg,
		journal: memory,
		audit:   auditPath,
		calls:   calls,
	}
}

// delayedLedger writes the record first and answers the gateway afterwards, so
// a test can kill the runtime in the window where the gateway holds the result
// and the journal does not.
type delayedLedger struct {
	journal.Ledger
	hold time.Duration
}

func (l *delayedLedger) Complete(ctx context.Context, session, key, content string, isError, interrupted bool) (journal.Call, error) {
	rec, err := l.Ledger.Complete(ctx, session, key, content, isError, interrupted)
	if err == nil && l.hold > 0 {
		time.Sleep(l.hold)
	}
	return rec, err
}

// extraGrants adds the test's stand-in grant entries to every session.
type extraGrants struct {
	gateway *gateway.Gateway
	tools   []gateway.GrantTool
}

func (e extraGrants) Register(grant gateway.Grant) {
	for _, tool := range e.tools {
		grant.Tools[tool.Name] = tool
	}
	e.gateway.Register(grant)
}

func (e extraGrants) Drop(session string) { e.gateway.Drop(session) }

// writeTestProject writes a bundle with two agents and, when fixture is set,
// the fixture plugin.
func writeTestProject(t *testing.T, dir, fixture, callLog string, sleepMS int) {
	t.Helper()
	agentsDir := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `schemaVersion: 1
name: mayfly-plugin-tests
version: 0.1.0
agents:
  - name: plain
    description: Uses no plugin.
    systemPrompt: You are a test agent.
    maxSteps: 20
`
	if fixture != "" {
		manifest += `  - name: fixture
    description: Uses the fixture plugin.
    systemPrompt: You are a test agent.
    maxSteps: 20
    plugins: [fixture]
`
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "agents.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if fixture == "" {
		return
	}
	pluginDir := filepath.Join(dir, "plugins", "fixture")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "fixture"), mustRead(t, fixture), 0o755); err != nil {
		t.Fatal(err)
	}
	pluginManifest := fmt.Sprintf(`schemaVersion: 1
name: fixture
version: 0.1.0
description: Fixture plugin for the integration tests.
placement: inline
command: fixture
local:
  command: ./fixture
env:
  CALL_LOG: %s
  FIXTURE_SLEEP_MS: "%d"
tools:
  - name: echo
  - name: record
    idempotent: true
  - name: sleep
limits:
  initTimeout: 20s
  callTimeout: 120s
`, callLog, sleepMS)
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(pluginManifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pluginLLM answers the first request with one tool call and then a reply.
func pluginLLM(tool, args string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			content = "finished"
		} else {
			calls = []llm.ToolCall{{
				ID:       "call_fixture",
				Type:     "function",
				Function: llm.FunctionCall{Name: tool, Arguments: args},
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
}

// fixtureCall is one call the fixture plugin logged.
type fixtureCall struct {
	Tool string `json:"tool"`
	Key  string `json:"key"`
	PID  string `json:"pid"`
}

func fixtureCalls(t *testing.T, path string) []fixtureCall {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []fixtureCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var call fixtureCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("decode call %q: %v", line, err)
		}
		out = append(out, call)
	}
	return out
}

func waitForFixtureCall(t *testing.T, path, tool string) {
	t.Helper()
	eventually(t, 60*time.Second, "fixture call "+tool, func() (bool, string) {
		calls := fixtureCalls(t, path)
		for _, call := range calls {
			if call.Tool == tool {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d calls", len(calls))
	})
}

// waitForTurn returns the session's last journaled turn once have accepts it.
func waitForTurn(t *testing.T, gs *gatewaySession, have func([]journal.Entry) bool) []journal.Entry {
	t.Helper()
	var entries []journal.Entry
	eventually(t, 60*time.Second, "journaled turn", func() (bool, string) {
		last, err := gs.journal.LastTurn(context.Background(), gs.id)
		if err != nil || last == 0 {
			return false, "no turn yet"
		}
		entries, err = gs.journal.Turn(context.Background(), gs.id, last)
		if err != nil {
			return false, err.Error()
		}
		return have(entries), fmt.Sprintf("%d steps", len(entries))
	})
	return entries
}

func decodeStep(t *testing.T, entry journal.Entry) toolStep {
	t.Helper()
	var step toolStep
	if err := json.Unmarshal(entry.Output, &step); err != nil {
		t.Fatalf("decode step %q: %v", entry.Name, err)
	}
	return step
}

// toolStep mirrors the journaled tool step the runtime writes.
type toolStep struct {
	Message struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"message"`
	IsError     bool `json:"is_error"`
	Executed    bool `json:"executed"`
	Replayed    bool `json:"replayed"`
	Interrupted bool `json:"interrupted"`
}

// killSession SIGKILLs the agent process of the session, which is how a
// container restart ends a runtime.
func killSession(t *testing.T, gs *gatewaySession) {
	t.Helper()
	cfgPath := filepath.Join(gs.cfg.Runtime.StateDir, "sessions", gs.id, "session.json")
	killChildren(t, cfgPath, syscall.SIGKILL)
}

func assertSessionStatus(t *testing.T, gs *gatewaySession, want model.SessionStatus) {
	t.Helper()
	resp, err := admin(http.MethodGet, gs.base+"/api/v1/sessions/"+gs.id, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got sessionResponse
	decode(t, resp, &got)
	if got.Session.Status != want {
		t.Fatalf("session status = %s, want %s", got.Session.Status, want)
	}
}

// auditRecord is one line of the gateway audit log the tests read.
type auditRecord struct {
	Event       string `json:"event"`
	Tool        string `json:"tool"`
	Key         string `json:"key"`
	Replayed    bool   `json:"replayed"`
	Interrupted bool   `json:"interrupted"`
}

func readAudit(t *testing.T, path string) []auditRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []auditRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var record auditRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode audit %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}

// waitForAudit waits until the audit log holds want for tool.
func waitForAudit(t *testing.T, path, tool, want string) []auditRecord {
	t.Helper()
	var records []auditRecord
	eventually(t, 30*time.Second, "audit "+want, func() (bool, string) {
		records = readAudit(t, path)
		for _, record := range records {
			if record.Tool == tool && record.Event == want {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d records", len(records))
	})
	return records
}

func hasAuditEvent(records []auditRecord, event string) bool {
	for _, record := range records {
		if record.Event == event {
			return true
		}
	}
	return false
}

// waitForLedgerDone waits for the gateway's ledger record of one tool and
// returns it once the result is recorded.
func waitForLedgerDone(t *testing.T, gs *gatewaySession, tool string) journal.Call {
	t.Helper()
	var found journal.Call
	eventually(t, 60*time.Second, "ledger record for "+tool, func() (bool, string) {
		ctx := context.Background()
		for turn := int64(1); turn <= 3; turn++ {
			for step := 2; step < 64; step++ {
				key := fmt.Sprintf("%s/%d/%d", gs.id, turn, step)
				call, ok, err := gs.journal.Get(ctx, gs.id, key)
				if err != nil {
					return false, err.Error()
				}
				if ok && call.Tool == tool {
					if call.State != journal.CallDone {
						return false, "record is " + string(call.State)
					}
					found = call
					return true, ""
				}
			}
		}
		return false, "no record yet"
	})
	return found
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func buildFixture(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fixture")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./test/integration/testdata/plugin")
	cmd.Dir = root
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	return bin
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
