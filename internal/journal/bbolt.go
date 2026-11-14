package journal

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketJournal = []byte("journal")
	bucketSeq     = []byte("seq")
	bucketLast    = []byte("last")
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
		_, err := tx.CreateBucketIfNotExists(bucketLast)
		return err
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

// Delete removes a session's steps and sequence reservation.
func (s *Bolt) Delete(_ context.Context, session string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketJournal).DeleteBucket([]byte(session)); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
			return err
		}
		if err := tx.Bucket(bucketSeq).Delete([]byte(session)); err != nil {
			return err
		}
		return tx.Bucket(bucketLast).Delete([]byte(session))
	})
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
