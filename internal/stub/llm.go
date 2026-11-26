// Package stub provides deterministic LLM and MCP servers for development and
// tests. They are never used by the production data path.
package stub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// toolDirectiveRE matches one scripted call in a prompt:
//
//	tool: plugin_inline_echo {"text":"hi"}
//
// The e2e suite needs deterministic calls to plugin tools whose names and
// arguments it chooses, which the automatic MCP flow cannot express.
var toolDirectiveRE = regexp.MustCompile(`(?m)^[ \t]*tool:[ \t]*(\S+)[ \t]*(.*)$`)

// noUsageDirectiveRE matches a prompt that asks the stub to report no usage:
//
//	no-usage
//
// A provider that omits usage exercises the budget's estimate path.
var noUsageDirectiveRE = regexp.MustCompile(`(?m)^[ \t]*no-usage[ \t]*$`)

// directive is one scripted tool call from the first user message.
type directive struct {
	name string
	args string
}

// parseDirectives reads the scripted calls of a prompt in order.
func parseDirectives(prompt string) []directive {
	matches := toolDirectiveRE.FindAllStringSubmatch(prompt, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]directive, 0, len(matches))
	for _, match := range matches {
		args := strings.TrimSpace(match[2])
		if args == "" {
			args = "{}"
		}
		out = append(out, directive{name: match[1], args: args})
	}
	return out
}

type llmFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type llmToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function llmFunctionCall `json:"function"`
}

// llmContent accepts either a string or an array of content parts, as a
// multimodal request carries. It keeps only the text.
type llmContent struct {
	text string
}

func (c *llmContent) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		c.text = text
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return err
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	c.text = b.String()
	return nil
}

func (c llmContent) MarshalJSON() ([]byte, error) { return json.Marshal(c.text) }

