package queue

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

// memoryStore keeps jobs in process memory. It is for development and tests:
// a restart drops accepted work.
type memoryStore struct {
	cfg  config.QueueConfig
	mu   sync.Mutex
	jobs map[string]*memEntry
	seen map[string]time.Time
}

type memEntry struct {
	job     Job
	readyAt time.Time
	dead    bool
}

func newMemory(cfg config.QueueConfig) *memoryStore {
	return &memoryStore{
		cfg:  cfg,
		jobs: map[string]*memEntry{},
		seen: map[string]time.Time{},
	}
}

func (s *memoryStore) Accept(_ context.Context, job Job) error {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, expiry := range s.seen {
		if now.After(expiry) {
			delete(s.seen, key)
		}
	}
	if job.Delivery != "" {
		if _, ok := s.seen[job.Delivery]; ok {
			return ErrDuplicate
		}
	}
	if s.cfg.MaxJobs > 0 && s.live() >= s.cfg.MaxJobs {
		return ErrFull
	}
	s.jobs[job.ID] = &memEntry{job: job, readyAt: now}
	if job.Delivery != "" {
		s.seen[job.Delivery] = now.Add(s.cfg.DedupeTTL.Std())
	}
	return nil
}

// live counts jobs that are not buried. The caller holds mu.
func (s *memoryStore) live() int {
	n := 0
	for _, entry := range s.jobs {
		if !entry.dead {
			n++
		}
	}
	return n
}

func (s *memoryStore) Claim(_ context.Context, max int) ([]Lease, error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	type candidate struct {
		id      string
		readyAt time.Time
	}
	ready := make([]candidate, 0, max)
	for id, entry := range s.jobs {
		if entry.dead || !due(entry.readyAt, now) {
			continue
		}
		ready = append(ready, candidate{id: id, readyAt: entry.readyAt})
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].readyAt.Before(ready[j].readyAt) })
	if len(ready) > max {
		ready = ready[:max]
	}
	leases := make([]Lease, 0, len(ready))
	for _, c := range ready {
		entry := s.jobs[c.id]
		entry.readyAt = now.Add(s.cfg.VisibilityTimeout.Std())
		leases = append(leases, Lease{ID: c.id, Job: entry.job, Deadline: entry.readyAt})
	}
	return leases, nil
}

func (s *memoryStore) Ack(_ context.Context, lease Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, lease.ID)
	return nil
}

func (s *memoryStore) Nack(_ context.Context, lease Lease, _ error) error {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[lease.ID]
	if !ok {
		return nil
	}
	if entry.dead || !entry.readyAt.Equal(lease.Deadline) {
		return ErrLeaseLost
	}
	entry.job.Attempts++
	if max := s.cfg.MaxAttempts; max > 0 && entry.job.Attempts >= max {
		entry.dead = true
		return nil
	}
	entry.readyAt = now.Add(Backoff(entry.job.Attempts, s.cfg.RetryBase.Std()))
	return nil
}

func (s *memoryStore) Release(_ context.Context, lease Lease, after time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[lease.ID]
	if !ok {
		return nil
	}
	if entry.dead || !entry.readyAt.Equal(lease.Deadline) {
		return ErrLeaseLost
	}
	entry.readyAt = time.Now().UTC().Add(after)
	return nil
}

func (s *memoryStore) Stats(_ context.Context) (Stats, error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	var stats Stats
	for _, entry := range s.jobs {
		switch {
		case entry.dead:
			stats.Dead++
		case due(entry.readyAt, now):
			stats.Pending++
		default:
			stats.Leased++
		}
		if !entry.dead && (stats.Oldest.IsZero() || entry.job.AcceptedAt.Before(stats.Oldest)) {
			stats.Oldest = entry.job.AcceptedAt
		}
	}
	return stats, nil
}

func (s *memoryStore) Close() error { return nil }
