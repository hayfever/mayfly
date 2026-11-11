// Package mcp implements a minimal Model Context Protocol client. It supports
// stdio servers (spawned processes) and Streamable HTTP servers. HTTP servers
// are reached through the control plane's credential proxy.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/hayden/mayfly/internal/version"
)

const protocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("mcp error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type transport interface {
	Do(ctx context.Context, id int64, payload []byte) ([]byte, error)
	Notify(ctx context.Context, payload []byte) error
	Close() error
}

// Client is one initialized MCP server connection.
type Client struct {
	name      string
	transport transport
	nextID    int64
	mu        sync.Mutex
}

// Tool is a tool exposed by an MCP server.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Server      string          `json:"-"`
	RemoteName  string          `json:"-"`
}

// NewStdio starts and returns an uninitialized stdio client.
func NewStdio(ctx context.Context, name, command string, args []string, env map[string]string) (*Client, error) {
	t, err := newStdioTransport(ctx, command, args, env)
	if err != nil {
		return nil, err
	}
	return &Client{name: name, transport: t}, nil
}

// NewHTTP returns an uninitialized Streamable HTTP client.
func NewHTTP(name, url, token string, headers map[string]string) *Client {
	return &Client{name: name, transport: newHTTPTransport(url, token, headers)}
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    "mayfly-agent",
			"version": version.Version,
		},
	}
	if _, err := c.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("%s: initialize: %w", c.name, err)
	}
	notify, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	if err != nil {
		return err
	}
	if err := c.transport.Notify(ctx, notify); err != nil {
		return fmt.Errorf("%s: initialized notification: %w", c.name, err)
	}
	return nil
}

// ListTools returns every tool the server exposes, following pagination.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var out []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("%s: decode tools/list: %w", c.name, err)
		}
		for i := range page.Tools {
			page.Tools[i].Server = c.name
		}
		out = append(out, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// CallTool invokes a tool and flattens its content into text.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", false, err
	}
	var result struct {
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		IsError           bool              `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", false, fmt.Errorf("%s: decode tools/call: %w", c.name, err)
	}
	var parts []string
	for _, item := range result.Content {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item, &block); err != nil {
			parts = append(parts, string(item))
			continue
		}
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "image", "audio":
			parts = append(parts, "["+block.Type+" content omitted]")
		case "resource":
			parts = append(parts, string(item))
		default:
			parts = append(parts, string(item))
		}
	}
	if len(result.StructuredContent) > 0 && string(result.StructuredContent) != "null" {
		parts = append(parts, string(result.StructuredContent))
	}
	if len(parts) == 0 {
		parts = append(parts, string(raw))
	}
	return strings.Join(parts, "\n"), result.IsError, nil
}

// Close shuts down the transport.
func (c *Client) Close() error { return c.transport.Close() }

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	raw, err := c.transport.Do(ctx, id, payload)
	if err != nil {
		return nil, err
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	if len(resp.Result) == 0 {
		return nil, errors.New("response has no result")
	}
	return resp.Result, nil
}

// tailBuffer keeps the last bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func decodeSSE(r io.Reader, id int64) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			raw := bytes.TrimSpace(data.Bytes())
			data.Reset()
			var probe struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				continue
			}
			if matchesID(probe.ID, id) {
				return append([]byte(nil), raw...), nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("stream ended without a response for id %d", id)
}

func matchesID(raw json.RawMessage, id int64) bool {
	var got int64
	if err := json.Unmarshal(raw, &got); err == nil {
		return got == id
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == fmt.Sprint(id)
	}
	return false
}
