package session

import (
	"context"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/model"
)

// TestDeleteRefusesLateAdoptionRegistration covers the interleaving a retry
// and DELETE can produce: the pass took its snapshot, DELETE removed the
// session, and the pass then finishes. The registration gate must refuse the
// session, and the failed-pass bookkeeping must not put it back in the retry
// set or the record store.
func TestDeleteRefusesLateAdoptionRegistration(t *testing.T) {
	ctx := context.Background()
	records := NewMemoryRecords()
	m := testManager(t)
	m.SetRecords(records)
	created := time.Now().UTC()
	m.history["race"] = model.Session{ID: "race", Status: model.StatusRunning, CreatedAt: created}
	m.adoptPending = map[string]adoptFailure{
		"race": {Attempts: 1, Reason: "read the session config"},
	}

	// The retry pass built an instance from the objects it read.
	inst := &instance{
		sess:      model.Session{ID: "race", Status: model.StatusRunning, CreatedAt: created},
		tokenHash: hashToken("token"),
	}

	if err := m.Delete("race"); err != nil {
		t.Fatalf("delete a pending session: %v", err)
	}
	if m.registerAdopted(inst) {
		t.Fatal("a pass that finished after DELETE registered the session")
	}
	m.mergeAdoptFailures(map[string]string{"race": "read the session config"}, "")

	// A later settlement sees the same snapshot and must not restore the
	// record either.
	m.mu.Lock()
	m.history["race"] = model.Session{ID: "race", Status: model.StatusRunning, CreatedAt: created}
	m.mu.Unlock()
	m.settleInterrupted()

	if _, ok := m.instance("race"); ok {
		t.Fatal("the deleted session was registered")
	}
	if _, ok := m.Get("race"); ok {
		t.Fatal("the deleted session is listed again")
	}
	if m.pendingAdoption("race") {
		t.Fatal("the deleted session went back into the retry set")
	}
	if stored, _ := records.List(ctx); len(stored) != 0 {
		t.Fatalf("stored records after delete = %+v", stored)
	}
}

// TestRegisterAdoptedKeepsLiveSession checks the gate does not reject an
// ordinary adoption: only a DELETE tombstone refuses registration.
func TestRegisterAdoptedKeepsLiveSession(t *testing.T) {
	m := testManager(t)
	inst := &instance{
		sess:      model.Session{ID: "keep", Status: model.StatusRunning},
		tokenHash: hashToken("token"),
	}
	if !m.registerAdopted(inst) {
		t.Fatal("an unknown id was refused")
	}
	if got, ok := m.Get("keep"); !ok || got.ID != "keep" {
		t.Fatalf("registered session = %+v, %v", got, ok)
	}
	if !m.Authenticate("keep", "token") {
		t.Fatal("the session token was not indexed")
	}
}
