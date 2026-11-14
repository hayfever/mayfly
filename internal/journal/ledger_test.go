package journal

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ledgers returns one empty ledger per backend.
func ledgers(t *testing.T) map[string]Ledger {
	t.Helper()
	out := map[string]Ledger{"memory": NewMemory()}
	bolt, err := OpenBolt(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bolt.Close() })
	out["bbolt"] = bolt
	return out
}

func TestLedgerConformance(t *testing.T) {
	for name, ledger := range ledgers(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			call := Call{
				Key:        "s1/2/3",
				SessionID:  "s1",
				Turn:       2,
				Step:       3,
				Tool:       "plugin_alpha_get",
				ArgsSHA256: strings.Repeat("a", 64),
				InstanceID: "run-1",
			}

			rec, created, err := ledger.Begin(ctx, call)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if !created {
				t.Fatal("Begin did not create the record")
			}
			if rec.State != CallPending || rec.Attempts != 1 {
				t.Fatalf("new record = %+v", rec)
			}
			if rec.InstanceID != "run-1" {
				t.Fatalf("new record lost the runtime instance: %+v", rec)
			}
			if rec.StartedAt.IsZero() {
				t.Fatal("new record has no start time")
			}

			// A second begin for the same key is a retry, not a new call.
			rec, created, err = ledger.Begin(ctx, call)
			if err != nil {
				t.Fatalf("Begin retry: %v", err)
			}
			if created || rec.Attempts != 2 || rec.State != CallPending {
				t.Fatalf("retry record = %+v, created = %v", rec, created)
			}
			if rec.InstanceID != "run-1" {
				t.Fatalf("retry record lost the runtime instance: %+v", rec)
			}

			done, err := ledger.Complete(ctx, "s1", call.Key, "hello", true, false)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if done.State != CallDone || done.Content != "hello" || !done.IsError || done.DoneAt.IsZero() {
				t.Fatalf("done record = %+v", done)
			}

			// Repeating the same completion is a no-op, including the flag.
			again, err := ledger.Complete(ctx, "s1", call.Key, "hello", true, false)
			if err != nil {
				t.Fatalf("Complete repeat: %v", err)
			}
			if again.Content != "hello" || !again.IsError {
				t.Fatalf("repeat record = %+v", again)
			}
			if _, err := ledger.Complete(ctx, "s1", call.Key, "other", true, false); !errors.Is(err, ErrCallConflict) {
				t.Fatalf("different content error = %v, want ErrCallConflict", err)
			}
			if _, err := ledger.Complete(ctx, "s1", call.Key, "hello", false, false); !errors.Is(err, ErrCallConflict) {
				t.Fatalf("different flag error = %v, want ErrCallConflict", err)
			}

			// A completed record keeps its attempt count when it is read
			// again.
			rec, created, err = ledger.Begin(ctx, call)
			if err != nil || created {
				t.Fatalf("Begin on done = %+v, %v, created = %v", rec, err, created)
			}
			if rec.State != CallDone || rec.Attempts != 2 || rec.Content != "hello" {
				t.Fatalf("done record after Begin = %+v", rec)
			}

			// A missing record is absent, not an error.
			if _, found, err := ledger.Get(ctx, "s1", "missing"); err != nil || found {
				t.Fatalf("Get missing = found %v, %v", found, err)
			}
			if _, err := ledger.Complete(ctx, "s1", "missing", "", false, false); !errors.Is(err, ErrCallNotFound) {
				t.Fatalf("Complete missing error = %v, want ErrCallNotFound", err)
			}

			// Sessions do not see each other's records.
			other := call
			other.SessionID = "s2"
			other.Key = "s2/2/3"
			if _, created, err := ledger.Begin(ctx, other); err != nil || !created {
				t.Fatalf("Begin other session = %v, created = %v", err, created)
			}
			if _, found, _ := ledger.Get(ctx, "s1", other.Key); found {
				t.Fatal("session s1 saw s2's record")
			}

			bad := []Call{
				{Key: "k", Turn: 1, Step: 1, Tool: "t", ArgsSHA256: "h"},
				{SessionID: "s", Turn: 1, Step: 1, Tool: "t", ArgsSHA256: "h"},
				{SessionID: "s", Key: "k", Step: 1, Tool: "t", ArgsSHA256: "h"},
				{SessionID: "s", Key: "k", Turn: 1, Tool: "t", ArgsSHA256: "h"},
				{SessionID: "s", Key: "k", Turn: 1, Step: 1, ArgsSHA256: "h"},
				{SessionID: "s", Key: "k", Turn: 1, Step: 1, Tool: "t"},
				{SessionID: "s", Key: "k", Turn: 1, Step: 1, Tool: "t", ArgsSHA256: "h", State: CallDone},
			}
			for i, c := range bad {
				if _, _, err := ledger.Begin(ctx, c); err == nil {
					t.Fatalf("case %d accepted invalid call %+v", i, c)
				}
			}
		})
	}
}

