package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hayden/mayfly/internal/model"
	"github.com/hayden/mayfly/internal/platformclient"
)

// emitter posts thread events to the control plane and writes a local copy.
// Streaming deltas are not written to the local log because the final message
// is already recorded in conversation.jsonl.
type emitter struct {
	client *platformclient.Client
	mu     sync.Mutex
	log    *os.File
	closed bool
	logger *slog.Logger
}

func newEmitter(client *platformclient.Client, path string, logger *slog.Logger) (*emitter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	return &emitter{client: client, log: f, logger: logger}, nil
}

func (e *emitter) Emit(ctx context.Context, typ model.EventType, actor, text string, data map[string]any) {
	ev := model.Event{
		Time:  time.Now().UTC(),
		Type:  typ,
		Actor: actor,
		Text:  text,
		Data:  data,
	}
	e.mu.Lock()
	if !e.closed && e.log != nil && typ != model.EventAssistantDelta {
		if line, err := json.Marshal(ev); err == nil {
			_, _ = e.log.Write(append(line, '\n'))
		}
	}
	e.mu.Unlock()

	if e.client == nil {
		return
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := e.client.Emit(sendCtx, ev); err != nil {
		e.logger.Warn("emit event failed", "type", typ, "error", err)
	}
}

func (e *emitter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.log == nil {
		return nil
	}
	return e.log.Close()
}
