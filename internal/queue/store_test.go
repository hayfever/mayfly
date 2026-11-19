package queue

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
)

func testConfig(t *testing.T) config.QueueConfig {
	t.Helper()
	cfg := config.Default().Queue
	cfg.Backend = "memory"
	cfg.MaxJobs = 10
	cfg.MaxAttempts = 2
	cfg.RetryBase = config.Duration(20 * time.Millisecond)
	// A claim stays leased for this long. Expiry tests use shortLeaseConfig:
	// the default must outlive an assertion on a loaded -race run, because
	// Stats reports an expired lease as pending again.
	cfg.VisibilityTimeout = config.Duration(time.Minute)
	cfg.DedupeTTL = config.Duration(time.Hour)
	return cfg
}

func testJob(delivery string) Job {
	return NewJob("generic", delivery, session.CreateOptions{
		Prompt:  "do the thing",
		Trigger: model.TriggerInfo{Kind: "generic", Name: "generic"},
	}, time.Now().UTC())
}

// backends returns every available store with the default test configuration
// on the system clock.
func backends(t *testing.T) map[string]Store {
	t.Helper()
	stores, _, _ := openBackends(t, testConfig(t), systemClock)
	return stores
}

// shortLeaseConfig makes a claim expire quickly on the store clock.
func shortLeaseConfig(t *testing.T) config.QueueConfig {
	t.Helper()
	cfg := testConfig(t)
	cfg.VisibilityTimeout = config.Duration(50 * time.Millisecond)
	return cfg
}

// backendsWith returns every available store for the conformance suite on the
// system clock.
func backendsWith(t *testing.T, cfg config.QueueConfig) map[string]Store {
	t.Helper()
	stores, _, _ := openBackends(t, cfg, systemClock)
	return stores
}

// testClock is a clock the test moves by hand, so lease and expiry
// assertions do not depend on wall-clock timing.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testBackends is one set of stores on a manually advanced clock.
type testBackends struct {
	stores        map[string]Store
	clock         *testClock
	redis         *miniredis.Miniredis
	externalRedis bool
}

// clockedBackends returns every available store on one test clock. Time moves
// only when a test calls advance, so a lease expires exactly when the test says
// it does.
func clockedBackends(t *testing.T, cfg config.QueueConfig) *testBackends {
	t.Helper()
	clock := newTestClock()
	stores, server, external := openBackends(t, cfg, clock.Now)
	return &testBackends{stores: stores, clock: clock, redis: server, externalRedis: external}
}

// advance moves the clock forward. Redis scheduling scores read the same
// clock, and the in-process server expires its keys with it.
func (b *testBackends) advance(d time.Duration) {
	b.clock.Advance(d)
	if b.redis != nil {
		b.redis.FastForward(d)
	}
}

// expire moves the clock forward and waits for a real Redis server, which
// expires keys on its own clock, to do the same.
func (b *testBackends) expire(d time.Duration) {
	b.advance(d)
	if b.externalRedis {
		time.Sleep(d)
	}
}

// openBackends builds every available store on one clock. Redis runs against an
// in-process miniredis unless MAYFLY_REDIS_TEST_URL names one.
func openBackends(t *testing.T, cfg config.QueueConfig, now clock) (map[string]Store, *miniredis.Miniredis, bool) {
	t.Helper()
	stores := map[string]Store{
		"memory": newMemoryWithClock(cfg, now),
	}

	boltCfg := cfg
	boltCfg.Backend = "bbolt"
	boltCfg.Bolt.Path = filepath.Join(t.TempDir(), "queue.db")
	bolt, err := newBoltWithClock(boltCfg, now)
	if err != nil {
		t.Fatal(err)
	}
	stores["bbolt"] = bolt

	var server *miniredis.Miniredis
	url := os.Getenv("MAYFLY_REDIS_TEST_URL")
	if url == "" {
		server = miniredis.RunT(t)
		// Expire keys as the clock advances; miniredis otherwise only
		// expires on FastForward.
		server.ClockTTL(true)
		url = "redis://" + server.Addr()
	}
	redisCfg := cfg
	redisCfg.Backend = "redis"
	redisCfg.Redis.URL = url
	// A fresh prefix keeps repeat runs independent.
	redisCfg.Redis.KeyPrefix = fmt.Sprintf("mayfly-test-%d", time.Now().UnixNano())
	redisBackend, err := newRedisWithClock(context.Background(), redisCfg, now)
	if err != nil {
		t.Fatal(err)
	}
	// Clear keys from an earlier run so counts start at zero.
	ctx := context.Background()
	_ = redisBackend.client.Del(ctx, redisBackend.queue, redisBackend.jobs, redisBackend.dead).Err()
	stores["redis"] = redisBackend
	for name, store := range stores {
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("%s close: %v", name, err)
			}
		})
	}
	return stores, server, server == nil
}

