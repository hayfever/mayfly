package agent

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// tracer writes structured JSONL traces for requests, responses, and tool calls.
type tracer struct {
	mu   sync.Mutex
	file *os.File
}

func openTracer(path string) (*tracer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	return &tracer{file: f}, nil
}

func (t *tracer) Write(kind string, fields map[string]any) {
	entry := map[string]any{
		"time": time.Now().UTC(),
		"type": kind,
	}
	for k, v := range fields {
		entry[k] = v
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = t.file.Write(append(line, '\n'))
}

func (t *tracer) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.file.Close()
}
