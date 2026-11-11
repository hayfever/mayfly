package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hayfever/mayfly/internal/config"
)

// redisStore is a Redis-protocol queue. Redis, Valkey, KeyDB, Dragonfly,
// ElastiCache, Memorystore, and Azure Managed Redis all serve it. The two Lua
// scripts keep accept and claim atomic: dedupe, payload, and readiness move
// together or not at all.
type redisStore struct {
	cfg    config.QueueConfig
	client redis.UniversalClient
	queue  string
	jobs   string
	dead   string
	seen   string

	accept *redis.Script
	claim  *redis.Script
}

const acceptScript = `
local dedupe = ARGV[6] == '1'
if dedupe and redis.call('EXISTS', KEYS[3]) == 1 then
  return 0
end
local max = tonumber(ARGV[4])
if max > 0 and redis.call('ZCARD', KEYS[1]) >= max then
  return -1
end
if dedupe then
  redis.call('SET', KEYS[3], '1', 'EX', ARGV[5])
end
redis.call('HSET', KEYS[2], ARGV[1], ARGV[2])
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[1])
return 1
`

const claimScript = `
local ids = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, tonumber(ARGV[2]))
if #ids == 0 then
  return {}
end
local out = {}
for i = 1, #ids do
  local id = ids[i]
  local payload = redis.call('HGET', KEYS[2], id)
  if payload then
    redis.call('ZADD', KEYS[1], ARGV[3], id)
    out[#out + 1] = id
    out[#out + 1] = payload
  else
    redis.call('ZREM', KEYS[1], id)
  end
end
return out
`

func newRedis(ctx context.Context, cfg config.QueueConfig) (*redisStore, error) {
	options, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return nil, fmt.Errorf("queue.redis.url: %w", err)
	}
	if cfg.Redis.Username != "" {
		options.Username = cfg.Redis.Username
	}
	if cfg.Redis.PasswordEnv != "" {
		options.Password = os.Getenv(cfg.Redis.PasswordEnv)
	}
	tag := "{" + cfg.Redis.KeyPrefix + "}"
	store := &redisStore{
		cfg:    cfg,
		queue:  tag + ":queue",
		jobs:   tag + ":jobs",
		dead:   tag + ":dead",
		seen:   tag + ":seen:",
		accept: redis.NewScript(acceptScript),
		claim:  redis.NewScript(claimScript),
	}
	if cfg.Redis.Cluster {
		store.client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:     []string{options.Addr},
			Username:  options.Username,
			Password:  options.Password,
			TLSConfig: options.TLSConfig,
		})
	} else {
		store.client = redis.NewClient(options)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := store.client.Ping(pingCtx).Err(); err != nil {
		_ = store.client.Close()
		return nil, fmt.Errorf("queue.redis: %w", err)
	}
	return store, nil
}

// dedupeFlag tells the accept script whether this job has a delivery key.
func dedupeFlag(delivery string) string {
	if delivery == "" {
		return "0"
	}
	return "1"
}

func (s *redisStore) Accept(ctx context.Context, job Job) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return err
	}
	res, err := s.accept.Run(ctx, s.client,
		[]string{s.queue, s.jobs, s.seen + job.ID, s.dead},
		job.ID,
		payload,
		time.Now().UTC().UnixMilli(),
		s.cfg.MaxJobs,
		int64(s.cfg.DedupeTTL.Std().Seconds()),
		dedupeFlag(job.Delivery),
	).Int()
	if err != nil {
		return fmt.Errorf("queue accept: %w", err)
	}
	switch res {
	case 0:
		return ErrDuplicate
	case -1:
		return ErrFull
	}
	return nil
}

func (s *redisStore) Claim(ctx context.Context, max int) ([]Lease, error) {
	now := time.Now().UTC()
	deadline := now.Add(s.cfg.VisibilityTimeout.Std())
	raw, err := s.claim.Run(ctx, s.client, []string{s.queue, s.jobs},
		now.UnixMilli(),
		max,
		deadline.UnixMilli(),
	).Result()
	if err != nil {
		return nil, fmt.Errorf("queue claim: %w", err)
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, nil
	}
	leases := make([]Lease, 0, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		id, _ := values[i].(string)
		payload, _ := values[i+1].(string)
		var job Job
		if err := json.Unmarshal([]byte(payload), &job); err != nil {
			pipe := s.client.TxPipeline()
			pipe.ZRem(ctx, s.queue, id)
			pipe.HDel(ctx, s.jobs, id)
			pipe.HSet(ctx, s.dead, id, payload)
			if _, perr := pipe.Exec(ctx); perr != nil {
				return leases, fmt.Errorf("queue bury undecodable job %s: %w", id, perr)
			}
			continue
		}
		leases = append(leases, Lease{ID: id, Job: job, Deadline: deadline})
	}
	return leases, nil
}

func (s *redisStore) Ack(ctx context.Context, lease Lease) error {
	pipe := s.client.TxPipeline()
	pipe.ZRem(ctx, s.queue, lease.ID)
	pipe.HDel(ctx, s.jobs, lease.ID)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) Nack(ctx context.Context, lease Lease, _ error) error {
	payload, err := s.client.HGet(ctx, s.jobs, lease.ID).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil
		}
		return err
	}
	var job Job
	if err := json.Unmarshal(payload, &job); err != nil {
		return err
	}
	job.Attempts++
	updated, err := json.Marshal(job)
	if err != nil {
		return err
	}
	pipe := s.client.TxPipeline()
	if max := s.cfg.MaxAttempts; max > 0 && job.Attempts >= max {
		pipe.HDel(ctx, s.jobs, lease.ID)
		pipe.ZRem(ctx, s.queue, lease.ID)
		pipe.HSet(ctx, s.dead, lease.ID, updated)
		_, err = pipe.Exec(ctx)
		return err
	}
	pipe.HSet(ctx, s.jobs, lease.ID, updated)
	pipe.ZAdd(ctx, s.queue, redis.Z{
		Score:  float64(time.Now().UTC().Add(Backoff(job.Attempts, s.cfg.RetryBase.Std())).UnixMilli()),
		Member: lease.ID,
	})
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) Release(ctx context.Context, lease Lease, after time.Duration) error {
	return s.client.ZAdd(ctx, s.queue, redis.Z{
		Score:  float64(time.Now().UTC().Add(after).UnixMilli()),
		Member: lease.ID,
	}).Err()
}

func (s *redisStore) Stats(ctx context.Context) (Stats, error) {
	now := time.Now().UTC().UnixMilli()
	pipe := s.client.Pipeline()
	pending := pipe.ZCount(ctx, s.queue, "-inf", fmt.Sprint(now))
	leased := pipe.ZCount(ctx, s.queue, "("+fmt.Sprint(now), "+inf")
	dead := pipe.HLen(ctx, s.dead)
	first := pipe.ZRangeWithScores(ctx, s.queue, 0, 0)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return Stats{}, err
	}
	stats := Stats{Pending: int(pending.Val()), Leased: int(leased.Val()), Dead: int(dead.Val())}
	if members := first.Val(); len(members) > 0 {
		id, _ := members[0].Member.(string)
		if payload, err := s.client.HGet(ctx, s.jobs, id).Bytes(); err == nil {
			var job Job
			if json.Unmarshal(payload, &job) == nil {
				stats.Oldest = job.AcceptedAt
			}
		}
	}
	return stats, nil
}

func (s *redisStore) Close() error { return s.client.Close() }

var _ Store = (*redisStore)(nil)
