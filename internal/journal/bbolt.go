package journal

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// formatVersion is the on-disk layout this build reads and writes. Version 1
// is the first stamped layout and matches what v0.1.0 wrote.
const formatVersion = 1

var (
	bucketJournal = []byte("journal")
	bucketSeq     = []byte("seq")
	bucketLast    = []byte("last")
	bucketCalls   = []byte("calls")
	// bucketMeta holds store-wide facts. The leading NUL keeps a session ID
	// from ever naming this bucket.
	bucketMeta = []byte("\x00meta")
	keyFormat  = []byte("format_version")
)

// Bolt is a single-file durable store. Each session has its own bucket, and
// each step key is the turn followed by the step, so both Turn and LastTurn
// are ordered scans.
type Bolt struct {
	db *bolt.DB
}

// OpenBolt opens or creates a journal database.
func OpenBolt(path string) (*Bolt, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create journal directory: %w", err)
		}
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open journal database: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketJournal); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketSeq); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketCalls); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketLast); err != nil {
			return err
		}
		return checkFormat(tx)
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create journal bucket: %w", err)
	}
	return &Bolt{db: db}, nil
}

// Close releases the database file.
func (s *Bolt) Close() error {
	return s.db.Close()
}

// checkFormat stamps a database written before the stamp existed as version 1
// and rejects a database written by a newer build. An unstamped database holds
// records in the version 1 layout, so it loads unchanged.
func checkFormat(tx *bolt.Tx) error {
	meta, err := tx.CreateBucketIfNotExists(bucketMeta)
	if err != nil {
		return err
	}
	raw := meta.Get(keyFormat)
	if raw == nil {
		return meta.Put(keyFormat, []byte(strconv.Itoa(formatVersion)))
	}
	stored, err := strconv.Atoi(string(raw))
	if err != nil {
		return fmt.Errorf("journal format version %q is not a number", raw)
	}
	if stored > formatVersion {
		return fmt.Errorf("journal format version %d is newer than this build reads (%d); upgrade mayflyd", stored, formatVersion)
	}
	return migrateFormat(tx, stored)
}

// migrateFormat moves a store written by an older build to formatVersion. Each
// branch rewrites one version's difference. Version 1 is the first stamped
// layout, so there is nothing to rewrite yet; a later version adds its branch
// before raising formatVersion.
func migrateFormat(_ *bolt.Tx, from int) error {
	if from == 1 {
		return nil
	}
	return fmt.Errorf("journal format version %d has no migration path", from)
}

func (s *Bolt) Append(_ context.Context, e Entry) error {
	if err := validate(e); err != nil {
		return err
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketJournal)
		steps, err := root.CreateBucketIfNotExists([]byte(e.SessionID))
		if err != nil {
			return err
		}
		if err := steps.Put(entryKey(e.Turn, e.Step), payload); err != nil {
			return err
		}
		raw := make([]byte, 8)
		binary.BigEndian.PutUint64(raw, uint64(e.Turn))
		return tx.Bucket(bucketLast).Put([]byte(e.SessionID), raw)
	})
}

func (s *Bolt) Turn(_ context.Context, session string, turn int64) ([]Entry, error) {
	out := []Entry{}
	err := s.db.View(func(tx *bolt.Tx) error {
		steps := tx.Bucket(bucketJournal).Bucket([]byte(session))
		if steps == nil {
			return nil
		}
		prefix := turnPrefix(turn)
		cursor := steps.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && hasPrefix(key, prefix); key, raw = cursor.Next() {
			var e Entry
			if err := json.Unmarshal(raw, &e); err != nil {
				return fmt.Errorf("decode journal entry: %w", err)
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

// LastTurn returns the turn of the most recently appended entry. Write order
// is the invariant: one control-plane process is the only writer of a
// session's journal, and the entry clock is not trusted for ordering.
func (s *Bolt) LastTurn(_ context.Context, session string) (int64, error) {
	var last int64
	err := s.db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(bucketLast).Get([]byte(session)); len(raw) == 8 {
			last = int64(binary.BigEndian.Uint64(raw))
			return nil
		}
		// A file written before the pointer existed has no key. The highest
		// turn is the best available answer; resume then keeps every turn.
		steps := tx.Bucket(bucketJournal).Bucket([]byte(session))
		if steps == nil {
			return nil
		}
		return steps.ForEach(func(key, _ []byte) error {
			if len(key) >= 8 {
				if turn := int64(binary.BigEndian.Uint64(key[:8])); turn > last {
					last = turn
				}
			}
			return nil
		})
	})
	return last, err
}

// MaxTurn returns the highest turn with a journaled entry.
func (s *Bolt) MaxTurn(_ context.Context, session string) (int64, error) {
	var turn int64
	err := s.db.View(func(tx *bolt.Tx) error {
		steps := tx.Bucket(bucketJournal).Bucket([]byte(session))
		if steps == nil {
			return nil
		}
		key, _ := steps.Cursor().Last()
		if len(key) >= 8 {
			turn = int64(binary.BigEndian.Uint64(key[:8]))
		}
		return nil
	})
	return turn, err
}

// HighWater returns the highest reserved sequence for a session.
func (s *Bolt) HighWater(_ context.Context, session string) (int64, error) {
	var floor int64
	err := s.db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(bucketSeq).Get([]byte(session)); len(raw) > 0 {
			floor = int64(binary.BigEndian.Uint64(raw))
		}
		return nil
	})
	return floor, err
}

// Reserve raises the reserved floor. It never lowers it, so concurrent
// reservations stay monotonic.
func (s *Bolt) Reserve(_ context.Context, session string, floor int64) error {
	if floor < 0 {
		return fmt.Errorf("%w: sequence floor must not be negative", ErrInvalid)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketSeq)
		key := []byte(session)
		if raw := bucket.Get(key); len(raw) > 0 && int64(binary.BigEndian.Uint64(raw)) >= floor {
			return nil
		}
		raw := make([]byte, 8)
		binary.BigEndian.PutUint64(raw, uint64(floor))
		return bucket.Put(key, raw)
	})
}

