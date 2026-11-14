package journal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func stores(t *testing.T) map[string]Durable {
	t.Helper()
	out := map[string]Durable{"memory": NewMemory()}
	b, err := OpenBolt(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	out["bbolt"] = b
	return out
}

func TestStoreRoundTrip(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			entries := []Entry{
				{SessionID: "s1", Turn: 4, Step: 0, Name: "user", Input: json.RawMessage(`{"type":"user_message"}`), Output: json.RawMessage(`{"role":"user","content":"hi"}`), ConvLen: 20, Time: now},
				{SessionID: "s1", Turn: 4, Step: 1, Name: "llm", Output: json.RawMessage(`{"role":"assistant"}`), ConvLen: 60, Time: now},
				{SessionID: "s1", Turn: 2, Step: 0, Name: "user", ConvLen: 10, Time: now},
				{SessionID: "s2", Turn: 9, Step: 0, Name: "user", ConvLen: 5, Time: now},
			}
			for _, e := range entries {
				if err := store.Append(ctx, e); err != nil {
					t.Fatalf("Append(%d/%d): %v", e.Turn, e.Step, err)
				}
			}

			got, err := store.Turn(ctx, "s1", 4)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0].Step != 0 || got[1].Step != 1 {
				t.Fatalf("Turn = %+v", got)
			}
			if string(got[0].Input) != `{"type":"user_message"}` || got[0].ConvLen != 20 {
				t.Fatalf("entry = %+v", got[0])
			}
			if !got[0].Time.Equal(now) {
				t.Fatalf("time = %v, want %v", got[0].Time, now)
			}

			// Turn 2 for s1 was appended last, so it is the turn a restart
			// would resume even though a higher key exists.
			last, err := store.LastTurn(ctx, "s1")
			if err != nil {
				t.Fatal(err)
			}
			if last != 2 {
				t.Fatalf("LastTurn(s1) = %d, want 2", last)
			}
			last, err = store.LastTurn(ctx, "s2")
			if err != nil || last != 9 {
				t.Fatalf("LastTurn(s2) = %d, %v", last, err)
			}
			last, err = store.LastTurn(ctx, "missing")
			if err != nil || last != 0 {
				t.Fatalf("LastTurn(missing) = %d, %v", last, err)
			}
			missing, err := store.Turn(ctx, "missing", 1)
			if err != nil || len(missing) != 0 {
				t.Fatalf("Turn(missing) = %+v, %v", missing, err)
			}
		})
	}
}

func TestLastTurnFollowsWriteOrder(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			base := time.Now().UTC().Truncate(time.Second)
			// A key that sorts high but was written before the newer turns,
			// which is what a reused sequence looks like.
			entries := []Entry{
				{SessionID: "s", Turn: 40, Step: 0, Name: "user", Time: base},
				{SessionID: "s", Turn: 3, Step: 0, Name: "user", Time: base.Add(time.Second)},
				{SessionID: "s", Turn: 4, Step: 0, Name: "user", Time: base.Add(2 * time.Second)},
			}
			for _, e := range entries {
				if err := store.Append(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			last, err := store.LastTurn(ctx, "s")
			if err != nil {
				t.Fatal(err)
			}
			if last != 4 {
				t.Fatalf("LastTurn = %d, want the most recently written turn 4", last)
			}
		})
	}
}

