package queue

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/config"
)

var (
	bucketJobs  = []byte("jobs")
	bucketReady = []byte("ready")
	bucketSeen  = []byte("seen")
	bucketDead  = []byte("dead")
	bucketDone  = []byte("done")
	// bucketSeenExpiry indexes dedupe keys by expiry (8-byte big-endian unix
	// nanoseconds, then the dedupe key) so expired keys purge in order.
	bucketSeenExpiry = []byte("seen_expiry")
	// bucketDoneExpiry indexes acknowledged jobs by tombstone expiry so their
	// job-to-session records do not accumulate.
	bucketDoneExpiry = []byte("done_expiry")
	bucketMeta       = []byte("meta")
	keyFormat        = []byte("format_version")
)

// purgeBatch bounds the extra work each Accept does to remove expired dedupe
// keys.
const purgeBatch = 256

// boltStore is a single-file durable queue. Dedupe and enqueue happen in one
// transaction, so an accepted job is either fully stored or not stored at all.
type boltStore struct {
	cfg config.QueueConfig
	db  *bolt.DB
}

func newBolt(cfg config.QueueConfig) (*boltStore, error) {
	db, err := bolt.Open(cfg.Bolt.Path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open queue database: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if err := checkBoltFormat(tx); err != nil {
			return err
		}
		for _, name := range [][]byte{bucketJobs, bucketReady, bucketSeen, bucketDead, bucketDone, bucketDoneExpiry} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		if tx.Bucket(bucketSeenExpiry) != nil {
			return nil
		}
		index, err := tx.CreateBucket(bucketSeenExpiry)
		if err != nil {
			return err
		}
		// An upgraded database indexes its existing dedupe keys once, so the
		// keys written before the index existed purge like new ones.
		return tx.Bucket(bucketSeen).ForEach(func(dedupeKey, expiry []byte) error {
			if len(expiry) != 8 {
				return nil
			}
			return index.Put(expiryKey(expiry, dedupeKey), nil)
		})
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create queue buckets: %w", err)
	}
	return &boltStore{cfg: cfg, db: db}, nil
}

// checkBoltFormat stamps a database written before the stamp existed as
// version 1 and rejects a database written by a newer build.
func checkBoltFormat(tx *bolt.Tx) error {
	meta, err := tx.CreateBucketIfNotExists(bucketMeta)
	if err != nil {
		return err
	}
	missing, err := checkFormat(meta.Get(keyFormat))
	if err != nil {
		return err
	}
	if !missing {
		return nil
	}
	return meta.Put(keyFormat, []byte(strconv.Itoa(formatVersion)))
}

func (s *boltStore) Accept(_ context.Context, job Job) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.db.Update(func(tx *bolt.Tx) error {
		seen := tx.Bucket(bucketSeen)
		ready := tx.Bucket(bucketReady)
		if job.IdempotencyKey != "" {
			if raw := seen.Get([]byte(job.ID)); raw != nil {
				expiry := time.Unix(0, int64(binary.BigEndian.Uint64(raw)))
				if now.Before(expiry) {
					return ErrDuplicate
				}
				if err := seen.Delete([]byte(job.ID)); err != nil {
					return err
				}
			}
		}
		if max := s.cfg.MaxJobs; max > 0 {
			live := ready.Stats().KeyN
			if live >= max {
				return ErrFull
			}
		}
		if err := tx.Bucket(bucketJobs).Put([]byte(job.ID), payload); err != nil {
			return err
		}
		if err := ready.Put(readyKey(now, job.ID), nil); err != nil {
			return err
		}
		if job.IdempotencyKey != "" {
			expiry := make([]byte, 8)
			binary.BigEndian.PutUint64(expiry, uint64(now.Add(s.cfg.DedupeTTL.Std()).UnixNano()))
			dedupeKey := []byte(job.ID)
			if err := seen.Put(dedupeKey, expiry); err != nil {
				return err
			}
			if err := tx.Bucket(bucketSeenExpiry).Put(expiryKey(expiry, dedupeKey), nil); err != nil {
				return err
			}
		}
		if err := purgeSeen(tx, now, purgeBatch); err != nil {
			return err
		}
		return purgeDone(tx, now, purgeBatch)
	})
}

