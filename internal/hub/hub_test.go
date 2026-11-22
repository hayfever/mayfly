package hub

import (
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
)

func TestDefaultByteBudgetMatchesConfig(t *testing.T) {
	if got := DefaultByteBudget; got != int64(config.DefaultEventBufferBytes) {
		t.Fatalf("hub default budget = %d, config default = %d", got, config.DefaultEventBufferBytes)
	}
}

func TestAppendDropsDuplicateStepKey(t *testing.T) {
	h := New(10)
	h.Create("s")
	first := h.Append("s", model.Event{Type: model.EventToolResult, StepKey: "1/2/result", Text: "one"})
	if first.Seq == 0 {
		t.Fatal("first event was dropped")
	}
	second := h.Append("s", model.Event{Type: model.EventToolResult, StepKey: "1/2/result", Text: "two"})
	if second.Seq != 0 {
		t.Fatalf("duplicate event got seq %d", second.Seq)
	}
	third := h.Append("s", model.Event{Type: model.EventToolResult, StepKey: "1/3/result", Text: "three"})
	if third.Seq == 0 {
		t.Fatal("a different step key was dropped")
	}
	events := h.EventsAfter("s", 0)
	if len(events) != 2 || events[0].Text != "one" || events[1].Text != "three" {
		t.Fatalf("events = %+v", events)
	}
}

func TestRestoreOmitsInbox(t *testing.T) {
	h := New(10)
	h.Create("s")
	restored := h.Restore("s", model.Event{Type: model.EventUserMessage, Text: "old"})
	if restored.Seq == 0 {
		t.Fatal("restored event was dropped")
	}
	if inbox := h.InboxAfter("s", 0); len(inbox) != 0 {
		t.Fatalf("restored user message reached the inbox: %+v", inbox)
	}
	if events := h.EventsAfter("s", 0); len(events) != 1 {
		t.Fatalf("thread events = %+v", events)
	}
}

func TestSeedAdvancesSequence(t *testing.T) {
	h := New(10)
	h.Create("s")
	h.Seed("s", 1000)
	if got := h.LastSeq("s"); got != 1000 {
		t.Fatalf("LastSeq after seed = %d", got)
	}
	ev := h.Append("s", model.Event{Type: model.EventStatus, Text: "next"})
	if ev.Seq != 1001 {
		t.Fatalf("seq after seed = %d", ev.Seq)
	}
	h.Seed("s", 10)
	if got := h.LastSeq("s"); got != 1001 {
		t.Fatalf("a lower seed moved the sequence back to %d", got)
	}
}

// snapshot returns the retained events and accounted bytes of one stream.
func snapshot(h *Hub, sessionID string) ([]model.Event, int64) {
	h.mu.Lock()
	s := h.streams[sessionID]
	h.mu.Unlock()
	if s == nil {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]model.Event, len(s.events))
	var bytes int64
	for i, re := range s.events {
		events[i] = re.ev
		bytes += re.bytes
	}
	return events, bytes
}

func TestByteBudgetEvictsOldestEvents(t *testing.T) {
	const budget = 4096
	h := New(100, WithByteBudget(budget))
	h.Create("s")
	for range 20 {
		h.Append("s", model.Event{Type: model.EventAssistantMessage, Text: strings.Repeat("x", 1000)})
	}
	events, bytes := snapshot(h, "s")
	if bytes > budget {
		t.Fatalf("retained bytes = %d, over the %d-byte budget", bytes, budget)
	}
	if len(events) == 0 {
		t.Fatal("a byte budget evicted every event")
	}
	if last := events[len(events)-1].Seq; last != h.LastSeq("s") {
		t.Fatalf("last retained seq = %d, LastSeq = %d", last, h.LastSeq("s"))
	}
	first := events[0].Seq
	if first <= 1 {
		t.Fatalf("first retained seq = %d, want an eviction", first)
	}
	for i, ev := range events {
		if want := first + int64(i); ev.Seq != want {
			t.Fatalf("event %d has seq %d, want %d (not a contiguous suffix)", i, ev.Seq, want)
		}
	}
}

func TestInboxSurvivesRuntimeEventFlood(t *testing.T) {
	const budget = 4096
	h := New(100, WithByteBudget(budget))
	h.Create("s")
	human := h.Append("s", model.Event{Type: model.EventUserMessage, Text: "keep this message"})
	if human.Seq == 0 {
		t.Fatal("human message was dropped")
	}
	for range 30 {
		h.Append("s", model.Event{Type: model.EventToolResult, Text: strings.Repeat("y", 1000)})
	}
	inbox := h.InboxAfter("s", 0)
	if len(inbox) != 1 || inbox[0].Seq != human.Seq || inbox[0].Text != "keep this message" {
		t.Fatalf("inbox after the flood = %+v", inbox)
	}
	if _, bytes := snapshot(h, "s"); bytes > budget {
		t.Fatalf("retained bytes = %d, over the %d-byte budget", bytes, budget)
	}
}

func TestEvictedStepKeyCanBeAppendedAgain(t *testing.T) {
	h := New(100, WithByteBudget(2048))
	h.Create("s")
	first := h.Append("s", model.Event{Type: model.EventToolResult, StepKey: "1/2/result", Text: strings.Repeat("a", 900)})
	if first.Seq == 0 {
		t.Fatal("first keyed event was dropped")
	}
	if dup := h.Append("s", model.Event{Type: model.EventToolResult, StepKey: "1/2/result", Text: "retry"}); dup.Seq != 0 {
		t.Fatalf("duplicate step key was accepted with seq %d", dup.Seq)
	}
	h.Append("s", model.Event{Type: model.EventToolResult, StepKey: "1/3/result", Text: strings.Repeat("b", 900)})
	again := h.Append("s", model.Event{Type: model.EventToolResult, StepKey: "1/2/result", Text: "retry"})
	if again.Seq == 0 {
		t.Fatal("step key still blocked an event whose original was evicted")
	}
	events, _ := snapshot(h, "s")
	for _, ev := range events {
		if ev.Seq == first.Seq {
			t.Fatal("the evicted event is still retained")
		}
	}
}

func TestSubscribeAfterEvictedSeqReplaysTheRetainedSuffix(t *testing.T) {
	const budget = 2048
	h := New(100, WithByteBudget(budget))
	h.Create("s")
	for range 5 {
		h.Append("s", model.Event{Type: model.EventToolResult, Text: strings.Repeat("x", 900)})
	}
	ch, cancel := h.SubscribeEvents("s", 0)
	defer cancel()

	var first model.Event
	select {
	case first = <-ch:
	default:
		t.Fatal("no replayed event")
	}
	if first.Seq == 1 {
		t.Fatal("the first event was not evicted, so the gap is untested")
	}
	last := first.Seq
	for range 10 {
		select {
		case ev := <-ch:
			if ev.Seq != last+1 {
				t.Fatalf("replayed seq %d after %d", ev.Seq, last)
			}
			last = ev.Seq
		default:
			if last != h.LastSeq("s") {
				t.Fatalf("replay ended at %d, LastSeq = %d", last, h.LastSeq("s"))
			}
			return
		}
	}
	t.Fatal("replay did not end")
}
