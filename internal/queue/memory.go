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
	now  clock
	mu   sync.Mutex
	jobs map[string]*memEntry
	seen map[string]time.Time
}

type memEntry struct {
	job        Job
	readyAt    time.Time
	dead       bool
	deadErr    string
	deadAt     time.Time
	done       bool
	sessionID  string
	finishedAt time.Time
}

func newMemory(cfg config.QueueConfig) *memoryStore {
	return newMemoryWithClock(cfg, systemClock)
}

func newMemoryWithClock(cfg config.QueueConfig, now clock) *memoryStore {
	return &memoryStore{
		cfg:  cfg,
		now:  now,
		jobs: map[string]*memEntry{},
		seen: map[string]time.Time{},
	}
}

func (s *memoryStore) Accept(_ context.Context, job Job) error {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	for key, expiry := range s.seen {
		if now.After(expiry) {
			delete(s.seen, key)
		}
	}
	key := dedupeKey(job)
	if key != "" {
		if _, ok := s.seen[key]; ok {
			return ErrDuplicate
		}
	}
	if s.cfg.MaxJobs > 0 && s.liveLocked() >= s.cfg.MaxJobs {
		return ErrFull
	}
	s.jobs[job.ID] = &memEntry{job: job, readyAt: now}
	if key != "" {
		s.seen[key] = now.Add(s.cfg.DedupeTTL.Std())
	}
	return nil
}

// liveLocked counts jobs that are neither buried nor finished. The caller
// holds mu.
func (s *memoryStore) liveLocked() int {
	n := 0
	for _, entry := range s.jobs {
		if !entry.dead && !entry.done {
			n++
		}
	}
	return n
}

// sweepLocked drops finished jobs whose tombstone expired. The caller holds mu.
func (s *memoryStore) sweepLocked(now time.Time) {
	ttl := s.cfg.TombstoneTTL.Std()
	if ttl <= 0 {
		return
	}
	for id, entry := range s.jobs {
		if entry.done && now.Sub(entry.finishedAt) > ttl {
			delete(s.jobs, id)
		}
	}
}

func (s *memoryStore) Claim(_ context.Context, max int) ([]Lease, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	type candidate struct {
		id      string
		readyAt time.Time
	}
	ready := make([]candidate, 0, max)
	for id, entry := range s.jobs {
		if entry.dead || entry.done || !due(entry.readyAt, now) {
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
		if max := s.cfg.MaxAttempts; max > 0 && entry.job.Attempts >= max {
			// The last attempt was never settled: the worker died, or its
			// process was killed. Count it once more only to bury the job.
			entry.dead = true
			entry.deadErr = expiredLeaseError(entry.job.Attempts)
			entry.deadAt = now
			continue
		}
		entry.job.Attempts++
		entry.readyAt = now.Add(s.cfg.VisibilityTimeout.Std())
		leases = append(leases, Lease{ID: c.id, Job: entry.job, Deadline: entry.readyAt})
	}
	return leases, nil
}

func (s *memoryStore) Ack(_ context.Context, lease Lease, result Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[lease.ID]
	if !ok {
		return nil
	}
	entry.done = true
	if entry.job.Attempts > 0 {
		entry.job.Attempts--
	}
	entry.sessionID = result.SessionID
	entry.finishedAt = s.now()
	return nil
}

func (s *memoryStore) Nack(_ context.Context, lease Lease, cause error) error {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[lease.ID]
	if !ok {
		return nil
	}
	if entry.done || entry.dead || !entry.readyAt.Equal(lease.Deadline) {
		return ErrLeaseLost
	}
	if max := s.cfg.MaxAttempts; max > 0 && entry.job.Attempts >= max {
		entry.dead = true
		entry.deadErr = causeString(cause)
		entry.deadAt = now
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
	if entry.done || entry.dead || !entry.readyAt.Equal(lease.Deadline) {
		return ErrLeaseLost
	}
	if entry.job.Attempts > 0 {
		entry.job.Attempts--
	}
	entry.readyAt = s.now().Add(after)
	return nil
}

func (s *memoryStore) Extend(_ context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[lease.ID]
	if !ok || entry.done || entry.dead || !entry.readyAt.Equal(lease.Deadline) {
		return lease, ErrLeaseLost
	}
	entry.readyAt = s.now().Add(ttl)
	lease.Deadline = entry.readyAt
	return lease, nil
}

func (s *memoryStore) Get(_ context.Context, id string) (Record, bool, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	entry, ok := s.jobs[id]
	if !ok {
		return Record{}, false, nil
	}
	record := Record{Job: entry.job}
	switch {
	case entry.done:
		record.State = StateDone
		record.SessionID = entry.sessionID
		record.FinishedAt = entry.finishedAt
	case entry.dead:
		record.State = StateDead
		record.Error = entry.deadErr
		record.FailedAt = entry.deadAt
	case due(entry.readyAt, now):
		record.State = StateQueued
	default:
		record.State = StateLeased
	}
	return record, true, nil
}

func (s *memoryStore) Dead(_ context.Context, limit int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0)
	for _, entry := range s.jobs {
		if !entry.dead {
			continue
		}
		out = append(out, Record{
			Job:      entry.job,
			State:    StateDead,
			Error:    entry.deadErr,
			FailedAt: entry.deadAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FailedAt.After(out[j].FailedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memoryStore) Retry(_ context.Context, id string) error {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[id]
	if !ok || !entry.dead {
		return ErrNotFound
	}
	entry.dead = false
	entry.deadErr = ""
	entry.deadAt = time.Time{}
	entry.job.Attempts = 0
	entry.readyAt = now
	return nil
}

func (s *memoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.jobs[id]
	if !ok || !entry.dead {
		return ErrNotFound
	}
	delete(s.jobs, id)
	return nil
}

func (s *memoryStore) Stats(_ context.Context) (Stats, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	var stats Stats
	for _, entry := range s.jobs {
		switch {
		case entry.dead:
			stats.Dead++
		case entry.done:
			continue
		case due(entry.readyAt, now):
			stats.Pending++
		default:
			stats.Leased++
		}
		if !entry.dead && !entry.done && (stats.Oldest.IsZero() || entry.job.AcceptedAt.Before(stats.Oldest)) {
			stats.Oldest = entry.job.AcceptedAt
		}
	}
	return stats, nil
}

func (s *memoryStore) Close() error { return nil }
