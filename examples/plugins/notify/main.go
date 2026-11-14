// Command notify is an inline Mayfly plugin: a stdio MCP server that posts a
// message to a team webhook.
//
// The webhook URL is a credential, so it arrives as a secret file that the
// runtime reads into NOTIFY_WEBHOOK_URL; the agent never sees it. The runtime
// speaks MCP over stdin and stdout, and stderr carries logs.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

const protocolVersion = "2025-06-18"

// request is one JSON-RPC message from the runtime. A message without an id
// is a notification and gets no response.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is one JSON-RPC result or error.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// callParams is one tools/call request. Meta carries the gateway's per-call
// facts: the idempotency key and the session. The plugin never sees a token.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      map[string]any  `json:"_meta"`
}

type postInput struct {
	Text string `json:"text"`
}

func main() {
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var msg request
		if err := json.Unmarshal(line, &msg); err != nil {
			fmt.Fprintln(os.Stderr, "decode request:", err)
			continue
		}
		if len(msg.ID) == 0 {
			continue // a notification, for example notifications/initialized
		}
		resp := response{JSONRPC: "2.0", ID: msg.ID}
		switch msg.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "notify", "version": "0.1.0"},
			}
		case "tools/list":
			resp.Result = map[string]any{"tools": tools()}
		case "tools/call":
			text, err := callTool(msg.Params)
			if err != nil {
				resp.Result = errorResult(err)
			} else {
				resp.Result = textResult(text)
			}
		default:
			resp.Error = &rpcError{Code: -32601, Message: "method not found: " + msg.Method}
		}
		if err := encoder.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "encode response:", err)
			return
		}
	}
}

// tools lists the tools this server exposes. plugin.yaml declares the same
// names, and the runtime compares the two lists at session start.
func tools() []any {
	return []any{map[string]any{
		"name":        "post",
		"description": "Post a message to the team channel.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "the message to post"},
			},
			"required": []string{"text"},
		},
	}}
}

func callTool(raw json.RawMessage) (string, error) {
	var params callParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", fmt.Errorf("decode params: %w", err)
	}
	if params.Name != "post" {
		return "", fmt.Errorf("unknown tool %q", params.Name)
	}
	var input postInput
	if err := json.Unmarshal(params.Arguments, &input); err != nil {
		return "", fmt.Errorf("decode arguments: %w", err)
	}
	return post(input.Text, params.Meta)
}

// post forwards the gateway's idempotency key to the webhook. The tool itself
// stays at `idempotent: false`: a webhook that ignores the header would post
// the message twice after a container restart.
func post(text string, meta map[string]any) (string, error) {
	target := os.Getenv("NOTIFY_WEBHOOK_URL")
	if target == "" {
		return "", fmt.Errorf("NOTIFY_WEBHOOK_URL is not set: declare the mayfly-plugin-notify secret")
	}
	key, _ := meta["mayfly.io/idempotency-key"].(string)
	body, err := json.Marshal(map[string]string{
		"text":    text,
		"channel": os.Getenv("NOTIFY_CHANNEL"),
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return fmt.Sprintf("posted to %s (status %d)", os.Getenv("NOTIFY_CHANNEL"), resp.StatusCode), nil
}

// textResult is one successful tools/call result.
func textResult(text string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

// errorResult reports a failed call to the model as an error result.
func errorResult(err error) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": err.Error()}},
		"isError": true,
	}
}
