package journal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hayfever/mayfly/internal/content"
)

// CallState is where a tool call stands in the ledger.
type CallState string

const (
	// CallPending means the gateway authorized the call and has no result.
	CallPending CallState = "pending"
	// CallDone means the result is recorded and a resume replays it.
	CallDone CallState = "done"
)

// Call is one tool call the gateway authorized, keyed by the idempotency key
// the runtime derives from the turn and step. Arguments and results are not
// journaled until the call completes, and only the result is stored.
type Call struct {
	Key        string `json:"key"` // <session>/<turn>/<step>
	SessionID  string `json:"session_id"`
	Turn       int64  `json:"turn"`
	Step       int    `json:"step"`
	Tool       string `json:"tool"` // model-visible name
	ArgsSHA256 string `json:"args_sha256"`
	// InstanceID names the runtime process that opened the call. A pending
	// record with the same instance means the answer to the begin was lost,
	// so the call never ran. A different instance means a restart.
	InstanceID string    `json:"instance_id,omitempty"`
	State      CallState `json:"state"`
	Content    string    `json:"content,omitempty"`
	// Parts holds structured output, including image data. It is the replay
	// authority for a result the runtime must apply again.
	Parts       []content.Part `json:"parts,omitempty"`
	IsError     bool           `json:"is_error,omitempty"`
	Interrupted bool           `json:"interrupted,omitempty"`
	Attempts    int            `json:"attempts"`
	StartedAt   time.Time      `json:"started_at"`
	DoneAt      time.Time      `json:"done_at,omitempty"`
}

// ErrCallNotFound reports a ledger key the session never began.
var ErrCallNotFound = errors.New("tool call not found")

// ErrCallConflict reports a completed record whose result differs from the
// result being written again.
var ErrCallConflict = errors.New("tool call completed with different content")

// partsEqual reports whether two part lists are identical.
func partsEqual(a, b []content.Part) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Text != b[i].Text || a[i].MIMEType != b[i].MIMEType ||
			!bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

// cloneParts returns a deep copy so a stored record cannot change under a
// caller.
func cloneParts(parts []content.Part) []content.Part {
	if parts == nil {
		return nil
	}
	out := make([]content.Part, len(parts))
	for i, p := range parts {
		p.Data = bytes.Clone(p.Data)
		out[i] = p
	}
	return out
}

// Result is the output recorded for one completed call: the text a log and
// the model see, plus any image parts.
type Result struct {
	Content     string
	Parts       []content.Part
	IsError     bool
	Interrupted bool
}

// Ledger records each authorized tool call so a replacement runtime can replay
// a completed result instead of running the tool twice. A record moves from
// pending to done once; the journal and the ledger are deleted together.
type Ledger interface {
	// Begin returns the existing record for c.Key, or stores c as pending.
	// created reports whether c was stored. Begin on a pending record
	// increments Attempts.
	Begin(ctx context.Context, c Call) (rec Call, created bool, err error)
	// Complete marks the record done. Repeating it with the same result is
	// a no-op; a different result is an error.
	Complete(ctx context.Context, session, key string, result Result) (Call, error)
	// Get returns the record for key, or false when the session has none.
	Get(ctx context.Context, session, key string) (Call, bool, error)
}

// validateCall rejects a record that cannot name the call it describes.
func validateCall(c Call) error {
	if c.SessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalid)
	}
	if c.Key == "" {
		return fmt.Errorf("%w: call key is required", ErrInvalid)
	}
	if c.Turn <= 0 {
		return fmt.Errorf("%w: call turn must be positive", ErrInvalid)
	}
	if c.Step <= 0 {
		return fmt.Errorf("%w: call step must be positive", ErrInvalid)
	}
	if c.Tool == "" {
		return fmt.Errorf("%w: call tool is required", ErrInvalid)
	}
	if c.ArgsSHA256 == "" {
		return fmt.Errorf("%w: call argument hash is required", ErrInvalid)
	}
	if c.State != "" && c.State != CallPending {
		return fmt.Errorf("%w: a new call must be pending, got %q", ErrInvalid, c.State)
	}
	return nil
}