func TestStoreLifecycle(t *testing.T) {
	backends := clockedBackends(t, testConfig(t))
	for name, store := range backends.stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			job := testJob("delivery-1")
			if err := store.Accept(ctx, job); err != nil {
				t.Fatal(err)
			}
			stats, err := store.Stats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Pending != 1 || stats.Leased != 0 || stats.Dead != 0 {
				t.Fatalf("stats after accept = %+v", stats)
			}
			leases, err := store.Claim(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(leases) != 1 || leases[0].ID != job.ID {
				t.Fatalf("claim = %+v", leases)
			}
			if leases[0].Job.Options.Prompt != "do the thing" {
				t.Fatalf("job payload = %+v", leases[0].Job)
			}
			stats, _ = store.Stats(ctx)
			if stats.Pending != 0 || stats.Leased != 1 {
				t.Fatalf("stats after claim = %+v", stats)
			}
			if err := store.Ack(ctx, leases[0], Result{SessionID: "session-1"}); err != nil {
				t.Fatal(err)
			}
			stats, _ = store.Stats(ctx)
			if stats.Pending != 0 || stats.Leased != 0 || stats.Dead != 0 {
				t.Fatalf("stats after ack = %+v", stats)
			}
		})
	}
}

func TestStoreDedupe(t *testing.T) {
	for name, store := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := store.Accept(ctx, testJob("same-delivery")); err != nil {
				t.Fatal(err)
			}
			if err := store.Accept(ctx, testJob("same-delivery")); !errors.Is(err, ErrDuplicate) {
				t.Fatalf("duplicate accept error = %v, want ErrDuplicate", err)
			}
			// Without a delivery key every request is new work.
			if err := store.Accept(ctx, testJob("")); err != nil {
				t.Fatal(err)
			}
			if err := store.Accept(ctx, testJob("")); err != nil {
				t.Fatal(err)
			}
			stats, _ := store.Stats(ctx)
			if stats.Pending != 3 {
				t.Fatalf("pending = %d, want 3", stats.Pending)
			}
		})
	}
}

func TestStoreDedupeIsPerTrigger(t *testing.T) {
	for name, store := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			opts := session.CreateOptions{Prompt: "p", Trigger: model.TriggerInfo{Kind: "generic"}}
			now := time.Now()
			if err := store.Accept(ctx, NewJob("trigger-a", "key-1", opts, now)); err != nil {
				t.Fatal(err)
			}
			if err := store.Accept(ctx, NewJob("trigger-b", "key-1", opts, now)); err != nil {
				t.Fatalf("same key on another trigger = %v, want nil", err)
			}
			if err := store.Accept(ctx, NewJob("trigger-a", "key-1", opts, now)); !errors.Is(err, ErrDuplicate) {
				t.Fatalf("repeat on the same trigger = %v, want ErrDuplicate", err)
			}
		})
	}
}