func (s *boltStore) Claim(_ context.Context, max int) ([]Lease, error) {
	now := time.Now().UTC()
	deadline := now.Add(s.cfg.VisibilityTimeout.Std())
	var leases []Lease
	err := s.db.Update(func(tx *bolt.Tx) error {
		ready := tx.Bucket(bucketReady)
		jobs := tx.Bucket(bucketJobs)
		leases = leases[:0]
		cursor := ready.Cursor()
		type move struct{ from []byte }
		moves := make([]move, 0, max)
		for key, _ := cursor.First(); key != nil && len(leases) < max; key, _ = cursor.Next() {
			readyAt, id := splitReadyKey(key)
			if !due(readyAt, now) {
				break
			}
			payload := jobs.Get([]byte(id))
			if payload == nil {
				moves = append(moves, move{from: append([]byte(nil), key...)})
				continue
			}
			var job Job
			if err := json.Unmarshal(payload, &job); err != nil {
				// A payload this build cannot read must not block the queue.
				// Bury it with the decode reason so an operator can see it.
				buried, berr := json.Marshal(deadRecord{
					Job:      Job{ID: id, Trigger: "unknown"},
					Error:    "job payload does not decode: " + err.Error(),
					FailedAt: now,
				})
				if berr != nil {
					return berr
				}
				if err := tx.Bucket(bucketDead).Put([]byte(id), buried); err != nil {
					return err
				}
				if err := jobs.Delete([]byte(id)); err != nil {
					return err
				}
				moves = append(moves, move{from: append([]byte(nil), key...)})
				continue
			}
			job.ID = id
			leases = append(leases, Lease{ID: id, Job: job, Deadline: deadline})
			moves = append(moves, move{from: append([]byte(nil), key...)})
		}
		for _, m := range moves {
			if err := ready.Delete(m.from); err != nil {
				return err
			}
		}
		for _, lease := range leases {
			if err := ready.Put(readyKey(deadline, lease.ID), nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return leases, nil
}

func (s *boltStore) Ack(_ context.Context, lease Lease, result Result) error {
	now := time.Now().UTC()
	payload, err := json.Marshal(doneRecord{Job: lease.Job, SessionID: result.SessionID, FinishedAt: now})
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketJobs).Delete([]byte(lease.ID)); err != nil {
			return err
		}
		if err := tx.Bucket(bucketReady).Delete(readyKey(lease.Deadline, lease.ID)); err != nil {
			return err
		}
		if err := tx.Bucket(bucketDone).Put([]byte(lease.ID), payload); err != nil {
			return err
		}
		if s.cfg.TombstoneTTL.Std() <= 0 {
			return nil
		}
		expiry := make([]byte, 8)
		binary.BigEndian.PutUint64(expiry, uint64(now.Add(s.cfg.TombstoneTTL.Std()).UnixNano()))
		return tx.Bucket(bucketDoneExpiry).Put(expiryKey(expiry, []byte(lease.ID)), nil)
	})
}

func (s *boltStore) Nack(_ context.Context, lease Lease, cause error) error {
	now := time.Now().UTC()
	return s.db.Update(func(tx *bolt.Tx) error {
		jobs := tx.Bucket(bucketJobs)
		payload := jobs.Get([]byte(lease.ID))
		if payload == nil {
			return nil
		}
		if !holdsLease(tx.Bucket(bucketReady), lease) {
			return ErrLeaseLost
		}
		var job Job
		if err := json.Unmarshal(payload, &job); err != nil {
			return err
		}
		job.Attempts++
		if max := s.cfg.MaxAttempts; max > 0 && job.Attempts >= max {
			buried, err := json.Marshal(deadRecord{Job: job, Error: causeString(cause), FailedAt: now})
			if err != nil {
				return err
			}
			if err := tx.Bucket(bucketDead).Put([]byte(lease.ID), buried); err != nil {
				return err
			}
			if err := tx.Bucket(bucketReady).Delete(readyKey(lease.Deadline, lease.ID)); err != nil {
				return err
			}
			return jobs.Delete([]byte(lease.ID))
		}
		updated, err := json.Marshal(job)
		if err != nil {
			return err
		}
		if err := jobs.Put([]byte(lease.ID), updated); err != nil {
			return err
		}
		if err := tx.Bucket(bucketReady).Delete(readyKey(lease.Deadline, lease.ID)); err != nil {
			return err
		}
		return tx.Bucket(bucketReady).Put(readyKey(now.Add(Backoff(job.Attempts, s.cfg.RetryBase.Std())), lease.ID), nil)
	})
}

func (s *boltStore) Release(_ context.Context, lease Lease, after time.Duration) error {
	now := time.Now().UTC().Add(after)
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketJobs).Get([]byte(lease.ID)) == nil {
			return nil
		}
		ready := tx.Bucket(bucketReady)
		if !holdsLease(ready, lease) {
			return ErrLeaseLost
		}
		if err := ready.Delete(readyKey(lease.Deadline, lease.ID)); err != nil {
			return err
		}
		return ready.Put(readyKey(now, lease.ID), nil)
	})
}

