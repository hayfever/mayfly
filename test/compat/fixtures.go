// Package compat holds one committed fixture set per release that wrote a
// persisted format, and the tests that open each set with the current build.
// A release adds a testdata/vX.Y.Z directory; every later build must keep
// reading it. See docs/development.md, "Persisted formats and migrations".
//
// Fixtures are written by Generate, which the gen command runs at release
// time. The committed bytes are the contract: do not regenerate a directory
// after its release ships. bbolt writes page layout and transaction
// identifiers, so regeneration changes database bytes but not stored data.
package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/state"
)

// FixtureVersion is the newest fixture set in the tree and the default output
// of gen. A release updates it when it adds its own set. Each set stays in the
// tree so a later build keeps reading it.
const FixtureVersion = "v0.2.0"

// Fixture file names inside a version directory.
const (
	FileQueueBolt   = "queue.db"
	FileRedisDump   = "queue-redis.json"
	FileJournal     = "journal.db"
	FileState       = "state.db"
	FileRecords     = "sessions.db"
	redisKeyPrefix  = "mayfly"
	legacyJobID     = "job-legacy-0001"
	deadJobID       = "job-dead-0001"
	doneJobID       = "job-done-0001"
	leasedJobID     = "job-leased-0001"
	pendingJobID    = "s-job-pending-0001"
	doneSessionID   = "sess-done-0001"
	journalSession  = "sess-journal-0001"
	legacyDelivery  = "legacy-delivery"
	deadCause       = "image pull failed"
	queueBoltBucket = "meta"
	queueBoltKey    = "format_version"
)

// Times carried by the fixtures. They are fixed so a regenerated set holds
// the same data. The lease and pending horizons are far enough away that the
// stored state does not change as the test suite runs.
var (
	fixtureAcceptedAt    = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	fixtureSlot          = time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)
	fixtureDeadFailedAt  = time.Date(2026, 11, 19, 12, 5, 0, 0, time.UTC)
	fixtureDoneAt        = time.Date(2026, 11, 19, 12, 6, 0, 0, time.UTC)
	fixtureLegacyAt      = time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)
	fixturePendingReady  = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	fixtureLeasedReady   = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	fixtureJournalStepAt = time.Date(2026, 11, 19, 12, 1, 0, 0, time.UTC)
)

// queueDeadRecord and queueDoneRecord match the payload the queue stores for
// a buried and an acknowledged job. They are written into the redis dump and
// overwrite the wall-clock timestamps the bbolt store writes, so both
// backends carry the same fixed data.
type queueDeadRecord struct {
	Job      queue.Job `json:"job"`
	Error    string    `json:"error,omitempty"`
	FailedAt time.Time `json:"failed_at"`
}

