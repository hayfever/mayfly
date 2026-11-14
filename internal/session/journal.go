package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
)

// AppendJournal records one runtime turn step for a session. Its recorded
// thread events are published here as well, so an event lost while the
// control plane was unavailable appears exactly once when the step becomes
// durable. Step keys make the publish idempotent.
func (m *Manager) AppendJournal(ctx context.Context, id, token string, e journal.Entry) (journal.Entry, error) {
	if !m.Authenticate(id, token) {
		return journal.Entry{}, ErrUnauthorized
	}
	inst, ok := m.instance(id)
	if !ok {
		return journal.Entry{}, ErrNotFound
	}
	e.SessionID = id
	// The runtime clock is not trusted for ordering or display. Resume picks
	// the turn to replay from this store, and the bundle shows these times.
	e.Time = time.Now().UTC()
	// A turn key is an inbox sequence this control plane assigned. Bounding
	// it stops a runtime from journaling a turn this lifetime never handed
	// out, which would corrupt the sequence floor and the next adoption's
	// seed. A turn the previous lifetime assigned is below the hub even when
	// its journal was lost: adoption seeds that session above it.
	if last := m.hub.LastSeq(id); e.Turn > last {
		return journal.Entry{}, fmt.Errorf("%w: turn %d was not assigned by this control plane", journal.ErrInvalid, e.Turn)
	}
	// A live runtime only appends to its newest turn, and resume replays the
	// last turn written. A late write for an older turn comes from a stale
	// container and would move the resume point back, so a later resume
	// would truncate the newer turn's history.
	inst.journalMu.Lock()
	defer inst.journalMu.Unlock()
	current, err := m.journal.LastTurn(ctx, id)
	if err != nil {
		return journal.Entry{}, err
	}
	if e.Turn < current {
		return journal.Entry{}, fmt.Errorf("%w: turn %d is older than journaled turn %d", journal.ErrInvalid, e.Turn, current)
	}
	if limit := maxJournalSteps(inst.runtimeCfg.MaxSteps); e.Step >= limit {
		return journal.Entry{}, fmt.Errorf("%w: step %d is over the %d steps a turn of this session can take", journal.ErrInvalid, e.Step, limit)
	}
	size := journalEntryBytes(e)
	if quota := m.cfg.Journal.MaxSessionBytes; quota > 0 && inst.journalBytes+size > quota {
		return journal.Entry{}, fmt.Errorf("%w: session journal is over journal.max_session_bytes (%d)", journal.ErrInvalid, quota)
	}
	if err := m.journal.Append(ctx, e); err != nil {
		return journal.Entry{}, err
	}
	inst.journalBytes += size
	m.publishStepEvents(inst, e)
	return e, nil
}

// maxToolCallsPerRound bounds the tool calls the step cap allows for one model
// round. It is generous: the cap exists to keep a runtime from writing
// arbitrary step keys, and journal.max_session_bytes bounds the volume.
const maxToolCallsPerRound = 128

// maxJournalSteps is the number of steps one turn can journal: the user step,
// then per round one model step and its tool calls.
func maxJournalSteps(maxSteps int) int {
	if maxSteps <= 0 {
		maxSteps = 1
	}
	return 1 + maxSteps*(1+maxToolCallsPerRound)
}

// journalEntryBytes is what an entry adds to the session's journal quota.
func journalEntryBytes(e journal.Entry) int64 {
	return int64(len(e.Name) + len(e.Input) + len(e.Output) + len(e.Err))
}

// publishStepEvents adds the thread events of a newly journaled step. It is
// the only publication path for an assistant message or tool result: the
// runtime emits only deterministic events before the append, so a crash
// cannot leave the thread and the journal describing different outputs.
func (m *Manager) publishStepEvents(inst *instance, entry journal.Entry) {
	for _, ev := range stepThreadEvents(entry) {
		m.appendEvent(inst, ev)
	}
}

// stepThreadEvents rebuilds the thread events a journaled step produced.
// Restore and live publication share it, so both paths use the same keys and
// the same type allowlist. A compromised runtime must not be able to journal a
// user message or a control action and have it appear as if a human wrote it.
func stepThreadEvents(entry journal.Entry) []model.Event {
	if entry.Step <= 0 {
		return nil
	}
	var payload struct {
		Events []journal.StepEvent `json:"events"`
	}
	if err := json.Unmarshal(entry.Output, &payload); err != nil {
		return nil
	}
	events := make([]model.Event, 0, len(payload.Events))
	for _, se := range payload.Events {
		if !threadEventAllowed(se.Type) {
			continue
		}
		events = append(events, model.Event{
			Time:    entry.Time,
			StepKey: fmt.Sprintf("%d/%d/%s", entry.Turn, entry.Step, stepEventKind(se.Type)),
			Type:    model.EventType(se.Type),
			Actor:   se.Actor,
			Text:    se.Text,
			Data:    se.Data,
		})
	}
	return events
}

func threadEventAllowed(typ string) bool {
	switch model.EventType(typ) {
	case model.EventAssistantMessage, model.EventToolCall, model.EventToolResult,
		model.EventError, model.EventSystem, model.EventStatus, model.EventArtifact:
		return true
	}
	return false
}

func stepEventKind(typ string) string {
	switch model.EventType(typ) {
	case model.EventAssistantMessage:
		return "assistant"
	case model.EventToolCall:
		return "call"
	case model.EventToolResult:
		return "result"
	default:
		return typ
	}
}

// userStepKey names the thread event of the user message that started a turn.
func userStepKey(turn int64) string {
	return fmt.Sprintf("%d/0/user", turn)
}

// JournalTurn returns the recorded steps of one turn. A missing turn is an
// empty list, not an error.
func (m *Manager) JournalTurn(ctx context.Context, id, token string, turn int64) ([]journal.Entry, error) {
	if !m.Authenticate(id, token) {
		return nil, ErrUnauthorized
	}
	if _, ok := m.instance(id); !ok {
		return nil, ErrNotFound
	}
	if turn <= 0 {
		return nil, fmt.Errorf("%w: turn must be positive", journal.ErrInvalid)
	}
	return m.journal.Turn(ctx, id, turn)
}

// JournalLastTurn returns the most recently written journaled turn for a
// session, or zero if there is none.
func (m *Manager) JournalLastTurn(ctx context.Context, id, token string) (int64, error) {
	if !m.Authenticate(id, token) {
		return 0, ErrUnauthorized
	}
	if _, ok := m.instance(id); !ok {
		return 0, ErrNotFound
	}
	return m.journal.LastTurn(ctx, id)
}