func (s *boltStore) Extend(_ context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	deadline := time.Now().UTC().Add(ttl)
	err := s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketJobs).Get([]byte(lease.ID)) == nil {
			return ErrLeaseLost
		}
		ready := tx.Bucket(bucketReady)
		if !holdsLease(ready, lease) {
			return ErrLeaseLost
		}
		if err := ready.Delete(readyKey(lease.Deadline, lease.ID)); err != nil {
			return err
		}
		return ready.Put(readyKey(deadline, lease.ID), nil)
	})
	if err != nil {
		return lease, err
	}
	lease.Deadline = deadline
	return lease, nil
}

func (s *boltStore) Get(_ context.Context, id string) (Record, bool, error) {
	now := time.Now().UTC()
	var record Record
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		if payload := tx.Bucket(bucketJobs).Get([]byte(id)); payload != nil {
			var job Job
			if err := json.Unmarshal(payload, &job); err != nil {
				return err
			}
			record = Record{Job: job, State: StateQueued}
			if deadline, ok := readyFor(tx.Bucket(bucketReady), id); ok && deadline.After(now) {
				record.State = StateLeased
			}
			found = true
			return nil
		}
		if payload := tx.Bucket(bucketDead).Get([]byte(id)); payload != nil {
			dead := decodeDead(payload)
			record = Record{Job: dead.Job, State: StateDead, Error: dead.Error, FailedAt: dead.FailedAt}
			found = true
			return nil
		}
		if payload := tx.Bucket(bucketDone).Get([]byte(id)); payload != nil {
			var done doneRecord
			if err := json.Unmarshal(payload, &done); err != nil {
				return err
			}
			record = Record{Job: done.Job, State: StateDone, SessionID: done.SessionID, FinishedAt: done.FinishedAt}
			found = true
		}
		return nil
	})
	return record, found, err
}

func (s *boltStore) Dead(_ context.Context, limit int) ([]Record, error) {
	var out []Record
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketDead).ForEach(func(_, payload []byte) error {
			dead := decodeDead(payload)
			out = append(out, Record{Job: dead.Job, State: StateDead, Error: dead.Error, FailedAt: dead.FailedAt})
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].FailedAt.After(out[j].FailedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

func (s *boltStore) Retry(_ context.Context, id string) error {
	now := time.Now().UTC()
	return s.db.Update(func(tx *bolt.Tx) error {
		payload := tx.Bucket(bucketDead).Get([]byte(id))
		if payload == nil {
			return ErrNotFound
		}
		dead := decodeDead(payload)
		job := dead.Job
		if job.ID == "" {
			job.ID = id
		}
		job.Attempts = 0
		encoded, err := json.Marshal(job)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketJobs).Put([]byte(id), encoded); err != nil {
			return err
		}
		if err := tx.Bucket(bucketReady).Put(readyKey(now, id), nil); err != nil {
			return err
		}
		return tx.Bucket(bucketDead).Delete([]byte(id))
	})
}

func (s *boltStore) Delete(_ context.Context, id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		dead := tx.Bucket(bucketDead)
		if dead.Get([]byte(id)) == nil {
			return ErrNotFound
		}
		return dead.Delete([]byte(id))
	})
}

func (s *boltStore) Stats(_ context.Context) (Stats, error) {
	now := time.Now().UTC()
	var stats Stats
	err := s.db.View(func(tx *bolt.Tx) error {
		stats = Stats{Dead: tx.Bucket(bucketDead).Stats().KeyN}
		return tx.Bucket(bucketReady).ForEach(func(key, _ []byte) error {
			readyAt, id := splitReadyKey(key)
			if due(readyAt, now) {
				stats.Pending++
			} else {
				stats.Leased++
			}
			payload := tx.Bucket(bucketJobs).Get([]byte(id))
			if payload == nil {
				return nil
			}
			var job Job
			if err := json.Unmarshal(payload, &job); err != nil {
				// An unreadable payload is buried on the next claim. It does
				// not stop the counters from reporting the rest of the queue.
				return nil
			}
			if stats.Oldest.IsZero() || job.AcceptedAt.Before(stats.Oldest) {
				stats.Oldest = job.AcceptedAt
			}
			return nil
		})
	})
	return stats, err
}

