package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
	done   string

	accept *redis.Script
	claim  *redis.Script
	settle *redis.Script
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

const settleScript = `
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if not score or tonumber(score) ~= tonumber(ARGV[2]) then
  return 0
end
if ARGV[3] == 'bury' then
  redis.call('ZREM', KEYS[1], ARGV[1])
  redis.call('HDEL', KEYS[2], ARGV[1])
  redis.call('HSET', KEYS[3], ARGV[1], ARGV[5])
  return 1
end
if ARGV[3] == 'retry' then
  redis.call('HSET', KEYS[2], ARGV[1], ARGV[5])
end
redis.call('ZADD', KEYS[1], ARGV[4], ARGV[1])
return 1
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
		done:   tag + ":done:",
		accept: redis.NewScript(acceptScript),
		claim:  redis.NewScript(claimScript),
		settle: redis.NewScript(settleScript),
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
			buried, _ := json.Marshal(deadRecord{Job: Job{ID: id}, Error: "payload does not decode", FailedAt: time.Now().UTC()})
			pipe := s.client.TxPipeline()
			pipe.ZRem(ctx, s.queue, id)
			pipe.HDel(ctx, s.jobs, id)
			pipe.HSet(ctx, s.dead, id, buried)
			if _, perr := pipe.Exec(ctx); perr != nil {
				return leases, fmt.Errorf("queue bury undecodable job %s: %w", id, perr)
			}
			continue
		}
		leases = append(leases, Lease{ID: id, Job: job, Deadline: deadline})
	}
	return leases, nil
}

func (s *redisStore) Ack(ctx context.Context, lease Lease, result Result) error {
	record, err := json.Marshal(doneRecord{Job: lease.Job, SessionID: result.SessionID, FinishedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	pipe := s.client.TxPipeline()
	pipe.ZRem(ctx, s.queue, lease.ID)
	pipe.HDel(ctx, s.jobs, lease.ID)
	pipe.Set(ctx, s.done+lease.ID, record, s.cfg.TombstoneTTL.Std())
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) Nack(ctx context.Context, lease Lease, cause error) error {
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
	mode := "retry"
	var updated []byte
	if max := s.cfg.MaxAttempts; max > 0 && job.Attempts >= max {
		mode = "bury"
		updated, err = json.Marshal(deadRecord{Job: job, Error: causeString(cause), FailedAt: time.Now().UTC()})
	} else {
		updated, err = json.Marshal(job)
	}
	if err != nil {
		return err
	}
	score := time.Now().UTC().Add(Backoff(job.Attempts, s.cfg.RetryBase.Std())).UnixMilli()
	return s.runSettle(ctx, lease, mode, score, updated)
}

func (s *redisStore) Release(ctx context.Context, lease Lease, after time.Duration) error {
	return s.runSettle(ctx, lease, "release", time.Now().UTC().Add(after).UnixMilli(), nil)
}

// runSettle reschedules or buries a job only while the caller still holds its
// lease, and returns ErrLeaseLost otherwise.
func (s *redisStore) runSettle(ctx context.Context, lease Lease, mode string, score int64, payload []byte) error {
	res, err := s.settle.Run(ctx, s.client, []string{s.queue, s.jobs, s.dead},
		lease.ID, lease.Deadline.UnixMilli(), mode, score, payload).Int()
	if err != nil {
		return fmt.Errorf("queue %s: %w", mode, err)
	}
	if res == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *redisStore) Get(ctx context.Context, id string) (Record, bool, error) {
	payload, err := s.client.HGet(ctx, s.jobs, id).Bytes()
	if err == nil {
		var job Job
		if err := json.Unmarshal(payload, &job); err != nil {
			return Record{}, false, err
		}
		record := Record{Job: job, State: StateQueued}
		if score, err := s.client.ZScore(ctx, s.queue, id).Result(); err == nil {
			if time.Now().UTC().UnixMilli() < int64(score) {
				record.State = StateLeased
			}
		}
		return record, true, nil
	}
	if err != redis.Nil {
		return Record{}, false, err
	}
	if payload, err := s.client.HGet(ctx, s.dead, id).Bytes(); err == nil {
		dead := decodeDead(payload)
		return Record{Job: dead.Job, State: StateDead, Error: dead.Error, FailedAt: dead.FailedAt}, true, nil
	} else if err != redis.Nil {
		return Record{}, false, err
	}
	payload, err = s.client.Get(ctx, s.done+id).Bytes()
	if err == redis.Nil {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var done doneRecord
	if err := json.Unmarshal(payload, &done); err != nil {
		return Record{}, false, err
	}
	return Record{Job: done.Job, State: StateDone, SessionID: done.SessionID, FinishedAt: done.FinishedAt}, true, nil
}

func (s *redisStore) Dead(ctx context.Context, limit int) ([]Record, error) {
	entries, err := s.client.HGetAll(ctx, s.dead).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(entries))
	for _, payload := range entries {
		dead := decodeDead([]byte(payload))
		out = append(out, Record{Job: dead.Job, State: StateDead, Error: dead.Error, FailedAt: dead.FailedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FailedAt.After(out[j].FailedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *redisStore) Retry(ctx context.Context, id string) error {
	payload, err := s.client.HGet(ctx, s.dead, id).Bytes()
	if err == redis.Nil {
		return ErrNotFound
	}
	if err != nil {
		return err
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
	pipe := s.client.TxPipeline()
	pipe.HDel(ctx, s.dead, id)
	pipe.HSet(ctx, s.jobs, id, encoded)
	pipe.ZAdd(ctx, s.queue, redis.Z{Score: float64(time.Now().UTC().UnixMilli()), Member: id})
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) Delete(ctx context.Context, id string) error {
	removed, err := s.client.HDel(ctx, s.dead, id).Result()
	if err != nil {
		return err
	}
	if removed == 0 {
		return ErrNotFound
	}
	return nil
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
