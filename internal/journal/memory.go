package journal

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Memory is an in-process store for tests and development.
type Memory struct {
	mu      sync.RWMutex
	session map[string]map[int64]map[int]Entry
	floor   map[string]int64
	last    map[string]int64
	calls   map[string]map[string]Call
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		session: map[string]map[int64]map[int]Entry{},
		floor:   map[string]int64{},
		last:    map[string]int64{},
		calls:   map[string]map[string]Call{},
	}
}

// Append stores an entry.
func (m *Memory) Append(_ context.Context, e Entry) error {
	if err := validate(e); err != nil {
		return err
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	turns, ok := m.session[e.SessionID]
	if !ok {
		turns = map[int64]map[int]Entry{}
		m.session[e.SessionID] = turns
	}
	steps, ok := turns[e.Turn]
	if !ok {
		steps = map[int]Entry{}
		turns[e.Turn] = steps
	}
	steps[e.Step] = cloneEntry(e)
	m.last[e.SessionID] = e.Turn
	return nil
}

// Turn returns the entries for one turn ordered by step.
func (m *Memory) Turn(_ context.Context, session string, turn int64) ([]Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	steps := m.session[session][turn]
	out := make([]Entry, 0, len(steps))
	for _, e := range steps {
		out = append(out, cloneEntry(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Step < out[j].Step })
	return out, nil
}

// LastTurn returns the turn of the most recently appended entry.
func (m *Memory) LastTurn(_ context.Context, session string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.last[session], nil
}

// MaxTurn returns the highest turn with a journaled entry.
func (m *Memory) MaxTurn(_ context.Context, session string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var turn int64
	for t := range m.session[session] {
		if t > turn {
			turn = t
		}
	}
	return turn, nil
}

// HighWater returns the highest reserved sequence for a session.
func (m *Memory) HighWater(_ context.Context, session string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.floor[session], nil
}

// Reserve raises the reserved floor. It never lowers it.
func (m *Memory) Reserve(_ context.Context, session string, floor int64) error {
	if floor < 0 {
		return fmt.Errorf("%w: sequence floor must not be negative", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if floor > m.floor[session] {
		m.floor[session] = floor
	}
	return nil
}

// Delete removes a session's steps, sequence reservation, and tool calls.
func (m *Memory) Delete(_ context.Context, session string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.session, session)
	delete(m.floor, session)
	delete(m.last, session)
	delete(m.calls, session)
	return nil
}

// Begin returns the existing call record for c.Key, or stores c as pending.
func (m *Memory) Begin(_ context.Context, c Call) (Call, bool, error) {
	if err := validateCall(c); err != nil {
		return Call{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	calls, ok := m.calls[c.SessionID]
	if !ok {
		calls = map[string]Call{}
		m.calls[c.SessionID] = calls
	}
	if rec, ok := calls[c.Key]; ok {
		if rec.State == CallPending {
			rec.Attempts++
			calls[c.Key] = rec
		}
		return rec, false, nil
	}
	c.State = CallPending
	if c.Attempts <= 0 {
		c.Attempts = 1
	}
	if c.StartedAt.IsZero() {
		c.StartedAt = time.Now().UTC()
	}
	calls[c.Key] = c
	return c, true, nil
}

// Complete marks a call done. The same content again is a no-op.
func (m *Memory) Complete(_ context.Context, session, key, content string, isError, interrupted bool) (Call, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls := m.calls[session]
	rec, ok := calls[key]
	if !ok {
		return Call{}, fmt.Errorf("%w: %s", ErrCallNotFound, key)
	}
	if rec.State == CallDone {
		if rec.Content == content && rec.IsError == isError && rec.Interrupted == interrupted {
			return rec, nil
		}
		return Call{}, fmt.Errorf("%w: %s", ErrCallConflict, key)
	}
	rec.State = CallDone
	rec.Content = content
	rec.IsError = isError
	rec.Interrupted = interrupted
	rec.DoneAt = time.Now().UTC()
	calls[key] = rec
	return rec, nil
}

// Get returns the call record for key.
func (m *Memory) Get(_ context.Context, session, key string) (Call, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.calls[session][key]
	return rec, ok, nil
}
