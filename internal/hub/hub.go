// Package hub is the in-memory thread bus for sessions. It assigns event
// sequence numbers, replays history to new subscribers, and fans out live
// events. Durable history lives in the artifact bundle.
package hub

import (
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/model"
)

// DefaultByteBudget bounds the retained thread bytes of one session when no
// option sets another budget.
const DefaultByteBudget int64 = 16 << 20

// Option configures a Hub.
type Option func(*Hub)

// WithByteBudget sets the retained thread byte budget for each session. The
// budget applies in addition to the event count cap. A value at or below zero
// leaves the default in place.
func WithByteBudget(bytes int64) Option {
	return func(h *Hub) {
		if bytes > 0 {
			h.budget = bytes
		}
	}
}

// retainedEvent is one thread event and its accounted size.
type retainedEvent struct {
	ev    model.Event
	bytes int64
}

type stream struct {
	mu       sync.Mutex
	seq      int64
	events   []retainedEvent
	bytes    int64
	inbox    []model.Event
	subs     map[*subscription]struct{}
	max      int
	budget   int64
	keys     map[string]struct{}
	keyOrder []string
}

type subscription struct {
	ch     chan model.Event
	filter func(model.Event) bool
}

// Hub owns one stream per session.
type Hub struct {
	mu      sync.Mutex
	streams map[string]*stream
	max     int
	budget  int64
}

// New creates a hub retaining at most max events per session, and at most the
// byte budget per session. The budget defaults to DefaultByteBudget.
func New(max int, opts ...Option) *Hub {
	if max <= 0 {
		max = 5000
	}
	h := &Hub{streams: map[string]*stream{}, max: max, budget: DefaultByteBudget}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Create ensures a stream exists.
func (h *Hub) Create(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.streams[sessionID]; !ok {
		h.streams[sessionID] = &stream{
			subs:   map[*subscription]struct{}{},
			max:    h.max,
			budget: h.budget,
			keys:   map[string]struct{}{},
		}
	}
}

// Remove drops a stream and closes its subscribers.
func (h *Hub) Remove(sessionID string) {
	h.mu.Lock()
	s := h.streams[sessionID]
	delete(h.streams, sessionID)
	h.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		close(sub.ch)
		delete(s.subs, sub)
	}
}

// Append records an event and broadcasts it. The returned event has Seq,
// Time, and SessionID set. An event with a StepKey that this session already
// accepted is dropped and returned with Seq 0.
func (h *Hub) Append(sessionID string, ev model.Event) model.Event {
	return h.append(sessionID, ev, true)
}

// Restore records a recovered event on the visible thread without adding it
// to the inbox. Adoption uses it to rebuild the part of a turn that the
// control plane lost; a live or replacement runtime resumes from the journal,
// not from a second delivery of an old message.
func (h *Hub) Restore(sessionID string, ev model.Event) model.Event {
	return h.append(sessionID, ev, false)
}

func (h *Hub) append(sessionID string, ev model.Event, inbox bool) model.Event {
	h.Create(sessionID)
	h.mu.Lock()
	s := h.streams[sessionID]
	h.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	ev.SessionID = sessionID
	if ev.StepKey != "" {
		if _, seen := s.keys[ev.StepKey]; seen {
			ev.Seq = 0
			return ev
		}
		s.keys[ev.StepKey] = struct{}{}
		s.keyOrder = append(s.keyOrder, ev.StepKey)
	}
	s.seq++
	ev.Seq = s.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	size := eventBytes(ev)
	s.events = append(s.events, retainedEvent{ev: ev, bytes: size})
	s.bytes += size
	s.trimLocked()
	if inbox && ev.IsInbox() {
		s.inbox = append(s.inbox, ev)
		if len(s.inbox) > s.max {
			s.inbox = append([]model.Event(nil), s.inbox[len(s.inbox)-s.max:]...)
		}
	}
	for sub := range s.subs {
		if sub.filter != nil && !sub.filter(ev) {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// Slow subscriber: drop this event for that subscriber only.
		}
	}
	return ev
}

// trimLocked drops the oldest retained events until the stream fits both the
// event count cap and the byte budget. The inbox is a separate slice, so a
// runtime event flood cannot evict a human message the runtime has not read;
// only the thread copy of that message can go. A dedup key lives only while
// its event is retained, so a retry after eviction can restore the event.
func (s *stream) trimLocked() {
	for len(s.events) > s.max || (s.budget > 0 && s.bytes > s.budget) {
		if !s.evictOldestLocked() {
			return
		}
	}
}

func (s *stream) evictOldestLocked() bool {
	if len(s.events) == 0 {
		return false
	}
	oldest := s.events[0]
	s.bytes -= oldest.bytes
	// Clear the slot so the evicted strings are not kept alive by the
	// backing array.
	s.events[0] = retainedEvent{}
	s.events = s.events[1:]
	if oldest.ev.StepKey != "" {
		delete(s.keys, oldest.ev.StepKey)
		s.dropOldestKeyLocked(oldest.ev.StepKey)
	}
	if cap(s.events) > 2*len(s.events)+16 {
		s.events = append([]retainedEvent(nil), s.events...)
	}
	return true
}

