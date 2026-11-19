package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
)

func testRecordStores(t *testing.T) map[string]func() RecordStore {
	t.Helper()
	return map[string]func() RecordStore{
		"memory": func() RecordStore { return NewMemoryRecords() },
		"bbolt": func() RecordStore {
			store, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
	}
}

func TestRecordStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	for name, open := range testRecordStores(t) {
		t.Run(name, func(t *testing.T) {
			store := open()
			defer store.Close()
			now := time.Now().UTC().Truncate(time.Second)
			closed := now
			rec := Record{Session: model.Session{
				ID:           "s1",
				Title:        "one",
				Status:       model.StatusCompleted,
				CreatedAt:    now.Add(-time.Minute),
				ClosedAt:     &closed,
				MessageCount: 3,
				Skills:       []string{"go"},
			}}
			if err := store.Save(ctx, rec); err != nil {
				t.Fatal(err)
			}
			if err := store.Save(ctx, Record{Session: model.Session{ID: "s2", Status: model.StatusFailed}}); err != nil {
				t.Fatal(err)
			}
			records, err := store.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 2 {
				t.Fatalf("records = %+v, want 2", records)
			}
			for _, got := range records {
				if got.Session.ID == "s1" {
					if got.Session.MessageCount != 3 || len(got.Session.Skills) != 1 || got.Session.Status != model.StatusCompleted {
						t.Fatalf("record = %+v", got)
					}
					if got.Version != RecordFormatVersion && got.Version != 0 {
						t.Fatalf("record version = %d", got.Version)
					}
				}
			}
			if err := store.Delete(ctx, "s1"); err != nil {
				t.Fatal(err)
			}
			if records, _ := store.List(ctx); len(records) != 1 {
				t.Fatalf("records after delete = %+v", records)
			}

			info := model.ArtifactInfo{Key: "sessions/s2/bundle.tar.gz", Size: 10, SHA256: strings.Repeat("a", 64), CreatedAt: now}
			if err := store.SaveArtifact(ctx, "s2", info); err != nil {
				t.Fatal(err)
			}
			entries, err := store.ListArtifacts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].SessionID != "s2" || entries[0].Info.Key != info.Key {
				t.Fatalf("artifacts = %+v", entries)
			}
			if err := store.DeleteArtifact(ctx, "s2"); err != nil {
				t.Fatal(err)
			}
			if entries, _ := store.ListArtifacts(ctx); len(entries) != 0 {
				t.Fatalf("artifacts after delete = %+v", entries)
			}
		})
	}
}

