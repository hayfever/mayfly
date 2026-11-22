package session

import (
	"context"
	"fmt"
	"time"

	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
)

// pendingTimeout bounds one pending-inbox read or write. A control-plane
// request waits on it, so it stays well inside an HTTP timeout.
const pendingTimeout = 10 * time.Second

// persistPending makes an accepted message durable before its sender is
// acknowledged. The event already carries the hub sequence that becomes its
// turn key, so a restart can redeliver it under the same key. A write failure
// fails the request: the caller must never see a 202 for a message the
// control plane cannot recover.
func (m *Manager) persistPending(inst *instance, ev model.Event) error {
	if m.journal == nil {
		return nil
	}
	if ev.Seq <= 0 {
		return fmt.Errorf("persist pending message: the event has no sequence")
	}
	ctx, cancel := context.WithTimeout(context.Background(), pendingTimeout)
	defer cancel()
	rec := journal.Pending{SessionID: inst.sess.ID, Seq: ev.Seq, Event: ev}
	if err := m.journal.AddPending(ctx, rec); err != nil {
		return fmt.Errorf("persist pending message: %w", err)
	}
	return nil
}

// redeliverPending returns the messages this session accepted but never
// started to the new hub's inbox. Adoption runs after the hub is seeded and
// the journaled thread is restored, so a runtime that reconnects sees them in
// sequence order. A record whose turn already has a journaled step 0 is
// dropped: the runtime is replaying it from the journal, and a second
// delivery would be unnecessary.
func (m *Manager) redeliverPending(inst *instance) {
	if m.journal == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pendingTimeout)
	defer cancel()
	records, err := m.journal.Pending(ctx, inst.sess.ID)
	if err != nil {
		m.logger.Warn("adopt: read pending messages", "session", inst.sess.ID, "error", err)
		return
	}
	limit := m.maxEventBytes()
	for _, rec := range records {
		entries, err := m.journal.Turn(ctx, inst.sess.ID, rec.Seq)
		if err != nil {
			m.logger.Warn("adopt: read pending turn", "session", inst.sess.ID, "turn", rec.Seq, "error", err)
			continue
		}
		if len(entries) > 0 {
			if err := m.journal.DeletePending(ctx, inst.sess.ID, rec.Seq); err != nil {
				m.logger.Warn("adopt: drop started pending message", "session", inst.sess.ID, "turn", rec.Seq, "error", err)
			}
			continue
		}
		ev := rec.Event
		ev.SessionID = ""
		if !m.hub.Redeliver(inst.sess.ID, ev) {
			continue
		}
		// The thread copy needs a sequence above the adoption seed. The step
		// key is the user-step key of the reserved turn, so a repeated
		// adoption pass cannot restore the same message twice.
		display := ev
		display.Seq = 0
		display.Time = rec.Event.Time
		display.StepKey = userStepKey(rec.Seq)
		display = boundRuntimeEvent(display, limit)
		if stored := m.hub.Restore(inst.sess.ID, display); stored.Seq != 0 {
			m.countRestoredMessage(inst.sess.ID, display)
		}
		m.logger.Info("adopt: redelivered pending message", "session", inst.sess.ID, "turn", rec.Seq)
	}
}
