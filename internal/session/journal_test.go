package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
)

func TestJournalMethodsAuthenticateAndScope(t *testing.T) {
	m := testManager(t)
	m.journal = journal.NewMemory()
	inst := &instance{sess: model.Session{ID: "s1"}, tokenHash: hashToken("token")}
	m.sessions["s1"] = inst
	m.tokenIndex[inst.tokenHash] = "s1"
	// A turn key is an inbox sequence this control plane assigned.
	m.hub.Create("s1")
	m.hub.Seed("s1", 4)

	ctx := context.Background()
	entry := journal.Entry{
		Turn:    4,
		Step:    0,
		Name:    "user",
		Input:   json.RawMessage(`{"type":"user_message","seq":4}`),
		Output:  json.RawMessage(`{"message":{"role":"user","content":"hi"}}`),
		ConvLen: 12,
	}
	stored, err := m.AppendJournal(ctx, "s1", "token", entry)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SessionID != "s1" {
		t.Fatalf("stored session = %q", stored.SessionID)
	}
	entries, err := m.JournalTurn(ctx, "s1", "token", 4)
	if err != nil || len(entries) != 1 || entries[0].ConvLen != 12 {
		t.Fatalf("JournalTurn = %+v, %v", entries, err)
	}
	last, err := m.JournalLastTurn(ctx, "s1", "token")
	if err != nil || last != 4 {
		t.Fatalf("JournalLastTurn = %d, %v", last, err)
	}
	if _, err := m.AppendJournal(ctx, "s1", "wrong", entry); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token error = %v", err)
	}
	if _, err := m.JournalTurn(ctx, "nope", "token", 4); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown session error = %v", err)
	}
	if _, err := m.JournalTurn(ctx, "s1", "token", 0); !errors.Is(err, journal.ErrInvalid) {
		t.Fatalf("zero turn error = %v", err)
	}
	if _, err := m.AppendJournal(ctx, "s1", "token", journal.Entry{Turn: 4}); !errors.Is(err, journal.ErrInvalid) {
		t.Fatalf("invalid entry error = %v", err)
	}
}

// journalInstance registers one session with the manager and a hub sequence
// range a runtime could have been assigned.
func journalInstance(t *testing.T, m *Manager) *instance {
	t.Helper()
	inst := &instance{sess: model.Session{ID: "s1"}, tokenHash: hashToken("token")}
	m.sessions["s1"] = inst
	m.tokenIndex[inst.tokenHash] = "s1"
	m.hub.Create("s1")
	m.hub.Seed("s1", 20)
	return inst
}

