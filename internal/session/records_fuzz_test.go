package session

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
)

// FuzzRecordJSON covers a persisted session record, which the manager reads
// whole at startup. Arbitrary bytes must not panic, the version check must
// never panic, and a record that decodes must survive one re-encode.
func FuzzRecordJSON(f *testing.F) {
	f.Add([]byte(`{"version":1,"session":{"id":"s1","title":"t","status":"completed","prompt":"p","agent":"default","harness":"claude","model":"m","skills":["a"],"mcp_servers":["b"],"plugins":["c"],"trigger":{"kind":"api","name":"http"},"backend":"kubernetes","pod_name":"p1","ttl_seconds":3600,"created_at":"2026-11-19T11:00:00Z","started_at":"2026-11-19T11:01:00Z","expires_at":"2026-11-19T12:00:00Z","closed_at":"2026-11-19T12:06:00Z","last_activity":"2026-11-19T12:06:00Z","message_count":4,"artifact":{"key":"k","uri":"u","size":2048,"sha256":"h","created_at":"2026-11-19T12:06:00Z","partial":true}}}`))
	f.Add([]byte(`{"version":2,"session":{"id":"s1"}}`))
	f.Add([]byte(`{"session":{"id":"s1"}}`))
	f.Add([]byte(`{"version":-1,"session":{}}`))
	f.Add([]byte(`{"version":1,"session":null}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return
		}
		_ = checkRecordVersion(rec.Version)
		first, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("encode record: %v\npayload: %s", err, data)
		}
		var again Record
		if err := json.Unmarshal(first, &again); err != nil {
			t.Fatalf("decode encoded record: %v\nencoded: %s", err, first)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("encode record a second time: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("record encoding is not stable:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

// FuzzArtifactInfoJSON covers the artifact metadata record. The store decodes
// it with json.Unmarshal and adds the session ID from the bucket key.
func FuzzArtifactInfoJSON(f *testing.F) {
	f.Add([]byte(`{"key":"sessions/s1.tar.gz","uri":"file:///a/b.tar.gz","size":2048,"sha256":"9f86","created_at":"2026-11-19T12:06:00Z","partial":true}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"size":-1}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var info model.ArtifactInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return
		}
		entry := ArtifactEntry{SessionID: "s1", Info: info}
		first, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("encode artifact entry: %v\npayload: %s", err, data)
		}
		var again ArtifactEntry
		if err := json.Unmarshal(first, &again); err != nil {
			t.Fatalf("decode encoded artifact entry: %v\nencoded: %s", err, first)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("encode artifact entry a second time: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("artifact entry encoding is not stable:\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}

// FuzzBoltStoredRecords writes raw values into the record buckets and reads
// them through List and ListArtifacts. A damaged sessions.db must surface an
// error, not panic, because these calls run before the API serves traffic.
func FuzzBoltStoredRecords(f *testing.F) {
	f.Add([]byte(`{"version":1,"session":{"id":"s1","status":"completed"}}`))
	f.Add([]byte(`{"key":"k","uri":"u","size":1,"sha256":"h","created_at":"2026-11-19T12:06:00Z"}`))
	f.Add([]byte(`{`))
	f.Add([]byte(`[]`))
	f.Add([]byte(nil))

	store, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: filepath.Join(f.TempDir(), "sessions.db")})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = store.Close() })
	records, ok := store.(*boltRecords)
	if !ok {
		f.Fatalf("bbolt backend returned %T", store)
	}
	// The database is thrown away, so skip the fsync that would dominate the
	// fuzz rate.
	records.db.NoSync = true
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, payload []byte) {
		err := records.db.Update(func(tx *bolt.Tx) error {
			if err := tx.Bucket(recordsBucketSessions).Put([]byte("key"), payload); err != nil {
				return err
			}
			return tx.Bucket(recordsBucketArtifacts).Put([]byte("key"), payload)
		})
		if err != nil {
			t.Fatalf("seed store: %v", err)
		}

		list, err := records.List(ctx)
		if err != nil {
			if len(list) != 0 {
				t.Fatalf("List returned %d records with an error", len(list))
			}
		} else if len(list) != 1 {
			t.Fatalf("List returned %d records, want 1", len(list))
		}

		artifacts, err := records.ListArtifacts(ctx)
		if err != nil {
			if len(artifacts) != 0 {
				t.Fatalf("ListArtifacts returned %d entries with an error", len(artifacts))
			}
		} else if len(artifacts) != 1 {
			t.Fatalf("ListArtifacts returned %d entries, want 1", len(artifacts))
		} else if artifacts[0].SessionID != "key" {
			t.Fatalf("artifact session id = %q, want key", artifacts[0].SessionID)
		}
	})
}
