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
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		session: map[string]map[int64]map[int]Entry{},
		floor:   map[string]int64{},
		last:    map[string]int64{},
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

// Delete removes a session's steps and sequence reservation.
func (m *Memory) Delete(_ context.Context, session string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.session, session)
	delete(m.floor, session)
	delete(m.last, session)
	return nil
}