// TestRecordStoreReadsLegacyRecord writes a record as v0.1.0 would have left
// it: no version field.
func TestRecordStoreReadsLegacyRecord(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"session":{"id":"legacy","status":"completed","title":"old"}}`)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketSessions).Put([]byte("legacy"), payload)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatalf("open a legacy record store: %v", err)
	}
	defer store.Close()
	records, err := store.List(ctx)
	if err != nil {
		t.Fatalf("read a legacy record: %v", err)
	}
	if len(records) != 1 || records[0].Session.ID != "legacy" || records[0].Session.Title != "old" {
		t.Fatalf("records = %+v", records)
	}
}

func TestRecordStoreRejectsNewerStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketMeta).Put(recordsKeyFormat, []byte("2"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open a newer store: %v, want a version error", err)
	}
}

func TestRecordStoreRejectsNewerRecord(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(Record{Version: RecordFormatVersion + 1, Session: model.Session{ID: "future"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketSessions).Put([]byte("future"), payload)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.List(ctx); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("read a newer record: %v, want a version error", err)
	}
}

// TestRestartKeepsFinishedSessionsAndArtifacts finalizes a session, closes its
// record store, and starts a second manager over the same store.
func TestRestartKeepsFinishedSessionsAndArtifacts(t *testing.T) {
	ctx := context.Background()
	bundleKey := t.TempDir()
	recordsPath := filepath.Join(bundleKey, "sessions.db")
	payload := []byte("retained bundle")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	key := "sessions/s1/bundle.tar.gz"

	records, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: recordsPath})
	if err != nil {
		t.Fatal(err)
	}
	first := testManager(t)
	first.SetRecords(records)
	if _, err := first.store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	closed := created.Add(5 * time.Minute)
	first.persistRecord(model.Session{
		ID:           "s1",
		Title:        "finished",
		Status:       model.StatusCompleted,
		Prompt:       "do it",
		Agent:        "default",
		CreatedAt:    created,
		ClosedAt:     &closed,
		LastActivity: closed,
		MessageCount: 4,
		Skills:       []string{"go"},
	})
	first.setArtifact("s1", model.ArtifactInfo{Key: key, Size: int64(len(payload)), SHA256: digest, CreatedAt: closed})
	if err := records.Close(); err != nil {
		t.Fatal(err)
	}

	records, err = OpenRecords(config.RecordConfig{Backend: "bbolt", Path: recordsPath})
	if err != nil {
		t.Fatal(err)
	}
	defer records.Close()
	second := testManager(t)
	second.store = first.store
	second.SetRecords(records)
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer second.Shutdown(context.Background())

	got, ok := second.Get("s1")
	if !ok {
		t.Fatal("finished session is missing after restart")
	}
	if got.Status != model.StatusCompleted || got.Title != "finished" || got.MessageCount != 4 || len(got.Skills) != 1 {
		t.Fatalf("restored session = %+v", got)
	}
	if got.Artifact == nil || got.Artifact.Key != key {
		t.Fatalf("restored artifact = %+v", got.Artifact)
	}
	list := second.List()
	if len(list) != 1 || list[0].ID != "s1" {
		t.Fatalf("list = %+v", list)
	}
	rc, size, err := second.OpenArtifact(ctx, "s1")
	if err != nil {
		t.Fatalf("download after restart: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(payload)) || !bytes.Equal(body, payload) {
		t.Fatalf("download = %d bytes %q", size, body)
	}
}

// TestRestartMarksUnfinishedSessionFailed covers a record whose runtime no
// control plane adopted: nothing can continue its turn.
func TestRestartMarksUnfinishedSessionFailed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	records, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	first := testManager(t)
	first.SetRecords(records)
	first.persistRecord(model.Session{ID: "s2", Status: model.StatusRunning, CreatedAt: time.Now().UTC().Add(-time.Minute)})
	if err := records.Close(); err != nil {
		t.Fatal(err)
	}

	records, err = OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer records.Close()
	second := testManager(t)
	second.SetRecords(records)
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer second.Shutdown(context.Background())

	got, ok := second.Get("s2")
	if !ok {
		t.Fatal("interrupted session is missing after restart")
	}
	if got.Status != model.StatusFailed || got.Error == "" || got.ClosedAt == nil {
		t.Fatalf("interrupted session = %+v", got)
	}
	// The transition is durable, so a third start reads it back.
	stored, err := records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Session.Status != model.StatusFailed {
		t.Fatalf("stored records = %+v", stored)
	}
}

func TestPruneRecordsDropsExpiredSessions(t *testing.T) {
	ctx := context.Background()
	records := NewMemoryRecords()
	m := testManager(t)
	m.SetRecords(records)
	m.cfg.Sessions.Retention = config.Duration(30 * time.Minute)
	m.cfg.Artifacts.IndexRetention = config.Duration(30 * time.Minute)
	now := time.Now().UTC()
	closedOld := now.Add(-2 * time.Hour)
	closedNew := now.Add(-time.Minute)
	if err := records.Save(ctx, Record{Session: model.Session{ID: "old", Status: model.StatusCompleted, CreatedAt: closedOld, ClosedAt: &closedOld}}); err != nil {
		t.Fatal(err)
	}
	if err := records.Save(ctx, Record{Session: model.Session{ID: "new", Status: model.StatusCompleted, CreatedAt: closedNew, ClosedAt: &closedNew}}); err != nil {
		t.Fatal(err)
	}
	m.history["old"] = model.Session{ID: "old", Status: model.StatusCompleted, ClosedAt: &closedOld}
	m.history["new"] = model.Session{ID: "new", Status: model.StatusCompleted, ClosedAt: &closedNew}
	m.artifacts["old"] = model.ArtifactInfo{Key: "k", CreatedAt: closedOld}
	m.artifacts["new"] = model.ArtifactInfo{Key: "k", CreatedAt: closedNew}
	if err := records.SaveArtifact(ctx, "old", m.artifacts["old"]); err != nil {
		t.Fatal(err)
	}
	if err := records.SaveArtifact(ctx, "new", m.artifacts["new"]); err != nil {
		t.Fatal(err)
	}

	m.pruneRecords(now)

	if _, ok := m.Get("old"); ok {
		t.Fatal("session past retention was kept")
	}
	if _, ok := m.Get("new"); !ok {
		t.Fatal("session inside retention was removed")
	}
	stored, err := records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Session.ID != "new" {
		t.Fatalf("stored records = %+v", stored)
	}
	if _, ok := m.ArtifactInfo("old"); ok {
		t.Fatal("artifact index entry past retention was kept")
	}
	if _, ok := m.ArtifactInfo("new"); !ok {
		t.Fatal("artifact index entry inside retention was removed")
	}
	if entries, _ := records.ListArtifacts(ctx); len(entries) != 1 || entries[0].SessionID != "new" {
		t.Fatalf("stored artifacts = %+v", entries)
	}
}

func TestDeleteDropsStoredSession(t *testing.T) {
	ctx := context.Background()
	records := NewMemoryRecords()
	m := testManager(t)
	m.SetRecords(records)
	closed := time.Now().UTC()
	if err := records.Save(ctx, Record{Session: model.Session{ID: "gone", Status: model.StatusCompleted, ClosedAt: &closed}}); err != nil {
		t.Fatal(err)
	}
	m.history["gone"] = model.Session{ID: "gone", Status: model.StatusCompleted, ClosedAt: &closed}

	if err := m.Delete("gone"); err != nil {
		t.Fatalf("delete a stored session: %v", err)
	}
	if _, ok := m.Get("gone"); ok {
		t.Fatal("deleted session is still listed")
	}
	if stored, _ := records.List(ctx); len(stored) != 0 {
		t.Fatalf("stored records = %+v", stored)
	}
	if err := m.Delete("gone"); err != ErrNotFound {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}
