package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

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
	cfg.VisibilityTimeout = config.Duration(50 * time.Millisecond)
	cfg.DedupeTTL = config.Duration(time.Hour)
	return cfg
}

func testJob(delivery string) Job {
	return NewJob("generic", delivery, session.CreateOptions{
		Prompt:  "do the thing",
		Trigger: model.TriggerInfo{Kind: "generic", Name: "generic"},
	}, time.Now().UTC())
}

// backends returns every available store with the default test configuration.
func backends(t *testing.T) map[string]Store {
	t.Helper()
	return backendsWith(t, testConfig(t))
}

// backendsWith returns every available store for the conformance suite. Redis
// runs against an in-process miniredis unless MAYFLY_REDIS_TEST_URL names one.
func backendsWith(t *testing.T, cfg config.QueueConfig) map[string]Store {
	t.Helper()
	stores := map[string]Store{}
	memory, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	stores["memory"] = memory

	boltCfg := cfg
	boltCfg.Backend = "bbolt"
	boltCfg.Bolt.Path = filepath.Join(t.TempDir(), "queue.db")
	bolt, err := New(context.Background(), boltCfg)
	if err != nil {
		t.Fatal(err)
	}
	stores["bbolt"] = bolt

	url := os.Getenv("MAYFLY_REDIS_TEST_URL")
	if url == "" {
		url = "redis://" + miniredis.RunT(t).Addr()
	}
	redisCfg := cfg
	redisCfg.Backend = "redis"
	redisCfg.Redis.URL = url
	// A fresh prefix keeps repeat runs independent.
	redisCfg.Redis.KeyPrefix = fmt.Sprintf("mayfly-test-%d", time.Now().UnixNano())
	redisBackend, err := New(context.Background(), redisCfg)
	if err != nil {
		t.Fatal(err)
	}
	// Clear keys from an earlier run so counts start at zero.
	if store, ok := redisBackend.(*redisStore); ok {
		ctx := context.Background()
		_ = store.client.Del(ctx, store.queue, store.jobs, store.dead).Err()
	}
	stores["redis"] = redisBackend
	for name, store := range stores {
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("%s close: %v", name, err)
			}
		})
	}
	return stores
}

func TestStoreLifecycle(t *testing.T) {
	for name, store := range backends(t) {
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
			if err := store.Ack(ctx, leases[0]); err != nil {
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

func TestStoreVisibilityExpiryReclaims(t *testing.T) {
	for name, store := range backends(t) {
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
			time.Sleep(80 * time.Millisecond)
			second, err := store.Claim(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(second) != 1 {
				t.Fatal("expired lease was not reclaimed")
			}
			if err := store.Ack(ctx, second[0]); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStoreRetryThenDead(t *testing.T) {
	for name, store := range backends(t) {
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
			time.Sleep(40 * time.Millisecond)
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
	for name, store := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := store.Accept(ctx, testJob("defer")); err != nil {
				t.Fatal(err)
			}
			lease := claimOne(t, store)
			if err := store.Release(ctx, lease, 10*time.Second); err != nil {
				t.Fatal(err)
			}
			if leases, _ := store.Claim(ctx, 1); len(leases) != 0 {
				t.Fatal("released job was claimed before its delay")
			}
			stats, _ := store.Stats(ctx)
			if stats.Leased != 1 || stats.Dead != 0 {
				t.Fatalf("stats after release = %+v", stats)
			}
			if err := store.Release(ctx, lease, 0); err != nil {
				t.Fatal(err)
			}
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
