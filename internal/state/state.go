// Package state stores the schedule watermark that makes catch-up and
// per-slot delivery idempotent. It keeps no document snapshots: a source that
// compares versions owns its own baseline.
package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Store records the most recent slot accepted for each schedule.
type Store interface {
	LastSlot(ctx context.Context, schedule string) (time.Time, bool, error)
	SetLastSlot(ctx context.Context, schedule string, slot time.Time) error
	Close() error
}

// formatVersion is the on-disk layout this build reads and writes. Version 1
// is the first stamped layout and matches what v0.1.0 wrote.
const formatVersion = 1

var (
	slotsBucket = []byte("slots")
	metaBucket  = []byte("meta")
	keyFormat   = []byte("format_version")
)

// New opens the watermark store. An empty path keeps the watermark in memory.
func New(path string) (Store, error) {
	if path == "" {
		return NewMemory(), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("state path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := bolt.Open(abs, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state %s: %w", abs, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(slotsBucket); err != nil {
			return err
		}
		return checkFormat(tx)
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create state bucket: %w", err)
	}
	return &boltStore{db: db}, nil
}

// checkFormat stamps a database written before the stamp existed as version 1
// and rejects a database written by a newer build. An unstamped database holds
// watermarks in the version 1 layout, so it loads unchanged.
func checkFormat(tx *bolt.Tx) error {
	meta, err := tx.CreateBucketIfNotExists(metaBucket)
	if err != nil {
		return err
	}
	raw := meta.Get(keyFormat)
	if raw == nil {
		return meta.Put(keyFormat, []byte(strconv.Itoa(formatVersion)))
	}
	stored, err := strconv.Atoi(string(raw))
	if err != nil {
		return fmt.Errorf("state format version %q is not a number", raw)
	}
	if stored > formatVersion {
		return fmt.Errorf("state format version %d is newer than this build reads (%d); upgrade mayflyd", stored, formatVersion)
	}
	return migrateFormat(stored)
}

// migrateFormat moves a store written by an older build to formatVersion. Each
// case rewrites one version's difference. Version 1 is the first stamped
// layout, so there is nothing to rewrite yet; a later version adds its case
// before raising formatVersion.
func migrateFormat(from int) error {
	switch from {
	case 1:
		return nil
	}
	return fmt.Errorf("state format version %d has no migration path", from)
}

type boltStore struct {
	db *bolt.DB
}

func (s *boltStore) LastSlot(ctx context.Context, schedule string) (time.Time, bool, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	var slot time.Time
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(slotsBucket).Get([]byte(schedule))
		if raw == nil {
			return nil
		}
		parsed, err := time.Parse(time.RFC3339Nano, string(raw))
		if err != nil {
			return fmt.Errorf("decode slot for %s: %w", schedule, err)
		}
		slot, found = parsed, true
		return nil
	})
	return slot, found, err
}

func (s *boltStore) SetLastSlot(ctx context.Context, schedule string, slot time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(slotsBucket).Put([]byte(schedule), []byte(slot.UTC().Format(time.RFC3339Nano)))
	})
}

func (s *boltStore) Close() error { return s.db.Close() }

// Memory is a Store for tests and for process-local runs.
type Memory struct {
	mu    sync.Mutex
	slots map[string]time.Time
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{slots: map[string]time.Time{}} }

// LastSlot returns the stored slot for schedule.
func (m *Memory) LastSlot(ctx context.Context, schedule string) (time.Time, bool, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	slot, ok := m.slots[schedule]
	return slot, ok, nil
}

// SetLastSlot records slot for schedule.
func (m *Memory) SetLastSlot(ctx context.Context, schedule string, slot time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slots[schedule] = slot.UTC()
	return nil
}

// Close implements Store.
func (m *Memory) Close() error { return nil }
