// Package journal records the steps of an agent turn so a replacement session
// container can continue after the original one dies. An entry is written only
// after a step has produced its output and before that output is committed to
// the conversation log, which makes the journal the source of truth for replay.
package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Entry is one completed step of a turn.
type Entry struct {
	SessionID string          `json:"session_id"`
	Turn      int64           `json:"turn"` // inbox event Seq that started the turn
	Step      int             `json:"step"` // 0-based, fixed order within the turn
	Name      string          `json:"name"` // user, llm, tool:<name>, assistant
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Err       string          `json:"err,omitempty"`
	ConvLen   int64           `json:"conv_len"` // conversation.jsonl size after applying this step
	Time      time.Time       `json:"time"`
}

// StepEvent is the thread event a step produced. It is stored inside an
// entry's output so control-plane recovery can rebuild the visible thread
// without executing the step.
type StepEvent struct {
	Type  string         `json:"type"`
	Actor string         `json:"actor,omitempty"`
	Text  string         `json:"text,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// Store persists turn steps. Append is idempotent for the same session, turn,
// and step; the last write wins. Turn returns entries ordered by step. A turn
// with no entries returns an empty slice and no error. LastTurn returns the
// turn of the most recently appended entry, or zero when the session has no
// entries.
type Store interface {
	Append(ctx context.Context, e Entry) error
	Turn(ctx context.Context, session string, turn int64) ([]Entry, error)
	LastTurn(ctx context.Context, session string) (int64, error)
}

// Sequencer reserves the hub sequence numbers a session may use. Event
// sequences are assigned in memory and restart at zero with the process, so
// the reserved floor is the only durable record of the keys a previous
// control-plane lifetime handed out. A turn key must never be reused.
type Sequencer interface {
	// HighWater reports the highest reserved sequence, or zero.
	HighWater(ctx context.Context, session string) (int64, error)
	// Reserve raises the reserved floor to at least floor. It never lowers it.
	Reserve(ctx context.Context, session string, floor int64) error
}

// Durable is the control plane's journal. The runtime only needs Store.
type Durable interface {
	Store
	Sequencer
	// MaxTurn reports the highest turn with a journaled entry, or zero.
	// Turn keys are reserved before use, so it is a safe seed floor.
	MaxTurn(ctx context.Context, session string) (int64, error)
	// Delete removes every step and reservation for a session. Finalizing a
	// session deletes its journal: the artifact bundle is the durable history.
	Delete(ctx context.Context, session string) error
}

// ErrInvalid reports an entry that cannot be journaled.
var ErrInvalid = errors.New("invalid journal entry")

func validate(e Entry) error {
	if e.SessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalid)
	}
	if e.Turn <= 0 {
		return fmt.Errorf("%w: turn must be positive", ErrInvalid)
	}
	if e.Step < 0 {
		return fmt.Errorf("%w: step must not be negative", ErrInvalid)
	}
	if e.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if e.ConvLen < 0 {
		return fmt.Errorf("%w: conversation length must not be negative", ErrInvalid)
	}
	if len(e.Input) > 0 && !json.Valid(e.Input) {
		return fmt.Errorf("%w: input is not valid JSON", ErrInvalid)
	}
	if len(e.Output) > 0 && !json.Valid(e.Output) {
		return fmt.Errorf("%w: output is not valid JSON", ErrInvalid)
	}
	return nil
}

func cloneRaw(in json.RawMessage) json.RawMessage {
	if in == nil {
		return nil
	}
	return append(json.RawMessage(nil), in...)
}

func cloneEntry(e Entry) Entry {
	e.Input = cloneRaw(e.Input)
	e.Output = cloneRaw(e.Output)
	return e
}
