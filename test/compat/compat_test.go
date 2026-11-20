package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	bolt "go.etcd.io/bbolt"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/state"
)

// v020 pins the fixture set the detailed assertions below were written for.
// A later release keeps this test and adds its own set and test.
const v020 = "v0.2.0"

// TestEveryFixtureSetOpens is the gate for every committed set. It opens each
// file with the current stores, so a set from an earlier release stays
// readable even when a newer set is present.
func TestEveryFixtureSetOpens(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "v*"))
	if err != nil {
		t.Fatalf("list fixture sets: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("no fixture sets under testdata")
	}
	sort.Strings(dirs)
	ctx := context.Background()
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			queuePath := copyFixtureFrom(t, dir, FileQueueBolt)
			store, err := queue.New(ctx, config.QueueConfig{
				Backend: "bbolt",
				Bolt:    config.BoltQueueConfig{Path: queuePath},
			})
			if err != nil {
				t.Fatalf("open bbolt queue: %v", err)
			}
			if _, err := store.Stats(ctx); err != nil {
				t.Errorf("bbolt queue stats: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("close bbolt queue: %v", err)
			}

			server := miniredis.RunT(t)
			loadRedisFixture(t, server, dir)
			redisStore, err := queue.New(ctx, config.QueueConfig{
				Backend: "redis",
				Redis: config.RedisQueueConfig{
					URL:       "redis://" + server.Addr(),
					KeyPrefix: redisKeyPrefix,
				},
			})
			if err != nil {
				t.Fatalf("open redis queue: %v", err)
			}
			if _, err := redisStore.Stats(ctx); err != nil {
				t.Errorf("redis queue stats: %v", err)
			}
			if err := redisStore.Close(); err != nil {
				t.Fatalf("close redis queue: %v", err)
			}

			journalPath := copyFixtureFrom(t, dir, FileJournal)
			journalStore, err := journal.OpenBolt(journalPath)
			if err != nil {
				t.Fatalf("open journal: %v", err)
			}
			if _, err := journalStore.LastTurn(ctx, journalSession); err != nil {
				t.Errorf("journal last turn: %v", err)
			}
			if err := journalStore.Close(); err != nil {
				t.Fatalf("close journal: %v", err)
			}

			statePath := copyFixtureFrom(t, dir, FileState)
			stateStore, err := state.New(statePath)
			if err != nil {
				t.Fatalf("open state: %v", err)
			}
			if _, _, err := stateStore.LastSlot(ctx, "nightly"); err != nil {
				t.Errorf("state last slot: %v", err)
			}
			if err := stateStore.Close(); err != nil {
				t.Fatalf("close state: %v", err)
			}

			recordsPath := copyFixtureFrom(t, dir, FileRecords)
			records, err := session.OpenRecords(config.RecordConfig{Backend: "bbolt", Path: recordsPath})
			if err != nil {
				t.Fatalf("open session records: %v", err)
			}
			if _, err := records.List(ctx); err != nil {
				t.Errorf("session records list: %v", err)
			}
			if err := records.Close(); err != nil {
				t.Fatalf("close session records: %v", err)
			}
		})
	}
}

