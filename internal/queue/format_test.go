package queue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/config"
)

func bboltStoreConfig(t *testing.T) config.QueueConfig {
	t.Helper()
	cfg := testConfig(t)
	cfg.Backend = "bbolt"
	cfg.Bolt.Path = filepath.Join(t.TempDir(), "queue.db")
	return cfg
}

// TestBoltFormatReadsLegacyDatabase removes the stamp a v0.1.0 database never
// had and checks that the stored job still loads.
func TestBoltFormatReadsLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	cfg := bboltStoreConfig(t)
	first, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	job := testJob("legacy")
	if err := first.Accept(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	stripBoltFormat(t, cfg.Bolt.Path)

	second, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("open a legacy database: %v", err)
	}
	leases, err := second.Claim(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Job.ID != job.ID {
		t.Fatalf("claim = %+v, want the legacy job", leases)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if got := boltStoredFormat(t, cfg.Bolt.Path); got != fmt.Sprint(formatVersion) {
		t.Fatalf("stamp after opening a legacy database = %q, want %d", got, formatVersion)
	}
	// A queue that carries the stamp opens like any other.
	third, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("reopen a stamped queue: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBoltFormatRejectsNewerDatabase(t *testing.T) {
	ctx := context.Background()
	cfg := bboltStoreConfig(t)
	first, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(cfg.Bolt.Path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyFormat, []byte("2"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, cfg); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open a newer database: %v, want a version error", err)
	}
}

func TestRedisFormatStampsLegacyQueue(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := testConfig(t)
	cfg.Backend = "redis"
	cfg.Redis.URL = "redis://" + server.Addr()
	cfg.Redis.KeyPrefix = "mayfly-format-test"
	ctx := context.Background()

	store, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	redisStore := store.(*redisStore)
	if got := redisStore.client.Get(ctx, redisStore.format).Val(); got != fmt.Sprint(formatVersion) {
		t.Fatalf("stamp = %q, want %d", got, formatVersion)
	}
	job := testJob("legacy")
	if err := store.Accept(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A queue written by v0.1.0 has no stamp, and its jobs must still load.
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	if err := client.Del(ctx, redisStore.format).Err(); err != nil {
		t.Fatal(err)
	}
	store, err = New(ctx, cfg)
	if err != nil {
		t.Fatalf("open a legacy queue: %v", err)
	}
	defer store.Close()
	if got := client.Get(ctx, redisStore.format).Val(); got != fmt.Sprint(formatVersion) {
		t.Fatalf("stamp after opening a legacy queue = %q, want %d", got, formatVersion)
	}
	// A queue that carries the stamp opens like any other.
	third, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("reopen a stamped queue: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	leases, err := store.Claim(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Job.ID != job.ID {
		t.Fatalf("claim = %+v, want the legacy job", leases)
	}
}

func TestRedisFormatRejectsNewerQueue(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := testConfig(t)
	cfg.Backend = "redis"
	cfg.Redis.URL = "redis://" + server.Addr()
	cfg.Redis.KeyPrefix = "mayfly-format-test"
	ctx := context.Background()

	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	if err := server.Set("{mayfly-format-test}:format_version", "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, cfg); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open a newer queue: %v, want a version error", err)
	}
	if got := client.Get(ctx, "{mayfly-format-test}:format_version").Val(); got != "2" {
		t.Fatalf("stamp = %q, want the original 2", got)
	}
}

// stripBoltFormat removes the stamp, leaving the layout a v0.1.0 build wrote.
func stripBoltFormat(t *testing.T, path string) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketMeta).Delete(keyFormat); err != nil {
			return err
		}
		return tx.DeleteBucket(bucketMeta)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func boltStoredFormat(t *testing.T, path string) string {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return errors.New("format bucket is gone")
		}
		got = string(meta.Get(keyFormat))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return got
}
