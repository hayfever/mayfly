package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"

	"github.com/hayfever/mayfly/internal/content"
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

// TestMCPFloodHelperProcess writes more lines than the stdio stream buffer
// holds, then exits without reading stdin. It is the helper process of
// TestStdioStreamClosesOnServerExit.
func TestMCPFloodHelperProcess(t *testing.T) {
	if os.Getenv("MAYFLY_MCP_FLOOD") != "1" {
		return
	}
	for id := 1; id <= 200; id++ {
		fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{}}\n", id)
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
		return map[string]any{"tools": []map[string]any{
			{
				"name":        "echo",
				"description": "echo",
				"inputSchema": map[string]any{"type": "object"},
			},
			{
				"name":        "env",
				"description": "returns the process environment",
				"inputSchema": map[string]any{"type": "object"},
			},
		}}, nil
	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(params, &call)
		if call.Name == "env" {
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": strings.Join(os.Environ(), "\n")}},
			}, nil
		}
		text, _ := call.Arguments["text"].(string)
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "echo: " + text}},
		}, nil
	default:
		return nil, map[string]any{"code": -32601, "message": "unknown method"}
	}
}

// TestStdioEnvDropsRuntimeSecrets checks what a stdio MCP server inherits: the
// toolchain and locale variables it needs, and its own env, but not the
// runtime's token, config path, or provider keys.
func TestStdioEnvDropsRuntimeSecrets(t *testing.T) {
	t.Setenv("MAYFLY_SESSION_TOKEN", "session-token")
	t.Setenv("MAYFLY_CONFIG", "/etc/mayfly/session.json")
	t.Setenv("OPENAI_API_KEY", "provider-key")
	t.Setenv("LANG", "C.UTF-8")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := NewStdio(ctx, "helper", os.Args[0], []string{"-test.run=TestMCPHelperProcess"},
		map[string]string{"MAYFLY_MCP_HELPER": "1", "SERVER_SETTING": "configured"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	out, isErr, err := client.CallTool(ctx, "env", json.RawMessage(`{}`))
	if err != nil || isErr {
		t.Fatalf("env call: error=%v err=%v", isErr, err)
	}
	inherited := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			inherited[name] = value
		}
	}
	for name, want := range map[string]string{
		"MAYFLY_MCP_HELPER": "1",
		"SERVER_SETTING":    "configured",
		"LANG":              "C.UTF-8",
	} {
		if got := inherited[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if inherited["PATH"] == "" {
		t.Error("the server has no PATH")
	}
	for _, name := range []string{"MAYFLY_SESSION_TOKEN", "MAYFLY_CONFIG", "OPENAI_API_KEY"} {
		if value, ok := inherited[name]; ok {
			t.Errorf("%s was inherited: %q", name, value)
		}
	}
}

// TestStdioStreamClosesOnServerExit pins the end of the response stream. A
// server that writes more lines than the stream buffer holds and then exits
// leaves a line pending; the stream must still close, so a waiting call ends
// with the exit instead of running to its deadline.
func TestStdioStreamClosesOnServerExit(t *testing.T) {
	transport, err := newStdioTransportExact(context.Background(), os.Args[0],
		[]string{"-test.run=TestMCPFloodHelperProcess"},
		append(os.Environ(), "MAYFLY_MCP_FLOOD=1"), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	select {
	case <-transport.Exited():
	case <-time.After(15 * time.Second):
		t.Fatal("the helper server did not exit")
	}
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-transport.lines:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the response stream did not close after the server exited")
		}
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
	if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "env" {
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
	if err != nil || len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "image" {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager := Start(ctx, []sessioncfg.MCPServer{{
		Name:      "my server",
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMCPHelperProcess"},
		Env:       map[string]string{"MAYFLY_MCP_HELPER": "1"},
	}}, testLogger(), content.Limits{})
	defer manager.Close()
	if errs := manager.Errors(); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	tools := manager.Tools()
	if len(tools) != 2 {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].Name != "mcp_my_server_echo" {
		t.Fatalf("tool name = %q", tools[0].Name)
	}
	res, err := manager.Call(ctx, tools[0].Name, json.RawMessage(`{"text":"x"}`))
	if err != nil || !strings.Contains(res.Text(), "echo: x") {
		t.Fatalf("call = %q err=%v", res.Text(), err)
	}
	if _, err := manager.Call(ctx, "missing", nil); err == nil {
		t.Fatal("expected unknown tool error")
	}
}

func TestManagerToolAllowlist(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	allowed := Start(ctx, []sessioncfg.MCPServer{{
		Name:      "search",
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMCPHelperProcess"},
		Env:       map[string]string{"MAYFLY_MCP_HELPER": "1"},
		Tools:     []string{"echo"},
	}}, testLogger(), content.Limits{})
	defer allowed.Close()
	if errs := allowed.Errors(); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if tools := allowed.Tools(); len(tools) != 1 || tools[0].Name != "mcp_search_echo" {
		t.Fatalf("allowed tools = %+v", tools)
	}

	hidden := Start(ctx, []sessioncfg.MCPServer{{
		Name:      "search",
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMCPHelperProcess"},
		Env:       map[string]string{"MAYFLY_MCP_HELPER": "1"},
		Tools:     []string{"missing"},
	}}, testLogger(), content.Limits{})
	defer hidden.Close()
	if errs := hidden.Errors(); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if tools := hidden.Tools(); len(tools) != 0 {
		t.Fatalf("allowlist did not hide echo: %+v", tools)
	}
}

// TestStartSkipsHTTPServers keeps an HTTP mcp.servers entry out of the
// runtime's manager. The gateway discovers and calls it, so a call whose
// discovery failed must not reach the broker proxy instead.
func TestStartSkipsHTTPServers(t *testing.T) {
	manager := Start(context.Background(), []sessioncfg.MCPServer{{
		Name:      "remote",
		Transport: "http",
	}}, testLogger(), content.Limits{})
	defer manager.Close()
	if errs := manager.Errors(); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if tools := manager.Tools(); len(tools) != 0 {
		t.Fatalf("tools = %+v", tools)
	}
}

// TestUpstreamClientsHaveNoFixedTimeout keeps http.Client.Timeout out of the
// default and Unix clients: a tool bounds its own call, and a fixed client cap
// would cut off a call the manifest allows to run longer.
func TestUpstreamClientsHaveNoFixedTimeout(t *testing.T) {
	plain := newHTTPTransport("http://127.0.0.1/mcp", nil, nil, nil)
	if plain.client.Timeout != 0 {
		t.Fatalf("default client timeout = %s", plain.client.Timeout)
	}
	unix := NewUnixHTTP("service", "/tmp/mayfly.sock", nil)
	transport, ok := unix.transport.(*httpTransport)
	if !ok {
		t.Fatalf("Unix client transport = %T", unix.transport)
	}
	if transport.client.Timeout != 0 {
		t.Fatalf("Unix client timeout = %s", transport.client.Timeout)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHTTPImageContent(t *testing.T) {
	server := httptest.NewServer(stub.MCPHandler())
	defer server.Close()
	client := NewHTTP("images", server.URL+"/mcp", "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := client.CallToolResult(ctx, "image", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Parts) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if res.Parts[0].Type != content.TypeText || res.Parts[0].Text != "image follows" {
		t.Fatalf("text part = %+v", res.Parts[0])
	}
	want, err := base64.StdEncoding.DecodeString(stub.ImagePNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	image := res.Parts[1]
	if !image.IsImage() || image.MIMEType != stub.ImagePNGMIME || !bytes.Equal(image.Data, want) {
		t.Fatalf("image part = mime %q bytes %d", image.MIMEType, len(image.Data))
	}
	text := res.Text()
	if !strings.Contains(text, "[image image/png,") {
		t.Fatalf("summary = %q", text)
	}
	if strings.Contains(text, stub.ImagePNGBase64) {
		t.Fatal("summary contains base64 image data")
	}
	flat, isErr, err := client.CallTool(ctx, "image", json.RawMessage(`{}`))
	if err != nil || isErr || flat != text {
		t.Fatalf("flattened = %q isErr=%v err=%v", flat, isErr, err)
	}
}

func TestParseContentBoundaries(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("png!"))
	blocks := func(values ...map[string]any) []json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	limits := content.Limits{MaxImageBytes: 4, MaxResultImageBytes: 6, MaxSessionImageBytes: 8}

	cases := []struct {
		name     string
		items    []json.RawMessage
		wantErr  bool
		wantText string
		images   int
	}{
		{name: "text", items: blocks(map[string]any{"type": "text", "text": "hello"}), wantText: "hello"},
		{name: "image", items: blocks(map[string]any{"type": "image", "mimeType": "image/png", "data": png}), images: 1},
		{name: "mixed", items: blocks(
			map[string]any{"type": "text", "text": "caption"},
			map[string]any{"type": "image", "mimeType": "image/png", "data": png},
		), wantText: "caption", images: 1},
		{name: "disallowed mime", items: blocks(map[string]any{"type": "image", "mimeType": "image/svg+xml", "data": png}), wantErr: true, wantText: "not an allowed MIME type"},
		{name: "missing mime", items: blocks(map[string]any{"type": "image", "data": png}), wantErr: true, wantText: "mimeType is missing"},
		{name: "invalid base64", items: blocks(map[string]any{"type": "image", "mimeType": "image/png", "data": "!!!"}), wantErr: true, wantText: "not valid base64"},
		{name: "oversize", items: blocks(map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("12345"))}), wantErr: true, wantText: "per-image limit"},
		{name: "over result", items: blocks(
			map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("1234"))},
			map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("5678"))},
		), images: 1, wantErr: true, wantText: "per-result limit"},
		{name: "audio", items: blocks(map[string]any{"type": "audio", "mimeType": "audio/wav", "data": png}), wantText: "tool-result audio is not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parseContent(tc.items, limits)
			if res.IsError != tc.wantErr {
				t.Fatalf("IsError = %v, want %v (parts %+v)", res.IsError, tc.wantErr, res.Parts)
			}
			if got := content.ImageCount(res.Parts); got != tc.images {
				t.Fatalf("images = %d, want %d (parts %+v)", got, tc.images, res.Parts)
			}
			if tc.wantText != "" && !strings.Contains(res.Text(), tc.wantText) {
				t.Fatalf("text = %q, want %q", res.Text(), tc.wantText)
			}
		})
	}
}
