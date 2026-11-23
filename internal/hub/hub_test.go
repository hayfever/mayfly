package hub

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
)

func TestDefaultByteBudgetMatchesConfig(t *testing.T) {
	if got := DefaultByteBudget; got != int64(config.DefaultEventBufferBytes) {
		t.Fatalf("hub default budget = %d, config default = %d", got, config.DefaultEventBufferBytes)
	}
	if got := DefaultInboxByteBudget; got != int64(config.DefaultEventBufferBytes) {
		t.Fatalf("hub default inbox budget = %d, config default = %d", got, config.DefaultEventBufferBytes)
	}
}

// inboxSnapshot returns the retained inbox events and their accounted bytes.
func inboxSnapshot(h *Hub, sessionID string) ([]model.Event, int64) {
	h.mu.Lock()
	s := h.streams[sessionID]
	h.mu.Unlock()
	if s == nil {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]model.Event, len(s.inbox))
	copy(events, s.inbox)
	return events, s.inboxBytes
}

func TestInboxByteBudgetDropsOldest(t *testing.T) {
	const budget = 4096
	h := New(100, WithInboxByteBudget(budget))
	h.Create("s")
	for i := range 20 {
		got := h.Append("s", model.Event{Type: model.EventUserMessage, Text: fmt.Sprintf("%d %s", i, strings.Repeat("m", 1000))})
		if got.Seq == 0 {
			t.Fatalf("human message %d was dropped", i)
		}
	}
	inbox, bytes := inboxSnapshot(h, "s")
	if bytes > budget {
		t.Fatalf("inbox bytes = %d, over the %d-byte budget", bytes, budget)
	}
	if len(inbox) == 0 {
		t.Fatal("the inbox byte budget dropped every message")
	}
	if inbox[len(inbox)-1].Text == "" || !strings.HasPrefix(inbox[len(inbox)-1].Text, "19 ") {
		t.Fatalf("newest message is missing: %+v", inbox[len(inbox)-1])
	}
	if inbox[0].Seq <= 1 {
		t.Fatalf("first inbox seq = %d, want an eviction", inbox[0].Seq)
	}
}

func TestInboxByteBudgetDropsMessageLargerThanBudget(t *testing.T) {
	// The byte bound wins even when one message is larger than the whole
	// budget: the inbox never exceeds what the operator allowed. Keep
	// inbox_buffer_bytes above the 32 KiB message cap in practice.
	h := New(10, WithInboxByteBudget(256))
	h.Create("s")
	got := h.Append("s", model.Event{Type: model.EventUserMessage, Text: strings.Repeat("x", 1000)})
	if got.Seq == 0 {
		t.Fatal("human message was dropped by the hub append")
	}
	inbox, bytes := inboxSnapshot(h, "s")
	if len(inbox) != 0 || bytes != 0 {
		t.Fatalf("inbox = %+v (%d bytes), want empty", inbox, bytes)
	}
}

func TestRedeliverRespectsInboxByteBudget(t *testing.T) {
	const budget = 4096
	h := New(100, WithInboxByteBudget(budget))
	h.Seed("s", 100)
	for i := range 20 {
		ev := model.Event{
			Seq:   int64(i + 1),
			Type:  model.EventUserMessage,
			Actor: "user",
			Text:  fmt.Sprintf("%d %s", i, strings.Repeat("r", 1000)),
		}
		if !h.Redeliver("s", ev) {
			t.Fatalf("Redeliver refused message %d", i)
		}
	}
	inbox, bytes := inboxSnapshot(h, "s")
	if bytes > budget {
		t.Fatalf("inbox bytes = %d, over the %d-byte budget", bytes, budget)
	}
	if len(inbox) == 0 || inbox[len(inbox)-1].Seq != 20 {
		t.Fatalf("inbox = %+v, want the newest redelivered message", inbox)
	}
	if inbox[0].Seq <= 1 {
		t.Fatalf("first inbox seq = %d, want an eviction", inbox[0].Seq)
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

func TestInboxCountCapDropsOldest(t *testing.T) {
	h := New(2)
	h.Create("s")
	for i := range 3 {
		got := h.Append("s", model.Event{Type: model.EventUserMessage, Text: fmt.Sprintf("message %d", i)})
		if got.Seq == 0 {
			t.Fatalf("human message %d was dropped", i)
		}
	}
	inbox := h.InboxAfter("s", 0)
	if len(inbox) != 2 {
		t.Fatalf("inbox = %+v, want the newest two messages", inbox)
	}
	if inbox[0].Text != "message 1" || inbox[1].Text != "message 2" {
		t.Fatalf("inbox = %+v, want the oldest entry dropped", inbox)
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

// TestRedeliverKeepsReservedSequence covers adoption handing a message the
// control plane accepted before a restart back to the inbox. The sequence is
// the turn key, so redelivery must not allocate a new one, and a runtime that
// already read that sequence must not receive it twice.
func TestRedeliverKeepsReservedSequence(t *testing.T) {
	h := New(10)
	h.Seed("s1", 100)
	ev := model.Event{
		Seq:   5,
		Time:  time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC),
		Type:  model.EventUserMessage,
		Actor: "user",
		Text:  "queued",
	}
	ch, cancel := h.SubscribeInbox("s1", 0)
	defer cancel()

	if !h.Redeliver("s1", ev) {
		t.Fatal("Redeliver refused a valid event")
	}
	if got := <-ch; got.Seq != ev.Seq || got.Text != ev.Text {
		t.Fatalf("subscriber got %+v, want the reserved event", got)
	}
	if got := h.LastSeq("s1"); got != 100 {
		t.Fatalf("LastSeq after redelivery = %d, want the adoption seed", got)
	}
	inbox := h.InboxAfter("s1", 0)
	if len(inbox) != 1 || inbox[0].Seq != ev.Seq || inbox[0].Text != ev.Text || inbox[0].SessionID != "s1" {
		t.Fatalf("inbox = %+v", inbox)
	}
	// A repeated adoption pass is idempotent.
	if h.Redeliver("s1", ev) {
		t.Fatal("Redeliver accepted the same sequence twice")
	}
	// The thread is not touched: adoption restores a separate display copy
	// with a sequence above the seed.
	if got := h.EventsAfter("s1", 0); len(got) != 0 {
		t.Fatalf("redelivery changed the thread: %+v", got)
	}
	// A runtime whose cursor already passed the reserved sequence is not
	// handed the message again.
	if got := h.InboxAfter("s1", ev.Seq); len(got) != 0 {
		t.Fatalf("inbox after the consumed sequence = %+v", got)
	}
}