func TestStoreVisibilityExpiryReclaims(t *testing.T) {
	cfg := testConfig(t)
	backends := clockedBackends(t, cfg)
	for name, store := range backends.stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := store.Accept(ctx, testJob("lease-me")); err != nil {
				t.Fatal(err)
			}
			first, err := store.Claim(ctx, 1)
			if err != nil || len(first) != 1 {
				t.Fatalf("first claim = %v, %v", first, err)
			}
			if again, _ := store.Claim(ctx, 1); len(again) != 0 {
				t.Fatalf("job claimed twice: %+v", again)
			}
			backends.advance(cfg.VisibilityTimeout.Std() + time.Millisecond)
			second, err := store.Claim(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(second) != 1 {
				t.Fatal("expired lease was not reclaimed")
			}
			if err := store.Ack(ctx, second[0], Result{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStoreRetryThenDead(t *testing.T) {
	cfg := testConfig(t)
	backends := clockedBackends(t, cfg)
	for name, store := range backends.stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := store.Accept(ctx, testJob("fails")); err != nil {
				t.Fatal(err)
			}
			lease := claimOne(t, store)
			if err := store.Nack(ctx, lease, errors.New("boom")); err != nil {
				t.Fatal(err)
			}
			stats, _ := store.Stats(ctx)
			if stats.Dead != 0 {
				t.Fatalf("job buried after one attempt: %+v", stats)
			}
			backends.advance(Backoff(1, cfg.RetryBase.Std()))
			retry := claimOne(t, store)
			if retry.Job.Attempts != 1 {
				t.Fatalf("attempts = %d, want 1", retry.Job.Attempts)
			}
			if err := store.Nack(ctx, retry, errors.New("boom again")); err != nil {
				t.Fatal(err)
			}
			stats, _ = store.Stats(ctx)
			if stats.Dead != 1 || stats.Pending != 0 || stats.Leased != 0 {
				t.Fatalf("stats after max attempts = %+v", stats)
			}
			if leases, _ := store.Claim(ctx, 1); len(leases) != 0 {
				t.Fatal("buried job was claimed")
			}
		})
	}
}

func TestStoreReleaseDefers(t *testing.T) {
	backends := clockedBackends(t, testConfig(t))
	for name, store := range backends.stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := store.Accept(ctx, testJob("defer")); err != nil {
				t.Fatal(err)
			}
			lease := claimOne(t, store)
			const delay = 150 * time.Millisecond
			if err := store.Release(ctx, lease, delay); err != nil {
				t.Fatal(err)
			}
			if leases, _ := store.Claim(ctx, 1); len(leases) != 0 {
				t.Fatal("released job was claimed before its delay")
			}
			stats, _ := store.Stats(ctx)
			if stats.Leased != 1 || stats.Dead != 0 {
				t.Fatalf("stats after release = %+v", stats)
			}
			backends.advance(delay)
			if got := claimOne(t, store); got.ID != lease.ID {
				t.Fatalf("released job = %s, want %s", got.ID, lease.ID)
			}
		})
	}
}

func TestStoreDeadJobsDoNotCountTowardMaxJobs(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxJobs = 1
	cfg.MaxAttempts = 1
	for name, store := range backendsWith(t, cfg) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := store.Accept(ctx, testJob("")); err != nil {
				t.Fatal(err)
			}
			if err := store.Nack(ctx, claimOne(t, store), errors.New("boom")); err != nil {
				t.Fatal(err)
			}
			if stats, _ := store.Stats(ctx); stats.Dead != 1 {
				t.Fatalf("stats = %+v, want one dead job", stats)
			}
			if err := store.Accept(ctx, testJob("")); err != nil {
				t.Fatalf("accept after a burial = %v, want nil", err)
			}
		})
	}
}

func TestRedisBuriesUndecodableJob(t *testing.T) {
	store, ok := backends(t)["redis"].(*redisStore)
	if !ok {
		t.Fatal("redis backend is not a *redisStore")
	}
	ctx := context.Background()
	if err := store.client.HSet(ctx, store.jobs, "bad", "{not json").Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.client.ZAdd(ctx, store.queue, redis.Z{Score: 0, Member: "bad"}).Err(); err != nil {
		t.Fatal(err)
	}
	leases, err := store.Claim(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("claim = %+v, want no leases", leases)
	}
	if stats, _ := store.Stats(ctx); stats.Dead != 1 {
		t.Fatalf("stats = %+v, want one dead job", stats)
	}
	if again, _ := store.Claim(ctx, 10); len(again) != 0 {
		t.Fatalf("second claim = %+v, want no leases", again)
	}
}

func TestStoreFull(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxJobs = 1
	store, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Accept(ctx, testJob("one")); err != nil {
		t.Fatal(err)
	}
	if err := store.Accept(ctx, testJob("two")); !errors.Is(err, ErrFull) {
		t.Fatalf("second accept error = %v, want ErrFull", err)
	}
}

func TestBoltSurvivesReopen(t *testing.T) {
	cfg := testConfig(t)
	cfg.Backend = "bbolt"
	cfg.Bolt.Path = filepath.Join(t.TempDir(), "queue.db")
	store, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Accept(context.Background(), testJob("persist")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stats, err := reopened.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Fatalf("pending after reopen = %+v", stats)
	}
}

