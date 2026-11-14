package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/hayfever/mayfly/internal/llm"
)

// conversation stores the full model-visible message history and persists it
// as JSON lines.
type conversation struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	messages []llm.Message
	size     int64
}

func openConversation(path string) (*conversation, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open conversation log: %w", err)
	}
	c := &conversation{path: path, file: f}
	if err := c.reloadLocked(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return c, nil
}

// reloadLocked rebuilds the message list from the file. A torn final line is
// removed so the next append starts on a line boundary. A complete line that
// does not decode marks the end of usable history; everything from there is
// removed rather than silently skipped.
func (c *conversation) reloadLocked() error {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return fmt.Errorf("read conversation log: %w", err)
	}
	messages := make([]llm.Message, 0, bytes.Count(data, []byte{'\n'})+1)
	offset := 0
	for offset < len(data) {
		idx := bytes.IndexByte(data[offset:], '\n')
		if idx < 0 {
			break
		}
		line := data[offset : offset+idx]
		var m llm.Message
		if len(bytes.TrimSpace(line)) > 0 {
			if err := json.Unmarshal(line, &m); err != nil {
				break
			}
			messages = append(messages, m)
		}
		offset += idx + 1
	}
	if offset != len(data) {
		if err := c.file.Truncate(int64(offset)); err != nil {
			return fmt.Errorf("trim conversation log: %w", err)
		}
	}
	c.messages = messages
	c.size = int64(offset)
	return nil
}

// Append adds a message and writes it to disk.
func (c *conversation) Append(m llm.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := c.file.Write(line); err != nil {
		return err
	}
	c.messages = append(c.messages, m)
	c.size += int64(len(line))
	return nil
}

// Size returns the current log size in bytes.
func (c *conversation) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

// Truncate cuts the log to n bytes and reloads the messages it still holds.
// It never extends the file.
func (c *conversation) Truncate(n int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 0 {
		return fmt.Errorf("conversation truncate %d: negative size", n)
	}
	if n > c.size {
		return fmt.Errorf("conversation truncate %d: file is %d bytes", n, c.size)
	}
	if err := c.file.Truncate(n); err != nil {
		return fmt.Errorf("truncate conversation log: %w", err)
	}
	return c.reloadLocked()
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
