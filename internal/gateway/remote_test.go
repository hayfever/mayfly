package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// fakeUpstream is a streamable HTTP MCP server that records what the gateway
// sends it. It sets a session id on initialize so Close sends a DELETE the
// test can count.
type fakeUpstream struct {
	mu       sync.Mutex
	tools    []string
	calls    []upstreamCall
	headers  []http.Header
	deletes  int
	failList bool
}

type upstreamCall struct {
	name string
	meta map[string]any
}

func newFakeUpstream(tools ...string) *fakeUpstream {
	return &fakeUpstream{tools: tools}
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	if r.Method == http.MethodDelete {
		f.mu.Lock()
		f.deletes++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "fake-session")
		writeMCPResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
		})
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		f.mu.Lock()
		fail := f.failList
		tools := append([]string(nil), f.tools...)
		f.mu.Unlock()
		if fail {
			http.Error(w, "upstream is down", http.StatusServiceUnavailable)
			return
		}
		list := make([]map[string]any, 0, len(tools))
		for _, name := range tools {
			list = append(list, map[string]any{
				"name":        name,
				"description": "description of " + name,
				"inputSchema": map[string]any{"type": "object"},
			})
		}
		writeMCPResult(w, req.ID, map[string]any{"tools": list})
	case "tools/call":
		var params struct {
			Name string         `json:"name"`
			Meta map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			http.Error(w, "bad params", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, upstreamCall{name: params.Name, meta: params.Meta})
		f.mu.Unlock()
		writeMCPResult(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok:" + params.Name}},
		})
	default:
		writeMCPResult(w, req.ID, map[string]any{})
	}
}

func writeMCPResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{"jsonrpc": "2.0", "result": result}
	if len(id) > 0 {
		response["id"] = json.RawMessage(id)
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (f *fakeUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeUpstream) lastCall(t *testing.T) upstreamCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the upstream received no tool call")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeUpstream) headerValues(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var values []string
	for _, header := range f.headers {
		if value := header.Get(name); value != "" {
			values = append(values, value)
		}
	}
	return values
}

// countingTransport proves the gateway speaks through Options.HTTPClient.
type countingTransport struct {
	base  http.RoundTripper
	count int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	atomic.AddInt32(&c.count, 1)
	return c.base.RoundTrip(r)
}

func httpServerOptions(t *testing.T, def config.MCPServerConfig, client *http.Client) Options {
	t.Helper()
	return Options{
		HTTPClient: client,
		MCPServer: func(name string) (config.MCPServerConfig, bool) {
			if name == def.Name {
				return def, true
			}
			return config.MCPServerConfig{}, false
		},
	}
}