func (s *stream) dropOldestKeyLocked(key string) {
	if len(s.keyOrder) == 0 {
		return
	}
	if s.keyOrder[0] == key {
		s.keyOrder[0] = ""
		s.keyOrder = s.keyOrder[1:]
		return
	}
	// Key order mirrors retained keyed events. Scan only if that invariant
	// does not hold, so a stale key cannot block a future append.
	for i, k := range s.keyOrder {
		if k != key {
			continue
		}
		copy(s.keyOrder[i:], s.keyOrder[i+1:])
		s.keyOrder[len(s.keyOrder)-1] = ""
		s.keyOrder = s.keyOrder[:len(s.keyOrder)-1]
		return
	}
}

// eventBytes estimates the memory one retained event holds. It counts the
// text bytes, the payload strings, and a fixed overhead for the event struct,
// its slice slot, the inbox copy, and the dedup key entry.
func eventBytes(ev model.Event) int64 {
	const overhead = 192
	n := int64(len(ev.SessionID)+len(ev.StepKey)+len(ev.Actor)+len(ev.Text)) + overhead
	switch d := ev.Data.(type) {
	case model.UserMessageData:
		n += stringBytes(d.Kind, d.Name, d.Event)
	case model.ToolCallData:
		n += stringBytes(d.Tool, d.CallID, d.Arguments)
	case model.ToolResultData:
		n += stringBytes(d.Tool, d.CallID)
	case model.StatusData:
		n += stringBytes(d.Reason, d.Error)
	case model.SystemData:
		n += stringBytes(d.Model)
		n += sliceBytes(d.Skills) + sliceBytes(d.MCPServers) + sliceBytes(d.Plugins)
	case model.ArtifactData:
		n += stringBytes(d.Key, d.URI, d.SHA256)
	case model.ControlData:
		n += stringBytes(d.Action, d.Reason)
	}
	return n
}

func stringBytes(values ...string) int64 {
	var n int64
	for _, v := range values {
		n += int64(len(v))
	}
	return n
}

func sliceBytes(values []string) int64 {
	var n int64
	for _, v := range values {
		n += int64(len(v)) + 16
	}
	return n
}

// Seed advances a session's sequence above every number a previous control
// plane handed out. A fresh process numbers events from zero, so an adopted
// session would otherwise reuse the turn keys of its journal.
func (h *Hub) Seed(sessionID string, floor int64) {
	h.Create(sessionID)
	h.mu.Lock()
	s := h.streams[sessionID]
	h.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if floor > s.seq {
		s.seq = floor
	}
}

// LastSeq returns the latest assigned sequence for a session.
func (h *Hub) LastSeq(sessionID string) int64 {
	h.mu.Lock()
	s := h.streams[sessionID]
	h.mu.Unlock()
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// EventsAfter returns retained events with Seq > after. A byte-budget
// eviction removes the oldest events, so a replay can start later than
// after+1; the caller sees the gap in the sequence numbers.
func (h *Hub) EventsAfter(sessionID string, after int64) []model.Event {
	h.mu.Lock()
	s := h.streams[sessionID]
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return filterRetained(s.events, after)
}

// InboxAfter returns retained inbox events with Seq > after.
func (h *Hub) InboxAfter(sessionID string, after int64) []model.Event {
	h.mu.Lock()
	s := h.streams[sessionID]
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return filterEvents(s.inbox, after)
}

// SubscribeEvents replays events after the sequence, then streams live events.
func (h *Hub) SubscribeEvents(sessionID string, after int64) (<-chan model.Event, func()) {
	return h.subscribe(sessionID, after, false, nil)
}

// SubscribeInbox replays inbox events after the sequence, then streams live
// inbox events only.
func (h *Hub) SubscribeInbox(sessionID string, after int64) (<-chan model.Event, func()) {
	return h.subscribe(sessionID, after, true, model.Event.IsInbox)
}

func (h *Hub) subscribe(sessionID string, after int64, inboxOnly bool, filter func(model.Event) bool) (<-chan model.Event, func()) {
	h.Create(sessionID)
	h.mu.Lock()
	s := h.streams[sessionID]
	h.mu.Unlock()

	ch := make(chan model.Event, h.max+1)
	sub := &subscription{ch: ch, filter: filter}

	s.mu.Lock()
	if inboxOnly {
		for _, ev := range filterEvents(s.inbox, after) {
			ch <- ev
		}
	} else {
		for _, ev := range filterRetained(s.events, after) {
			ch <- ev
		}
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if _, ok := s.subs[sub]; ok {
				delete(s.subs, sub)
				close(ch)
			}
			s.mu.Unlock()
		})
	}
	return ch, cancel
}

func filterRetained(events []retainedEvent, after int64) []model.Event {
	out := make([]model.Event, 0, len(events))
	for _, ev := range events {
		if ev.ev.Seq > after {
			out = append(out, ev.ev)
		}
	}
	return out
}

func filterEvents(events []model.Event, after int64) []model.Event {
	out := make([]model.Event, 0, len(events))
	for _, ev := range events {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out
}
