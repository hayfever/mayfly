package pluginhost

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// sidecarShim is a minimal streamable HTTP MCP server on a Unix socket. It
// records the bearer token of every request, so a test can see which token a
// phase used, and the _meta of every tool call.
type sidecarShim struct {
	socket string
	tools  []string

	mu     sync.Mutex
	tokens map[string][]string
	meta   []map[string]any
}

// newSidecarShim serves the given tools on a socket in a temporary directory.
func newSidecarShim(t *testing.T, tools ...string) *sidecarShim {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "plugin.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	shim := &sidecarShim{socket: socket, tools: tools, tokens: map[string][]string{}}
	server := &http.Server{Handler: shim}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
	})
	return shim
}

// ServeHTTP answers the MCP methods the runtime uses.
func (s *sidecarShim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	s.mu.Lock()
	s.tokens[message.Method] = append(s.tokens[message.Method], r.Header.Get("Authorization"))
	s.mu.Unlock()

	switch message.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "shim-1")
		writeShimResult(w, message.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "fake", "version": "0"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		listed := make([]map[string]any, 0, len(s.tools))
		for _, name := range s.tools {
			listed = append(listed, map[string]any{
				"name":        name,
				"description": "Tool " + name + ".",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			})
		}
		writeShimResult(w, message.ID, map[string]any{"tools": listed})
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
		s.mu.Lock()
		s.meta = append(s.meta, call.Meta)
		s.mu.Unlock()
		text, _ := call.Arguments["text"].(string)
		writeShimResult(w, message.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": call.Name + ":" + text}},
		})
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// writeShimResult answers one JSON-RPC request.
func writeShimResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// seen returns the bearer tokens one method received.
func (s *sidecarShim) seen(method string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tokens[method]...)
}

// sidecarDef is one sidecar plugin description, as session.json carries it.
func sidecarDef(socket string, tools ...string) sessioncfg.Plugin {
	def := sessioncfg.Plugin{
		Name:               "notify",
		Placement:          "sidecar",
		Socket:             socket,
		Command:            "bin/notify",
		InitTimeoutSeconds: 5,
		MaxOutputBytes:     4096,
		Sidecar:            &sessioncfg.PluginSidecar{RunAsUser: 65532},
	}
	for _, name := range tools {
		def.Tools = append(def.Tools, sessioncfg.PluginTool{Name: name, TimeoutSeconds: 5})
	}
	return def
}

// TestSidecarClientTokens checks that discovery uses the discovery token, that
// a call uses the call token, and that the tool call carries the gateway's
// key and session in _meta.
func TestSidecarClientTokens(t *testing.T) {
	shim := newSidecarShim(t, "post", "undeclared")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	host, errs := Start(ctx, []sessioncfg.Plugin{sidecarDef(shim.socket, "post")}, Options{
		SessionID: "s1",
		Agent:     "writer",
	})
	if len(errs) != 0 {
		t.Fatalf("Start: %v", errs)
	}
	defer host.Close()
	if _, ok := host.Tool("plugin_notify_post"); ok {
		t.Fatal("a sidecar tool was offered before the sidecar connected")
	}

	if errs := host.StartSidecars(ctx, map[string]string{"notify": "v1.discovery"}); len(errs) != 0 {
		t.Fatalf("StartSidecars: %v", errs)
	}
	tool, ok := host.Tool("plugin_notify_post")
	if !ok {
		t.Fatal("the sidecar tool was not published")
	}
	if tool.Description != "Tool post." || tool.Plugin != "notify" || tool.Remote != "post" {
		t.Fatalf("tool = %+v", tool)
	}
	if hidden := host.Hidden()["notify"]; len(hidden) != 1 || hidden[0] != "undeclared" {
		t.Fatalf("hidden tools = %v", hidden)
	}
	if got := shim.seen("initialize"); len(got) != 1 || got[0] != "Bearer v1.discovery" {
		t.Fatalf("initialize tokens = %v", got)
	}
	if got := shim.seen("tools/list"); len(got) != 1 || got[0] != "Bearer v1.discovery" {
		t.Fatalf("tools/list tokens = %v", got)
	}

	out, isErr, err := host.Call(ctx, "plugin_notify_post", json.RawMessage(`{"text":"hi"}`), "s1/4/9", "v1.call")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if isErr || out != "post:hi" {
		t.Fatalf("call result = %q isErr=%v", out, isErr)
	}
	if got := shim.seen("tools/call"); len(got) != 1 || got[0] != "Bearer v1.call" {
		t.Fatalf("tools/call tokens = %v", got)
	}
	shim.mu.Lock()
	meta := append([]map[string]any(nil), shim.meta...)
	shim.mu.Unlock()
	if len(meta) != 1 {
		t.Fatalf("recorded calls = %d, want 1", len(meta))
	}
	if meta[0]["mayfly.io/idempotency-key"] != "s1/4/9" || meta[0]["mayfly.io/session"] != "s1" || meta[0]["mayfly.io/agent"] != "writer" {
		t.Fatalf("_meta = %+v", meta[0])
	}

	// A second call with another token must carry that token, not a stale one.
	if _, _, err := host.Call(ctx, "plugin_notify_post", json.RawMessage(`{"text":"again"}`), "s1/4/10", "v1.call2"); err != nil {
		t.Fatalf("second Call: %v", err)
	}
	if got := shim.seen("tools/call"); len(got) != 2 || got[1] != "Bearer v1.call2" {
		t.Fatalf("tools/call tokens = %v", got)
	}

	// The host reports a missing call token instead of sending an unauthenticated call.
	if _, _, err := host.Call(ctx, "plugin_notify_post", json.RawMessage(`{}`), "s1/4/11", ""); err == nil {
		t.Fatal("expected a missing call token error")
	}
}

// TestSidecarConnectErrors keeps a failed sidecar from blocking the plugins
// that did connect, and names the plugin that failed.
func TestSidecarConnectErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	missing := filepath.Join(t.TempDir(), "absent.sock")
	def := sidecarDef(missing, "post")
	host, errs := Start(ctx, []sessioncfg.Plugin{def}, Options{SessionID: "s1", Agent: "writer"})
	if len(errs) != 0 {
		t.Fatalf("Start: %v", errs)
	}
	defer host.Close()

	errs = host.StartSidecars(ctx, map[string]string{})
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one", errs)
	}
	if got := errs[0].Error(); !strings.Contains(got, `plugin "notify"`) {
		t.Fatalf("error = %q", got)
	}
	if host.RequiredErr() != nil {
		t.Fatal("an optional plugin failure must not fail the session")
	}
}
