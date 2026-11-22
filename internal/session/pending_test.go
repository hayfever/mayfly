package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// newPostableInstance registers an instance that PostMessage accepts without a
// runner. The hub stream is created the way Create and adoption create it.
func newPostableInstance(t *testing.T, m *Manager, id string) *instance {
	t.Helper()
	inst := &instance{
		sess:      model.Session{ID: id, Status: model.StatusRunning},
		tokenHash: hashToken("token"),
		runtimeCfg: sessioncfg.Config{
			SessionID: id,
			MaxSteps:  3,
		},
	}
	m.sessions[id] = inst
	m.tokenIndex[inst.tokenHash] = id
	m.hub.Create(id)
	return inst
}

// TestPostMessageSurvivesRestartBeforeTurn covers the guarantee behind the
// 202: PostMessage writes the message to the journal before it returns, and a
// new control plane redelivers it under the same sequence after adoption. The
// test uses a fresh Manager and hub, which is what a process restart leaves.
func TestPostMessageSurvivesRestartBeforeTurn(t *testing.T) {
	store := journal.NewMemory()
	first := testManager(t)
	first.journal = store
	newPostableInstance(t, first, "s1")

	sent, err := first.PostMessage("s1", "user", "never started")
	if err != nil {
		t.Fatal(err)
	}
	if sent.Seq <= 0 {
		t.Fatalf("message sequence = %d", sent.Seq)
	}
	pending, err := store.Pending(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Seq != sent.Seq {
		t.Fatalf("pending after PostMessage = %+v", pending)
	}

	// The process restarts: hub and instances are gone, the journal is not.
	second := testManager(t)
	second.journal = store
	restarted := &instance{sess: model.Session{ID: "s1", Status: model.StatusPending}}
	second.sessions["s1"] = restarted
	floor, err := store.HighWater(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	second.hub.Seed("s1", floor)
	second.redeliverPending(restarted)

	ch, cancel, ok := second.SubscribeInbox("s1", 0)
	if !ok {
		t.Fatal("SubscribeInbox refused the adopted session")
	}
	defer cancel()
	got := nextEvent(t, ch)
	if got.Seq != sent.Seq {
		t.Fatalf("redelivered sequence = %d, want the acknowledged turn key %d", got.Seq, sent.Seq)
	}
	if got.Text != sent.Text || !got.Time.Equal(sent.Time) {
		t.Fatalf("redelivered event = %+v, want %+v", got, sent)
	}

	// A second adoption pass must not enqueue it again.
	second.redeliverPending(restarted)
	ch2, cancel2, _ := second.SubscribeInbox("s1", 0)
	defer cancel2()
	if extra := drainEvents(ch2); len(extra) != 1 {
		t.Fatalf("inbox after a repeated adoption = %d events, want 1", len(extra))
	}
}

// TestJournaledStepZeroDropsPendingMessage covers the delete side: once the
// runtime starts the turn, adoption has nothing to redeliver.
func TestJournaledStepZeroDropsPendingMessage(t *testing.T) {
	store := journal.NewMemory()
	m := testManager(t)
	m.journal = store
	newPostableInstance(t, m, "s1")

	sent, err := m.PostMessage("s1", "user", "started")
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendJournal(context.Background(), "s1", "token", journal.Entry{
		Turn: sent.Seq, Step: 0, Name: "user", Input: input,
		Output: json.RawMessage(`{"message":{"role":"user","content":"started"}}`), ConvLen: 10,
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Pending(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after step 0 = %+v, want none", pending)
	}
	// Adoption must not deliver a turn that already started.
	restarted := &instance{sess: model.Session{ID: "s1", Status: model.StatusPending}}
	m.sessions["s1"] = restarted
	m.hub = hub.New(100)
	m.hub.Seed("s1", sent.Seq)
	m.redeliverPending(restarted)
	ch, cancel, _ := m.SubscribeInbox("s1", 0)
	defer cancel()
	if events := drainEvents(ch); len(events) != 0 {
		t.Fatalf("inbox after a started turn = %+v, want none", events)
	}
}

// TestCreatePersistsInitialPrompt covers the initial trigger message, which is
// also a turn the runtime journals only after it connects.
func TestCreatePersistsInitialPrompt(t *testing.T) {
	store := journal.NewMemory()
	m := testManager(t)
	m.journal = store
	m.cfg.LLM.Model = "test-model"
	m.runner = nopRunner{}
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	sess, err := m.Create(context.Background(), CreateOptions{Prompt: "bootstrap"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.Pending(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending after Create = %+v, want the initial prompt", pending)
	}
	if pending[0].Event.Type != model.EventUserMessage || pending[0].Event.Text != "bootstrap" {
		t.Fatalf("pending event = %+v", pending[0].Event)
	}
}

func nextEvent(t *testing.T, ch <-chan model.Event) model.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no inbox event")
		return model.Event{}
	}
}

func drainEvents(ch <-chan model.Event) []model.Event {
	var out []model.Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}