func TestBoltPurgesExpiredDedupeKeys(t *testing.T) {
	cfg := testConfig(t)
	cfg.Backend = "bbolt"
	cfg.MaxJobs = 0
	cfg.Bolt.Path = filepath.Join(t.TempDir(), "queue.db")
	cfg.DedupeTTL = config.Duration(20 * time.Millisecond)
	clock := newTestClock()
	store, err := newBoltWithClock(cfg, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for i := range 50 {
		if err := store.Accept(ctx, testJob(fmt.Sprintf("old-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(cfg.DedupeTTL.Std() + time.Nanosecond)
	if err := store.Accept(ctx, testJob("fresh")); err != nil {
		t.Fatal(err)
	}
	if err := store.Accept(ctx, testJob("fresh")); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("fresh duplicate = %v, want ErrDuplicate", err)
	}
	var seen int
	_ = store.db.View(func(tx *bolt.Tx) error {
		seen = tx.Bucket(bucketSeen).Stats().KeyN
		return nil
	})
	if seen != 1 {
		t.Fatalf("dedupe keys after purge = %d, want 1", seen)
	}
}

func TestBoltBackfillsDedupeIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	expiry := make([]byte, 8)
	binary.BigEndian.PutUint64(expiry, uint64(time.Now().Add(-time.Hour).UnixNano()))
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		seen, err := tx.CreateBucket(bucketSeen)
		if err != nil {
			return err
		}
		for _, key := range []string{"d-old-1", "d-old-2"} {
			if err := seen.Put([]byte(key), expiry); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	cfg.Backend = "bbolt"
	cfg.MaxJobs = 0
	cfg.Bolt.Path = path
	store, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Accept(context.Background(), testJob("upgrade")); err != nil {
		t.Fatal(err)
	}
	var seen int
	_ = store.(*boltStore).db.View(func(tx *bolt.Tx) error {
		seen = tx.Bucket(bucketSeen).Stats().KeyN
		return nil
	})
	if seen != 1 {
		t.Fatalf("dedupe keys after backfill purge = %d, want 1", seen)
	}
}

func claimOne(t *testing.T, store Store) Lease {
	t.Helper()
	leases, err := store.Claim(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("claim returned %d leases", len(leases))
	}
	return leases[0]
}

func TestStoreStaleLeaseCannotSettle(t *testing.T) {
	cfg := shortLeaseConfig(t)
	backends := clockedBackends(t, cfg)
	for name, store := range backends.stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := store.Accept(ctx, testJob("stale")); err != nil {
				t.Fatal(err)
			}
			stale := claimOne(t, store)
			backends.advance(cfg.VisibilityTimeout.Std())
			current := claimOne(t, store)
			if err := store.Nack(ctx, stale, errors.New("late failure")); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("stale nack = %v, want ErrLeaseLost", err)
			}
			if err := store.Release(ctx, stale, 0); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("stale release = %v, want ErrLeaseLost", err)
			}
			if leases, _ := store.Claim(ctx, 10); len(leases) != 0 {
				t.Fatalf("job claimable while another lease holds it: %+v", leases)
			}
			if err := store.Nack(ctx, current, errors.New("real failure")); err != nil {
				t.Fatalf("current nack = %v", err)
			}
			backends.advance(Backoff(1, cfg.RetryBase.Std()))
			if retry := claimOne(t, store); retry.Job.Attempts != 1 {
				t.Fatalf("attempts = %d, want 1", retry.Job.Attempts)
			}
		})
	}
}

