package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/llm"
	"github.com/hayfever/mayfly/internal/model"
)

// Journal payloads. Only data needed to apply a step or rebuild its thread
// events on recovery is stored; the conversation itself stays in
// conversation.jsonl.
type userStepOutput struct {
	Message llm.Message         `json:"message"`
	Events  []journal.StepEvent `json:"events,omitempty"`
}

type modelStepInput struct {
	Model    string `json:"model"`
	Messages int    `json:"messages"`
	Tools    int    `json:"tools"`
}

type modelStepOutput struct {
	Message      llm.Message         `json:"message"`
	FinishReason string              `json:"finish_reason,omitempty"`
	Events       []journal.StepEvent `json:"events,omitempty"`
}

type toolStepInput struct {
	Tool      string `json:"tool"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
}

type toolStepOutput struct {
	Message  llm.Message         `json:"message"`
	IsError  bool                `json:"is_error,omitempty"`
	Executed bool                `json:"executed"`
	Events   []journal.StepEvent `json:"events,omitempty"`
}

// fatalError marks a failure the runtime cannot recover from in place. The
// process exits non-zero so the control plane replaces the container, and the
// replacement resumes from the journal.
type fatalError struct{ err error }

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

// fatal wraps err unless it is already fatal.
func fatal(err error) error {
	if err == nil {
		return nil
	}
	var existing fatalError
	if errors.As(err, &existing) {
		return err
	}
	return fatalError{err: err}
}

func isFatal(err error) bool {
	var e fatalError
	return errors.As(err, &e)
}

// stepRun is the result of executing a step. The runtime serializes it before
// touching the conversation log.
type stepRun struct {
	name    string
	message llm.Message
	output  json.RawMessage
	errMsg  string
}

// entryAt returns the journal entry at step when the recorded turn has one.
func entryAt(entries []journal.Entry, step int) *journal.Entry {
	if step < 0 || step >= len(entries) {
		return nil
	}
	if entries[step].Step != step {
		return nil
	}
	return &entries[step]
}

// conversationLineLen is the exact number of bytes Append will add for m.
func conversationLineLen(m llm.Message) int64 {
	line, _ := json.Marshal(m)
	return int64(len(line) + 1)
}

// runOrReplay executes a step unless the journal already holds it. A recorded
// step is applied from its recorded output and emits no events. A missing step
// runs, is appended to the journal, and is then applied. A crash between the
// append and the apply is repaired by the apply check on the next start.
func (r *Runtime) runOrReplay(
	ctx context.Context,
	turn int64,
	step int,
	input json.RawMessage,
	existing *journal.Entry,
	exec func() (stepRun, error),
	apply func(json.RawMessage) error,
) (journal.Entry, bool, error) {
	if existing != nil {
		// A recorded step must have been produced from the same input. Step
		// zero is compared by its event in handleTurn; later steps are
		// deterministic from the previous step's output.
		if step > 0 && !sameStepInput(existing.Name, existing.Input, input) {
			return journal.Entry{}, true, fatal(fmt.Errorf("journal step %d (%s) does not match the replay input", step, existing.Name))
		}
		if err := r.applyExisting(*existing, apply); err != nil {
			return journal.Entry{}, true, fatal(err)
		}
		// The crashed process may have applied the step and stopped before
		// logging it. The log skips events it already holds for this step.
		if r.events != nil {
			r.events.RecordLocal(fmt.Sprintf("%d/%d", turn, step), localStepEvents(existing.Output))
		}
		return *existing, true, nil
	}
	run, err := exec()
	if err != nil {
		return journal.Entry{}, false, err
	}
	convLen := r.conv.Size() + conversationLineLen(run.message)
	entry := journal.Entry{
		SessionID: r.cfg.SessionID,
		Turn:      turn,
		Step:      step,
		Name:      run.name,
		Input:     input,
		Output:    run.output,
		Err:       run.errMsg,
		ConvLen:   convLen,
		Time:      time.Now().UTC(),
	}
	if err := r.journal.Append(ctx, entry); err != nil {
		return journal.Entry{}, false, fatal(fmt.Errorf("journal step %d (%s): %w", step, run.name, err))
	}
	if err := apply(run.output); err != nil {
		return entry, false, fatal(fmt.Errorf("apply journal step %d (%s): %w", step, run.name, err))
	}
	if got := r.conv.Size(); got != convLen {
		return entry, false, fatal(fmt.Errorf("conversation length after step %d (%s) is %d, want %d", step, run.name, got, convLen))
	}
	// The control plane publishes these from the journal entry. The local log
	// is the bundle's durable thread, so it records them once the step is
	// durable too.
	if r.events != nil {
		r.events.RecordLocal(fmt.Sprintf("%d/%d", turn, step), localStepEvents(run.output))
	}
	return entry, false, nil
}

// localStepEvents returns the thread events of a step output that belong in
// the local thread log. A tool call is logged when the runtime emits it, so
// only the reply and the result are added here.
func localStepEvents(output json.RawMessage) []journal.StepEvent {
	var payload struct {
		Events []journal.StepEvent `json:"events"`
	}
	if len(output) == 0 || json.Unmarshal(output, &payload) != nil {
		return nil
	}
	out := make([]journal.StepEvent, 0, len(payload.Events))
	for _, ev := range payload.Events {
		switch model.EventType(ev.Type) {
		case model.EventAssistantMessage, model.EventToolResult:
			out = append(out, ev)
		}
	}
	return out
}

// sameStepInput reports whether a recorded step was produced from the same
// input as this replay. A model step records the message count it saw, which
// necessarily differs once later steps of the same turn have been applied, so
// only its stable fields are compared.
func sameStepInput(name string, recorded, current json.RawMessage) bool {
	switch {
	case len(recorded) == 0 || len(current) == 0:
		return true
	case strings.HasPrefix(name, "tool:"):
		var a, b toolStepInput
		if json.Unmarshal(recorded, &a) != nil || json.Unmarshal(current, &b) != nil {
			return false
		}
		return a == b
	case name == "llm" || name == "assistant":
		var a, b modelStepInput
		if json.Unmarshal(recorded, &a) != nil || json.Unmarshal(current, &b) != nil {
			return false
		}
		return a.Model == b.Model && a.Tools == b.Tools
	default:
		return bytes.Equal(recorded, current)
	}
}

// applyExisting applies a recorded step only when the conversation does not
// already contain it. On resume the log is truncated to the last entry's
// ConvLen, so an entry whose effect was lost has a size below its ConvLen.
func (r *Runtime) applyExisting(entry journal.Entry, apply func(json.RawMessage) error) error {
	// A conversation longer than this entry's length already contains the
	// step, because resume truncates to the last entry and replays forward.
	// A shorter one must be repaired exactly: any other result is a partial
	// write that the journal cannot describe.
	if r.conv.Size() >= entry.ConvLen {
		return nil
	}
	if err := apply(entry.Output); err != nil {
		return fmt.Errorf("apply journal step %d (%s): %w", entry.Step, entry.Name, err)
	}
	if got := r.conv.Size(); got != entry.ConvLen {
		return fmt.Errorf("replayed conversation length after step %d (%s) is %d, want %d", entry.Step, entry.Name, got, entry.ConvLen)
	}
	return nil
}

func (r *Runtime) applyUser(output json.RawMessage) error {
	var out userStepOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return err
	}
	return r.conv.Append(out.Message)
}

func (r *Runtime) applyModel(output json.RawMessage) error {
	var out modelStepOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return err
	}
	return r.conv.Append(out.Message)
}

func (r *Runtime) applyTool(output json.RawMessage) error {
	var out toolStepOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return err
	}
	return r.conv.Append(out.Message)
}

// stepEventKey names an event belonging to a journaled step. The control
// plane deduplicates on it, so re-executing a step that never reached the
// journal cannot post the same thread message twice.
func stepEventKey(turn int64, step int, kind string) string {
	return fmt.Sprintf("%d/%d/%s", turn, step, kind)
}

// userMessage builds the model-visible user message for an inbox event.
func userMessage(ev model.Event) llm.Message {
	text := ev.Text
	if ev.Actor != "" && ev.Actor != "trigger" {
		text = "[" + ev.Actor + "] " + text
	}
	return llm.Message{Role: "user", Content: text}
}

// modelStepName reports the journal name for a model response.
func modelStepName(m llm.Message) string {
	if len(m.ToolCalls) == 0 {
		return "assistant"
	}
	return "llm"
}