// TestLastTurnIgnoresEntryClock covers clock skew between nodes: the turn
// order follows the journal's write order, not the runtime-stamped entry time.
func TestLastTurnIgnoresEntryClock(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			base := time.Now().UTC().Truncate(time.Second)
			if err := store.Append(ctx, Entry{SessionID: "s", Turn: 10, Step: 0, Name: "user", Time: base}); err != nil {
				t.Fatal(err)
			}
			if err := store.Append(ctx, Entry{SessionID: "s", Turn: 12, Step: 0, Name: "user", Time: base.Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			last, err := store.LastTurn(ctx, "s")
			if err != nil || last != 12 {
				t.Fatalf("LastTurn = %d, %v, want 12", last, err)
			}
			maxTurn, err := store.MaxTurn(ctx, "s")
			if err != nil || maxTurn != 12 {
				t.Fatalf("MaxTurn = %d, %v, want 12", maxTurn, err)
			}
			// A late retry of an older turn was written last; write order wins.
			if err := store.Append(ctx, Entry{SessionID: "s", Turn: 10, Step: 0, Name: "user", Time: base.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if last, err := store.LastTurn(ctx, "s"); err != nil || last != 10 {
				t.Fatalf("LastTurn after retry = %d, %v, want 10", last, err)
			}
			if err := store.Delete(ctx, "s"); err != nil {
				t.Fatal(err)
			}
			if last, err := store.LastTurn(ctx, "s"); err != nil || last != 0 {
				t.Fatalf("LastTurn after delete = %d, %v", last, err)
			}
			if maxTurn, err := store.MaxTurn(ctx, "s"); err != nil || maxTurn != 0 {
				t.Fatalf("MaxTurn after delete = %d, %v", maxTurn, err)
			}
		})
	}
}

// TestBoltLastTurnFallsBackWithoutPointer reads a file written before the
// last-turn pointer existed. The highest key is the best available answer.
func TestBoltLastTurnFallsBackWithoutPointer(t *testing.T) {
	ctx := context.Background()
	s, err := OpenBolt(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Append(ctx, Entry{SessionID: "s", Turn: 7, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketLast).Delete([]byte("s"))
	}); err != nil {
		t.Fatal(err)
	}
	if last, err := s.LastTurn(ctx, "s"); err != nil || last != 7 {
		t.Fatalf("LastTurn = %d, %v, want 7", last, err)
	}
}

func TestReserveSequenceAndDelete(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if got, err := store.HighWater(ctx, "s"); err != nil || got != 0 {
				t.Fatalf("HighWater = %d, %v", got, err)
			}
			if err := store.Reserve(ctx, "s", 1000); err != nil {
				t.Fatal(err)
			}
			if err := store.Reserve(ctx, "s", 500); err != nil {
				t.Fatal(err)
			}
			if got, _ := store.HighWater(ctx, "s"); got != 1000 {
				t.Fatalf("HighWater after a lower reservation = %d", got)
			}
			if err := store.Reserve(ctx, "s", 2000); err != nil {
				t.Fatal(err)
			}
			if got, _ := store.HighWater(ctx, "s"); got != 2000 {
				t.Fatalf("HighWater = %d", got)
			}
			if err := store.Append(ctx, Entry{SessionID: "s", Turn: 1, Step: 0, Name: "user"}); err != nil {
				t.Fatal(err)
			}
			if err := store.Delete(ctx, "s"); err != nil {
				t.Fatal(err)
			}
			if got, _ := store.HighWater(ctx, "s"); got != 0 {
				t.Fatalf("HighWater after delete = %d", got)
			}
			if entries, err := store.Turn(ctx, "s", 1); err != nil || len(entries) != 0 {
				t.Fatalf("Turn after delete = %+v, %v", entries, err)
			}
			if last, err := store.LastTurn(ctx, "s"); err != nil || last != 0 {
				t.Fatalf("LastTurn after delete = %d, %v", last, err)
			}
			// Deleting twice stays a no-op.
			if err := store.Delete(ctx, "s"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStoreOverwritesStep(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			base := Entry{SessionID: "s", Turn: 1, Step: 0, Name: "user", ConvLen: 1, Time: time.Now().UTC()}
			if err := store.Append(ctx, base); err != nil {
				t.Fatal(err)
			}
			base.ConvLen = 99
			if err := store.Append(ctx, base); err != nil {
				t.Fatal(err)
			}
			got, err := store.Turn(ctx, "s", 1)
			if err != nil || len(got) != 1 || got[0].ConvLen != 99 {
				t.Fatalf("Turn = %+v, %v", got, err)
			}
		})
	}
}

func TestStoreRejectsInvalidEntry(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			cases := []Entry{
				{Turn: 1, Name: "user"},
				{SessionID: "s", Name: "user"},
				{SessionID: "s", Turn: 1},
				{SessionID: "s", Turn: 1, Step: -1, Name: "user"},
				{SessionID: "s", Turn: 1, Name: "user", ConvLen: -1},
				{SessionID: "s", Turn: 1, Name: "user", Input: json.RawMessage(`{`)},
			}
			for i, e := range cases {
				if err := store.Append(context.Background(), e); err == nil {
					t.Fatalf("case %d accepted invalid entry %+v", i, e)
				}
			}
		})
	}
}
