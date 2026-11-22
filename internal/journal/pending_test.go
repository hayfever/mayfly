package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/model"
)

func testPending(session string, seq int64, text string) Pending {
	return Pending{
		SessionID: session,
		Seq:       seq,
		Event: model.Event{
			SessionID: session,
			Seq:       seq,
			Time:      time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC),
			Type:      model.EventUserMessage,
			Actor:     "user",
			Text:      text,
		},
	}
}

func TestPendingValidation(t *testing.T) {
	valid := testPending("s1", 1, "hello")
	if _, err := encodePending(valid); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	cases := map[string]Pending{
		"no session":   {Seq: 1, Event: valid.Event},
		"zero seq":     {SessionID: "s1", Event: valid.Event},
		"wrong type":   {SessionID: "s1", Seq: 1, Event: model.Event{Seq: 1, Time: valid.Event.Time, Type: model.EventStatus}},
		"empty text":   {SessionID: "s1", Seq: 1, Event: model.Event{Seq: 1, Time: valid.Event.Time, Type: model.EventUserMessage}},
		"no time":      {SessionID: "s1", Seq: 1, Event: model.Event{Seq: 1, Type: model.EventUserMessage, Text: "x"}},
		"seq mismatch": {SessionID: "s1", Seq: 2, Event: valid.Event},
	}
	for name, p := range cases {
		if _, err := encodePending(p); err == nil {
			t.Fatalf("%s: malformed record was accepted", name)
		}
	}
}

func TestDecodePendingRejectsMalformedJSON(t *testing.T) {
	for _, raw := range []string{
		``,
		`null`,
		`{}`,
		`{"session_id":"s1","seq":1}`,
		`{"session_id":"s1","seq":1,"event":{"seq":1,"time":"2026-11-22T10:00:00Z","type":"user_message","text":"x"`,
		`{"session_id":"s1","seq":1,"event":{"seq":2,"time":"2026-11-22T10:00:00Z","type":"user_message","text":"x"}}`,
	} {
		if _, err := decodePending([]byte(raw)); err == nil {
			t.Fatalf("malformed record was accepted: %s", raw)
		}
	}
}

func TestMemoryPendingLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	second := testPending("s1", 2, "second")
	first := testPending("s1", 1, "first")
	for _, p := range []Pending{second, first} {
		if err := s.AddPending(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Pending(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("pending order = %+v", got)
	}
	// Step 0 makes the turn durable, so its pending copy is gone.
	if err := s.Append(ctx, Entry{
		SessionID: "s1", Turn: 1, Step: 0, Name: "user",
		Input: mustMarshal(t, first.Event), ConvLen: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Pending(ctx, "s1")
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("pending after step 0 = %+v", got)
	}
	if err := s.DeletePending(ctx, "s1", 2); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Pending(ctx, "s1"); len(got) != 0 {
		t.Fatalf("pending after delete = %+v", got)
	}
	if err := s.AddPending(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Pending(ctx, "s1"); len(got) != 0 {
		t.Fatalf("pending after session delete = %+v", got)
	}
}

func TestBoltPendingSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	s, err := OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	p := testPending("s1", 7, "durable")
	if err := s.AddPending(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Pending(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != p.Seq || got[0].Event.Text != p.Event.Text || !got[0].Event.Time.Equal(p.Event.Time) {
		t.Fatalf("pending after reopen = %+v", got)
	}
	if err := s.Append(ctx, Entry{
		SessionID: "s1", Turn: 7, Step: 0, Name: "user",
		Input: mustMarshal(t, p.Event), ConvLen: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Pending(ctx, "s1"); len(got) != 0 {
		t.Fatalf("pending after step 0 = %+v", got)
	}
}

// TestBoltOpensPrePendingDatabase covers the on-disk upgrade rule: the pending
// bucket is additive, so a database written by the release before it opens
// unchanged, and a build without the bucket keeps working on a new database.
func TestBoltOpensPrePendingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")

	// Write the layout the previous release produced: no pending bucket.
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry := Entry{
		SessionID: "s1", Turn: 4, Step: 0, Name: "user",
		Input: json.RawMessage(`{"prompt":"old"}`), ConvLen: 10,
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketJournal, bucketSeq, bucketCalls, bucketLast} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta, err := tx.CreateBucketIfNotExists(bucketMeta)
		if err != nil {
			return err
		}
		if err := meta.Put(keyFormat, []byte("1")); err != nil {
			return err
		}
		steps, err := tx.Bucket(bucketJournal).CreateBucket([]byte("s1"))
		if err != nil {
			return err
		}
		return steps.Put(entryKey(4, 0), payload)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenBolt(path)
	if err != nil {
		t.Fatalf("the new build did not open a previous-release database: %v", err)
	}
	turn, err := s.Turn(ctx, "s1", 4)
	if err != nil || len(turn) != 1 {
		t.Fatalf("old entry after open = %+v, %v", turn, err)
	}
	if got, err := s.Pending(ctx, "s1"); err != nil || len(got) != 0 {
		t.Fatalf("pending on an upgraded database = %+v, %v", got, err)
	}
	if err := s.AddPending(ctx, testPending("s1", 5, "new")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A build from before this change does not know the bucket. Its view of
	// the file must still be valid: format 1, the old entry readable, and a
	// write to a known bucket accepted.
	db, err = bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if got := string(tx.Bucket(bucketMeta).Get(keyFormat)); got != "1" {
			return fmt.Errorf("format version = %q, want 1", got)
		}
		if tx.Bucket(bucketPending) == nil {
			return fmt.Errorf("pending bucket is missing")
		}
		return tx.Bucket(bucketLast).Put([]byte("s1"), turnPrefix(4))
	}); err != nil {
		t.Fatalf("the previous-release view rejected the new database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// And the new build opens it again with the pending record intact.
	s, err = OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Pending(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != 5 {
		t.Fatalf("pending after a previous-release write = %+v", got)
	}
	if turn, err := s.Turn(ctx, "s1", 4); err != nil || len(turn) != 1 {
		t.Fatalf("old entry after reopen = %+v, %v", turn, err)
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