func (s *boltStore) Close() error { return s.db.Close() }

// holdsLease reports whether the ready entry written by this lease's Claim
// is still present, meaning no later Claim has taken the job.
func holdsLease(ready *bolt.Bucket, lease Lease) bool {
	key := readyKey(lease.Deadline, lease.ID)
	found, _ := ready.Cursor().Seek(key)
	return bytes.Equal(found, key)
}

// readyKey orders jobs by the time they become claimable.
func readyKey(at time.Time, id string) []byte {
	key := make([]byte, 8+len(id))
	binary.BigEndian.PutUint64(key[:8], uint64(at.UTC().UnixNano()))
	copy(key[8:], id)
	return key
}

// readyFor finds the ready entry for one job. Only admin lookups use it.
func readyFor(ready *bolt.Bucket, id string) (time.Time, bool) {
	cursor := ready.Cursor()
	for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
		at, keyID := splitReadyKey(key)
		if keyID == id {
			return at, true
		}
	}
	return time.Time{}, false
}

// decodeDead reads a buried job, accepting records written before the error
// and timestamp were stored.
func decodeDead(payload []byte) deadRecord {
	var dead deadRecord
	if json.Unmarshal(payload, &dead) == nil && dead.Job.ID != "" {
		return dead
	}
	var job Job
	if json.Unmarshal(payload, &job) == nil {
		return deadRecord{Job: job}
	}
	return dead
}

func splitReadyKey(key []byte) (time.Time, string) {
	if len(key) < 8 {
		return time.Time{}, ""
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(key[:8]))).UTC(), string(key[8:])
}

// expiryKey orders records by expiry, then by key.
func expiryKey(expiry, dedupeKey []byte) []byte {
	key := make([]byte, 0, len(expiry)+len(dedupeKey))
	return append(append(key, expiry...), dedupeKey...)
}

// purgeSeen deletes up to limit dedupe keys whose expiry is before now. A
// dedupe key re-recorded with a later expiry keeps its newer entry, because
// the stored value no longer equals the expired index key's prefix.
func purgeSeen(tx *bolt.Tx, now time.Time, limit int) error {
	seen := tx.Bucket(bucketSeen)
	index := tx.Bucket(bucketSeenExpiry)
	if seen == nil || index == nil {
		return nil
	}
	cutoff := make([]byte, 8)
	binary.BigEndian.PutUint64(cutoff, uint64(now.UTC().UnixNano()))
	var expired [][]byte
	cursor := index.Cursor()
	for key, _ := cursor.First(); key != nil && len(expired) < limit; key, _ = cursor.Next() {
		if len(key) < 8 || bytes.Compare(key[:8], cutoff) >= 0 {
			break
		}
		expired = append(expired, append([]byte(nil), key...))
	}
	for _, key := range expired {
		dedupeKey := key[8:]
		if bytes.Equal(seen.Get(dedupeKey), key[:8]) {
			if err := seen.Delete(dedupeKey); err != nil {
				return err
			}
		}
		if err := index.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

// purgeDone deletes up to limit acknowledged job records whose tombstone
// expired.
func purgeDone(tx *bolt.Tx, now time.Time, limit int) error {
	done := tx.Bucket(bucketDone)
	index := tx.Bucket(bucketDoneExpiry)
	if done == nil || index == nil {
		return nil
	}
	cutoff := make([]byte, 8)
	binary.BigEndian.PutUint64(cutoff, uint64(now.UTC().UnixNano()))
	var expired [][]byte
	cursor := index.Cursor()
	for key, _ := cursor.First(); key != nil && len(expired) < limit; key, _ = cursor.Next() {
		if len(key) < 8 || bytes.Compare(key[:8], cutoff) >= 0 {
			break
		}
		expired = append(expired, append([]byte(nil), key...))
	}
	for _, key := range expired {
		if err := done.Delete(key[8:]); err != nil {
			return err
		}
		if err := index.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

var _ Store = (*boltStore)(nil)
