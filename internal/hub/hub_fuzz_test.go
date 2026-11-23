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

// FuzzHubInboxBudget drives the inbox byte budget and count cap with a fuzzed
// op stream. Whatever the human posts, redeliveries, and runtime events, the
// retained inbox bytes stay within the byte budget and the count cap, the
// byte accounting matches the retained events, and a runtime append or
// restore never removes an inbox message.
func FuzzHubInboxBudget(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2, 0, 3, 0, 4, 0, 5, 1, 3, 2, 3, 3, 1})
	f.Add([]byte{0, 31, 0, 31})
	f.Add([]byte{0, 30, 0, 1, 3, 1})
	f.Add([]byte{0, 2, 1, 9, 2, 9, 0, 3, 3, 1})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 9, 1, 9})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, ops []byte) {
		const session = "fuzz"
		const threadCap = 8
		const threadBudget = 4096
		const inboxCap = threadCap
		const inboxBudget = 2048
		h := New(threadCap, WithByteBudget(threadBudget), WithInboxByteBudget(inboxBudget))
		h.Create(session)
		var last int64
		var posted []int64

		check := func() {
			t.Helper()
			events, bytes := inboxSnapshot(h, session)
			var sum int64
			for _, ev := range events {
				sum += eventBytes(ev)
				if ev.Type != model.EventUserMessage {
					t.Fatalf("inbox holds a %q event", ev.Type)
				}
			}
			if sum != bytes {
				t.Fatalf("inbox accounting = %d, retained events sum to %d", bytes, sum)
			}
			if bytes > inboxBudget {
				t.Fatalf("inbox bytes = %d, over the %d-byte budget", bytes, inboxBudget)
			}
			if len(events) > inboxCap {
				t.Fatalf("inbox holds %d entries, over the %d-entry cap", len(events), inboxCap)
			}
		}

		for i := 0; i+1 < len(ops); i += 2 {
			op, arg := ops[i]%4, int(ops[i+1])
			switch op {
			case 0:
				text := strings.Repeat("h", arg*64+1)
				got := h.Append(session, model.Event{Type: model.EventUserMessage, Text: text})
				if got.Seq == 0 {
					t.Fatalf("human post %d was dropped", i)
				}
				if got.Seq <= last {
					t.Fatalf("append returned seq %d after %d", got.Seq, last)
				}
				last = got.Seq
				posted = append(posted, got.Seq)
			case 1:
				before := inboxSeqs(h, session)
				got := h.Append(session, model.Event{Type: model.EventToolResult, Text: strings.Repeat("e", arg*64+1)})
				if got.Seq <= last {
					t.Fatalf("append returned seq %d after %d", got.Seq, last)
				}
				last = got.Seq
				after := inboxSeqs(h, session)
				if !sameSeqs(before, after) {
					t.Fatalf("a runtime append changed the inbox: %v -> %v", before, after)
				}
			case 2:
				before := inboxSeqs(h, session)
				got := h.Restore(session, model.Event{
					Type:    model.EventAssistantMessage,
					StepKey: fmt.Sprintf("step/%d", arg),
					Text:    strings.Repeat("z", arg*64+1),
				})
				if got.Seq != 0 {
					if got.Seq <= last {
						t.Fatalf("restore returned seq %d after %d", got.Seq, last)
					}
					last = got.Seq
				}
				after := inboxSeqs(h, session)
				if !sameSeqs(before, after) {
					t.Fatalf("a restored event changed the inbox: %v -> %v", before, after)
				}
			case 3:
				if len(posted) == 0 {
					break
				}
				seq := posted[arg%len(posted)]
				h.Redeliver(session, model.Event{
					Seq:   seq,
					Type:  model.EventUserMessage,
					Actor: "user",
					Text:  strings.Repeat("r", arg*64+1),
				})
			}
			check()
			if got := h.LastSeq(session); got < last {
				t.Fatalf("LastSeq fell from %d to %d", last, got)
			}
			last = h.LastSeq(session)
		}
		check()
	})
}

// inboxSeqs returns the retained inbox sequence numbers in order.
func inboxSeqs(h *Hub, sessionID string) []int64 {
	events, _ := inboxSnapshot(h, sessionID)
	out := make([]int64, len(events))
	for i, ev := range events {
		out[i] = ev.Seq
	}
	return out
}

func sameSeqs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
		last := human.Seq
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