type queueDoneRecord struct {
	Job        queue.Job `json:"job"`
	SessionID  string    `json:"session_id,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// redisFixture is the deterministic dump format for the redis queue. A test
// loads it into miniredis; the layout is data, not a redis persistence file.
type redisFixture struct {
	Strings map[string]string            `json:"strings"`
	Hashes  map[string]map[string]string `json:"hashes"`
	ZSets   map[string][]redisZMember    `json:"zsets"`
}

type redisZMember struct {
	Score  int64  `json:"score"`
	Member string `json:"member"`
}

func redisKey(suffix string) string {
	return "{" + redisKeyPrefix + "}:" + suffix
}

// Generate writes the FixtureVersion fixture set to dir. It refuses to replace
// an existing set unless force is true, because a released set must stay
// readable unchanged.
func Generate(dir string, force bool) error {
	if !force {
		if _, err := os.Stat(filepath.Join(dir, FileQueueBolt)); err == nil {
			return fmt.Errorf("%s already holds a fixture set; pass -force only to regenerate a set before its release ships", dir)
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, name := range []string{FileQueueBolt, FileRedisDump, FileJournal, FileState, FileRecords} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := generateQueueBolt(dir); err != nil {
		return fmt.Errorf("queue fixture: %w", err)
	}
	if err := writeRedisDump(dir); err != nil {
		return fmt.Errorf("redis fixture: %w", err)
	}
	if err := generateJournal(dir); err != nil {
		return fmt.Errorf("journal fixture: %w", err)
	}
	if err := generateState(dir); err != nil {
		return fmt.Errorf("state fixture: %w", err)
	}
	if err := generateRecords(dir); err != nil {
		return fmt.Errorf("session records fixture: %w", err)
	}
	return nil
}

// fixtureJobs returns the four stored jobs shared by the bbolt and redis
// fixtures. The dead job is buried by the generator; the done job is
// acknowledged. The leased and pending jobs stay in the queue. The legacy job
// is written by hand because no current code path writes that shape.
func fixtureJobs() (dead, done, leased, pending queue.Job) {
	dead = queue.Job{
		ID:             deadJobID,
		Kind:           queue.KindSession,
		IdempotencyKey: "dead-delivery",
		Trigger:        "github",
		Options: session.CreateOptions{
			Prompt: "Summarize the failing build",
			Title:  "Build failure",
			Model:  "gpt-test",
			TTL:    time.Hour,
			Skills: []string{"repo"},
			Trigger: model.TriggerInfo{
				Kind:     "github",
				Name:     "github",
				Event:    "push",
				Actor:    "octocat",
				Delivery: "dead-delivery",
				JobID:    deadJobID,
			},
			IdempotencyKey: deadJobID,
		},
		AcceptedAt: fixtureAcceptedAt,
	}
	done = queue.Job{
		ID:             doneJobID,
		Kind:           queue.KindSession,
		IdempotencyKey: "done-delivery",
		Trigger:        "generic",
		Options: session.CreateOptions{
			Prompt: "List the changed files",
			Title:  "Changed files",
			Agent:  "default",
			Trigger: model.TriggerInfo{
				Kind:     "generic",
				Name:     "generic",
				Delivery: "done-delivery",
				JobID:    doneJobID,
			},
			IdempotencyKey: doneJobID,
		},
		AcceptedAt: fixtureAcceptedAt,
	}
	leased = queue.Job{
		ID:             leasedJobID,
		Kind:           queue.KindSession,
		IdempotencyKey: "leased-delivery",
		Trigger:        "slack",
		Options: session.CreateOptions{
			Prompt: "Answer the support thread",
			Title:  "Support thread",
			Trigger: model.TriggerInfo{
				Kind:     "slack",
				Name:     "slack",
				Event:    "app_mention",
				Delivery: "leased-delivery",
				JobID:    leasedJobID,
			},
			IdempotencyKey: leasedJobID,
		},
		AcceptedAt: fixtureAcceptedAt,
	}
	sourceSlot := fixtureSlot
	pending = queue.Job{
		ID:             pendingJobID,
		Kind:           queue.KindSource,
		IdempotencyKey: "schedule:nightly:2026-11-19T02:00:00Z",
		Trigger:        "schedule",
		Options: session.CreateOptions{
			Trigger: model.TriggerInfo{
				Kind:     "schedule",
				Name:     "nightly",
				Event:    "schedule",
				Delivery: "schedule:nightly:2026-11-19T02:00:00Z",
				JobID:    pendingJobID,
			},
			IdempotencyKey: pendingJobID,
		},
		Source: &queue.SourceJob{
			RunID:    "run-0001",
			Schedule: "nightly",
			Source:   "repo-scan",
			Slot:     sourceSlot,
		},
		AcceptedAt: fixtureAcceptedAt,
	}
	return dead, done, leased, pending
}

// legacyJobJSON is the shape v0.1.0 wrote: a top-level options object with Go
// field names, a numeric ttl in nanoseconds, and the kind "session".
func legacyJobJSON() []byte {
	return []byte(`{"id":"` + legacyJobID + `","kind":"session","delivery":"` + legacyDelivery + `","trigger":"github","options":{"Prompt":"Legacy job from v0.1.0","Title":"Legacy build","Model":"gpt-test","TTL":3600000000000,"Skills":["repo"],"Agent":"default","Trigger":{"kind":"github","name":"github","event":"push","delivery":"` + legacyDelivery + `","job_id":"` + legacyJobID + `"}},"accepted_at":"2026-11-01T08:30:00Z","attempts":2}`)
}

func generateQueueBolt(dir string) error {
	path := filepath.Join(dir, FileQueueBolt)
	ctx := context.Background()
	cfg := config.QueueConfig{
		Backend:           "bbolt",
		MaxJobs:           100,
		MaxAttempts:       1,
		RetryBase:         config.Duration(time.Second),
		VisibilityTimeout: config.Duration(24 * time.Hour),
		DedupeTTL:         config.Duration(24 * time.Hour),
		TombstoneTTL:      config.Duration(0),
		Bolt:              config.BoltQueueConfig{Path: path},
	}
	store, err := queue.New(ctx, cfg)
	if err != nil {
		return err
	}
	dead, done, leased, pending := fixtureJobs()

	if err := store.Accept(ctx, dead); err != nil {
		return fmt.Errorf("accept dead job: %w", err)
	}
	lease, err := claimOne(ctx, store)
	if err != nil {
		return err
	}
	if err := store.Nack(ctx, lease, errors.New(deadCause)); err != nil {
		return fmt.Errorf("bury dead job: %w", err)
	}

	if err := store.Accept(ctx, done); err != nil {
		return fmt.Errorf("accept done job: %w", err)
	}
	lease, err = claimOne(ctx, store)
	if err != nil {
		return err
	}
	if err := store.Ack(ctx, lease, queue.Result{SessionID: doneSessionID}); err != nil {
		return fmt.Errorf("ack done job: %w", err)
	}

	if err := store.Accept(ctx, leased); err != nil {
		return fmt.Errorf("accept leased job: %w", err)
	}
	if _, err := claimOne(ctx, store); err != nil {
		return err
	}

	if err := store.Accept(ctx, pending); err != nil {
		return fmt.Errorf("accept pending job: %w", err)
	}
	if err := store.Close(); err != nil {
		return err
	}
	return rewriteQueueDatabase(path, dead, done)
}

func claimOne(ctx context.Context, store queue.Store) (queue.Lease, error) {
	leases, err := store.Claim(ctx, 1)
	if err != nil {
		return queue.Lease{}, fmt.Errorf("claim: %w", err)
	}
	if len(leases) != 1 {
		return queue.Lease{}, fmt.Errorf("claimed %d jobs, want 1", len(leases))
	}
	return leases[0], nil
}

// rewriteQueueDatabase makes the store-written fixture deterministic. It
// replaces the wall-clock timestamps in the buried and acknowledged records
// with fixed times, adds the v0.1.0-shaped payload, and pins the ready keys so
// a pending job stays pending and a leased job stays leased.
func rewriteQueueDatabase(path string, dead, done queue.Job) error {
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	dead.Attempts = 1
	deadPayload, err := json.Marshal(queueDeadRecord{Job: dead, Error: deadCause, FailedAt: fixtureDeadFailedAt})
	if err != nil {
		return err
	}
	donePayload, err := json.Marshal(queueDoneRecord{Job: done, SessionID: doneSessionID, FinishedAt: fixtureDoneAt})
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		jobs := tx.Bucket([]byte("jobs"))
		ready := tx.Bucket([]byte("ready"))
		if jobs == nil || ready == nil {
			return errors.New("queue fixture has no jobs or ready bucket")
		}
		if err := jobs.Put([]byte(legacyJobID), legacyJobJSON()); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("dead")).Put([]byte(deadJobID), deadPayload); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("done")).Put([]byte(doneJobID), donePayload); err != nil {
			return err
		}
		want := map[string]time.Time{
			pendingJobID: fixturePendingReady,
			legacyJobID:  fixturePendingReady,
			leasedJobID:  fixtureLeasedReady,
		}
		type entry struct {
			key []byte
			id  string
			at  time.Time
		}
		var entries []entry
		cursor := ready.Cursor()
		for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
			if len(key) < 8 {
				return fmt.Errorf("ready key %x is shorter than 8 bytes", key)
			}
			id := string(key[8:])
			at, ok := want[id]
			if !ok {
				return fmt.Errorf("unexpected ready entry for job %s", id)
			}
			entries = append(entries, entry{key: append([]byte(nil), key...), id: id, at: at})
		}
		for _, e := range entries {
			if err := ready.Delete(e.key); err != nil {
				return err
			}
			if err := ready.Put(fixtureReadyKey(e.at, e.id), nil); err != nil {
				return err
			}
		}
		return ready.Put(fixtureReadyKey(fixturePendingReady, legacyJobID), nil)
	})
}

// fixtureReadyKey mirrors the 8-byte big-endian nanosecond timestamp followed
// by the job id that the queue uses to order ready work.
func fixtureReadyKey(at time.Time, id string) []byte {
	key := make([]byte, 8, 8+len(id))
	ns := uint64(at.UTC().UnixNano())
	for i := 7; i >= 0; i-- {
		key[i] = byte(ns)
		ns >>= 8
	}
	return append(key, id...)
}

func writeRedisDump(dir string) error {
	dead, done, leased, pending := fixtureJobs()
	dead.Attempts = 1
	deadPayload, err := json.Marshal(queueDeadRecord{Job: dead, Error: deadCause, FailedAt: fixtureDeadFailedAt})
	if err != nil {
		return err
	}
	donePayload, err := json.Marshal(queueDoneRecord{Job: done, SessionID: doneSessionID, FinishedAt: fixtureDoneAt})
	if err != nil {
		return err
	}
	leasedPayload, err := json.Marshal(leased)
	if err != nil {
		return err
	}
	pendingPayload, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	dump := redisFixture{
		Strings: map[string]string{
			redisKey("format_version"):    "1",
			redisKey("done:" + doneJobID): string(donePayload),
		},
		Hashes: map[string]map[string]string{
			redisKey("jobs"): {
				leasedJobID:  string(leasedPayload),
				pendingJobID: string(pendingPayload),
				legacyJobID:  string(legacyJobJSON()),
			},
			redisKey("dead"): {
				deadJobID: string(deadPayload),
			},
		},
		ZSets: map[string][]redisZMember{
			redisKey("queue"): {
				{Score: 0, Member: legacyJobID},
				{Score: 0, Member: pendingJobID},
				{Score: fixtureLeasedReady.UnixMilli(), Member: leasedJobID},
			},
		},
	}
	payload, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, FileRedisDump), append(payload, '\n'), 0o600)
}

func generateJournal(dir string) error {
	path := filepath.Join(dir, FileJournal)
	store, err := journal.OpenBolt(path)
	if err != nil {
		return err
	}
	ctx := context.Background()
	entries := []journal.Entry{
		{
			SessionID: journalSession,
			Turn:      1,
			Step:      0,
			Name:      "user",
			Input:     json.RawMessage(`{"prompt":"summarize the build"}`),
			Output:    json.RawMessage(`{"type":"user_message","text":"summarize the build"}`),
			ConvLen:   120,
			Time:      fixtureJournalStepAt,
		},
		{
			SessionID: journalSession,
			Turn:      1,
			Step:      1,
			Name:      "llm",
			Input:     json.RawMessage(`{"model":"gpt-test"}`),
			Output:    json.RawMessage(`{"type":"tool_call","text":"echo"}`),
			ConvLen:   260,
			Time:      fixtureJournalStepAt.Add(time.Second),
		},
		{
			SessionID: journalSession,
			Turn:      1,
			Step:      2,
			Name:      "tool:echo",
			Input:     json.RawMessage(`{"text":"hello"}`),
			Output:    json.RawMessage(`{"type":"tool_result","text":"hello"}`),
			ConvLen:   410,
			Time:      fixtureJournalStepAt.Add(2 * time.Second),
		},
		{
			SessionID: journalSession,
			Turn:      2,
			Step:      0,
			Name:      "assistant",
			Output:    json.RawMessage(`{"type":"assistant_message","text":"done"}`),
			ConvLen:   470,
			Time:      fixtureJournalStepAt.Add(3 * time.Second),
		},
	}
	for _, e := range entries {
		if err := store.Append(ctx, e); err != nil {
			return err
		}
	}
	if err := store.Reserve(ctx, journalSession, 42); err != nil {
		return err
	}
	callKey := journalSession + "/1/2"
	if _, _, err := store.Begin(ctx, journal.Call{
		Key:        callKey,
		SessionID:  journalSession,
		Turn:       1,
		Step:       2,
		Tool:       "echo",
		ArgsSHA256: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		InstanceID: "runtime-0001",
		StartedAt:  fixtureJournalStepAt.Add(2 * time.Second),
	}); err != nil {
		return err
	}
	if _, err := store.Complete(ctx, journalSession, callKey, journal.Result{Content: `{"text":"hello"}`}); err != nil {
		return err
	}
	return store.Close()
}

func generateState(dir string) error {
	path := filepath.Join(dir, FileState)
	store, err := state.New(path)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := store.SetLastSlot(ctx, "nightly", fixtureSlot); err != nil {
		return err
	}
	return store.SetLastSlot(ctx, "hourly", fixtureSlot.Add(time.Hour))
}

func generateRecords(dir string) error {
	path := filepath.Join(dir, FileRecords)
	store, err := session.OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		return err
	}
	ctx := context.Background()
	started := fixtureAcceptedAt.Add(-58 * time.Minute)
	closed := fixtureDoneAt.Add(time.Minute)
	completed := session.Record{
		Version: 1,
		Session: model.Session{
			ID:         "sess-completed-0001",
			Title:      "Nightly scan",
			Status:     model.StatusCompleted,
			Prompt:     "Scan the repository",
			Agent:      "default",
			Harness:    "claude",
			Model:      "gpt-test",
			Skills:     []string{"repo"},
			MCPServers: []string{"echo"},
			Plugins:    []string{"notes"},
			Trigger: model.TriggerInfo{
				Kind:     "schedule",
				Name:     "nightly",
				Event:    "schedule",
				Delivery: "schedule:nightly:2026-11-19T02:00:00Z",
				JobID:    pendingJobID,
			},
			Backend:      "kubernetes",
			PodName:      "mayfly-sess-completed",
			TTLSeconds:   3600,
			CreatedAt:    fixtureAcceptedAt.Add(-time.Hour),
			StartedAt:    &started,
			ExpiresAt:    fixtureAcceptedAt.Add(time.Hour),
			ClosedAt:     &closed,
			LastActivity: fixtureDoneAt,
			MessageCount: 4,
			Artifact: &model.ArtifactInfo{
				Key:       "sessions/sess-completed-0001.tar.gz",
				URI:       "file:///var/lib/mayfly/artifacts/sessions/sess-completed-0001.tar.gz",
				Size:      2048,
				SHA256:    "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
				CreatedAt: fixtureDoneAt,
			},
		},
	}
	failed := session.Record{
		Version: 1,
		Session: model.Session{
			ID:      "sess-failed-0001",
			Title:   "Failed build",
			Status:  model.StatusFailed,
			Prompt:  "Summarize the failing build",
			Agent:   "default",
			Harness: "claude",
			Model:   "gpt-test",
			Trigger: model.TriggerInfo{
				Kind:     "github",
				Name:     "github",
				Event:    "push",
				Delivery: "dead-delivery",
				JobID:    deadJobID,
			},
			Backend:      "kubernetes",
			TTLSeconds:   1800,
			CreatedAt:    fixtureAcceptedAt,
			ExpiresAt:    fixtureAcceptedAt.Add(30 * time.Minute),
			LastActivity: fixtureDeadFailedAt,
			Error:        "image pull failed",
			Artifact: &model.ArtifactInfo{
				Key:       "sessions/sess-failed-0001.tar.gz",
				URI:       "file:///var/lib/mayfly/artifacts/sessions/sess-failed-0001.tar.gz",
				Size:      1024,
				SHA256:    "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
				CreatedAt: fixtureDeadFailedAt,
				Partial:   true,
			},
		},
	}
	for _, rec := range []session.Record{completed, failed} {
		if err := store.Save(ctx, rec); err != nil {
			return err
		}
		if rec.Session.Artifact == nil {
			continue
		}
		if err := store.SaveArtifact(ctx, rec.Session.ID, *rec.Session.Artifact); err != nil {
			return err
		}
	}
	return store.Close()
}
