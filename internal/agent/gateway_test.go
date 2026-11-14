package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/llm"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/platformclient"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// fakeGateway scripts the control-plane answers of one test.
type fakeGateway struct {
	discovery    gateway.Discovery
	discoveryErr error
	begin        func(gateway.BeginRequest) (gateway.BeginResponse, error)
	completeErr  error

	begins    []gateway.BeginRequest
	completes []gateway.CompleteRequest
	// beginAttempt is the request deadline of the last begin attempt.
	beginAttempt time.Duration
}

func (f *fakeGateway) Tools(context.Context) (gateway.Discovery, error) {
	return f.discovery, f.discoveryErr
}

func (f *fakeGateway) BeginCall(ctx context.Context, req gateway.BeginRequest) (gateway.BeginResponse, error) {
	f.begins = append(f.begins, req)
	if deadline, ok := ctx.Deadline(); ok {
		f.beginAttempt = time.Until(deadline)
	}
	return f.begin(req)
}

func (f *fakeGateway) CompleteCall(_ context.Context, req gateway.CompleteRequest) error {
	f.completes = append(f.completes, req)
	return f.completeErr
}

// grantedTool is the demo plugin tool every unit test routes.
var grantedTool = gateway.DiscoveryTool{
	Name:       "plugin_demo_echo",
	Owner:      "demo",
	Placement:  gateway.Inline,
	Idempotent: true,
	TimeoutMS:  1000,
}

type callOutcome struct {
	run    stepRun
	events []model.Event
	tools  gateway.DiscoveryTool
}