func TestAppendJournalPublishesStepEventsOnce(t *testing.T) {
	m := testManager(t)
	m.journal = journal.NewMemory()
	journalInstance(t, m)

	output, err := json.Marshal(map[string]any{"events": []journal.StepEvent{
		{Type: string(model.EventAssistantMessage), Actor: "agent", Text: "hello"},
		{Type: string(model.EventUserMessage), Actor: "user", Text: "injected"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	entry := journal.Entry{Turn: 11, Step: 1, Name: "assistant", Output: output, ConvLen: 10}
	ctx := context.Background()
	if _, err := m.AppendJournal(ctx, "s1", "token", entry); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendJournal(ctx, "s1", "token", entry); err != nil {
		t.Fatal(err)
	}
	events := m.hub.EventsAfter("s1", 0)
	if len(events) != 1 {
		t.Fatalf("published events = %+v", events)
	}
	if events[0].Type != model.EventAssistantMessage || events[0].Text != "hello" {
		t.Fatalf("published event = %+v", events[0])
	}
	if events[0].StepKey != "11/1/assistant" {
		t.Fatalf("step key = %q", events[0].StepKey)
	}
}

func TestAppendJournalRejectsUnassignedTurn(t *testing.T) {
	m := testManager(t)
	m.journal = journal.NewMemory()
	journalInstance(t, m)

	entry := journal.Entry{Turn: 21, Step: 0, Name: "user", ConvLen: 1}
	if _, err := m.AppendJournal(context.Background(), "s1", "token", entry); !errors.Is(err, journal.ErrInvalid) {
		t.Fatalf("unassigned turn error = %v", err)
	}
}

// lostJournalInstance registers a running session the way adoption does
// after a control-plane restart whose journal did not survive.
func lostJournalInstance(t *testing.T, m *Manager) *instance {
	t.Helper()
	m.journal = journal.NewMemory()
	inst := &instance{sess: model.Session{ID: "s1", Status: model.StatusRunning}, tokenHash: hashToken("token")}
	m.sessions["s1"] = inst
	m.tokenIndex[inst.tokenHash] = "s1"
	empty, err := m.seedSequence(context.Background(), "s1")
	if err != nil || !empty {
		t.Fatalf("seed: empty = %v, err = %v", empty, err)
	}
	m.hub.Create("s1")
	m.hub.Append("s1", model.Event{Type: model.EventSystem, Actor: "mayfly", Text: "Session recovered by control plane restart."})
	return inst
}

// TestAppendJournalAcceptsInFlightTurnAfterLostJournal covers a control-plane
// restart without a persisted journal while a runtime is mid-turn. The turn
// key was assigned by the previous lifetime, so its steps must be accepted,
// and no new message may reuse it.
func TestAppendJournalAcceptsInFlightTurnAfterLostJournal(t *testing.T) {
	m := testManager(t)
	inst := lostJournalInstance(t, m)
	ctx := context.Background()

	entry := journal.Entry{Turn: 50, Step: 2, Name: "tool:run_command", Output: json.RawMessage(`{}`), ConvLen: 300}
	if _, err := m.AppendJournal(ctx, "s1", "token", entry); err != nil {
		t.Fatalf("in-flight turn after a lost journal: %v", err)
	}
	entry.Step = 3
	if _, err := m.AppendJournal(ctx, "s1", "token", entry); err != nil {
		t.Fatalf("next step of the in-flight turn: %v", err)
	}
	ev := m.appendEvent(inst, model.Event{Type: model.EventUserMessage, Actor: "user", Text: "next"})
	if ev.Seq <= 50 {
		t.Fatalf("next message sequence = %d, want above 50", ev.Seq)
	}
	// A turn this lifetime never assigned is still rejected.
	unassigned := m.hub.LastSeq("s1") + 1
	if _, err := m.AppendJournal(ctx, "s1", "token", journal.Entry{Turn: unassigned, Step: 0, Name: "user", ConvLen: 1}); !errors.Is(err, journal.ErrInvalid) {
		t.Fatalf("unassigned turn error = %v", err)
	}
}

// TestLostJournalKeyCollision covers messages that arrive after a lost-journal
// restart but before the runtime's next append for its in-flight turn. The new
// message must not get the in-flight turn's key: the runtime would handle it
// as that turn and fail to match the journal.
func TestLostJournalKeyCollision(t *testing.T) {
	m := testManager(t)
	lostJournalInstance(t, m)
	ctx := context.Background()

	var msgs []model.Event
	for i := 0; i < 5; i++ {
		msg, err := m.PostMessage("s1", "user", "new message")
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, msg)
	}
	// The previous lifetime handed out keys from 1; its in-flight turn could
	// be any of the first few.
	for turn := int64(1); turn <= 10; turn++ {
		for _, msg := range msgs {
			if msg.Seq == turn {
				t.Fatalf("new message seq %d shares the key of a possible in-flight turn", msg.Seq)
			}
		}
	}
	if _, err := m.AppendJournal(ctx, "s1", "token", journal.Entry{Turn: 3, Step: 1, Name: "llm", Output: json.RawMessage(`{}`), ConvLen: 4}); err != nil {
		t.Fatalf("in-flight append: %v", err)
	}
}

// TestLostJournalFloorClearsAnEarlierLoss covers a journal lost twice. Keys
// handed out after the first loss start at that floor, so the next floor must
// be above them.
func TestLostJournalFloorClearsAnEarlierLoss(t *testing.T) {
	m := testManager(t)
	lostJournalInstance(t, m)
	var last model.Event
	for i := 0; i < 100; i++ {
		var err error
		if last, err = m.PostMessage("s1", "user", "message"); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(time.Millisecond)

	m2 := testManager(t)
	lostJournalInstance(t, m2)
	if got := m2.hub.LastSeq("s1"); got <= last.Seq {
		t.Fatalf("second lost-journal floor %d is not above the first lifetime's key %d", got, last.Seq)
	}
}

// TestAppendJournalRejectsOlderTurn keeps a stale container from moving the
// resume point back to an older turn. Steps of the newest turn, including a
// replay of the same step, are still accepted.
func TestAppendJournalRejectsOlderTurn(t *testing.T) {
	m := testManager(t)
	m.journal = journal.NewMemory()
	journalInstance(t, m)
	ctx := context.Background()

	for _, e := range []journal.Entry{
		{Turn: 5, Step: 0, Name: "user", ConvLen: 1},
		{Turn: 5, Step: 1, Name: "assistant", ConvLen: 2},
		{Turn: 9, Step: 0, Name: "user", ConvLen: 3},
		{Turn: 9, Step: 1, Name: "llm", ConvLen: 4},
		{Turn: 9, Step: 1, Name: "llm", ConvLen: 4},
	} {
		if _, err := m.AppendJournal(ctx, "s1", "token", e); err != nil {
			t.Fatalf("append turn %d step %d: %v", e.Turn, e.Step, err)
		}
	}
	if _, err := m.AppendJournal(ctx, "s1", "token", journal.Entry{Turn: 5, Step: 2, Name: "assistant", ConvLen: 5}); !errors.Is(err, journal.ErrInvalid) {
		t.Fatalf("older turn error = %v, want ErrInvalid", err)
	}
	last, err := m.JournalLastTurn(ctx, "s1", "token")
	if err != nil || last != 9 {
		t.Fatalf("JournalLastTurn = %d, %v, want 9", last, err)
	}
}

// TestAppendJournalStampsServerTime ignores the runtime clock: the control
// plane is the only writer, so its time orders the journal.
func TestAppendJournalStampsServerTime(t *testing.T) {
	m := testManager(t)
	m.journal = journal.NewMemory()
	journalInstance(t, m)
	ctx := context.Background()

	entry := journal.Entry{Turn: 11, Step: 0, Name: "user", ConvLen: 1, Time: time.Unix(0, 0)}
	stored, err := m.AppendJournal(ctx, "s1", "token", entry)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, got := range []time.Time{stored.Time} {
		if got.Before(now.Add(-5*time.Second)) || got.After(now.Add(5*time.Second)) {
			t.Fatalf("stored time = %v, want near %v", got, now)
		}
	}
	entries, err := m.JournalTurn(ctx, "s1", "token", 11)
	if err != nil || len(entries) != 1 {
		t.Fatalf("JournalTurn = %+v, %v", entries, err)
	}
	if entries[0].Time.Before(now.Add(-5*time.Second)) || entries[0].Time.After(now.Add(5*time.Second)) {
		t.Fatalf("journaled time = %v, want near %v", entries[0].Time, now)
	}
}

// reserveError is a journal whose sequence reservation always fails.
type reserveError struct{ *journal.Memory }

func (reserveError) Reserve(context.Context, string, int64) error {
	return errors.New("reservation failed")
}

// TestPostMessageFailsWhenReservationFails keeps a turn key from being handed
// out when it cannot be made durable first: a restart would reuse it.
func TestPostMessageFailsWhenReservationFails(t *testing.T) {
	m := testManager(t)
	m.journal = reserveError{journal.NewMemory()}
	inst := journalInstance(t, m)
	inst.sess.Status = model.StatusRunning
	before := m.hub.LastSeq("s1")

	if _, err := m.PostMessage("s1", "user", "hello"); err == nil {
		t.Fatal("PostMessage succeeded without a reserved sequence")
	}
	if got := m.hub.LastSeq("s1"); got != before {
		t.Fatalf("hub sequence = %d, want %d", got, before)
	}
}

func TestRestoreJournalThreadKeepsKeysAndTypes(t *testing.T) {
	m := testManager(t)
	m.journal = journal.NewMemory()
	journalInstance(t, m)
	ctx := context.Background()
	now := time.Now().UTC()

	input, err := json.Marshal(model.Event{Seq: 11, Type: model.EventUserMessage, Actor: "trigger", Text: "hi", Time: now})
	if err != nil {
		t.Fatal(err)
	}
	user := journal.Entry{SessionID: "s1", Turn: 11, Step: 0, Name: "user", Input: input, ConvLen: 10, Time: now}
	output, err := json.Marshal(map[string]any{"events": []journal.StepEvent{
		{Type: string(model.EventAssistantMessage), Actor: "agent", Text: "done"},
		{Type: string(model.EventUserMessage), Actor: "user", Text: "injected"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	assistant := journal.Entry{SessionID: "s1", Turn: 11, Step: 1, Name: "assistant", Output: output, ConvLen: 20, Time: now}
	for _, entry := range []journal.Entry{user, assistant} {
		if err := m.journal.Append(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}

	m.restoreJournalThread("s1")
	m.restoreJournalThread("s1")
	events := m.hub.EventsAfter("s1", 0)
	if len(events) != 2 {
		t.Fatalf("restored events = %+v", events)
	}
	if events[0].Type != model.EventUserMessage || events[0].Text != "hi" || events[0].StepKey != "11/0/user" {
		t.Fatalf("user event = %+v", events[0])
	}
	if events[1].Type != model.EventAssistantMessage || events[1].StepKey != "11/1/assistant" {
		t.Fatalf("assistant event = %+v", events[1])
	}
	if inbox := m.hub.InboxAfter("s1", 0); len(inbox) != 0 {
		t.Fatalf("restored events reached the inbox: %+v", inbox)
	}

	// A retried append that the previous control plane already committed must
	// not duplicate what restore already showed.
	if _, err := m.AppendJournal(ctx, "s1", "token", assistant); err != nil {
		t.Fatal(err)
	}
	if got := m.hub.EventsAfter("s1", 0); len(got) != 2 {
		t.Fatalf("events after a retried append = %+v", got)
	}
}

// TestSeedSequenceAdvancesPastJournal covers a control-plane restart: the new
// hub must start above the turn keys the previous lifetime handed out, so a
// new message cannot land on a journaled turn.
func TestSeedSequenceAdvancesPastJournal(t *testing.T) {
	m := testManager(t)
	m.journal = journal.NewMemory()
	inst := journalInstance(t, m)
	ctx := context.Background()

	if err := m.journal.Append(ctx, journal.Entry{SessionID: "s1", Turn: 40, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := m.journal.Reserve(ctx, "s1", 1000); err != nil {
		t.Fatal(err)
	}
	// A fresh hub has no events; adoption seeds it from the journal.
	m.hub = hub.New(1000)
	m.hub.Create("s1")
	if _, err := m.seedSequence(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if got := m.hub.LastSeq("s1"); got != 1000 {
		t.Fatalf("seeded sequence = %d, want 1000", got)
	}
	ev := m.appendEvent(inst, model.Event{Type: model.EventUserMessage, Actor: "user", Text: "new"})
	if ev.Seq != 1001 {
		t.Fatalf("new message sequence = %d, want 1001", ev.Seq)
	}
	if high, err := m.journal.HighWater(ctx, "s1"); err != nil || high != 2000 {
		t.Fatalf("reserved floor = %d, %v, want 2000", high, err)
	}
}