// TestBoltLedgerSurvivesClose covers the durability claim: a control-plane
// restart replays a completed call instead of running it again.
func TestBoltLedgerSurvivesClose(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	first, err := OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{
		Key:        "s1/1/1",
		SessionID:  "s1",
		Turn:       1,
		Step:       1,
		Tool:       "plugin_alpha_post",
		ArgsSHA256: strings.Repeat("b", 64),
	}
	if _, _, err := first.Begin(ctx, call); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Complete(ctx, "s1", call.Key, "stored", true, true); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	rec, found, err := second.Get(ctx, "s1", call.Key)
	if err != nil || !found {
		t.Fatalf("Get after reopen = found %v, %v", found, err)
	}
	if rec.State != CallDone || rec.Content != "stored" || !rec.IsError || !rec.Interrupted {
		t.Fatalf("record after reopen = %+v", rec)
	}
}

func TestLedgerDeleteWithJournal(t *testing.T) {
	ctx := context.Background()
	type durableLedger interface {
		Durable
		Ledger
	}
	stores := map[string]durableLedger{"memory": NewMemory()}
	bolt, err := OpenBolt(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bolt.Close() })
	stores["bbolt"] = bolt

	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			session := "s-" + name
			entry := Entry{SessionID: session, Turn: 3, Step: 1, Name: "tool:plugin_alpha_get", Time: time.Now().UTC()}
			if err := store.Append(ctx, entry); err != nil {
				t.Fatal(err)
			}
			if err := store.Reserve(ctx, session, 3000); err != nil {
				t.Fatal(err)
			}
			call := Call{
				Key:        session + "/3/1",
				SessionID:  session,
				Turn:       3,
				Step:       1,
				Tool:       "plugin_alpha_get",
				ArgsSHA256: strings.Repeat("c", 64),
			}
			if _, _, err := store.Begin(ctx, call); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Complete(ctx, session, call.Key, "ok", false, false); err != nil {
				t.Fatal(err)
			}

			// A second session must survive the first session's delete.
			other := call
			other.SessionID = session + "-other"
			other.Key = other.SessionID + "/1/1"
			if _, _, err := store.Begin(ctx, other); err != nil {
				t.Fatal(err)
			}

			if err := store.Delete(ctx, session); err != nil {
				t.Fatal(err)
			}
			if entries, err := store.Turn(ctx, session, 3); err != nil || len(entries) != 0 {
				t.Fatalf("journal after delete = %+v, %v", entries, err)
			}
			if rec, found, err := store.Get(ctx, session, call.Key); err != nil || found {
				t.Fatalf("ledger after delete = %+v, found %v, %v", rec, found, err)
			}
			if high, err := store.HighWater(ctx, session); err != nil || high != 0 {
				t.Fatalf("high water after delete = %d, %v", high, err)
			}
			if _, found, err := store.Get(ctx, other.SessionID, other.Key); err != nil || !found {
				t.Fatalf("other session's record was deleted: found %v, %v", found, err)
			}
		})
	}
}