// runTool executes one tool step against the fake gateway and reads the local
// event log, which holds everything the runtime publishes on this path.
func runTool(t *testing.T, gw *fakeGateway, dispatch localDispatch, call llm.ToolCall) callOutcome {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	conv, err := openConversation(filepath.Join(dir, "conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	trace, err := openTracer(filepath.Join(dir, "traces.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(dir, "events.jsonl")
	events, err := newEmitter(nil, eventsPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := newToolset(toolsetConfig{
		Workspace: dir,
		Grant:     map[string]gateway.DiscoveryTool{grantedTool.Name: grantedTool},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tools.Close()
		_ = conv.Close()
		_ = trace.Close()
		_ = events.Close()
	})
	r := &Runtime{
		cfg:      sessioncfg.Config{Version: sessioncfg.Version, SessionID: "s1"},
		logger:   logger,
		gateway:  gw,
		dispatch: dispatch,
		tools:    tools,
		conv:     conv,
		trace:    trace,
		events:   events,
		fatalCh:  make(chan error, 1),
	}
	run, err := r.executeTool(context.Background(), 3, 4, call)
	if err != nil {
		t.Fatalf("executeTool: %v", err)
	}
	_ = events.Close()
	return callOutcome{run: run, events: readEvents(t, eventsPath), tools: grantedTool}
}

func readEvents(t *testing.T, path string) []model.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []model.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev model.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func decodeToolStep(t *testing.T, run stepRun) toolStepOutput {
	t.Helper()
	var out toolStepOutput
	if err := json.Unmarshal(run.output, &out); err != nil {
		t.Fatalf("decode step output: %v", err)
	}
	return out
}

func echoCall() llm.ToolCall {
	return llm.ToolCall{
		ID:       "call_echo",
		Type:     "function",
		Function: llm.FunctionCall{Name: grantedTool.Name, Arguments: `{"text":"hi"}`},
	}
}

func TestExecuteToolDispatch(t *testing.T) {
	gw := &fakeGateway{
		begin: func(gateway.BeginRequest) (gateway.BeginResponse, error) {
			return gateway.BeginResponse{Dispatch: &gateway.Dispatch{Key: "s1/3/4", TimeoutMS: 2500}}, nil
		},
	}
	var gotKey, gotToken string
	var gotArgs json.RawMessage
	dispatch := func(_ context.Context, tool gateway.DiscoveryTool, args json.RawMessage, key, token string) (string, bool, error) {
		if tool.Name != grantedTool.Name {
			t.Errorf("dispatched %q", tool.Name)
		}
		gotKey, gotToken, gotArgs = key, token, args
		return `{"echo":"hi"}`, false, nil
	}
	out := runTool(t, gw, dispatch, echoCall())

	if len(gw.begins) != 1 {
		t.Fatalf("begins = %d, want 1", len(gw.begins))
	}
	begin := gw.begins[0]
	if begin.Turn != 3 || begin.Step != 4 || begin.CallID != "call_echo" || begin.Tool != grantedTool.Name {
		t.Fatalf("begin = %+v", begin)
	}
	if string(begin.Arguments) != `{"text":"hi"}` {
		t.Fatalf("begin arguments = %s", begin.Arguments)
	}
	if gotKey != "s1/3/4" || gotToken != "" || string(gotArgs) != `{"text":"hi"}` {
		t.Fatalf("dispatch = key %q token %q args %s", gotKey, gotToken, gotArgs)
	}
	if len(gw.completes) != 1 {
		t.Fatalf("completes = %d, want 1", len(gw.completes))
	}
	if got := gw.completes[0]; got.Key != "s1/3/4" || got.Content != `{"echo":"hi"}` || got.IsError {
		t.Fatalf("complete = %+v", got)
	}
	step := decodeToolStep(t, out.run)
	if !step.Executed || step.Replayed || step.Interrupted || step.IsError {
		t.Fatalf("step = %+v", step)
	}
	if step.Message.Content != `{"echo":"hi"}` || step.Message.ToolCallID != "call_echo" {
		t.Fatalf("message = %+v", step.Message)
	}
	if out.run.name != "tool:"+grantedTool.Name {
		t.Fatalf("step name = %q", out.run.name)
	}
}

func TestExecuteToolReplayed(t *testing.T) {
	gw := &fakeGateway{
		begin: func(gateway.BeginRequest) (gateway.BeginResponse, error) {
			return gateway.BeginResponse{Result: &gateway.Result{Content: "cached", Replayed: true}}, nil
		},
	}
	dispatch := func(context.Context, gateway.DiscoveryTool, json.RawMessage, string, string) (string, bool, error) {
		t.Error("a replayed result must not run the tool")
		return "", false, nil
	}
	out := runTool(t, gw, dispatch, echoCall())

	step := decodeToolStep(t, out.run)
	if !step.Replayed || step.Executed || step.Interrupted {
		t.Fatalf("step = %+v", step)
	}
	if step.Message.Content != "cached" {
		t.Fatalf("message = %+v", step.Message)
	}
	if len(gw.completes) != 0 {
		t.Fatalf("completes = %d, want 0", len(gw.completes))
	}
}

func TestExecuteToolInterrupted(t *testing.T) {
	message := "ERROR: tool plugin_demo_echo was interrupted by a container restart and may already have taken effect. " +
		"Mayfly did not run it again because the tool is not idempotent. Check the target before retrying."
	gw := &fakeGateway{
		begin: func(gateway.BeginRequest) (gateway.BeginResponse, error) {
			return gateway.BeginResponse{Result: &gateway.Result{
				Content:     message,
				IsError:     true,
				Interrupted: true,
			}}, nil
		},
	}
	dispatch := func(context.Context, gateway.DiscoveryTool, json.RawMessage, string, string) (string, bool, error) {
		t.Error("an interrupted result must not run the tool")
		return "", false, nil
	}
	out := runTool(t, gw, dispatch, echoCall())

	step := decodeToolStep(t, out.run)
	if !step.Interrupted || step.Executed || step.Replayed {
		t.Fatalf("step = %+v", step)
	}
	if !step.IsError || step.Message.Content != message {
		t.Fatalf("message = %+v", step.Message)
	}
	var result *journal.StepEvent
	for i := range step.Events {
		if step.Events[i].Type == string(model.EventToolResult) {
			result = &step.Events[i]
		}
	}
	if result == nil {
		t.Fatalf("no tool_result event in %+v", step.Events)
	}
	if interrupted, _ := result.Data["interrupted"].(bool); !interrupted {
		t.Fatalf("tool_result data = %+v", result.Data)
	}
	var system []string
	for _, ev := range out.events {
		if ev.Type == model.EventSystem {
			system = append(system, ev.Text)
		}
	}
	if len(system) != 1 || system[0] != message {
		t.Fatalf("system events = %q, want the interrupted message", system)
	}
}

func TestExecuteToolRateLimited(t *testing.T) {
	gw := &fakeGateway{
		begin: func(gateway.BeginRequest) (gateway.BeginResponse, error) {
			return gateway.BeginResponse{}, &platformclient.ToolError{
				Op:         "begin tool call",
				Status:     429,
				Message:    `plugin "demo" is rate limited`,
				RetryAfter: 2 * time.Second,
			}
		},
	}
	out := runTool(t, gw, nil, echoCall())

	step := decodeToolStep(t, out.run)
	if !step.IsError || step.Executed || step.Replayed || step.Interrupted {
		t.Fatalf("step = %+v", step)
	}
	if !strings.HasPrefix(step.Message.Content, "ERROR: ") || !strings.Contains(step.Message.Content, "rate limited") {
		t.Fatalf("message = %q", step.Message.Content)
	}
	if len(gw.begins) != 1 {
		t.Fatalf("begins = %d, want 1: a rate limit is not retried", len(gw.begins))
	}
	if len(gw.completes) != 0 {
		t.Fatalf("completes = %d, want 0", len(gw.completes))
	}
}

func TestGatewayRetryBudget(t *testing.T) {
	budget, backoff := gatewayRetryBudget, gatewayRetryBackoff
	gatewayRetryBudget, gatewayRetryBackoff = 300*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		gatewayRetryBudget, gatewayRetryBackoff = budget, backoff
	})

	t.Run("recovers within the budget", func(t *testing.T) {
		attempts := 0
		gw := &fakeGateway{
			begin: func(gateway.BeginRequest) (gateway.BeginResponse, error) {
				attempts++
				if attempts < 3 {
					return gateway.BeginResponse{}, &platformclient.ToolError{
						Op: "begin tool call", Status: 503, Message: "too many tool calls are in flight",
					}
				}
				return gateway.BeginResponse{Dispatch: &gateway.Dispatch{Key: "s1/3/4", TimeoutMS: 1000}}, nil
			},
		}
		dispatch := func(context.Context, gateway.DiscoveryTool, json.RawMessage, string, string) (string, bool, error) {
			return "ok", false, nil
		}
		out := runTool(t, gw, dispatch, echoCall())
		if attempts != 3 {
			t.Fatalf("attempts = %d, want 3", attempts)
		}
		step := decodeToolStep(t, out.run)
		if !step.Executed || step.Message.Content != "ok" {
			t.Fatalf("step = %+v", step)
		}
	})

	t.Run("exhausts the budget", func(t *testing.T) {
		gw := &fakeGateway{
			begin: func(gateway.BeginRequest) (gateway.BeginResponse, error) {
				return gateway.BeginResponse{}, &platformclient.ToolError{
					Op: "begin tool call", Status: 503, Message: "too many tool calls are in flight",
				}
			},
		}
		dir := t.TempDir()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		r := &Runtime{
			logger:  logger,
			gateway: gw,
			conv:    mustConversation(t, dir),
			trace:   mustTracer(t, dir),
			events:  mustEmitter(t, dir, logger),
		}
		tools, err := newToolset(toolsetConfig{
			Workspace: dir,
			Grant:     map[string]gateway.DiscoveryTool{grantedTool.Name: grantedTool},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tools.Close() })
		r.tools = tools
		r.dispatch = func(context.Context, gateway.DiscoveryTool, json.RawMessage, string, string) (string, bool, error) {
			return "ok", false, nil
		}
		_, err = r.executeTool(context.Background(), 3, 4, echoCall())
		if err == nil || !isFatal(err) {
			t.Fatalf("err = %v, want a fatal error that ends the process", err)
		}
		if len(gw.begins) < 2 {
			t.Fatalf("begins = %d, want retries", len(gw.begins))
		}
	})
}

func mustConversation(t *testing.T, dir string) *conversation {
	t.Helper()
	conv, err := openConversation(filepath.Join(dir, "conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conv.Close() })
	return conv
}

func mustTracer(t *testing.T, dir string) *tracer {
	t.Helper()
	tr, err := openTracer(filepath.Join(dir, "traces.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func mustEmitter(t *testing.T, dir string, logger *slog.Logger) *emitter {
	t.Helper()
	ev, err := newEmitter(nil, filepath.Join(dir, "events.jsonl"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ev.Close() })
	return ev
}

// TestBeginAttemptFollowsPlacement covers the attempt bound of one begin: a
// remote tool answer arrives only after the upstream call, so its attempt
// outlasts the tool's timeout.
func TestBeginAttemptFollowsPlacement(t *testing.T) {
	if got := beginAttempt(grantedTool); got != gatewayAttemptTimeout {
		t.Fatalf("local attempt = %s, want %s", got, gatewayAttemptTimeout)
	}
	remote := grantedTool
	remote.Placement = gateway.MCPHTTP
	remote.TimeoutMS = 60_000
	if got := beginAttempt(remote); got != time.Minute+gatewayRemoteMargin {
		t.Fatalf("remote attempt = %s, want the tool timeout plus the margin", got)
	}
	remote.TimeoutMS = 0
	if got := beginAttempt(remote); got != mcpCallTimeout+gatewayRemoteMargin {
		t.Fatalf("unsized remote attempt = %s, want the default plus the margin", got)
	}
}

// TestBeginAttemptOutlastsRemoteTool proves the runtime sizes the attempt of a
// remote begin past the tool's own timeout, so a long call is not abandoned.
func TestBeginAttemptOutlastsRemoteTool(t *testing.T) {
	gw := &fakeGateway{begin: func(gateway.BeginRequest) (gateway.BeginResponse, error) {
		return gateway.BeginResponse{Result: &gateway.Result{Content: "recorded"}}, nil
	}}
	r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), gateway: gw}

	remote := grantedTool
	remote.Placement = gateway.Service
	remote.TimeoutMS = 60_000
	if _, err := r.beginCall(context.Background(), remote, gateway.BeginRequest{Tool: remote.Name}); err != nil {
		t.Fatal(err)
	}
	if gw.beginAttempt < time.Minute {
		t.Fatalf("remote begin attempt = %s, want at least the tool timeout", gw.beginAttempt)
	}

	if _, err := r.beginCall(context.Background(), grantedTool, gateway.BeginRequest{Tool: grantedTool.Name}); err != nil {
		t.Fatal(err)
	}
	if gw.beginAttempt > 2*gatewayAttemptTimeout {
		t.Fatalf("local begin attempt = %s, want the short attempt", gw.beginAttempt)
	}
}

// TestBeginCarriesRuntimeInstance proves every begin names this runtime
// process, so the gateway can tell a lost answer from a restart.
func TestBeginCarriesRuntimeInstance(t *testing.T) {
	if runtimeInstance == "" {
		t.Fatal("the runtime instance is empty")
	}
	gw := &fakeGateway{begin: func(gateway.BeginRequest) (gateway.BeginResponse, error) {
		return gateway.BeginResponse{Result: &gateway.Result{Content: "recorded", Replayed: true}}, nil
	}}
	r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), gateway: gw}
	res, err := r.callGranted(context.Background(), 3, 4, echoCall(), grantedTool)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "recorded" || !res.Replayed {
		t.Fatalf("result = %+v", res)
	}
	if len(gw.begins) != 1 || gw.begins[0].InstanceID != runtimeInstance {
		t.Fatalf("begin = %+v, want the runtime instance", gw.begins)
	}
}
