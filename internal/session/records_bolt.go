package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
)

// recordsFormatVersion is the bucket layout of the record database. Version 1
// is the first stamped layout.
const recordsFormatVersion = 1

var (
	recordsBucketSessions  = []byte("sessions")
	recordsBucketArtifacts = []byte("artifacts")
	recordsBucketMeta      = []byte("meta")
	recordsKeyFormat       = []byte("format_version")
)

// OpenRecords opens the configured record store. The memory backend lives in
// the process; bbolt writes one file beside the journal.
func OpenRecords(cfg config.RecordConfig) (RecordStore, error) {
	switch cfg.Backend {
	case "", "memory":
		return NewMemoryRecords(), nil
	case "bbolt":
		if cfg.Path == "" {
			return nil, fmt.Errorf("sessions.records.path is required for the bbolt backend")
		}
		abs, err := filepath.Abs(cfg.Path)
		if err != nil {
			return nil, fmt.Errorf("sessions.records.path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return nil, fmt.Errorf("create record directory: %w", err)
		}
		db, err := bolt.Open(abs, 0o600, &bolt.Options{Timeout: 5 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("open session records %s: %w", abs, err)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			if err := checkRecordsFormat(tx); err != nil {
				return err
			}
			for _, name := range [][]byte{recordsBucketSessions, recordsBucketArtifacts} {
				if _, err := tx.CreateBucketIfNotExists(name); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("create session record buckets: %w", err)
		}
		return &boltRecords{db: db}, nil
	}
	return nil, fmt.Errorf("unknown session record backend %q", cfg.Backend)
}

type boltRecords struct {
	db *bolt.DB
}

// checkRecordsFormat stamps a database written by v0.1.0 as version 1 and
// rejects a database written by a newer build.
func checkRecordsFormat(tx *bolt.Tx) error {
	meta, err := tx.CreateBucketIfNotExists(recordsBucketMeta)
	if err != nil {
		return err
	}
	raw := meta.Get(recordsKeyFormat)
	if raw == nil {
		return meta.Put(recordsKeyFormat, []byte(strconv.Itoa(recordsFormatVersion)))
	}
	stored, err := strconv.Atoi(string(raw))
	if err != nil {
		return fmt.Errorf("session record format version %q is not a number", raw)
	}
	if stored > recordsFormatVersion {
		return fmt.Errorf("session record format version %d is newer than this build reads (%d); upgrade mayflyd", stored, recordsFormatVersion)
	}
	return migrateRecordsFormat(stored)
}

// migrateRecordsFormat moves a database written by an older build to
// recordsFormatVersion. Each branch rewrites one version's difference. Version
// 1 is the first stamped layout, so there is nothing to rewrite yet; a later
// version adds its branch before raising recordsFormatVersion.
func migrateRecordsFormat(from int) error {
	if from == 1 {
		return nil
	}
	return fmt.Errorf("session record format version %d has no migration path", from)
}

func (s *boltRecords) Save(ctx context.Context, rec Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rec.Session.ID == "" {
		return fmt.Errorf("session record has no id")
	}
	if rec.Version == 0 {
		rec.Version = RecordFormatVersion
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketSessions).Put([]byte(rec.Session.ID), payload)
	})
}

func (s *boltRecords) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketSessions).Delete([]byte(id))
	})
}

func (s *boltRecords) List(ctx context.Context) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := []Record{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketSessions).ForEach(func(key, raw []byte) error {
			var rec Record
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("decode session record %s: %w", key, err)
			}
			if err := checkRecordVersion(rec.Version); err != nil {
				return err
			}
			out = append(out, rec)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *boltRecords) SaveArtifact(ctx context.Context, id string, info model.ArtifactInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketArtifacts).Put([]byte(id), payload)
	})
}

func (s *boltRecords) DeleteArtifact(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketArtifacts).Delete([]byte(id))
	})
}

func (s *boltRecords) ListArtifacts(ctx context.Context) ([]ArtifactEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := []ArtifactEntry{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketArtifacts).ForEach(func(key, raw []byte) error {
			var info model.ArtifactInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				return fmt.Errorf("decode artifact record %s: %w", key, err)
			}
			out = append(out, ArtifactEntry{SessionID: string(key), Info: info})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *boltRecords) Close() error { return s.db.Close() }
