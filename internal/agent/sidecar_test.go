package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/llm"
	"github.com/hayfever/mayfly/internal/pluginhost"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// sidecarStub is a streamable HTTP MCP server on a Unix socket. It answers the
// three methods the runtime uses and records the bearer token of each call.
type sidecarStub struct {
	socket string

	mu     sync.Mutex
	tokens []string
	keys   []string
}

// serveSidecar starts the stub on a socket in a temporary directory.
func serveSidecar(t *testing.T) *sidecarStub {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "plugin.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	stub := &sidecarStub{socket: socket}
	server := &http.Server{Handler: stub}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return stub
}

// ServeHTTP answers the MCP requests of one session runtime.
func (s *sidecarStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch message.Method {
	case "initialize":
		writeStubResult(w, message.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "stub", "version": "0"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		writeStubResult(w, message.ID, map[string]any{"tools": []map[string]any{{
			"name":        "post",
			"description": "Post a message.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}})
	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(message.Params, &call); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		key, _ := call.Meta["mayfly.io/idempotency-key"].(string)
		s.mu.Lock()
		s.tokens = append(s.tokens, r.Header.Get("Authorization"))
		s.keys = append(s.keys, key)
		s.mu.Unlock()
		text, _ := call.Arguments["text"].(string)
		writeStubResult(w, message.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "posted:" + text}},
		})
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// writeStubResult answers one JSON-RPC request.
func writeStubResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// TestExecuteToolSidecar checks that a granted sidecar tool is dispatched over
// the Unix socket with the gateway's call token and key, and that its result
// reaches the ledger and the journal step.
func TestExecuteToolSidecar(t *testing.T) {
	stub := serveSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	def := sessioncfg.Plugin{
		Name:               "notify",
		Placement:          "sidecar",
		Socket:             stub.socket,
		Command:            "bin/notify",
		InitTimeoutSeconds: 5,
		MaxOutputBytes:     4096,
		Sidecar:            &sessioncfg.PluginSidecar{RunAsUser: 65532},
		Tools:              []sessioncfg.PluginTool{{Name: "post", TimeoutSeconds: 5}},
	}
	host, errs := pluginhost.Start(ctx, []sessioncfg.Plugin{def}, pluginhost.Options{
		SessionID: "s1",
		Agent:     "writer",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if len(errs) != 0 {
		t.Fatalf("start plugin host: %v", errs)
	}
	defer host.Close()
	if errs := host.StartSidecars(ctx, map[string]string{"notify": "v1.discovery"}); len(errs) != 0 {
		t.Fatalf("start sidecar: %v", errs)
	}

	gw := &fakeGateway{
		begin: func(req gateway.BeginRequest) (gateway.BeginResponse, error) {
			return gateway.BeginResponse{Dispatch: &gateway.Dispatch{
				Key:       "s1/3/4",
				TimeoutMS: 5000,
				Token:     "v1.call",
			}}, nil
		},
	}
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
	events, err := newEmitter(nil, filepath.Join(dir, "events.jsonl"), logger)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := newToolset(toolsetConfig{
		Workspace: dir,
		Plugins:   host,
		Grant: map[string]gateway.DiscoveryTool{
			"plugin_notify_post": {
				Name:       "plugin_notify_post",
				Owner:      "notify",
				Placement:  gateway.Sidecar,
				Idempotent: false,
				TimeoutMS:  5000,
			},
		},
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
		dispatch: tools.Dispatch,
		tools:    tools,
		conv:     conv,
		trace:    trace,
		events:   events,
		fatalCh:  make(chan error, 1),
	}
	call := llm.ToolCall{
		ID:       "call_post",
		Type:     "function",
		Function: llm.FunctionCall{Name: "plugin_notify_post", Arguments: `{"text":"hi"}`},
	}
	run, err := r.executeTool(ctx, 3, 4, call)
	if err != nil {
		t.Fatalf("executeTool: %v", err)
	}
	step := decodeToolStep(t, run)
	if step.IsError || !step.Executed || step.Replayed || step.Interrupted {
		t.Fatalf("step = %+v", step)
	}
	if step.Message.Content != "posted:hi" {
		t.Fatalf("content = %q", step.Message.Content)
	}

	stub.mu.Lock()
	tokens := append([]string(nil), stub.tokens...)
	keys := append([]string(nil), stub.keys...)
	stub.mu.Unlock()
	if len(tokens) != 1 || tokens[0] != "Bearer v1.call" {
		t.Fatalf("sidecar call tokens = %v", tokens)
	}
	if len(keys) != 1 || keys[0] != "s1/3/4" {
		t.Fatalf("sidecar idempotency keys = %v", keys)
	}
	if len(gw.completes) != 1 {
		t.Fatalf("completes = %d, want 1", len(gw.completes))
	}
	if got := gw.completes[0]; got.Key != "s1/3/4" || got.Content != "posted:hi" || got.IsError {
		t.Fatalf("complete = %+v", got)
	}
}
