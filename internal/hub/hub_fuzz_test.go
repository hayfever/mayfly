package hub

import (
	"fmt"
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
