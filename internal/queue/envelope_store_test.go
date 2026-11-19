package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	bolt "go.etcd.io/bbolt"
)

// TestBoltBuriesUnreadablePayload covers a job written by a newer build: the
// worker must move it out of the queue with the decode reason, not crash.
func TestBoltBuriesUnreadablePayload(t *testing.T) {
	ctx := context.Background()
	cfg := bboltStoreConfig(t)
	first, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"version":2,"kind":"session.create","created_at":"2026-11-18T09:00:00Z","prompt":"from the future","trigger":{"kind":"generic"}}`)
	injectBoltJob(t, cfg.Bolt.Path, "future", payload)

	store, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	leases, err := store.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("unknown version was leased: %+v", leases)
	}
	assertBuriedVersion(t, store, "future", "version 2")
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 || stats.Dead != 1 {
		t.Fatalf("stats = %+v, want one dead job", stats)
	}
}

func TestRedisBuriesUnreadablePayload(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	cfg := testConfig(t)
	cfg.Backend = "redis"
	cfg.Redis.URL = "redis://" + server.Addr()
	cfg.Redis.KeyPrefix = "mayfly-envelope-test"
	store, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	redisStore := store.(*redisStore)

	payload := `{"version":2,"kind":"session.create","created_at":"2026-11-18T09:00:00Z","prompt":"from the future","trigger":{"kind":"generic"}}`
	if err := redisStore.client.HSet(ctx, redisStore.jobs, "future", payload).Err(); err != nil {
		t.Fatal(err)
	}
	if err := redisStore.client.ZAdd(ctx, redisStore.queue, redis.Z{Score: float64(time.Now().Add(-time.Second).UnixMilli()), Member: "future"}).Err(); err != nil {
		t.Fatal(err)
	}

	leases, err := store.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("unknown version was leased: %+v", leases)
	}
	assertBuriedVersion(t, store, "future", "version 2")
	if n := redisStore.client.ZCard(ctx, redisStore.queue).Val(); n != 0 {
		t.Fatalf("queue still holds %d members", n)
	}
	if n := redisStore.client.HLen(ctx, redisStore.jobs).Val(); n != 0 {
		t.Fatalf("jobs hash still holds %d fields", n)
	}
}

// TestBoltDrainsLegacyPayload runs the v0.1.0 shape through claim, so an
// upgraded control plane finishes work accepted by the older build.
func TestBoltDrainsLegacyPayload(t *testing.T) {
	ctx := context.Background()
	cfg := bboltStoreConfig(t)
	first, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"id":"d-legacy","kind":"session","delivery":"delivery-1","trigger":"repo-hooks","options":{"Prompt":"legacy prompt","Title":"","Model":"","TTL":900000000000,"Skills":null,"MCPServers":null,"Agent":"","Trigger":{"kind":"github","name":"repo-hooks","event":"pull_request","delivery":"delivery-1","job_id":"d-legacy"},"Artifacts":null,"IdempotencyKey":""},"source":null,"accepted_at":"2026-01-02T03:04:05Z","attempts":0}`)
	injectBoltJob(t, cfg.Bolt.Path, "d-legacy", payload)

	store, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	leases, err := store.Claim(ctx, 1)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("claim = %+v, want the legacy job", leases)
	}
	job := leases[0].Job
	if job.Options.Prompt != "legacy prompt" || job.Options.TTL != 15*time.Minute ||
		job.JobKind() != KindSession || job.Options.IdempotencyKey != "d-legacy" {
		t.Fatalf("legacy job = %+v", job)
	}
	if err := store.Ack(ctx, leases[0], Result{SessionID: "sess-legacy"}); err != nil {
		t.Fatal(err)
	}
	record, ok, err := store.Get(ctx, "d-legacy")
	if err != nil || !ok || record.State != StateDone || record.SessionID != "sess-legacy" {
		t.Fatalf("record = %+v, %v, %v", record, ok, err)
	}
}

func TestRedisDrainsLegacyPayload(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	cfg := testConfig(t)
	cfg.Backend = "redis"
	cfg.Redis.URL = "redis://" + server.Addr()
	cfg.Redis.KeyPrefix = "mayfly-envelope-legacy"
	store, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	redisStore := store.(*redisStore)

	payload := `{"id":"d-legacy","kind":"session","delivery":"delivery-1","trigger":"repo-hooks","options":{"Prompt":"legacy prompt","Title":"","Model":"","TTL":900000000000,"Skills":null,"MCPServers":null,"Agent":"","Trigger":{"kind":"github","name":"repo-hooks","event":"pull_request","delivery":"delivery-1","job_id":"d-legacy"},"Artifacts":null,"IdempotencyKey":""},"source":null,"accepted_at":"2026-01-02T03:04:05Z","attempts":0}`
	if err := redisStore.client.HSet(ctx, redisStore.jobs, "d-legacy", payload).Err(); err != nil {
		t.Fatal(err)
	}
	if err := redisStore.client.ZAdd(ctx, redisStore.queue, redis.Z{Score: float64(time.Now().Add(-time.Second).UnixMilli()), Member: "d-legacy"}).Err(); err != nil {
		t.Fatal(err)
	}

	leases, err := store.Claim(ctx, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim = %+v, %v", leases, err)
	}
	if leases[0].Job.Options.Prompt != "legacy prompt" || leases[0].Job.Options.TTL != 15*time.Minute {
		t.Fatalf("legacy job = %+v", leases[0].Job)
	}
}

func injectBoltJob(t *testing.T, path, id string, payload []byte) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketJobs).Put([]byte(id), payload); err != nil {
			return err
		}
		return tx.Bucket(bucketReady).Put(readyKey(time.Now().Add(-time.Second), id), nil)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertBuriedVersion(t *testing.T, store Store, id, reason string) {
	t.Helper()
	records, err := store.Dead(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("dead records = %+v, want one", records)
	}
	if records[0].Job.ID != id {
		t.Fatalf("dead job id = %q, want %q", records[0].Job.ID, id)
	}
	if !strings.Contains(records[0].Error, reason) {
		t.Fatalf("dead reason = %q, want it to contain %q", records[0].Error, reason)
	}
}