type llmMessage struct {
	Role       string        `json:"role"`
	Content    llmContent    `json:"content"`
	ToolCalls  []llmToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type llmRequest struct {
	Model    string       `json:"model"`
	Messages []llmMessage `json:"messages"`
	Tools    []struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Stream        bool `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// llmUsage is the OpenAI usage object. The stub's counts are a stable function
// of the request text, so tests can assert them.
type llmUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// stubTokenCount is one token per four bytes of text, minimum one. It
// approximates a tokenizer closely enough to exercise the budget path and is
// deterministic for the same input.
func stubTokenCount(parts []string) int {
	n := 0
	for _, part := range parts {
		n += len(part)
	}
	return n/4 + 1
}

// usageFor counts the request text and the reply the stub is about to send.
func usageFor(req llmRequest, content string, calls []llmToolCall) llmUsage {
	parts := make([]string, 0, len(req.Messages)*3+len(req.Tools)*2)
	for _, msg := range req.Messages {
		parts = append(parts, msg.Role, msg.Content.text, msg.ToolCallID)
		for _, call := range msg.ToolCalls {
			parts = append(parts, call.ID, call.Function.Name, call.Function.Arguments)
		}
	}
	for _, tool := range req.Tools {
		parts = append(parts, tool.Type, tool.Function.Name)
	}
	prompt := stubTokenCount(parts)
	parts = parts[:0]
	parts = append(parts, content)
	for _, call := range calls {
		parts = append(parts, call.ID, call.Function.Name, call.Function.Arguments)
	}
	completion := stubTokenCount(parts)
	return llmUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}
}

// noUsageRequested reports whether any message asks the stub to omit usage.
func noUsageRequested(req llmRequest) bool {
	for _, msg := range req.Messages {
		if noUsageDirectiveRE.MatchString(msg.Content.text) {
			return true
		}
	}
	return false
}

// LLMHandler returns a deterministic OpenAI-compatible server. Its sequence is:
// write report.md, call the first available MCP tool, then reply.
func LLMHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /v1/chat/completions", handleLLM)
	return mux
}

func handleLLM(w http.ResponseWriter, r *http.Request) {
	var req llmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	toolNames := map[string]bool{}
	for _, tool := range req.Tools {
		toolNames[tool.Function.Name] = true
	}
	callNames := map[string]string{}
	executed := map[string]bool{}
	firstUser := ""
	for _, msg := range req.Messages {
		if msg.Role == "user" && firstUser == "" {
			firstUser = msg.Content.text
		}
		for _, call := range msg.ToolCalls {
			callNames[call.ID] = call.Function.Name
		}
		if msg.Role == "tool" {
			if name, ok := callNames[msg.ToolCallID]; ok {
				executed[name] = true
			}
		}
	}

	var calls []llmToolCall
	content := ""
	finish := "stop"
	mcpTool := ""
	for _, tool := range req.Tools {
		name := tool.Function.Name
		if strings.HasPrefix(name, "mcp_") && !executed[name] {
			mcpTool = name
			break
		}
	}
	directives := parseDirectives(firstUser)
	answered := 0
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			answered++
		}
	}
	switch {
	case len(directives) > 0 && answered < len(directives):
		next := directives[answered]
		calls = []llmToolCall{{
			ID:       fmt.Sprintf("call_stub_plugin_%d", answered),
			Type:     "function",
			Function: llmFunctionCall{Name: next.name, Arguments: next.args},
		}}
		finish = "tool_calls"
	case len(directives) > 0:
		content = "Completed. " + summarize(firstUser)
	case mcpTool != "":
		args, _ := json.Marshal(map[string]string{"text": "hello from mayfly"})
		calls = []llmToolCall{{
			ID:       "call_stub_mcp",
			Type:     "function",
			Function: llmFunctionCall{Name: mcpTool, Arguments: string(args)},
		}}
		finish = "tool_calls"
	case toolNames["write_file"] && !executed["write_file"]:
		body, _ := json.Marshal(map[string]string{
			"path":    "report.md",
			"content": "# Mayfly session report\n\nTask: " + strings.TrimSpace(firstUser) + "\n\nGenerated by llmstub.\n",
		})
		calls = []llmToolCall{{
			ID:       "call_stub_write",
			Type:     "function",
			Function: llmFunctionCall{Name: "write_file", Arguments: string(body)},
		}}
		finish = "tool_calls"
	default:
		// The resume e2e test kills the container while this reply is in
		// flight; other scenarios get an immediate answer.
		if strings.Contains(strings.ToLower(firstUser), "resume-slow-final") {
			time.Sleep(5 * time.Second)
		}
		content = "Completed. " + summarize(firstUser)
	}

	usage := usageFor(req, content, calls)
	if noUsageRequested(req) {
		usage = llmUsage{}
	}
	if req.Stream {
		var streamUsage *llmUsage
		if usage.TotalTokens > 0 && req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
			streamUsage = &usage
		}
		writeLLMStream(w, req.Model, calls, content, finish, streamUsage)
		return
	}
	msg := llmMessage{Role: "assistant", Content: llmContent{text: content}, ToolCalls: calls}
	body := map[string]any{
		"id":      "chatcmpl-stub",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if usage.TotalTokens > 0 {
		body["usage"] = usage
	}
	writeStubJSON(w, body)
}

func summarize(prompt string) string {
	prompt = strings.TrimSpace(strings.ReplaceAll(prompt, "\n", " "))
	if prompt == "" {
		return "No task was provided."
	}
	if len(prompt) > 200 {
		prompt = prompt[:200] + "…"
	}
	return "Handled task: " + prompt
}

func writeLLMStream(w http.ResponseWriter, model string, calls []llmToolCall, content, finish string, usage *llmUsage) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(payload any) {
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	base := map[string]any{
		"id":      "chatcmpl-stub",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
	}
	delta := map[string]any{"role": "assistant"}
	if len(calls) > 0 {
		delta["tool_calls"] = []map[string]any{{
			"index": 0,
			"id":    calls[0].ID,
			"type":  "function",
			"function": map[string]string{
				"name":      calls[0].Function.Name,
				"arguments": calls[0].Function.Arguments,
			},
		}}
	} else {
		delta["content"] = content
	}
	chunk := cloneMap(base)
	chunk["choices"] = []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}}
	send(chunk)
	final := cloneMap(base)
	final["choices"] = []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}
	send(final)
	if usage != nil {
		// OpenAI sends a usage-only chunk with no choices after the finish
		// chunk. Clients that ignore empty choices skip it.
		last := cloneMap(base)
		last["choices"] = []map[string]any{}
		last["usage"] = usage
		send(last)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func writeStubJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
