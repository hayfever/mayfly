package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"

	"github.com/hayfever/mayfly/internal/sessioncfg"
	"github.com/hayfever/mayfly/internal/stub"
)

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("MAYFLY_MCP_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		result, rpcErr := helperDispatch(req.Method, req.Params)
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			response["error"] = rpcErr
		} else {
			response["result"] = result
		}
		_ = encoder.Encode(response)
	}
	os.Exit(0)
}

func helperDispatch(method string, params json.RawMessage) (any, map[string]any) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "helper", "version": "1"},
		}, nil
	case "tools/list":
		return map[string]any{"tools": []map[string]any{{
			"name":        "echo",
			"description": "echo",
			"inputSchema": map[string]any{"type": "object"},
		}}}, nil
	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(params, &call)
		text, _ := call.Arguments["text"].(string)
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "echo: " + text}},
		}, nil
	default:
		return nil, map[string]any{"code": -32601, "message": "unknown method"}
	}
}

func TestStdioClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := NewStdio(ctx, "helper", os.Args[0], []string{"-test.run=TestMCPHelperProcess"}, map[string]string{"MAYFLY_MCP_HELPER": "1"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	out, isErr, err := client.CallTool(ctx, "echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if isErr || out != "echo: hello" {
		t.Fatalf("call = %q error=%v", out, isErr)
	}
}

func TestHTTPClientAndAuth(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		stub.MCPHandler().ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := NewHTTP("remote", server.URL+"/mcp", "session-token", nil)
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer session-token" {
		t.Fatalf("authorization = %q", auth)
	}
	tools, err := client.ListTools(ctx)
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools = %+v err=%v", tools, err)
	}
	out, _, err := client.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || out != "echo: hi" {
		t.Fatalf("call = %q err=%v", out, err)
	}
}

func TestHTTPWithClientHeaders(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		stub.MCPHandler().ServeHTTP(w, r)
	}))
	defer server.Close()

	transport := &headerTransport{base: http.DefaultTransport}
	client := NewHTTPWithClient("remote", server.URL+"/mcp",
		map[string]string{"X-Plugin": "a"}, &http.Client{Transport: transport},
		func(r *http.Request) { r.Header.Set("X-Hook", "b") })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Plugin") != "a" {
		t.Fatalf("X-Plugin = %q", got.Get("X-Plugin"))
	}
	if got.Get("X-Hook") != "b" {
		t.Fatalf("X-Hook = %q", got.Get("X-Hook"))
	}
	if got.Get("X-Client") != "custom" {
		t.Fatalf("given client was not used: X-Client = %q", got.Get("X-Client"))
	}
	if got.Get("Authorization") != "" {
		t.Fatalf("Authorization = %q, want none", got.Get("Authorization"))
	}
}

// headerTransport marks every request so a test can see that the client it
// built was the one that ran.
type headerTransport struct{ base http.RoundTripper }

func (t *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Client", "custom")
	return t.base.RoundTrip(r)
}

func TestDecodeSSE(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n"
	raw, err := decodeSSE(strings.NewReader(stream), 1)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			OK bool `json:"ok"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if !response.Result.OK {
		t.Fatal("result missing")
	}
	if _, err := decodeSSE(strings.NewReader(stream), 2); err == nil {
		t.Fatal("expected missing response error")
	}
}

func TestManagerNamesTools(t *testing.T) {
	server := httptest.NewServer(stub.MCPHandler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager := Start(ctx, []sessioncfg.MCPServer{{
		Name:      "my server",
		Transport: "http",
		URL:       server.URL + "/mcp",
	}}, "tok", testLogger())
	defer manager.Close()
	if errs := manager.Errors(); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	tools := manager.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].Name != "mcp_my_server_echo" {
		t.Fatalf("tool name = %q", tools[0].Name)
	}
	out, _, err := manager.Call(ctx, tools[0].Name, json.RawMessage(`{"text":"x"}`))
	if err != nil || !strings.Contains(out, "echo: x") {
		t.Fatalf("call = %q err=%v", out, err)
	}
	if _, _, err := manager.Call(ctx, "missing", nil); err == nil {
		t.Fatal("expected unknown tool error")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
