// Package llm is a small OpenAI-compatible chat client with tool support and
// streaming. It talks to the control plane's credential broker, never to the
// model provider directly.
package llm

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

	"github.com/hayfever/mayfly/internal/content"
)

// Message is one chat message. Content is the text-only form. Parts holds
// structured tool output, including images; a message with Parts sends the
// parts array to the provider and ignores Content. Content stays the text a
// log, event, or text-only reader sees.
type Message struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	Parts      []content.Part `json:"parts,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// ToolCall is an assistant-requested tool invocation.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the function name and JSON arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is an OpenAI tool declaration.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef is one callable function.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// NewTool builds a function tool with a JSON schema.
func NewTool(name, description string, schema json.RawMessage) Tool {
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return Tool{Type: "function", Function: FunctionDef{
		Name:        name,
		Description: description,
		Parameters:  schema,
	}}
}

// Client calls the broker's chat completions endpoint.
type Client struct {
	baseURL string
	token   string
	model   string
	client  *http.Client
}

// New creates a client. baseURL is the broker root, for example
// http://mayfly:8080/internal/v1/llm.
func New(baseURL, token, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		model:   model,
		client:  &http.Client{},
	}
}

// StatusError reports a non-200 upstream response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("llm status %d: %s", e.Code, e.Body)
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []Tool        `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
}

// wireMessage is the provider's message shape. Content is a string for a
// text-only message and an array of parts when a tool result carries an
// image.
type wireMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// wirePart is one OpenAI chat-completions content part.
type wirePart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL string `json:"url"`
}

// providerMessage converts m to the provider shape. A message without image
// parts keeps a string content field, which is what every text-only provider
// already accepts. An image becomes an image_url part carrying a data URI.
func providerMessage(m Message) wireMessage {
	out := wireMessage{Role: m.Role, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	if !content.HasImage(m.Parts) {
		out.Content = m.Content
		if out.Content == "" {
			out.Content = nil
		}
		return out
	}
	parts := make([]wirePart, 0, len(m.Parts))
	for _, p := range m.Parts {
		if !p.IsImage() {
			parts = append(parts, wirePart{Type: "text", Text: p.Text})
			continue
		}
		mime, ok := content.NormalizeMIMEType(p.MIMEType)
		if !ok || len(p.Data) == 0 {
			// The runtime rejects such a part before it reaches a message.
			// A stale log must still produce a request the provider accepts.
			parts = append(parts, wirePart{Type: "text", Text: content.ImagePlaceholder(p, "invalid image part")})
			continue
		}
		parts = append(parts, wirePart{
			Type: "image_url",
			ImageURL: &wireImageURL{
				URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(p.Data),
			},
		})
	}
	out.Content = parts
	return out
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends the conversation and returns the assistant message.
// When onDelta is non-nil the response is streamed and onDelta receives text
// fragments as they arrive.
func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool, onDelta func(string)) (Message, string, error) {
	wire := make([]wireMessage, len(messages))
	for i, m := range messages {
		wire[i] = providerMessage(m)
	}
	body := chatRequest{
		Model:    c.model,
		Messages: wire,
		Tools:    tools,
		Stream:   onDelta != nil,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Message{}, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Message{}, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if onDelta != nil {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return Message{}, "", fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return Message{}, "", &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	if onDelta != nil {
		return c.readStream(resp.Body, onDelta)
	}
	return c.readJSON(resp.Body)
}

func (c *Client) readJSON(r io.Reader) (Message, string, error) {
	var out chatResponse
	if err := json.NewDecoder(io.LimitReader(r, 64<<20)).Decode(&out); err != nil {
		return Message{}, "", fmt.Errorf("decode llm response: %w", err)
	}
	if out.Error != nil {
		return Message{}, "", fmt.Errorf("llm error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return Message{}, "", fmt.Errorf("llm response has no choices")
	}
	return out.Choices[0].Message, out.Choices[0].FinishReason, nil
}

func (c *Client) readStream(r io.Reader, onDelta func(string)) (Message, string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var content strings.Builder
	calls := map[int]*ToolCall{}
	order := []int{}
	finish := ""

	handle := func(data string) error {
		if data == "[DONE]" {
			return io.EOF
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil // ignore malformed keepalive data
		}
		if chunk.Error != nil {
			return fmt.Errorf("llm stream error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			return nil
		}
		choice := chunk.Choices[0]
		if choice.Delta.Content != "" {
			content.WriteString(choice.Delta.Content)
			onDelta(choice.Delta.Content)
		}
		for _, tc := range choice.Delta.ToolCalls {
			call, ok := calls[tc.Index]
			if !ok {
				call = &ToolCall{Type: "function"}
				calls[tc.Index] = call
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				call.ID = tc.ID
			}
			if tc.Type != "" {
				call.Type = tc.Type
			}
			call.Function.Name += tc.Function.Name
			call.Function.Arguments += tc.Function.Arguments
		}
		if choice.FinishReason != "" {
			finish = choice.FinishReason
		}
		return nil
	}

	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			err := handle(strings.TrimSpace(data.String()))
			data.Reset()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return Message{}, "", err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			data.WriteString(v)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return Message{}, "", fmt.Errorf("read llm stream: %w", err)
	}
	msg := Message{Role: "assistant", Content: content.String()}
	for _, idx := range order {
		if call := calls[idx]; call.ID != "" || call.Function.Name != "" {
			msg.ToolCalls = append(msg.ToolCalls, *call)
		}
	}
	return msg, finish, nil
}
