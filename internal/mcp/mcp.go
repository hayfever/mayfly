// Package mcp implements a minimal Model Context Protocol client. It supports
// stdio servers (spawned processes) and Streamable HTTP servers. HTTP servers
// are reached through the control plane's credential proxy.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/hayfever/mayfly/internal/content"
	"github.com/hayfever/mayfly/internal/version"
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
	limits    content.Limits
}

// Tool is a tool exposed by an MCP server.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Server      string          `json:"-"`
	RemoteName  string          `json:"-"`
}

// NewStdio starts and returns an uninitialized stdio client. The server
// inherits the runtime's PATH, HOME, TMPDIR, locale, TLS trust store, and proxy
// settings, with env applied on top. Nothing else is inherited: see
// sessioncfg.StdioEnv.
func NewStdio(ctx context.Context, name, command string, args []string, env map[string]string) (*Client, error) {
	t, err := newStdioTransport(ctx, command, args, env)
	if err != nil {
		return nil, err
	}
	return &Client{name: name, transport: t}, nil
}

// NewStdioExact is NewStdio with a complete environment: env is the whole
// process environment, and dir is the working directory.
func NewStdioExact(ctx context.Context, name, command string, args, env []string, dir string, stderr io.Writer) (*Client, error) {
	t, err := newStdioTransportExact(ctx, command, args, env, dir, stderr)
	if err != nil {
		return nil, err
	}
	return &Client{name: name, transport: t}, nil
}

// NewHTTP returns an uninitialized Streamable HTTP client.
func NewHTTP(name, url, token string, headers map[string]string) *Client {
	var perRequest func(*http.Request)
	if token != "" {
		perRequest = func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	}
	return NewHTTPWithClient(name, url, headers, defaultHTTPClient(), perRequest)
}

