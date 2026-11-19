package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestBoltStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedule.db")
	first, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	slot := time.Now().UTC().Truncate(time.Second)
	if err := first.SetLastSlot(ctx, "hourly", slot); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	got, ok, err := second.LastSlot(ctx, "hourly")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !got.Equal(slot) {
		t.Fatalf("LastSlot = %v, %v; want %v", got, ok, slot)
	}
}

// TestBoltStoreReadsLegacyDatabase removes the stamp a v0.1.0 database never
// had and checks that the stored watermark still loads.
func TestBoltStoreReadsLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedule.db")
	slot := time.Now().UTC().Truncate(time.Second)
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		slots, err := tx.CreateBucketIfNotExists(slotsBucket)
		if err != nil {
			return err
		}
		return slots.Put([]byte("hourly"), []byte(slot.Format(time.RFC3339Nano)))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := New(path)
	if err != nil {
		t.Fatalf("open a legacy database: %v", err)
	}
	got, ok, err := store.LastSlot(ctx, "hourly")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !got.Equal(slot) {
		t.Fatalf("LastSlot = %v, %v; want %v", got, ok, slot)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := storedFormat(t, path); got != "1" {
		t.Fatalf("stamp after opening a legacy database = %q, want 1", got)
	}
	// A database that carries the stamp opens like any other.
	again, err := New(path)
	if err != nil {
		t.Fatalf("reopen a stamped database: %v", err)
	}
	defer again.Close()
	if got, ok, err := again.LastSlot(ctx, "hourly"); err != nil || !ok || !got.Equal(slot) {
		t.Fatalf("LastSlot after reopen = %v, %v, %v", got, ok, err)
	}
}

func TestBoltStoreRejectsNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		return meta.Put(keyFormat, []byte("2"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open a newer database: %v, want a version error", err)
	}
}

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	if err := store.SetLastSlot(ctx, "daily", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LastSlot(ctx, "daily"); err != nil || !ok {
		t.Fatalf("LastSlot = %v, %v", ok, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.SetLastSlot(cancelled, "daily", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetLastSlot error = %v, want context.Canceled", err)
	}
}

func storedFormat(t *testing.T, path string) string {
	t.Helper()
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return errors.New("format bucket is gone")
		}
		got = string(meta.Get(keyFormat))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return got
}
