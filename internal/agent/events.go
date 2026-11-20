package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/platformclient"
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
	// recorded holds the step-keyed events already in the log, including
	// those a previous process wrote, so a replay can record a step's
	// events without knowing whether the crashed process got to them.
	recorded map[string]struct{}
}

func newEmitter(client *platformclient.Client, path string, logger *slog.Logger) (*emitter, error) {
	recorded, err := loggedStepEvents(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	return &emitter{client: client, log: f, logger: logger, recorded: recorded}, nil
}

// loggedStepEvents reads the step-keyed events of an existing local log. A
// torn last line from a crash ends the scan; at worst an event is logged
// twice, never dropped.
func loggedStepEvents(path string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	for {
		var ev model.Event
		if dec.Decode(&ev) != nil {
			return out, nil
		}
		if ev.StepKey != "" {
			out[recordKey(ev.StepKey, string(ev.Type))] = struct{}{}
		}
	}
}

func recordKey(stepKey, typ string) string { return stepKey + " " + typ }

func (e *emitter) Emit(ctx context.Context, typ model.EventType, actor, text string, data model.EventData) {
	e.emit(ctx, "", typ, actor, text, data)
}

// EmitStep emits an event that belongs to one journaled step. The control
// plane drops a repeated StepKey so re-running a step that never reached the
// journal does not duplicate its thread message.
func (e *emitter) EmitStep(ctx context.Context, stepKey string, typ model.EventType, actor, text string, data model.EventData) {
	e.emit(ctx, stepKey, typ, actor, text, data)
}

// EmitRecord emits one recorded step event.
func (e *emitter) EmitRecord(ctx context.Context, stepKey string, ev journal.StepEvent) {
	e.emit(ctx, stepKey, model.EventType(ev.Type), ev.Actor, ev.Text, ev.Data)
}

// RecordLocal writes journaled step events to the local thread log without
// posting them. The control plane publishes them from the journal entry. An
// event the log already holds for the same step is skipped, so replaying a
// step records it at most once.
func (e *emitter) RecordLocal(stepKey string, evs []journal.StepEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.log == nil {
		return
	}
	for _, se := range evs {
		key := recordKey(stepKey, se.Type)
		if _, ok := e.recorded[key]; ok {
			continue
		}
		if e.recorded == nil {
			e.recorded = map[string]struct{}{}
		}
		e.recorded[key] = struct{}{}
		ev := model.Event{
			Time:    time.Now().UTC(),
			StepKey: stepKey,
			Type:    model.EventType(se.Type),
			Actor:   se.Actor,
			Text:    se.Text,
			Data:    se.Data,
		}
		if line, err := json.Marshal(ev); err == nil {
			_, _ = e.log.Write(append(line, '\n'))
		}
	}
}

func (e *emitter) emit(ctx context.Context, stepKey string, typ model.EventType, actor, text string, data model.EventData) {
	ev := model.Event{
		Time:    time.Now().UTC(),
		StepKey: stepKey,
		Type:    typ,
		Actor:   actor,
		Text:    text,
		Data:    data,
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