// Delete removes a session's steps, sequence reservation, and tool calls.
func (s *Bolt) Delete(_ context.Context, session string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketJournal).DeleteBucket([]byte(session)); err != nil && !errors.Is(err, bolterrors.ErrBucketNotFound) {
			return err
		}
		if err := tx.Bucket(bucketSeq).Delete([]byte(session)); err != nil {
			return err
		}
		if err := tx.Bucket(bucketCalls).DeleteBucket([]byte(session)); err != nil && !errors.Is(err, bolterrors.ErrBucketNotFound) {
			return err
		}
		return tx.Bucket(bucketLast).Delete([]byte(session))
	})
}

// Begin returns the existing call record for c.Key, or stores c as pending.
func (s *Bolt) Begin(_ context.Context, c Call) (Call, bool, error) {
	if err := validateCall(c); err != nil {
		return Call{}, false, err
	}
	c.State = CallPending
	if c.Attempts <= 0 {
		c.Attempts = 1
	}
	if c.StartedAt.IsZero() {
		c.StartedAt = time.Now().UTC()
	}
	var rec Call
	var created bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketCalls)
		calls, err := root.CreateBucketIfNotExists([]byte(c.SessionID))
		if err != nil {
			return err
		}
		if raw := calls.Get([]byte(c.Key)); raw != nil {
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("decode call record: %w", err)
			}
			if rec.State == CallPending {
				rec.Attempts++
				payload, err := json.Marshal(rec)
				if err != nil {
					return err
				}
				return calls.Put([]byte(c.Key), payload)
			}
			return nil
		}
		payload, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := calls.Put([]byte(c.Key), payload); err != nil {
			return err
		}
		rec = c
		created = true
		return nil
	})
	return rec, created, err
}

// Complete marks a call done. The same content again is a no-op.
func (s *Bolt) Complete(_ context.Context, session, key, content string, isError, interrupted bool) (Call, error) {
	var rec Call
	err := s.db.Update(func(tx *bolt.Tx) error {
		calls := tx.Bucket(bucketCalls).Bucket([]byte(session))
		if calls == nil {
			return fmt.Errorf("%w: %s", ErrCallNotFound, key)
		}
		raw := calls.Get([]byte(key))
		if raw == nil {
			return fmt.Errorf("%w: %s", ErrCallNotFound, key)
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("decode call record: %w", err)
		}
		if rec.State == CallDone {
			if rec.Content == content && rec.IsError == isError && rec.Interrupted == interrupted {
				return nil
			}
			return fmt.Errorf("%w: %s", ErrCallConflict, key)
		}
		rec.State = CallDone
		rec.Content = content
		rec.IsError = isError
		rec.Interrupted = interrupted
		rec.DoneAt = time.Now().UTC()
		payload, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return calls.Put([]byte(key), payload)
	})
	return rec, err
}

// Get returns the call record for key.
func (s *Bolt) Get(_ context.Context, session, key string) (Call, bool, error) {
	var rec Call
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		calls := tx.Bucket(bucketCalls).Bucket([]byte(session))
		if calls == nil {
			return nil
		}
		raw := calls.Get([]byte(key))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("decode call record: %w", err)
		}
		found = true
		return nil
	})
	return rec, found, err
}

func entryKey(turn int64, step int) []byte {
	key := make([]byte, 12)
	binary.BigEndian.PutUint64(key[:8], uint64(turn))
	binary.BigEndian.PutUint32(key[8:], uint32(step))
	return key
}

func turnPrefix(turn int64) []byte {
	prefix := make([]byte, 8)
	binary.BigEndian.PutUint64(prefix, uint64(turn))
	return prefix
}

func hasPrefix(key, prefix []byte) bool {
	if len(key) < len(prefix) {
		return false
	}
	for i := range prefix {
		if key[i] != prefix[i] {
			return false
		}
	}
	return true
}
