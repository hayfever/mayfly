package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hayfever/mayfly/internal/model"
)

// Pending is a message the control plane accepted but the runtime has not
// started. It is written before the 202 response and removed when the turn's
// step 0 is journaled, or when the session ends. Adoption reads it back and
// redelivers the event with its original sequence, so the turn key and the
// runtime's identity check stay stable across a control-plane restart.
type Pending struct {
	SessionID string      `json:"session_id"`
	Seq       int64       `json:"seq"` // hub sequence, which is the turn key
	Event     model.Event `json:"event"`
}

// PendingStore keeps the messages accepted before their turn starts. The
// runtime never reads it; only the control plane does, during PostMessage,
// Create, and adoption.
type PendingStore interface {
	// AddPending records one accepted message. Writing the same session and
	// sequence again replaces the record, so a retried write is safe.
	AddPending(ctx context.Context, p Pending) error
	// Pending returns a session's unstarted messages in sequence order.
	Pending(ctx context.Context, session string) ([]Pending, error)
	// DeletePending removes one record. A missing record is not an error.
	DeletePending(ctx context.Context, session string, seq int64) error
}

// ErrPendingInvalid reports a message that cannot be made durable.
var ErrPendingInvalid = errors.New("invalid pending inbox record")

func validatePending(p Pending) error {
	if p.SessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrPendingInvalid)
	}
	if p.Seq <= 0 {
		return fmt.Errorf("%w: sequence must be positive", ErrPendingInvalid)
	}
	if p.Event.Type != model.EventUserMessage {
		return fmt.Errorf("%w: event type %q is not a user message", ErrPendingInvalid, p.Event.Type)
	}
	if p.Event.Seq != p.Seq {
		return fmt.Errorf("%w: event sequence %d does not match record sequence %d", ErrPendingInvalid, p.Event.Seq, p.Seq)
	}
	if p.Event.Text == "" {
		return fmt.Errorf("%w: message text is required", ErrPendingInvalid)
	}
	if p.Event.Time.IsZero() {
		return fmt.Errorf("%w: message time is required", ErrPendingInvalid)
	}
	return nil
}

// clonePending copies the record so a caller cannot change stored state.
func clonePending(p Pending) Pending {
	p.Event = cloneModelEvent(p.Event)
	return p
}

func cloneModelEvent(ev model.Event) model.Event {
	// A user message carries only strings, so a struct copy is enough. The
	// other payload types are handled so the helper stays correct if a caller
	// stores one.
	if d, ok := ev.Data.(model.SystemData); ok {
		d.Skills = append([]string(nil), d.Skills...)
		d.MCPServers = append([]string(nil), d.MCPServers...)
		d.Plugins = append([]string(nil), d.Plugins...)
		ev.Data = d
	}
	return ev
}

func encodePending(p Pending) ([]byte, error) {
	if err := validatePending(p); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

func decodePending(raw []byte) (Pending, error) {
	var p Pending
	if err := json.Unmarshal(raw, &p); err != nil {
		return Pending{}, fmt.Errorf("%w: %w", ErrPendingInvalid, err)
	}
	if err := validatePending(p); err != nil {
		return Pending{}, err
	}
	return p, nil
}
