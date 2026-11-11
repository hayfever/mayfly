package queue

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/config"
)

var (
	bucketJobs  = []byte("jobs")
	bucketReady = []byte("ready")
	bucketSeen  = []byte("seen")
	bucketDead  = []byte("dead")
)

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
		for _, name := range [][]byte{bucketJobs, bucketReady, bucketSeen, bucketDead} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create queue buckets: %w", err)
	}
	return &boltStore{cfg: cfg, db: db}, nil
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
		if job.Delivery != "" {
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
		if job.Delivery != "" {
			expiry := make([]byte, 8)
			binary.BigEndian.PutUint64(expiry, uint64(now.Add(s.cfg.DedupeTTL.Std()).UnixNano()))
			if err := seen.Put([]byte(job.ID), expiry); err != nil {
				return err
			}
		}
		return nil
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
				return err
			}
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

func (s *boltStore) Ack(_ context.Context, lease Lease) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketJobs).Delete([]byte(lease.ID)); err != nil {
			return err
		}
		return tx.Bucket(bucketReady).Delete(readyKey(lease.Deadline, lease.ID))
	})
}

func (s *boltStore) Nack(_ context.Context, lease Lease, _ error) error {
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
			buried, err := json.Marshal(job)
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
				return err
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

func splitReadyKey(key []byte) (time.Time, string) {
	if len(key) < 8 {
		return time.Time{}, ""
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(key[:8]))).UTC(), string(key[8:])
}

var _ Store = (*boltStore)(nil)
