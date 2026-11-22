package hub

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/model"
)

// FuzzSequenceInvariants drives the in-memory thread bus with a fuzzed op
// stream. The hub is the only assigner of event sequence numbers, and a
// resume position is a sequence, so the invariants here are what replay
// correctness rests on: numbers strictly increase, Seed never lowers the
// counter, and EventsAfter(after) returns only later events in order.
func FuzzSequenceInvariants(f *testing.F) {
	f.Add([]byte{0, 1, 2, 1, 3, 4, 2, 9, 1, 0, 5, 7, 3, 12, 0, 4, 1, 1})
	f.Add([]byte{})
	f.Add([]byte{3, 40, 0, 0, 1, 2})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{2, 1, 1, 2, 1, 1, 4, 0, 0})

	f.Fuzz(func(t *testing.T, ops []byte) {
		const session = "fuzz"
		h := New(4)
		h.Create(session)
		var last int64
		for i := 0; i+2 < len(ops); i += 3 {
			op, arg, aux := ops[i]%5, int64(int8(ops[i+1])), ops[i+2]
			switch op {
			case 0, 1:
				ev := model.Event{Type: model.EventAssistantMessage, Text: fmt.Sprintf("append %d", i)}
				if op == 1 {
					ev.StepKey = fmt.Sprintf("k%d", aux%3)
				}
				got := h.Append(session, ev)
				if got.Seq != 0 {
					if got.Seq <= last {
						t.Fatalf("append returned seq %d after %d", got.Seq, last)
					}
					if got.Seq != h.LastSeq(session) {
						t.Fatalf("append returned seq %d, LastSeq is %d", got.Seq, h.LastSeq(session))
					}
					last = got.Seq
				}
			case 2:
				ev := model.Event{
					Type:    model.EventUserMessage,
					Text:    fmt.Sprintf("restore %d", i),
					StepKey: fmt.Sprintf("r%d", aux%3),
				}
				got := h.Restore(session, ev)
				if got.Seq != 0 {
					if got.Seq <= last {
						t.Fatalf("restore returned seq %d after %d", got.Seq, last)
					}
					last = got.Seq
				}
				// A restored event belongs to the visible thread only; the
				// runtime must not receive it a second time.
				if inbox := h.InboxAfter(session, 0); len(inbox) != 0 {
					t.Fatalf("Restore added %d events to the inbox", len(inbox))
				}
			case 3:
				h.Seed(session, arg)
				if got := h.LastSeq(session); got < last || got < arg {
					t.Fatalf("Seed(%d) left LastSeq at %d (was %d)", arg, got, last)
				}
				last = h.LastSeq(session)
			case 4:
				after := int64(int8(ops[i+1]))
				prev := after
				for _, ev := range h.EventsAfter(session, after) {
					if ev.Seq <= after {
						t.Fatalf("EventsAfter(%d) returned seq %d", after, ev.Seq)
					}
					if ev.Seq <= prev {
						t.Fatalf("EventsAfter(%d) returned %d after %d", after, ev.Seq, prev)
					}
					prev = ev.Seq
				}
			}
			if got := h.LastSeq(session); got < last {
				t.Fatalf("LastSeq fell from %d to %d", last, got)
			}
			last = h.LastSeq(session)
			if tail := h.EventsAfter(session, last); len(tail) != 0 {
				t.Fatalf("EventsAfter(%d) returned %d retained events", last, len(tail))
			}
		}
	})
}

// FuzzHubAppendBudget drives the hub's retention policy with a fuzzed op
// stream. Whatever the sequence of runtime and restored events, the retained
// thread must stay within the byte budget and the event count cap, the
// retained sequences must be a contiguous suffix, and a human message must
// stay in the inbox while runtime events arrive.
func FuzzHubAppendBudget(f *testing.F) {
	f.Add([]byte{0, 1, 1, 2, 2, 0, 0, 255, 1, 0})
	f.Add([]byte{})
	f.Add([]byte{0, 255, 0, 255, 0, 255})
	f.Add([]byte{1, 0, 2, 7, 0, 8})
	f.Add([]byte{2, 3, 2, 3})

	f.Fuzz(func(t *testing.T, ops []byte) {
		const session = "fuzz"
		const budget = 2048
		const count = 8
		h := New(count, WithByteBudget(budget))
		h.Create(session)
		human := h.Append(session, model.Event{Type: model.EventUserMessage, Text: "keep me"})
		if human.Seq == 0 {
			t.Fatal("human message was dropped")
		}
		var last int64 = human.Seq
		for i := 0; i+1 < len(ops); i += 2 {
			switch ops[i] % 3 {
			case 0:
				text := strings.Repeat("é", int(ops[i+1])*8+1)
				got := h.Append(session, model.Event{Type: model.EventToolResult, Text: text})
				if got.Seq != 0 {
					if got.Seq <= last {
						t.Fatalf("append returned seq %d after %d", got.Seq, last)
					}
					last = got.Seq
				}
			case 1:
				text := strings.Repeat("z", int(ops[i+1])*8+1)
				got := h.Restore(session, model.Event{
					Type:    model.EventAssistantMessage,
					StepKey: fmt.Sprintf("step/%d", ops[i+1]),
					Text:    text,
				})
				if got.Seq != 0 {
					if got.Seq <= last {
						t.Fatalf("restore returned seq %d after %d", got.Seq, last)
					}
					last = got.Seq
				}
			case 2:
				after := int64(ops[i+1])
				events, bytes := snapshot(h, session)
				if bytes > budget {
					t.Fatalf("retained bytes = %d, over the %d-byte budget", bytes, budget)
				}
				if len(events) > count {
					t.Fatalf("retained %d events, over the %d-event cap", len(events), count)
				}
				if len(events) > 0 {
					if events[len(events)-1].Seq != h.LastSeq(session) {
						t.Fatalf("last retained seq = %d, LastSeq = %d", events[len(events)-1].Seq, h.LastSeq(session))
					}
					for j, ev := range events {
						if want := events[0].Seq + int64(j); ev.Seq != want {
							t.Fatalf("retained event %d has seq %d, want %d", j, ev.Seq, want)
						}
					}
				}
				prev := after
				for _, ev := range h.EventsAfter(session, after) {
					if ev.Seq <= after || ev.Seq <= prev {
						t.Fatalf("EventsAfter(%d) returned seq %d after %d", after, ev.Seq, prev)
					}
					prev = ev.Seq
				}
			}
			if got := h.LastSeq(session); got < last {
				t.Fatalf("LastSeq fell from %d to %d", last, got)
			}
			last = h.LastSeq(session)
			inbox := h.InboxAfter(session, 0)
			if len(inbox) != 1 || inbox[0].Seq != human.Seq {
				t.Fatalf("inbox after runtime events = %+v", inbox)
			}
		}
	})
}