// TestQueueBoltFixture reads the committed bbolt queue with the current store
// and checks every state the fixture holds.
func TestQueueBoltFixture(t *testing.T) {
	path := copyFixture(t, FileQueueBolt)
	store, err := queue.New(context.Background(), config.QueueConfig{
		Backend: "bbolt",
		Bolt:    config.BoltQueueConfig{Path: path},
	})
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	ctx := context.Background()

	dead, found, err := store.Get(ctx, deadJobID)
	if err != nil || !found {
		t.Fatalf("get dead job: found=%v err=%v", found, err)
	}
	if dead.State != queue.StateDead {
		t.Errorf("dead state = %q, want %q", dead.State, queue.StateDead)
	}
	if dead.Error != deadCause {
		t.Errorf("dead error = %q, want %q", dead.Error, deadCause)
	}
	if !dead.FailedAt.Equal(fixtureDeadFailedAt) {
		t.Errorf("dead failed_at = %v, want %v", dead.FailedAt, fixtureDeadFailedAt)
	}
	if dead.Job.Attempts != 1 {
		t.Errorf("dead attempts = %d, want 1", dead.Job.Attempts)
	}
	if dead.Job.Kind != queue.KindSession || dead.Job.Trigger != "github" {
		t.Errorf("dead identity = kind %q trigger %q", dead.Job.Kind, dead.Job.Trigger)
	}
	if dead.Job.Options.Prompt != "Summarize the failing build" || dead.Job.Options.Title != "Build failure" {
		t.Errorf("dead options = %+v", dead.Job.Options)
	}

	done, found, err := store.Get(ctx, doneJobID)
	if err != nil || !found {
		t.Fatalf("get done job: found=%v err=%v", found, err)
	}
	if done.State != queue.StateDone {
		t.Errorf("done state = %q, want %q", done.State, queue.StateDone)
	}
	if done.SessionID != doneSessionID {
		t.Errorf("done session = %q, want %q", done.SessionID, doneSessionID)
	}
	if !done.FinishedAt.Equal(fixtureDoneAt) {
		t.Errorf("done finished_at = %v, want %v", done.FinishedAt, fixtureDoneAt)
	}
	if done.Job.Options.Prompt != "List the changed files" {
		t.Errorf("done prompt = %q", done.Job.Options.Prompt)
	}

	leased, found, err := store.Get(ctx, leasedJobID)
	if err != nil || !found {
		t.Fatalf("get leased job: found=%v err=%v", found, err)
	}
	if leased.State != queue.StateLeased {
		t.Errorf("leased state = %q, want %q", leased.State, queue.StateLeased)
	}
	if leased.Job.Options.Prompt != "Answer the support thread" {
		t.Errorf("leased prompt = %q", leased.Job.Options.Prompt)
	}

	pending, found, err := store.Get(ctx, pendingJobID)
	if err != nil || !found {
		t.Fatalf("get pending job: found=%v err=%v", found, err)
	}
	if pending.State != queue.StateQueued {
		t.Errorf("pending state = %q, want %q", pending.State, queue.StateQueued)
	}
	if pending.Job.JobKind() != queue.KindSource {
		t.Errorf("pending kind = %q, want %q", pending.Job.JobKind(), queue.KindSource)
	}
	if pending.Job.Source == nil {
		t.Fatal("pending source is nil")
	}
	if got := *pending.Job.Source; got.RunID != "run-0001" || got.Schedule != "nightly" || got.Source != "repo-scan" || !got.Slot.Equal(fixtureSlot) {
		t.Errorf("pending source = %+v", got)
	}
	if pending.Job.Trigger != "schedule" {
		t.Errorf("pending trigger = %q, want schedule", pending.Job.Trigger)
	}

	legacy, found, err := store.Get(ctx, legacyJobID)
	if err != nil || !found {
		t.Fatalf("get legacy job: found=%v err=%v", found, err)
	}
	if legacy.State != queue.StateQueued {
		t.Errorf("legacy state = %q, want %q", legacy.State, queue.StateQueued)
	}
	if legacy.Job.JobKind() != queue.KindSession {
		t.Errorf("legacy kind = %q, want %q", legacy.Job.JobKind(), queue.KindSession)
	}
	if legacy.Job.IdempotencyKey != legacyDelivery || legacy.Job.Trigger != "github" {
		t.Errorf("legacy identity = key %q trigger %q", legacy.Job.IdempotencyKey, legacy.Job.Trigger)
	}
	if legacy.Job.Options.Prompt != "Legacy job from v0.1.0" || legacy.Job.Options.Title != "Legacy build" {
		t.Errorf("legacy options = %+v", legacy.Job.Options)
	}
	if legacy.Job.Options.TTL != time.Hour {
		t.Errorf("legacy ttl = %v, want 1h", legacy.Job.Options.TTL)
	}
	if legacy.Job.Attempts != 2 {
		t.Errorf("legacy attempts = %d, want 2", legacy.Job.Attempts)
	}
	if !legacy.Job.AcceptedAt.Equal(fixtureLegacyAt) {
		t.Errorf("legacy accepted_at = %v, want %v", legacy.Job.AcceptedAt, fixtureLegacyAt)
	}
	if legacy.Job.Options.IdempotencyKey != legacyJobID {
		t.Errorf("legacy options idempotency key = %q, want the job id", legacy.Job.Options.IdempotencyKey)
	}
	if legacy.Job.Options.Trigger.JobID != legacyJobID || legacy.Job.Options.Trigger.Delivery != legacyDelivery {
		t.Errorf("legacy trigger = %+v", legacy.Job.Options.Trigger)
	}

	if _, found, err := store.Get(ctx, "job-missing"); err != nil || found {
		t.Errorf("missing job: found=%v err=%v", found, err)
	}

	deadList, err := store.Dead(ctx, 10)
	if err != nil {
		t.Fatalf("dead list: %v", err)
	}
	if len(deadList) != 1 || deadList[0].Job.ID != deadJobID {
		t.Fatalf("dead list = %+v, want only %s", deadList, deadJobID)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if want := (queue.Stats{Pending: 2, Leased: 1, Dead: 1, Oldest: fixtureLegacyAt}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertBoltString(t, path, queueBoltBucket, queueBoltKey, "1")
}

// TestQueueRedisFixture loads the deterministic redis dump into miniredis and
// reads it with the current redis store.
func TestQueueRedisFixture(t *testing.T) {
	server := miniredis.RunT(t)
	loadRedisFixture(t, server, filepath.Join("testdata", v020))
	store, err := queue.New(context.Background(), config.QueueConfig{
		Backend: "redis",
		Redis: config.RedisQueueConfig{
			URL:       "redis://" + server.Addr(),
			KeyPrefix: redisKeyPrefix,
		},
	})
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	ctx := context.Background()

	dead, found, err := store.Get(ctx, deadJobID)
	if err != nil || !found {
		t.Fatalf("get dead job: found=%v err=%v", found, err)
	}
	if dead.State != queue.StateDead || dead.Error != deadCause || !dead.FailedAt.Equal(fixtureDeadFailedAt) {
		t.Errorf("dead record = %+v", dead)
	}
	if dead.Job.Attempts != 1 || dead.Job.Options.Prompt != "Summarize the failing build" {
		t.Errorf("dead job = %+v", dead.Job)
	}

	done, found, err := store.Get(ctx, doneJobID)
	if err != nil || !found {
		t.Fatalf("get done job: found=%v err=%v", found, err)
	}
	if done.State != queue.StateDone || done.SessionID != doneSessionID || !done.FinishedAt.Equal(fixtureDoneAt) {
		t.Errorf("done record = %+v", done)
	}

	leased, found, err := store.Get(ctx, leasedJobID)
	if err != nil || !found {
		t.Fatalf("get leased job: found=%v err=%v", found, err)
	}
	if leased.State != queue.StateLeased {
		t.Errorf("leased state = %q, want %q", leased.State, queue.StateLeased)
	}

	pending, found, err := store.Get(ctx, pendingJobID)
	if err != nil || !found {
		t.Fatalf("get pending job: found=%v err=%v", found, err)
	}
	if pending.State != queue.StateQueued || pending.Job.JobKind() != queue.KindSource {
		t.Errorf("pending record = %+v", pending)
	}
	if pending.Job.Source == nil || pending.Job.Source.Source != "repo-scan" {
		t.Errorf("pending source = %+v", pending.Job.Source)
	}

	legacy, found, err := store.Get(ctx, legacyJobID)
	if err != nil || !found {
		t.Fatalf("get legacy job: found=%v err=%v", found, err)
	}
	if legacy.State != queue.StateQueued || legacy.Job.Attempts != 2 || legacy.Job.Options.TTL != time.Hour {
		t.Errorf("legacy record = %+v", legacy)
	}
	if legacy.Job.Options.Prompt != "Legacy job from v0.1.0" || legacy.Job.Options.IdempotencyKey != legacyJobID {
		t.Errorf("legacy options = %+v", legacy.Job.Options)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if want := (queue.Stats{Pending: 2, Leased: 1, Dead: 1, Oldest: fixtureLegacyAt}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestJournalFixture(t *testing.T) {
	path := copyFixture(t, FileJournal)
	store, err := journal.OpenBolt(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	ctx := context.Background()

	want := []struct {
		step    int
		name    string
		input   string
		output  string
		convLen int64
		at      time.Time
	}{
		{0, "user", `{"prompt":"summarize the build"}`, `{"type":"user_message","text":"summarize the build"}`, 120, fixtureJournalStepAt},
		{1, "llm", `{"model":"gpt-test"}`, `{"type":"tool_call","text":"echo"}`, 260, fixtureJournalStepAt.Add(time.Second)},
		{2, "tool:echo", `{"text":"hello"}`, `{"type":"tool_result","text":"hello"}`, 410, fixtureJournalStepAt.Add(2 * time.Second)},
	}
	entries, err := store.Turn(ctx, journalSession, 1)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if len(entries) != len(want) {
		t.Fatalf("turn 1 has %d entries, want %d", len(entries), len(want))
	}
	for i, w := range want {
		got := entries[i]
		if got.SessionID != journalSession || got.Turn != 1 || got.Step != w.step || got.Name != w.name {
			t.Errorf("entry %d identity = session %q turn %d step %d name %q", i, got.SessionID, got.Turn, got.Step, got.Name)
		}
		if string(got.Input) != w.input || string(got.Output) != w.output {
			t.Errorf("entry %d payload = input %s output %s", i, got.Input, got.Output)
		}
		if got.ConvLen != w.convLen || !got.Time.Equal(w.at) {
			t.Errorf("entry %d conv_len %d time %v", i, got.ConvLen, got.Time)
		}
	}

	turnTwo, err := store.Turn(ctx, journalSession, 2)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if len(turnTwo) != 1 || turnTwo[0].Step != 0 || turnTwo[0].Name != "assistant" {
		t.Errorf("turn 2 = %+v", turnTwo)
	}
	empty, err := store.Turn(ctx, journalSession, 3)
	if err != nil || len(empty) != 0 {
		t.Errorf("turn 3 = %+v, err = %v", empty, err)
	}
	if last, err := store.LastTurn(ctx, journalSession); err != nil || last != 2 {
		t.Errorf("last turn = %d, err = %v, want 2", last, err)
	}
	if max, err := store.MaxTurn(ctx, journalSession); err != nil || max != 2 {
		t.Errorf("max turn = %d, err = %v, want 2", max, err)
	}
	if high, err := store.HighWater(ctx, journalSession); err != nil || high != 42 {
		t.Errorf("high water = %d, err = %v, want 42", high, err)
	}

	call, found, err := store.Get(ctx, journalSession, journalSession+"/1/2")
	if err != nil || !found {
		t.Fatalf("get call: found=%v err=%v", found, err)
	}
	if call.State != journal.CallDone || call.Tool != "echo" || call.InstanceID != "runtime-0001" {
		t.Errorf("call = %+v", call)
	}
	if call.Content != `{"text":"hello"}` || call.Attempts != 1 {
		t.Errorf("call content = %q attempts = %d", call.Content, call.Attempts)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertBoltString(t, path, "\x00meta", "format_version", "1")
}

func TestStateFixture(t *testing.T) {
	path := copyFixture(t, FileState)
	store, err := state.New(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	ctx := context.Background()
	nightly, found, err := store.LastSlot(ctx, "nightly")
	if err != nil || !found {
		t.Fatalf("nightly: found=%v err=%v", found, err)
	}
	if !nightly.Equal(fixtureSlot) {
		t.Errorf("nightly = %v, want %v", nightly, fixtureSlot)
	}
	hourly, found, err := store.LastSlot(ctx, "hourly")
	if err != nil || !found {
		t.Fatalf("hourly: found=%v err=%v", found, err)
	}
	if want := fixtureSlot.Add(time.Hour); !hourly.Equal(want) {
		t.Errorf("hourly = %v, want %v", hourly, want)
	}
	if _, found, err := store.LastSlot(ctx, "weekly"); err != nil || found {
		t.Errorf("weekly: found=%v err=%v, want absent", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertBoltString(t, path, "meta", "format_version", "1")
}

func TestSessionRecordsFixture(t *testing.T) {
	path := copyFixture(t, FileRecords)
	store, err := session.OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	ctx := context.Background()

	records, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("list returned %d records, want 2", len(records))
	}
	byID := map[string]session.Record{}
	for _, rec := range records {
		byID[rec.Session.ID] = rec
		if rec.Version != 1 {
			t.Errorf("%s version = %d, want 1", rec.Session.ID, rec.Version)
		}
	}
	completed, ok := byID["sess-completed-0001"]
	if !ok {
		t.Fatal("completed session is absent")
	}
	if completed.Session.Status != model.StatusCompleted {
		t.Errorf("completed status = %q", completed.Session.Status)
	}
	if completed.Session.Artifact == nil || completed.Session.Artifact.Key != "sessions/sess-completed-0001.tar.gz" || completed.Session.Artifact.Size != 2048 {
		t.Errorf("completed artifact = %+v", completed.Session.Artifact)
	}
	if completed.Session.MessageCount != 4 || completed.Session.ClosedAt == nil {
		t.Errorf("completed counters = messages %d closed %v", completed.Session.MessageCount, completed.Session.ClosedAt)
	}
	if got := completed.Session.Trigger; got.Kind != "schedule" || got.Name != "nightly" || got.JobID != pendingJobID {
		t.Errorf("completed trigger = %+v", got)
	}
	failed, ok := byID["sess-failed-0001"]
	if !ok {
		t.Fatal("failed session is absent")
	}
	if failed.Session.Status != model.StatusFailed || failed.Session.Error != "image pull failed" {
		t.Errorf("failed session = %+v", failed.Session)
	}
	if failed.Session.Artifact == nil || !failed.Session.Artifact.Partial {
		t.Errorf("failed artifact = %+v", failed.Session.Artifact)
	}

	artifacts, err := store.ListArtifacts(ctx)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifact list has %d entries, want 2", len(artifacts))
	}
	for _, entry := range artifacts {
		want := "sessions/" + entry.SessionID + ".tar.gz"
		if entry.Info.Key != want {
			t.Errorf("artifact %s key = %q, want %q", entry.SessionID, entry.Info.Key, want)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertBoltString(t, path, "meta", "format_version", "1")
}

// TestNewerFormatRefused checks that every store refuses a fixture stamped
// with a format the build does not read, and names both versions.
func TestNewerFormatRefused(t *testing.T) {
	const wantVersion = " is newer than this build reads (1); upgrade mayflyd"
	ctx := context.Background()

	t.Run("queue bbolt", func(t *testing.T) {
		path := copyFixture(t, FileQueueBolt)
		setBoltString(t, path, queueBoltBucket, queueBoltKey, "2")
		_, err := queue.New(ctx, config.QueueConfig{Backend: "bbolt", Bolt: config.BoltQueueConfig{Path: path}})
		requireFormatError(t, err, "queue format version 2"+wantVersion)
	})
	t.Run("queue redis", func(t *testing.T) {
		server := miniredis.RunT(t)
		if err := server.Set(redisKey("format_version"), "2"); err != nil {
			t.Fatal(err)
		}
		_, err := queue.New(ctx, config.QueueConfig{Backend: "redis", Redis: config.RedisQueueConfig{
			URL:       "redis://" + server.Addr(),
			KeyPrefix: redisKeyPrefix,
		}})
		requireFormatError(t, err, "queue format version 2"+wantVersion)
	})
	t.Run("journal", func(t *testing.T) {
		path := copyFixture(t, FileJournal)
		setBoltString(t, path, "\x00meta", "format_version", "2")
		store, err := journal.OpenBolt(path)
		if err == nil {
			_ = store.Close()
		}
		requireFormatError(t, err, "journal format version 2"+wantVersion)
	})
	t.Run("state", func(t *testing.T) {
		path := copyFixture(t, FileState)
		setBoltString(t, path, "meta", "format_version", "2")
		store, err := state.New(path)
		if err == nil {
			_ = store.Close()
		}
		requireFormatError(t, err, "state format version 2"+wantVersion)
	})
	t.Run("session records", func(t *testing.T) {
		path := copyFixture(t, FileRecords)
		setBoltString(t, path, "meta", "format_version", "2")
		store, err := session.OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
		if err == nil {
			_ = store.Close()
		}
		requireFormatError(t, err, "session record format version 2"+wantVersion)
	})
}

func requireFormatError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("store opened a newer format, want an error")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func loadRedisFixture(t *testing.T, server *miniredis.Miniredis, dir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, FileRedisDump))
	if err != nil {
		t.Fatalf("read redis dump: %v", err)
	}
	var dump redisFixture
	if err := json.Unmarshal(data, &dump); err != nil {
		t.Fatalf("decode redis dump: %v", err)
	}
	for key, value := range dump.Strings {
		if err := server.Set(key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}
	for key, fields := range dump.Hashes {
		for field, value := range fields {
			server.HSet(key, field, value)
		}
	}
	for key, members := range dump.ZSets {
		for _, member := range members {
			if _, err := server.ZAdd(key, float64(member.Score), member.Member); err != nil {
				t.Fatalf("zadd %s %s: %v", key, member.Member, err)
			}
		}
	}
}

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	return copyFixtureFrom(t, filepath.Join("testdata", v020), name)
}

func copyFixtureFrom(t *testing.T, dir, name string) string {
	t.Helper()
	src := filepath.Join(dir, name)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write fixture copy: %v", err)
	}
	return dst
}

func assertBoltString(t *testing.T, path, bucket, key, want string) {
	t.Helper()
	got := readBoltValue(t, path, bucket, key)
	if string(got) != want {
		t.Errorf("%s/%s = %q, want %q", bucket, key, got, want)
	}
}

func readBoltValue(t *testing.T, path, bucket, key string) []byte {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open %s read-only: %v", path, err)
	}
	defer db.Close()
	var out []byte
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %q is missing", bucket)
		}
		out = append([]byte(nil), b.Get([]byte(key))...)
		return nil
	})
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, key, err)
	}
	return out
}

func setBoltString(t *testing.T, path, bucket, key, value string) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), []byte(value))
	})
	if err != nil {
		t.Fatalf("set %s/%s: %v", bucket, key, err)
	}
}
