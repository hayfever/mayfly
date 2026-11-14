package hub

import (
	"testing"

	"github.com/hayfever/mayfly/internal/model"
)

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
