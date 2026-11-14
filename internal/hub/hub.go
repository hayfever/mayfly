// Package hub is the in-memory thread bus for sessions. It assigns event
// sequence numbers, replays history to new subscribers, and fans out live
// events. Durable history lives in the artifact bundle.
package hub

import (
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/model"
)

type stream struct {
	mu       sync.Mutex
	seq      int64
	events   []model.Event
	inbox    []model.Event
	subs     map[*subscription]struct{}
	max      int
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
}

// New creates a hub retaining at most max events per session.
func New(max int) *Hub {
	if max <= 0 {
		max = 5000
	}
	return &Hub{streams: map[string]*stream{}, max: max}
}

// Create ensures a stream exists.
func (h *Hub) Create(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.streams[sessionID]; !ok {
		h.streams[sessionID] = &stream{
			subs: map[*subscription]struct{}{},
			max:  h.max,
			keys: map[string]struct{}{},
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
		if len(s.keyOrder) > s.max {
			delete(s.keys, s.keyOrder[0])
			// Clear the slot so the dropped key is not kept alive by the
			// backing array until the next append reallocates it.
			s.keyOrder[0] = ""
			s.keyOrder = s.keyOrder[1:]
		}
	}
	s.seq++
	ev.Seq = s.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	s.events = append(s.events, ev)
	if len(s.events) > s.max {
		s.events = append([]model.Event(nil), s.events[len(s.events)-s.max:]...)
	}
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

// EventsAfter returns retained events with Seq > after.
func (h *Hub) EventsAfter(sessionID string, after int64) []model.Event {
	h.mu.Lock()
	s := h.streams[sessionID]
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return filterEvents(s.events, after)
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
		for _, ev := range filterEvents(s.events, after) {
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

func filterEvents(events []model.Event, after int64) []model.Event {
	out := make([]model.Event, 0, len(events))
	for _, ev := range events {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out
}