// NewHTTPWithClient returns an uninitialized Streamable HTTP client that uses
// client for every request. perRequest can change each request, for example to
// add a credential the client's transport does not know.
func NewHTTPWithClient(name, url string, headers map[string]string, client *http.Client, perRequest func(*http.Request)) *Client {
	return &Client{name: name, transport: newHTTPTransport(url, headers, client, perRequest)}
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
	if _, _, err := c.call(ctx, "initialize", params); err != nil {
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
		raw, _, err := c.call(ctx, "tools/list", params)
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

// ToolResult is one tools/call result. Parts preserves text and image blocks
// in order. Text is the only rendering that leaves for a log or a text-only
// caller.
type ToolResult struct {
	Parts   []content.Part
	IsError bool
}

// Text renders the result as newline-joined text. Image parts become a line
// with their type, size, and digest, never their bytes.
func (r ToolResult) Text() string { return content.Text(r.Parts) }

// HasImage reports whether the result carries image content.
func (r ToolResult) HasImage() bool { return content.HasImage(r.Parts) }

// TextResult returns a text-only result.
func TextResult(text string, isError bool) ToolResult {
	return ToolResult{Parts: []content.Part{content.TextPart(text)}, IsError: isError}
}

// SetContentLimits bounds the image content this client accepts. A zero limit
// uses the content package defaults.
func (c *Client) SetContentLimits(limits content.Limits) {
	c.limits = limits.WithDefaults()
}

// CallTool invokes a tool and flattens its content into text. A caller that
// must forward images uses CallToolResult.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	out, isErr, _, err := c.CallToolMeta(ctx, name, args, nil)
	return out, isErr, err
}

// CallToolMeta is CallTool with request metadata. The _meta field is omitted
// when meta is nil. The returned id is the JSON-RPC id of the request, which
// Cancel accepts.
func (c *Client) CallToolMeta(ctx context.Context, name string, args json.RawMessage, meta map[string]any) (string, bool, int64, error) {
	res, id, err := c.CallToolResultMeta(ctx, name, args, meta)
	return res.Text(), res.IsError, id, err
}

// CallToolResult invokes a tool and returns its structured content.
func (c *Client) CallToolResult(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	res, _, err := c.CallToolResultMeta(ctx, name, args, nil)
	return res, err
}

// CallToolResultMeta is CallToolResult with request metadata and the JSON-RPC
// id Cancel accepts.
func (c *Client) CallToolResultMeta(ctx context.Context, name string, args json.RawMessage, meta map[string]any) (ToolResult, int64, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	params := map[string]any{
		"name":      name,
		"arguments": args,
	}
	if meta != nil {
		params["_meta"] = meta
	}
	raw, id, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return ToolResult{}, id, err
	}
	var result struct {
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		IsError           bool              `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return ToolResult{}, id, fmt.Errorf("%s: decode tools/call: %w", c.name, err)
	}
	out := parseContent(result.Content, c.limits)
	if len(result.StructuredContent) > 0 && string(result.StructuredContent) != "null" {
		out.Parts = append(out.Parts, content.TextPart(string(result.StructuredContent)))
	}
	if len(out.Parts) == 0 {
		out.Parts = append(out.Parts, content.TextPart(string(raw)))
	}
	out.IsError = result.IsError || out.IsError
	return out, id, nil
}

// parseContent converts MCP content blocks into parts. An image that fails
// the MIME, base64, or size rules becomes a text error and marks the result
// an error: a caller must never see the bytes half-delivered. Audio has no
// supported tool-result content part, so it becomes a labeled placeholder.
func parseContent(items []json.RawMessage, limits content.Limits) ToolResult {
	limits = limits.WithDefaults()
	var out ToolResult
	var images int64
	for _, item := range items {
		var block struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MIMEType string `json:"mimeType"`
			Data     string `json:"data"`
		}
		if err := json.Unmarshal(item, &block); err != nil {
			out.Parts = append(out.Parts, content.TextPart(string(item)))
			continue
		}
		switch block.Type {
		case "text":
			out.Parts = append(out.Parts, content.TextPart(block.Text))
		case "image":
			part, reason := decodeImage(block.MIMEType, block.Data, limits, images)
			if reason != "" {
				out.Parts = append(out.Parts, content.TextPart("ERROR: tool result image was rejected: "+reason))
				out.IsError = true
				continue
			}
			images += int64(len(part.Data))
			out.Parts = append(out.Parts, part)
		case "audio":
			size := int64(base64.StdEncoding.DecodedLen(len(block.Data)))
			mime := block.MIMEType
			if mime == "" {
				mime = "unknown"
			}
			out.Parts = append(out.Parts, content.TextPart(fmt.Sprintf(
				"[audio %s, ~%d bytes omitted: tool-result audio is not supported]", mime, size)))
		default:
			out.Parts = append(out.Parts, content.TextPart(string(item)))
		}
	}
	return out
}

// decodeImage validates one MCP image block and returns the part or the
// reason it was rejected.
func decodeImage(mime, data string, limits content.Limits, consumed int64) (content.Part, string) {
	normalized, ok := content.NormalizeMIMEType(mime)
	if normalized == "" {
		return content.Part{}, "mimeType is missing"
	}
	if !ok {
		return content.Part{}, fmt.Sprintf("%q is not an allowed MIME type (allowed: %s)",
			mime, strings.Join(content.AllowedMIMETypes(), ", "))
	}
	if data == "" {
		return content.Part{}, "data is missing"
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return content.Part{}, "data is not valid base64"
	}
	part := content.ImagePart(normalized, raw)
	if reason := content.ValidateImage(part, limits); reason != "" {
		return content.Part{}, reason
	}
	if consumed+int64(len(raw)) > limits.MaxResultImageBytes {
		return content.Part{}, fmt.Sprintf("images total %d bytes, over the %d-byte per-result limit",
			consumed+int64(len(raw)), limits.MaxResultImageBytes)
	}
	return part, ""
}

// Cancel tells the server that it will not receive a result for the request
// with the given id.
func (c *Client) Cancel(ctx context.Context, id int64, reason string) error {
	if id == 0 {
		return nil
	}
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/cancelled",
		Params:  map[string]any{"requestId": id, "reason": reason},
	})
	if err != nil {
		return err
	}
	return c.transport.Notify(ctx, payload)
}

// Exited reports the end of a stdio server process. It is nil for clients
// whose server is not a process this client started.
func (c *Client) Exited() <-chan struct{} {
	if w, ok := c.transport.(interface{ Exited() <-chan struct{} }); ok {
		return w.Exited()
	}
	return nil
}

// Close shuts down the transport.
func (c *Client) Close() error { return c.transport.Close() }

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, int64, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, id, err
	}
	raw, err := c.transport.Do(ctx, id, payload)
	if err != nil {
		return nil, id, err
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, id, fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return nil, id, resp.Error
	}
	if len(resp.Result) == 0 {
		return nil, id, errors.New("response has no result")
	}
	return resp.Result, id, nil
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