func TestStoreRecordsAndDeadLetters(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxJobs = 0
	cfg.MaxAttempts = 1
	cfg.TombstoneTTL = config.Duration(time.Hour)
	for name, store := range backendsWith(t, cfg) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			job := testJob("lookup")
			if err := store.Accept(ctx, job); err != nil {
				t.Fatal(err)
			}
			record, ok, err := store.Get(ctx, job.ID)
			if err != nil || !ok {
				t.Fatalf("get accepted job = %+v, %v, %v", record, ok, err)
			}
			if record.State != StateQueued {
				t.Fatalf("state = %s, want queued", record.State)
			}
			if record.Job.Options.Trigger.JobID != job.ID {
				t.Fatalf("trigger job id = %q, want %q", record.Job.Options.Trigger.JobID, job.ID)
			}

			lease := claimOne(t, store)
			if record, _, _ = store.Get(ctx, job.ID); record.State != StateLeased {
				t.Fatalf("state after claim = %s, want leased", record.State)
			}
			if err := store.Ack(ctx, lease, Result{SessionID: "session-42"}); err != nil {
				t.Fatal(err)
			}
			record, ok, err = store.Get(ctx, job.ID)
			if err != nil || !ok {
				t.Fatalf("get completed job = %+v, %v, %v", record, ok, err)
			}
			if record.State != StateDone || record.SessionID != "session-42" {
				t.Fatalf("completed record = %+v", record)
			}
			if _, ok, err := store.Get(ctx, "missing"); ok || err != nil {
				t.Fatalf("unknown job = %v, %v", ok, err)
			}

			doomed := testJob("doomed")
			if err := store.Accept(ctx, doomed); err != nil {
				t.Fatal(err)
			}
			failure := claimOne(t, store)
			if err := store.Nack(ctx, failure, errors.New("provider rejected the prompt")); err != nil {
				t.Fatal(err)
			}
			dead, err := store.Dead(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(dead) != 1 || dead[0].Job.ID != doomed.ID {
				t.Fatalf("dead list = %+v", dead)
			}
			if dead[0].State != StateDead || !strings.Contains(dead[0].Error, "provider rejected") {
				t.Fatalf("dead record = %+v", dead[0])
			}

			if err := store.Retry(ctx, doomed.ID); err != nil {
				t.Fatal(err)
			}
			record, ok, err = store.Get(ctx, doomed.ID)
			if err != nil || !ok {
				t.Fatalf("get retried job = %+v, %v, %v", record, ok, err)
			}
			if record.State != StateQueued || record.Job.Attempts != 0 {
				t.Fatalf("retried record = %+v", record)
			}
			if dead, _ := store.Dead(ctx, 10); len(dead) != 0 {
				t.Fatalf("dead list after retry = %+v", dead)
			}
			if err := store.Retry(ctx, "missing"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("retry unknown job = %v, want ErrNotFound", err)
			}

			failure = claimOne(t, store)
			if err := store.Nack(ctx, failure, errors.New("second failure")); err != nil {
				t.Fatal(err)
			}
			if err := store.Delete(ctx, doomed.ID); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := store.Get(ctx, doomed.ID); ok {
				t.Fatal("deleted job is still readable")
			}
			if err := store.Delete(ctx, doomed.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("second delete = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestStoreExpiresDoneTombstones(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxJobs = 0
	cfg.MaxAttempts = 1
	cfg.TombstoneTTL = config.Duration(100 * time.Millisecond)
	backends := clockedBackends(t, cfg)
	for name, store := range backends.stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			job := testJob("tombstone")
			if err := store.Accept(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := store.Ack(ctx, claimOne(t, store), Result{SessionID: "sess"}); err != nil {
				t.Fatal(err)
			}
			backends.expire(cfg.TombstoneTTL.Std() + time.Nanosecond)
			// bbolt and memory purge on the next accept; Redis expires the key.
			if err := store.Accept(ctx, testJob("")); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := store.Get(ctx, job.ID); ok || err != nil {
				t.Fatalf("expired tombstone = %v, %v; want absent", ok, err)
			}
		})
	}
}

func TestStoreExtendKeepsLease(t *testing.T) {
	cfg := shortLeaseConfig(t)
	ttl := cfg.VisibilityTimeout.Std()
	backends := clockedBackends(t, cfg)
	for name, store := range backends.stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := store.Accept(ctx, testJob("extend")); err != nil {
				t.Fatal(err)
			}
			lease := claimOne(t, store)
			// Renew before each visibility timeout runs out, twice.
			for i := range 2 {
				backends.advance(30 * time.Millisecond)
				renewed, err := store.Extend(ctx, lease, ttl)
				if err != nil {
					t.Fatalf("extend %d = %v", i, err)
				}
				lease = renewed
			}
			backends.advance(30 * time.Millisecond)
			if leases, _ := store.Claim(ctx, 10); len(leases) != 0 {
				t.Fatalf("renewed job was claimed again: %+v", leases)
			}
			if err := store.Release(ctx, lease, 0); err != nil {
				t.Fatalf("release with renewed lease = %v", err)
			}
			stale := claimOne(t, store)
			backends.advance(ttl + time.Millisecond)
			current := claimOne(t, store)
			if _, err := store.Extend(ctx, stale, time.Second); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("stale extend = %v, want ErrLeaseLost", err)
			}
			if err := store.Ack(ctx, current, Result{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