func TestRemoteCallInjectsHeaders(t *testing.T) {
	upstream := newFakeUpstream("send", "read")
	server := httptest.NewServer(upstream)
	defer server.Close()

	client := &http.Client{Transport: &countingTransport{base: http.DefaultTransport}}
	def := config.MCPServerConfig{
		Name:            "chat",
		Transport:       "http",
		URL:             server.URL,
		ResolvedHeaders: map[string]string{"Authorization": "Bearer SECRET", "X-Tenant": "acme"},
	}
	g, mem := newTestGateway(t, httpServerOptions(t, def, client),
		GrantTool{Name: "mcp_chat_send", Owner: "chat", Remote: "send", Placement: MCPHTTP,
			Timeout: 5 * time.Second, MaxOutput: 4096},
	)

	resp, err := begin(g, "mcp_chat_send", 3, 4, `{"text":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result == nil || resp.Dispatch != nil {
		t.Fatalf("response = %+v, want a result", resp)
	}
	if resp.Result.Content != "ok:send" || resp.Result.IsError {
		t.Fatalf("result = %+v", resp.Result)
	}

	header := upstream.headerValues("Authorization")
	if len(header) == 0 || header[0] != "Bearer SECRET" {
		t.Fatalf("upstream authorization = %v", header)
	}
	if tenant := upstream.headerValues("X-Tenant"); len(tenant) == 0 || tenant[0] != "acme" {
		t.Fatalf("upstream tenant = %v", tenant)
	}
	call := upstream.lastCall(t)
	if call.name != "send" {
		t.Fatalf("upstream tool = %q", call.name)
	}
	if got := call.meta["mayfly.io/idempotency-key"]; got != "s1/3/4" {
		t.Fatalf("idempotency key = %v", got)
	}
	if got := call.meta["mayfly.io/session"]; got != "s1" {
		t.Fatalf("session meta = %v", got)
	}
	if got := call.meta["mayfly.io/agent"]; got != "helper" {
		t.Fatalf("agent meta = %v", got)
	}
	if atomic.LoadInt32(&client.Transport.(*countingTransport).count) == 0 {
		t.Fatal("the gateway ignored Options.HTTPClient")
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET") {
		t.Fatalf("the runtime response carries the upstream credential: %s", encoded)
	}
	rec, found, err := mem.Get(context.Background(), "s1", "s1/3/4")
	if err != nil || !found || rec.State != journal.CallDone {
		t.Fatalf("ledger record = %+v found=%v err=%v", rec, found, err)
	}
}

func TestRemoteAllowlist(t *testing.T) {
	upstream := newFakeUpstream("send", "hidden")
	server := httptest.NewServer(upstream)
	defer server.Close()

	def := config.MCPServerConfig{
		Name:            "chat",
		Transport:       "http",
		URL:             server.URL,
		Tools:           []string{"send"},
		IdempotentTools: []string{"send"},
	}
	mem := journal.NewMemory()
	if err := mem.Reserve(context.Background(), "s1", 10); err != nil {
		t.Fatal(err)
	}
	opts := httpServerOptions(t, def, server.Client())
	opts.Ledger = mem
	opts.Sequencer = mem
	opts.Logger = quietLogger()
	g := New(opts)
	g.Register(GrantFor(sessioncfg.Config{SessionID: "s1", MaxSteps: 4}, "helper",
		[]config.MCPServerConfig{def}))

	discovery, err := g.Discover(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Tools) != 1 {
		t.Fatalf("discovery tools = %+v", discovery.Tools)
	}
	tool := discovery.Tools[0]
	if tool.Name != "mcp_chat_send" || tool.Owner != "chat" || !tool.Idempotent {
		t.Fatalf("discovery tool = %+v", tool)
	}
	if tool.Description != "description of send" || len(tool.InputSchema) == 0 {
		t.Fatalf("discovery lost the upstream metadata: %+v", tool)
	}

	if _, err := begin(g, "mcp_chat_hidden", 1, 1, `{}`); statusOf(err) != http.StatusForbidden {
		t.Fatalf("hidden tool error = %v, want 403", err)
	}
	if upstream.callCount() != 0 {
		t.Fatalf("a hidden tool reached the upstream")
	}

	resp, err := begin(g, "mcp_chat_send", 1, 1, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result == nil || resp.Result.Content != "ok:send" {
		t.Fatalf("allowed tool = %+v", resp)
	}
}

func TestRemoteWildcard(t *testing.T) {
	upstream := newFakeUpstream("send", "read")
	server := httptest.NewServer(upstream)
	defer server.Close()

	def := config.MCPServerConfig{
		Name:            "chat",
		Transport:       "http",
		URL:             server.URL,
		IdempotentTools: []string{"read"},
	}
	mem := journal.NewMemory()
	if err := mem.Reserve(context.Background(), "s1", 10); err != nil {
		t.Fatal(err)
	}
	opts := httpServerOptions(t, def, server.Client())
	opts.Ledger = mem
	opts.Sequencer = mem
	opts.Logger = quietLogger()
	g := New(opts)
	g.Register(GrantFor(sessioncfg.Config{SessionID: "s1", MaxSteps: 4}, "helper",
		[]config.MCPServerConfig{def}))

	discovery, err := g.Discover(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Tools) != 2 {
		t.Fatalf("discovery tools = %+v", discovery.Tools)
	}
	idempotent := map[string]bool{}
	for _, tool := range discovery.Tools {
		idempotent[tool.Name] = tool.Idempotent
	}
	if !idempotent["mcp_chat_read"] || idempotent["mcp_chat_send"] {
		t.Fatalf("wildcard idempotency = %v", idempotent)
	}

	resp, err := begin(g, "mcp_chat_send", 1, 1, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result == nil || resp.Result.Content != "ok:send" {
		t.Fatalf("wildcard call = %+v", resp)
	}
	if _, err := begin(g, "mcp_other_send", 1, 2, `{}`); statusOf(err) != http.StatusForbidden {
		t.Fatalf("foreign server error = %v, want 403", err)
	}
}

func TestRemoteResumeNonIdempotent(t *testing.T) {
	ctx := context.Background()

	t.Run("non-idempotent call is not repeated", func(t *testing.T) {
		upstream := newFakeUpstream("send")
		server := httptest.NewServer(upstream)
		defer server.Close()
		def := config.MCPServerConfig{Name: "chat", Transport: "http", URL: server.URL, Tools: []string{"send"}}
		g, mem := newTestGateway(t, httpServerOptions(t, def, server.Client()),
			GrantTool{Name: "mcp_chat_send", Owner: "chat", Remote: "send", Placement: MCPHTTP,
				Timeout: 5 * time.Second, MaxOutput: 4096},
		)
		seedPending(t, mem, "s1/3/4", "mcp_chat_send", `{}`)

		resp, err := begin(g, "mcp_chat_send", 3, 4, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Result == nil || !resp.Result.Interrupted || !resp.Result.IsError || resp.Result.Replayed {
			t.Fatalf("result = %+v", resp.Result)
		}
		if !strings.Contains(resp.Result.Content, "interrupted") {
			t.Fatalf("interrupted content = %q", resp.Result.Content)
		}
		if upstream.callCount() != 0 {
			t.Fatal("a non-idempotent interrupted call was repeated")
		}

		again, err := begin(g, "mcp_chat_send", 3, 4, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if again.Result == nil || !again.Result.Replayed || again.Result.Content != resp.Result.Content {
			t.Fatalf("second resume = %+v", again.Result)
		}
		if upstream.callCount() != 0 {
			t.Fatal("a recorded interrupted call was repeated")
		}
	})

	t.Run("idempotent call is repeated", func(t *testing.T) {
		upstream := newFakeUpstream("send")
		server := httptest.NewServer(upstream)
		defer server.Close()
		def := config.MCPServerConfig{Name: "chat", Transport: "http", URL: server.URL,
			Tools: []string{"send"}, IdempotentTools: []string{"send"}}
		g, mem := newTestGateway(t, httpServerOptions(t, def, server.Client()),
			GrantTool{Name: "mcp_chat_send", Owner: "chat", Remote: "send", Placement: MCPHTTP,
				Idempotent: true, Timeout: 5 * time.Second, MaxOutput: 4096},
		)
		seedPending(t, mem, "s1/3/4", "mcp_chat_send", `{}`)

		resp, err := begin(g, "mcp_chat_send", 3, 4, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Result == nil || resp.Result.Interrupted || resp.Result.Content != "ok:send" {
			t.Fatalf("result = %+v", resp.Result)
		}
		if upstream.callCount() != 1 {
			t.Fatalf("upstream calls = %d, want 1", upstream.callCount())
		}
		rec, found, err := mem.Get(ctx, "s1", "s1/3/4")
		if err != nil || !found || rec.State != journal.CallDone || rec.Attempts < 2 {
			t.Fatalf("ledger record = %+v found=%v err=%v", rec, found, err)
		}
	})
}

// seedPending stores an in-flight call, as a runtime that died after begin
// leaves it.
func seedPending(t *testing.T, mem *journal.Memory, key, tool, args string) {
	t.Helper()
	hash, err := argsHash(json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mem.Begin(context.Background(), journal.Call{
		Key:        key,
		SessionID:  "s1",
		Turn:       3,
		Step:       4,
		Tool:       tool,
		ArgsSHA256: hash,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteClosesOnFinalize(t *testing.T) {
	upstream := newFakeUpstream("send")
	server := httptest.NewServer(upstream)
	defer server.Close()

	def := config.MCPServerConfig{Name: "chat", Transport: "http", URL: server.URL, Tools: []string{"send"}}
	opts := httpServerOptions(t, def, server.Client())
	opts.Logger = quietLogger()
	g := New(opts)
	g.Register(GrantFor(sessioncfg.Config{SessionID: "s1", MaxSteps: 4}, "helper",
		[]config.MCPServerConfig{def}))

	if _, err := g.Discover(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	g.Drop("s1")

	upstream.mu.Lock()
	deletes := upstream.deletes
	upstream.mu.Unlock()
	if deletes != 1 {
		t.Fatalf("upstream DELETE count = %d, want 1", deletes)
	}
	if _, err := g.Discover(context.Background(), "s1"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("discovery after finalize = %v, want 404", err)
	}
}

func TestStdioWildcardGrant(t *testing.T) {
	ctx := context.Background()
	def := config.MCPServerConfig{
		Name:            "local",
		Transport:       "stdio",
		Command:         "mcp-local",
		IdempotentTools: []string{"read"},
	}
	mem := journal.NewMemory()
	if err := mem.Reserve(ctx, "s1", 10); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Ledger:    mem,
		Sequencer: mem,
		Logger:    quietLogger(),
		MCPServer: func(name string) (config.MCPServerConfig, bool) {
			if name == def.Name {
				return def, true
			}
			return config.MCPServerConfig{}, false
		},
	}
	g := New(opts)
	g.Register(GrantFor(sessioncfg.Config{SessionID: "s1", MaxSteps: 4}, "helper",
		[]config.MCPServerConfig{def}))

	idempotent, ok := g.wildcardTool(ctx, mustGrant(t, g), "mcp_local_read")
	if !ok || !idempotent.Idempotent || idempotent.Placement != MCPStdio {
		t.Fatalf("read grant = %+v ok=%v", idempotent, ok)
	}
	effect, ok := g.wildcardTool(ctx, mustGrant(t, g), "mcp_local_write")
	if !ok || effect.Idempotent {
		t.Fatalf("write grant = %+v ok=%v", effect, ok)
	}
	if _, ok := g.wildcardTool(ctx, mustGrant(t, g), "mcp_other_read"); ok {
		t.Fatal("a tool of another server was granted")
	}

	seedPending(t, mem, "s1/3/4", "mcp_local_read", `{}`)
	resp, err := begin(g, "mcp_local_read", 3, 4, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Dispatch == nil || resp.Result != nil {
		t.Fatalf("idempotent stdio resume = %+v, want a dispatch", resp)
	}

	seedPending(t, mem, "s1/3/5", "mcp_local_write", `{}`)
	resp, err = begin(g, "mcp_local_write", 3, 5, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result == nil || !resp.Result.Interrupted {
		t.Fatalf("non-idempotent stdio resume = %+v, want the interrupted result", resp)
	}
}

func mustGrant(t *testing.T, g *Gateway) Grant {
	t.Helper()
	grant, ok := g.grantFor("s1")
	if !ok {
		t.Fatal("the session has no grant")
	}
	return grant
}

func TestGatewayServiceCall(t *testing.T) {
	upstream := newFakeUpstream("query")
	server := httptest.NewServer(upstream)
	defer server.Close()

	key := []byte("0123456789abcdef0123456789abcdef")
	mem := journal.NewMemory()
	if err := mem.Reserve(context.Background(), "s1", 10); err != nil {
		t.Fatal(err)
	}
	g := New(Options{
		Ledger:    mem,
		Sequencer: mem,
		ServiceURL: func(name string) (string, []byte, error) {
			return server.URL, key, nil
		},
	})
	cfg := sessioncfg.Config{
		SessionID: "s1",
		MaxSteps:  4,
		Plugins: []sessioncfg.Plugin{{
			Name:      "tickets",
			Placement: "service",
			Tools:     []sessioncfg.PluginTool{{Name: "query", Idempotent: true, TimeoutSeconds: 5}},
		}},
	}
	g.Register(GrantFor(cfg, "helper", nil))

	discovery, err := g.Discover(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Tools) != 1 || discovery.Tools[0].Name != "plugin_tickets_query" {
		t.Fatalf("discovery tools = %+v", discovery.Tools)
	}
	authorization := upstream.headerValues("Authorization")
	if len(authorization) == 0 {
		t.Fatal("discovery carried no discovery token")
	}
	if _, err := VerifyToken(key, strings.TrimPrefix(authorization[0], "Bearer "), TokenScope{
		SessionID: "s1",
		Plugin:    "tickets",
		Tool:      "anything",
	}, time.Now()); err != nil {
		t.Fatalf("discovery token: %v", err)
	}

	resp, err := begin(g, "plugin_tickets_query", 2, 3, `{"id":"abc"}`)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result == nil || resp.Result.Content != "ok:query" {
		t.Fatalf("service result = %+v", resp)
	}
	call := upstream.lastCall(t)
	if call.meta["mayfly.io/idempotency-key"] != "s1/2/3" {
		t.Fatalf("call meta = %v", call.meta)
	}
	tokens := upstream.headerValues("Authorization")
	raw := strings.TrimPrefix(tokens[len(tokens)-1], "Bearer ")
	claims, err := VerifyToken(key, raw, TokenScope{
		SessionID:  "s1",
		Plugin:     "tickets",
		Tool:       "query",
		ArgsSHA256: mustHash(t, `{"id":"abc"}`),
	}, time.Now())
	if err != nil {
		t.Fatalf("call token: %v", err)
	}
	if claims.Key != "s1/2/3" {
		t.Fatalf("call token key = %q", claims.Key)
	}
}

// TestServiceMCPServerRef proves that a service plugin with service.mcpServer
// calls the configured mcp.servers entry with its headers, under the plugin's
// tool names, and never asks the service reconciler for an endpoint.
func TestServiceMCPServerRef(t *testing.T) {
	upstream := newFakeUpstream("find_account")
	server := httptest.NewServer(upstream)
	defer server.Close()

	def := config.MCPServerConfig{
		Name:            "crm",
		Transport:       "http",
		URL:             server.URL,
		ResolvedHeaders: map[string]string{"Authorization": "Bearer crm-token"},
	}
	manifest := plugin.Manifest{
		Name:      "crm",
		Placement: plugin.Service,
		Service:   &plugin.ServiceSpec{MCPServer: "crm"},
		Tools:     []plugin.Tool{{Name: "find_account", Idempotent: true}},
	}
	mem := journal.NewMemory()
	if err := mem.Reserve(context.Background(), "s1", 10); err != nil {
		t.Fatal(err)
	}
	g := New(Options{
		Ledger:    mem,
		Sequencer: mem,
		Manifests: func() []plugin.Manifest { return []plugin.Manifest{manifest} },
		MCPServer: func(name string) (config.MCPServerConfig, bool) {
			if name == def.Name {
				return def, true
			}
			return config.MCPServerConfig{}, false
		},
		ServiceURL: func(name string) (string, []byte, error) {
			t.Fatalf("ServiceURL was called for an mcpServer plugin %q", name)
			return "", nil, nil
		},
	})
	cfg := sessioncfg.Config{
		SessionID: "s1",
		MaxSteps:  4,
		Plugins: []sessioncfg.Plugin{{
			Name:      "crm",
			Placement: "service",
			Tools:     []sessioncfg.PluginTool{{Name: "find_account", Idempotent: true, TimeoutSeconds: 5}},
		}},
	}
	g.Register(GrantFor(cfg, "helper", nil))

	discovery, err := g.Discover(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Tools) != 1 || discovery.Tools[0].Name != "plugin_crm_find_account" {
		t.Fatalf("discovery tools = %+v", discovery.Tools)
	}
	if discovery.Tools[0].Description != "description of find_account" {
		t.Fatalf("discovery description = %q", discovery.Tools[0].Description)
	}
	if len(discovery.Errors) != 0 {
		t.Fatalf("discovery errors = %+v", discovery.Errors)
	}

	resp, err := begin(g, "plugin_crm_find_account", 2, 3, `{"id":"abc"}`)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result == nil || resp.Result.Content != "ok:find_account" {
		t.Fatalf("endpoint result = %+v", resp)
	}
	call := upstream.lastCall(t)
	if call.meta["mayfly.io/idempotency-key"] != "s1/2/3" {
		t.Fatalf("call meta = %v", call.meta)
	}
	if call.meta["mayfly.io/session"] != "s1" {
		t.Fatalf("call meta session = %v", call.meta)
	}
	headers := upstream.headerValues("Authorization")
	if len(headers) == 0 || headers[len(headers)-1] != "Bearer crm-token" {
		t.Fatalf("authorization headers = %v", headers)
	}
}

func mustHash(t *testing.T, args string) string {
	t.Helper()
	hash, err := argsHash(json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
