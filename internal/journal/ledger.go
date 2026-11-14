package journal

import (
	"context"
	"errors"
	"fmt"
	"time"
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
	InstanceID  string    `json:"instance_id,omitempty"`
	State       CallState `json:"state"`
	Content     string    `json:"content,omitempty"`
	IsError     bool      `json:"is_error,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
	Attempts    int       `json:"attempts"`
	StartedAt   time.Time `json:"started_at"`
	DoneAt      time.Time `json:"done_at,omitempty"`
}

// ErrCallNotFound reports a ledger key the session never began.
var ErrCallNotFound = errors.New("tool call not found")

// ErrCallConflict reports a completed record whose content differs from the
// content being written again.
var ErrCallConflict = errors.New("tool call completed with different content")

// Ledger records each authorized tool call so a replacement runtime can replay
// a completed result instead of running the tool twice. A record moves from
// pending to done once; the journal and the ledger are deleted together.
type Ledger interface {
	// Begin returns the existing record for c.Key, or stores c as pending.
	// created reports whether c was stored. Begin on a pending record
	// increments Attempts.
	Begin(ctx context.Context, c Call) (rec Call, created bool, err error)
	// Complete marks the record done. Repeating it with the same content is
	// a no-op; different content is an error.
	Complete(ctx context.Context, session, key, content string, isError, interrupted bool) (Call, error)
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
