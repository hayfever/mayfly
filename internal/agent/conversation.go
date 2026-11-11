package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/hayden/mayfly/internal/llm"
)

// conversation stores the full model-visible message history and persists it
// as JSON lines.
type conversation struct {
	mu       sync.Mutex
	file     *os.File
	messages []llm.Message
}

func openConversation(path string) (*conversation, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open conversation log: %w", err)
	}
	return &conversation{file: f}, nil
}

// Append adds a message and writes it to disk.
func (c *conversation) Append(m llm.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, m)
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := c.file.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// MessagesForModel returns the system prompt plus a trimmed history. The cut
// point always lands on a user message so tool call/result pairs stay intact.
func (c *conversation) MessagesForModel(system string, max int) []llm.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	history := c.messages
	if max > 0 && len(history) > max {
		start := len(history) - max
		for start < len(history) && history[start].Role != "user" {
			start++
		}
		if start >= len(history) {
			start = len(history) - max
		}
		history = history[start:]
	}
	out := make([]llm.Message, 0, len(history)+1)
	out = append(out, llm.Message{Role: "system", Content: system})
	out = append(out, history...)
	return out
}

// Count returns the number of stored messages.
func (c *conversation) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

func (c *conversation) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.file.Close()
}
