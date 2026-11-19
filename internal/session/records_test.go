package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

func testRecordStores(t *testing.T) map[string]func() RecordStore {
	t.Helper()
	return map[string]func() RecordStore{
		"memory": func() RecordStore { return NewMemoryRecords() },
		"bbolt": func() RecordStore {
			store, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
	}
}

func TestRecordStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	for name, open := range testRecordStores(t) {
		t.Run(name, func(t *testing.T) {
			store := open()
			defer store.Close()
			now := time.Now().UTC().Truncate(time.Second)
			closed := now
			rec := Record{Session: model.Session{
				ID:           "s1",
				Title:        "one",
				Status:       model.StatusCompleted,
				CreatedAt:    now.Add(-time.Minute),
				ClosedAt:     &closed,
				MessageCount: 3,
				Skills:       []string{"go"},
			}}
			if err := store.Save(ctx, rec); err != nil {
				t.Fatal(err)
			}
			if err := store.Save(ctx, Record{Session: model.Session{ID: "s2", Status: model.StatusFailed}}); err != nil {
				t.Fatal(err)
			}
			records, err := store.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 2 {
				t.Fatalf("records = %+v, want 2", records)
			}
			for _, got := range records {
				if got.Session.ID == "s1" {
					if got.Session.MessageCount != 3 || len(got.Session.Skills) != 1 || got.Session.Status != model.StatusCompleted {
						t.Fatalf("record = %+v", got)
					}
					if got.Version != RecordFormatVersion && got.Version != 0 {
						t.Fatalf("record version = %d", got.Version)
					}
				}
			}
			if err := store.Delete(ctx, "s1"); err != nil {
				t.Fatal(err)
			}
			if records, _ := store.List(ctx); len(records) != 1 {
				t.Fatalf("records after delete = %+v", records)
			}

			info := model.ArtifactInfo{Key: "sessions/s2/bundle.tar.gz", Size: 10, SHA256: strings.Repeat("a", 64), CreatedAt: now}
			if err := store.SaveArtifact(ctx, "s2", info); err != nil {
				t.Fatal(err)
			}
			entries, err := store.ListArtifacts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].SessionID != "s2" || entries[0].Info.Key != info.Key {
				t.Fatalf("artifacts = %+v", entries)
			}
			if err := store.DeleteArtifact(ctx, "s2"); err != nil {
				t.Fatal(err)
			}
			if entries, _ := store.ListArtifacts(ctx); len(entries) != 0 {
				t.Fatalf("artifacts after delete = %+v", entries)
			}
		})
	}
}

// TestRecordStoreReadsLegacyRecord writes a record as v0.1.0 would have left
// it: no version field.
func TestRecordStoreReadsLegacyRecord(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"session":{"id":"legacy","status":"completed","title":"old"}}`)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketSessions).Put([]byte("legacy"), payload)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatalf("open a legacy record store: %v", err)
	}
	defer store.Close()
	records, err := store.List(ctx)
	if err != nil {
		t.Fatalf("read a legacy record: %v", err)
	}
	if len(records) != 1 || records[0].Session.ID != "legacy" || records[0].Session.Title != "old" {
		t.Fatalf("records = %+v", records)
	}
}

func TestRecordStoreRejectsNewerStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketMeta).Put(recordsKeyFormat, []byte("2"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open a newer store: %v, want a version error", err)
	}
}

func TestRecordStoreRejectsNewerRecord(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(Record{Version: RecordFormatVersion + 1, Session: model.Session{ID: "future"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucketSessions).Put([]byte("future"), payload)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.List(ctx); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("read a newer record: %v, want a version error", err)
	}
}

// TestRestartKeepsFinishedSessionsAndArtifacts creates and finalizes a
// session, closes its record store, and starts a second manager over the same
// store.
func TestRestartKeepsFinishedSessionsAndArtifacts(t *testing.T) {
	ctx := context.Background()
	recordsPath := filepath.Join(t.TempDir(), "sessions.db")
	payload := []byte("retained bundle")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])

	records, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: recordsPath})
	if err != nil {
		t.Fatal(err)
	}
	first := testManager(t)
	first.runner = nopRunner{}
	if err := first.loadAgents(); err != nil {
		t.Fatal(err)
	}
	first.SetRecords(records)
	sess, err := first.Create(ctx, CreateOptions{Prompt: "do it", Title: "finished"})
	if err != nil {
		t.Fatal(err)
	}
	inst, ok := first.instance(sess.ID)
	if !ok {
		t.Fatal("created session is not registered")
	}
	if _, err := first.UploadArtifact(ctx, sess.ID, inst.token, bytes.NewReader(payload), int64(len(payload)), digest, false); err != nil {
		t.Fatal(err)
	}
	first.finalize(inst, model.StatusCompleted, "closed", "")
	if err := records.Close(); err != nil {
		t.Fatal(err)
	}

	records, err = OpenRecords(config.RecordConfig{Backend: "bbolt", Path: recordsPath})
	if err != nil {
		t.Fatal(err)
	}
	defer records.Close()
	second := testManager(t)
	second.store = first.store
	second.SetRecords(records)
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer second.Shutdown(context.Background())

	got, ok := second.Get(sess.ID)
	if !ok {
		t.Fatal("finished session is missing after restart")
	}
	if got.Status != model.StatusCompleted || got.Title != "finished" || got.Prompt != "do it" {
		t.Fatalf("restored session = %+v", got)
	}
	if got.ClosedAt == nil || got.CreatedAt.IsZero() {
		t.Fatalf("restored timestamps = %+v", got)
	}
	if got.Artifact == nil || got.Artifact.SHA256 != digest {
		t.Fatalf("restored artifact = %+v", got.Artifact)
	}
	list := second.List()
	found := false
	for _, item := range list {
		if item.ID == sess.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("list = %+v", list)
	}
	rc, size, err := second.OpenArtifact(ctx, sess.ID)
	if err != nil {
		t.Fatalf("download after restart: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(payload)) || !bytes.Equal(body, payload) {
		t.Fatalf("download = %d bytes %q", size, body)
	}
}

// TestRestartMarksUnfinishedSessionFailed covers a record whose runtime no
// control plane adopted: nothing can continue its turn.
func TestRestartMarksUnfinishedSessionFailed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	records, err := OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	first := testManager(t)
	first.SetRecords(records)
	first.persistRecord(model.Session{ID: "s2", Status: model.StatusRunning, CreatedAt: time.Now().UTC().Add(-time.Minute)})
	if err := records.Close(); err != nil {
		t.Fatal(err)
	}

	records, err = OpenRecords(config.RecordConfig{Backend: "bbolt", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer records.Close()
	second := testManager(t)
	second.SetRecords(records)
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer second.Shutdown(context.Background())

	got, ok := second.Get("s2")
	if !ok {
		t.Fatal("interrupted session is missing after restart")
	}
	if got.Status != model.StatusFailed || got.Error == "" || got.ClosedAt == nil {
		t.Fatalf("interrupted session = %+v", got)
	}
	// The transition is durable, so a third start reads it back.
	stored, err := records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Session.Status != model.StatusFailed {
		t.Fatalf("stored records = %+v", stored)
	}
}

func TestPruneRecordsDropsExpiredSessions(t *testing.T) {
	ctx := context.Background()
	records := NewMemoryRecords()
	m := testManager(t)
	m.SetRecords(records)
	m.cfg.Sessions.Retention = config.Duration(30 * time.Minute)
	m.cfg.Artifacts.IndexRetention = config.Duration(30 * time.Minute)
	now := time.Now().UTC()
	closedOld := now.Add(-2 * time.Hour)
	closedNew := now.Add(-time.Minute)
	if err := records.Save(ctx, Record{Session: model.Session{ID: "old", Status: model.StatusCompleted, CreatedAt: closedOld, ClosedAt: &closedOld}}); err != nil {
		t.Fatal(err)
	}
	if err := records.Save(ctx, Record{Session: model.Session{ID: "new", Status: model.StatusCompleted, CreatedAt: closedNew, ClosedAt: &closedNew}}); err != nil {
		t.Fatal(err)
	}
	m.history["old"] = model.Session{ID: "old", Status: model.StatusCompleted, ClosedAt: &closedOld}
	m.history["new"] = model.Session{ID: "new", Status: model.StatusCompleted, ClosedAt: &closedNew}
	m.artifacts["old"] = model.ArtifactInfo{Key: "k", CreatedAt: closedOld}
	m.artifacts["new"] = model.ArtifactInfo{Key: "k", CreatedAt: closedNew}
	if err := records.SaveArtifact(ctx, "old", m.artifacts["old"]); err != nil {
		t.Fatal(err)
	}
	if err := records.SaveArtifact(ctx, "new", m.artifacts["new"]); err != nil {
		t.Fatal(err)
	}

	m.pruneRecords(now)

	if _, ok := m.Get("old"); ok {
		t.Fatal("session past retention was kept")
	}
	if _, ok := m.Get("new"); !ok {
		t.Fatal("session inside retention was removed")
	}
	stored, err := records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Session.ID != "new" {
		t.Fatalf("stored records = %+v", stored)
	}
	if _, ok := m.ArtifactInfo("old"); ok {
		t.Fatal("artifact index entry past retention was kept")
	}
	if _, ok := m.ArtifactInfo("new"); !ok {
		t.Fatal("artifact index entry inside retention was removed")
	}
	if entries, _ := records.ListArtifacts(ctx); len(entries) != 1 || entries[0].SessionID != "new" {
		t.Fatalf("stored artifacts = %+v", entries)
	}
}

func TestDeleteDropsStoredSession(t *testing.T) {
	ctx := context.Background()
	records := NewMemoryRecords()
	m := testManager(t)
	m.SetRecords(records)
	closed := time.Now().UTC()
	if err := records.Save(ctx, Record{Session: model.Session{ID: "gone", Status: model.StatusCompleted, ClosedAt: &closed}}); err != nil {
		t.Fatal(err)
	}
	m.history["gone"] = model.Session{ID: "gone", Status: model.StatusCompleted, ClosedAt: &closed}

	if err := m.Delete("gone"); err != nil {
		t.Fatalf("delete a stored session: %v", err)
	}
	if _, ok := m.Get("gone"); ok {
		t.Fatal("deleted session is still listed")
	}
	if stored, _ := records.List(ctx); len(stored) != 0 {
		t.Fatalf("stored records = %+v", stored)
	}
	if err := m.Delete("gone"); err != ErrNotFound {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

// TestPersistInstanceOrdersSnapshots forces two transitions to overlap and
// checks the later state is the one stored: a snapshot taken earlier must not
// land after a newer one.
func TestPersistInstanceOrdersSnapshots(t *testing.T) {
	m := testManager(t)
	gated := &gatedRecords{
		RecordStore: NewMemoryRecords(),
		entered:     make(chan model.Session, 4),
		release:     make(chan struct{}),
	}
	m.SetRecords(gated)
	inst := &instance{sess: model.Session{ID: "s1", Status: model.StatusRunning}}

	go m.persistInstance(inst)
	if got := <-gated.entered; got.Status != model.StatusRunning {
		t.Fatalf("first snapshot = %q, want running", got.Status)
	}
	inst.mu.Lock()
	inst.sess.Status = model.StatusCompleted
	inst.mu.Unlock()

	secondDone := make(chan struct{})
	go func() {
		m.persistInstance(inst)
		close(secondDone)
	}()
	select {
	case got := <-gated.entered:
		t.Fatalf("second snapshot entered the store while the first was in flight: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}

	gated.release <- struct{}{}
	select {
	case got := <-gated.entered:
		if got.Status != model.StatusCompleted {
			t.Fatalf("second snapshot = %q, want completed", got.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second snapshot was never written")
	}
	gated.release <- struct{}{}
	<-secondDone

	gated.mu.Lock()
	last := gated.saved[len(gated.saved)-1]
	gated.mu.Unlock()
	if last.Status != model.StatusCompleted {
		t.Fatalf("last stored status = %q, want completed", last.Status)
	}
}

type gatedRecords struct {
	RecordStore
	entered chan model.Session
	release chan struct{}
	mu      sync.Mutex
	saved   []model.Session
}

func (s *gatedRecords) Save(_ context.Context, rec Record) error {
	s.entered <- rec.Session
	<-s.release
	s.mu.Lock()
	s.saved = append(s.saved, rec.Session)
	s.mu.Unlock()
	return nil
}

// TestLoadRecordsWaitsForAdoptionRetry covers a control plane whose adoption
// pass could not list pods. The restored record must stay non-terminal until a
// later pass reports what it can see.
func TestLoadRecordsWaitsForAdoptionRetry(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	var failList atomic.Bool
	failList.Store(true)
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failList.Load() {
			return true, nil, errors.New("api unavailable")
		}
		return false, nil, nil
	})

	m := testManager(t)
	m.cfg.Runtime.Backend = "kubernetes"
	m.kube = k8s.NewForClient(cs, "mayfly")
	m.journal = journal.NewMemory()
	m.runner = &fakeRunner{}
	records := NewMemoryRecords()
	m.SetRecords(records)
	created := time.Now().UTC().Add(-time.Minute)
	m.persistRecord(model.Session{ID: "a2", Status: model.StatusRunning, CreatedAt: created})

	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())

	got, ok := m.Get("a2")
	if !ok {
		t.Fatal("record is missing after a failed adoption pass")
	}
	if got.Status != model.StatusRunning || got.ClosedAt != nil {
		t.Fatalf("record after a failed adoption pass = %+v, want running", got)
	}

	// The API recovers: the retry sees no pod for the session, so the record
	// can be settled.
	failList.Store(false)
	m.retryAdopt()

	got, ok = m.Get("a2")
	if !ok {
		t.Fatal("record disappeared after adoption completed")
	}
	if got.Status != model.StatusFailed || got.ClosedAt == nil {
		t.Fatalf("record after adoption completed = %+v, want failed", got)
	}
}

// TestAdoptFailureKeepsRecordOutOfFailedState covers a session whose pod
// adoption could not read. Its record must not be reported failed while the
// pod may still run.
func TestAdoptFailureKeepsRecordOutOfFailedState(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	var failRead atomic.Bool
	failRead.Store(true)
	cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failRead.Load() {
			return true, nil, errors.New("api unavailable")
		}
		return false, nil, nil
	})

	m := testManager(t)
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	m.cfg.Runtime.Backend = "kubernetes"
	m.cfg.Sessions.MaxRestarts = 2
	m.kube = k8s.NewForClient(cs, "mayfly")
	m.journal = journal.NewMemory()
	m.runner = &fakeRunner{}
	m.stateDir = t.TempDir()
	records := NewMemoryRecords()
	m.SetRecords(records)

	cfg := sessioncfg.Config{
		Version:    sessioncfg.Version,
		SessionID:  "a1",
		Agent:      "default",
		Model:      "stub",
		Title:      "live",
		TTLSeconds: 600,
		CreatedAt:  time.Now().UTC(),
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec := &k8s.SessionSpec{
		Namespace:           "mayfly",
		SessionID:           "a1",
		PodName:             "mayfly-a1",
		SecretName:          "mayfly-a1",
		ConfigMapName:       "mayfly-a1",
		Image:               "example/agent:1",
		PlatformNs:          "mayfly",
		PersistentWorkspace: true,
		SessionJSON:         raw,
		Token:               "token",
		TTL:                 time.Hour,
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := m.kube.CreateSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	m.persistRecord(model.Session{ID: "a1", Status: model.StatusRunning, CreatedAt: cfg.CreatedAt})

	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())

	got, ok := m.Get("a1")
	if !ok {
		t.Fatal("record of an unreadable session is missing")
	}
	if got.Status != model.StatusRunning || got.ClosedAt != nil {
		t.Fatalf("record before adoption recovered = %+v, want running", got)
	}

	failRead.Store(false)
	m.retryAdopt()

	if _, ok := m.instance("a1"); !ok {
		t.Fatal("adoption did not register the session after the read recovered")
	}
	got, ok = m.Get("a1")
	if !ok {
		t.Fatal("session is missing after adoption")
	}
	if got.Status != model.StatusPending || got.ClosedAt != nil {
		t.Fatalf("session after adoption = %+v, want pending", got)
	}
	list := m.List()
	if len(list) != 1 {
		t.Fatalf("list = %+v, want one entry", list)
	}
	stored, err := records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Session.Status != model.StatusPending {
		t.Fatalf("stored records = %+v, want one pending", stored)
	}
}

// adoptTestManager builds a manager whose adoption talks to a fake cluster.
func adoptTestManager(t *testing.T, cs *fake.Clientset) *Manager {
	t.Helper()
	m := testManager(t)
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	m.cfg.Runtime.Backend = "kubernetes"
	m.kube = k8s.NewForClient(cs, "mayfly")
	m.journal = journal.NewMemory()
	m.runner = &fakeRunner{}
	m.stateDir = t.TempDir()
	return m
}

// createSessionObjects writes one session's Secret, ConfigMap, policy, claim,
// and pod into the fake cluster.
func createSessionObjects(t *testing.T, ctx context.Context, kube *k8s.Client, id string, cfg sessioncfg.Config) *k8s.SessionSpec {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec := &k8s.SessionSpec{
		Namespace:           "mayfly",
		SessionID:           id,
		PodName:             "mayfly-" + id,
		SecretName:          "mayfly-" + id,
		ConfigMapName:       "mayfly-" + id,
		Image:               "example/agent:1",
		PlatformNs:          "mayfly",
		PersistentWorkspace: true,
		EnableNetworkPolicy: true,
		SessionJSON:         raw,
		Token:               "token",
		TTL:                 time.Hour,
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := kube.CreateSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func liveConfig(id string) sessioncfg.Config {
	return sessioncfg.Config{
		Version:    sessioncfg.Version,
		SessionID:  id,
		Agent:      "default",
		Model:      "stub",
		Title:      id,
		TTLSeconds: 600,
		CreatedAt:  time.Now().UTC(),
	}
}

// assertSessionObjects reads the tracker directly, so a reactor that makes
// the API fail cannot hide an object that still exists.
func assertSessionObjects(t *testing.T, ctx context.Context, cs *fake.Clientset, spec *k8s.SessionSpec, want bool) {
	t.Helper()
	_ = ctx
	checks := []struct {
		name      string
		resource  string
		object    string
		groupName string
	}{
		{"configmap", "configmaps", spec.ConfigMapName, ""},
		{"secret", "secrets", spec.SecretName, ""},
		{"claim", "persistentvolumeclaims", spec.PVCName, ""},
		{"policy", "networkpolicies", spec.NetworkPolicyName, "networking.k8s.io"},
	}
	for _, check := range checks {
		gvr := schema.GroupVersionResource{Version: "v1", Resource: check.resource}
		if check.groupName != "" {
			gvr = schema.GroupVersionResource{Group: check.groupName, Version: "v1", Resource: check.resource}
		}
		_, err := cs.Tracker().Get(gvr, spec.Namespace, check.object)
		if want && err != nil {
			t.Fatalf("%s was removed from a live session: %v", check.name, err)
		}
		if !want && !apierrors.IsNotFound(err) {
			t.Fatalf("%s still exists: %v", check.name, err)
		}
	}
}

// TestAdoptKeepsObjectsOfLivePodlessSession covers the keep set: a session
// this control plane is running has no pod between replacements, so neither
// list names it, and garbage collection must still leave its objects alone.
func TestAdoptKeepsObjectsOfLivePodlessSession(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	m := adoptTestManager(t, cs)
	spec := createSessionObjects(t, ctx, m.kube, "live2", liveConfig("live2"))
	if err := m.kube.DeletePod(ctx, spec.PodName, 0); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.sessions["live2"] = &instance{sess: model.Session{ID: "live2", Status: model.StatusRunning}, tokenHash: hashToken("t")}
	m.mu.Unlock()
	if err := m.journal.Append(ctx, journal.Entry{SessionID: "live2", Turn: 3, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.adopt(ctx); err != nil {
		t.Fatal(err)
	}

	assertSessionObjects(t, ctx, cs, spec, true)
	if last, err := m.journal.LastTurn(ctx, "live2"); err != nil || last != 3 {
		t.Fatalf("journal after adopt: last turn %d, %v; want 3", last, err)
	}
}

// TestRetryAdoptKeepsLivePodlessSession covers a retry that runs while a live
// session has no pod: the retry adopts pending sessions only and must not
// collect anything.
func TestRetryAdoptKeepsLivePodlessSession(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	m := adoptTestManager(t, cs)
	spec := createSessionObjects(t, ctx, m.kube, "live3", liveConfig("live3"))
	if err := m.kube.DeletePod(ctx, spec.PodName, 0); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.sessions["live3"] = &instance{sess: model.Session{ID: "live3", Status: model.StatusRunning}, tokenHash: hashToken("t")}
	m.adoptPending = map[string]adoptFailure{"gone": {Attempts: 1, Reason: "read the session config"}}
	m.mu.Unlock()
	if err := m.journal.Append(ctx, journal.Entry{SessionID: "live3", Turn: 5, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}

	m.retryAdopt()

	assertSessionObjects(t, ctx, cs, spec, true)
	if last, err := m.journal.LastTurn(ctx, "live3"); err != nil || last != 5 {
		t.Fatalf("journal after retry: last turn %d, %v; want 5", last, err)
	}
	if m.pendingCount() != 0 {
		t.Fatalf("pending = %d, want the vanished session resolved", m.pendingCount())
	}
	if _, ok := m.instance("gone"); ok {
		t.Fatal("a session with no pod or objects was adopted")
	}
}

// TestAdoptGivesUpAfterMaxAttempts checks a session that can never be read
// stops being retried and its record says why.
func TestAdoptGivesUpAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	records := NewMemoryRecords()
	m := testManager(t)
	m.SetRecords(records)
	created := time.Now().UTC().Add(-time.Minute)
	m.history["c1"] = model.Session{ID: "c1", Status: model.StatusRunning, CreatedAt: created}
	m.history["c2"] = model.Session{ID: "c2", Status: model.StatusRunning, CreatedAt: created}
	m.adoptPending = map[string]adoptFailure{
		"c1": {Attempts: maxAdoptAttempts, Reason: "read the session config"},
		"c2": {Attempts: maxAdoptAttempts - 1, Reason: "read the session config"},
	}

	m.settleInterrupted()

	got, ok := m.Get("c1")
	if !ok {
		t.Fatal("capped session is missing from the list")
	}
	if got.Status != model.StatusFailed || got.ClosedAt == nil {
		t.Fatalf("capped session = %+v, want failed", got)
	}
	if !strings.Contains(got.Error, "after 10 attempts") || !strings.Contains(got.Error, "read the session config") {
		t.Fatalf("error = %q", got.Error)
	}
	if got, ok := m.Get("c2"); !ok || got.Status != model.StatusRunning {
		t.Fatalf("session under the cap = %+v, want running", got)
	}
	if m.pendingCount() != 1 {
		t.Fatalf("pending = %d, want only the session under the cap", m.pendingCount())
	}
	stored, err := records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Session.ID != "c1" || stored[0].Session.Status != model.StatusFailed {
		t.Fatalf("stored records = %+v, want the capped session failed", stored)
	}
}

// TestDeleteCleansPendingAdoptionSession checks DELETE stops the pod, removes
// the objects and journal, drops the record, and stops the retry.
func TestDeleteCleansPendingAdoptionSession(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	var failRead atomic.Bool
	failRead.Store(true)
	cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failRead.Load() {
			return true, nil, errors.New("api unavailable")
		}
		return false, nil, nil
	})
	m := adoptTestManager(t, cs)
	records := NewMemoryRecords()
	m.SetRecords(records)
	spec := createSessionObjects(t, ctx, m.kube, "d1", liveConfig("d1"))
	m.persistRecord(model.Session{ID: "d1", Status: model.StatusRunning, CreatedAt: time.Now().UTC()})
	if err := m.journal.Append(ctx, journal.Entry{SessionID: "d1", Turn: 4, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())
	if !m.pendingAdoption("d1") {
		t.Fatal("the session was not left pending after a failed read")
	}
	// The API recovers; the cleanup path must not depend on the read that
	// failed.
	failRead.Store(false)

	if err := m.Delete("d1"); err != nil {
		t.Fatalf("delete a pending session: %v", err)
	}

	if m.pendingAdoption("d1") {
		t.Fatal("the pending id survived delete")
	}
	if _, ok := m.Get("d1"); ok {
		t.Fatal("the record survived delete")
	}
	if stored, _ := records.List(ctx); len(stored) != 0 {
		t.Fatalf("stored records = %+v", stored)
	}
	if _, err := cs.CoreV1().Pods("mayfly").Get(ctx, spec.PodName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod after delete: %v, want gone", err)
	}
	assertSessionObjects(t, ctx, cs, spec, false)
	if last, err := m.journal.LastTurn(ctx, "d1"); err != nil || last != 0 {
		t.Fatalf("journal after delete: last turn %d, %v; want deleted", last, err)
	}
}

// TestPendingAdoptionWithoutRecordGetsFailedRecord covers a session adopted
// from a previous lifetime that left no record. Without a record it cannot be
// listed or deleted, so settlement creates one, and the retry continues.
func TestPendingAdoptionWithoutRecordGetsFailedRecord(t *testing.T) {
	ctx := context.Background()
	records := NewMemoryRecords()
	m := testManager(t)
	m.SetRecords(records)
	m.adoptPending = map[string]adoptFailure{
		"u1": {Attempts: 1, Reason: "read the session config"},
	}

	m.settleInterrupted()

	got, ok := m.Get("u1")
	if !ok {
		t.Fatal("a session with no record is still not listed")
	}
	if got.Status != model.StatusFailed || got.ClosedAt == nil {
		t.Fatalf("settled session = %+v, want failed", got)
	}
	if !strings.Contains(got.Error, "could not be adopted") {
		t.Fatalf("error = %q", got.Error)
	}
	if m.pendingCount() != 1 {
		t.Fatalf("pending = %d, want the retry to continue", m.pendingCount())
	}
	stored, err := records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Session.ID != "u1" || stored[0].Session.Status != model.StatusFailed {
		t.Fatalf("stored records = %+v", stored)
	}
	if err := m.Delete("u1"); err != nil {
		t.Fatalf("delete a settled session: %v", err)
	}
	if _, ok := m.Get("u1"); ok {
		t.Fatal("settled session survived delete")
	}
	if m.pendingCount() != 0 {
		t.Fatalf("pending after delete = %d, want 0", m.pendingCount())
	}
}

// TestAdoptCapCleansSettledSession checks a session that fails adoption to the
// cap is cleaned up, not left running with no control plane behind it.
func TestAdoptCapCleansSettledSession(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	m := adoptTestManager(t, cs)
	spec := createSessionObjects(t, ctx, m.kube, "cap1", liveConfig("cap1"))
	if err := m.journal.Append(ctx, journal.Entry{SessionID: "cap1", Turn: 6, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-time.Minute)
	m.history["cap1"] = model.Session{ID: "cap1", Status: model.StatusRunning, CreatedAt: created}
	m.adoptPending = map[string]adoptFailure{
		"cap1": {Attempts: maxAdoptAttempts, Reason: "read the session config"},
	}

	m.settleInterrupted()

	got, ok := m.Get("cap1")
	if !ok || got.Status != model.StatusFailed {
		t.Fatalf("settled session = %+v, want failed", got)
	}
	if m.pendingCount() != 0 {
		t.Fatalf("pending = %d, want the session settled", m.pendingCount())
	}
	if _, err := cs.CoreV1().Pods("mayfly").Get(ctx, spec.PodName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod after settlement: %v, want gone", err)
	}
	assertSessionObjects(t, ctx, cs, spec, false)
	if last, err := m.journal.LastTurn(ctx, "cap1"); err != nil || last != 0 {
		t.Fatalf("journal after settlement: last turn %d, %v; want deleted", last, err)
	}
}

// TestSettleNeverCleansLiveSession keeps settlement from removing the pod,
// objects, or journal of a session this control plane runs, even when a stale
// record and a pending entry name it.
func TestSettleNeverCleansLiveSession(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	m := adoptTestManager(t, cs)
	spec := createSessionObjects(t, ctx, m.kube, "live4", liveConfig("live4"))
	if err := m.journal.Append(ctx, journal.Entry{SessionID: "live4", Turn: 7, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.sessions["live4"] = &instance{sess: model.Session{ID: "live4", Status: model.StatusRunning}, tokenHash: hashToken("t")}
	m.history["live4"] = model.Session{ID: "live4", Status: model.StatusRunning, CreatedAt: time.Now().UTC()}
	m.adoptPending = map[string]adoptFailure{
		"live4": {Attempts: maxAdoptAttempts, Reason: "read the session config"},
	}
	m.mu.Unlock()

	m.settleInterrupted()

	if _, ok := m.instance("live4"); !ok {
		t.Fatal("the live session was removed")
	}
	assertSessionObjects(t, ctx, cs, spec, true)
	if last, err := m.journal.LastTurn(ctx, "live4"); err != nil || last != 7 {
		t.Fatalf("journal after settlement: last turn %d, %v; want 7", last, err)
	}
	if m.pendingCount() != 0 {
		t.Fatalf("pending = %d, want the live session removed from retries", m.pendingCount())
	}
	if _, stored := m.historySession("live4"); stored {
		t.Fatal("the stale record of a live session was kept")
	}
}

// TestAdoptionWithoutRecordIsListedAndRetried drives a session the previous
// lifetime left no record for through Start and two retries: it is listed as
// failed at once, stays in the retry set through a second failure, and is
// adopted when the API recovers without losing its objects or journal.
func TestAdoptionWithoutRecordIsListedAndRetried(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	var failRead atomic.Bool
	failRead.Store(true)
	cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failRead.Load() {
			return true, nil, errors.New("api unavailable")
		}
		return false, nil, nil
	})
	m := adoptTestManager(t, cs)
	records := NewMemoryRecords()
	m.SetRecords(records)
	spec := createSessionObjects(t, ctx, m.kube, "n1", liveConfig("n1"))
	if err := m.journal.Append(ctx, journal.Entry{SessionID: "n1", Turn: 2, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())

	got, ok := m.Get("n1")
	if !ok {
		t.Fatal("a session with no record cannot be listed")
	}
	if got.Status != model.StatusFailed || !strings.Contains(got.Error, "after 1 attempt:") {
		t.Fatalf("first settlement = %+v", got)
	}
	if !m.pendingAdoption("n1") {
		t.Fatal("the first failure stopped the retry")
	}
	assertSessionObjects(t, ctx, cs, spec, true)
	if last, err := m.journal.LastTurn(ctx, "n1"); err != nil || last != 2 {
		t.Fatalf("journal after the first failure: last turn %d, %v; want 2", last, err)
	}

	m.retryAdopt()
	if !m.pendingAdoption("n1") {
		t.Fatal("the second failure stopped the retry")
	}
	assertSessionObjects(t, ctx, cs, spec, true)
	if last, err := m.journal.LastTurn(ctx, "n1"); err != nil || last != 2 {
		t.Fatalf("journal after the second failure: last turn %d, %v; want 2", last, err)
	}

	failRead.Store(false)
	m.retryAdopt()

	if _, ok := m.instance("n1"); !ok {
		t.Fatal("the recovered session was not adopted")
	}
	if m.pendingAdoption("n1") {
		t.Fatal("the adopted session stayed in the retry set")
	}
	got, ok = m.Get("n1")
	if !ok || got.Status != model.StatusPending {
		t.Fatalf("adopted session = %+v, want pending", got)
	}
	stored, err := records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Session.ID != "n1" || stored[0].Session.Status != model.StatusPending {
		t.Fatalf("stored records = %+v, want the live session", stored)
	}
	assertSessionObjects(t, ctx, cs, spec, true)
	if last, err := m.journal.LastTurn(ctx, "n1"); err != nil || last != 2 {
		t.Fatalf("journal after adoption: last turn %d, %v; want 2", last, err)
	}
}

// TestAdoptionWithoutRecordTransientErrorKeepsLeftovers checks one failed pass
// is not enough to remove a session's pod, objects, or journal.
func TestAdoptionWithoutRecordTransientErrorKeepsLeftovers(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	var failRead atomic.Bool
	failRead.Store(true)
	cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failRead.Load() {
			return true, nil, errors.New("api unavailable")
		}
		return false, nil, nil
	})
	m := adoptTestManager(t, cs)
	m.SetRecords(NewMemoryRecords())
	spec := createSessionObjects(t, ctx, m.kube, "n2", liveConfig("n2"))
	if err := m.journal.Append(ctx, journal.Entry{SessionID: "n2", Turn: 8, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())
	assertSessionObjects(t, ctx, cs, spec, true)

	failRead.Store(false)
	m.retryAdopt()

	if _, ok := m.instance("n2"); !ok {
		t.Fatal("the session was not adopted after the transient error")
	}
	assertSessionObjects(t, ctx, cs, spec, true)
	if last, err := m.journal.LastTurn(ctx, "n2"); err != nil || last != 8 {
		t.Fatalf("journal after the transient error: last turn %d, %v; want 8", last, err)
	}
}

// TestAdoptCapCleansSettlementWithoutRecord checks a no-record session that
// reaches the cap is cleaned up once, not on the first failure.
func TestAdoptCapCleansSettlementWithoutRecord(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	m := adoptTestManager(t, cs)
	spec := createSessionObjects(t, ctx, m.kube, "n3", liveConfig("n3"))
	if err := m.journal.Append(ctx, journal.Entry{SessionID: "n3", Turn: 9, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}
	m.adoptPending = map[string]adoptFailure{
		"n3": {Attempts: maxAdoptAttempts, Reason: "read the session config"},
	}

	m.settleInterrupted()

	got, ok := m.Get("n3")
	if !ok || got.Status != model.StatusFailed {
		t.Fatalf("capped session = %+v, want a listed failure", got)
	}
	if !strings.Contains(got.Error, "after 10 attempts:") {
		t.Fatalf("error = %q", got.Error)
	}
	if m.pendingCount() != 0 {
		t.Fatalf("pending = %d, want the cap to stop retries", m.pendingCount())
	}
	if _, err := cs.CoreV1().Pods("mayfly").Get(ctx, spec.PodName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod after the cap: %v, want gone", err)
	}
	assertSessionObjects(t, ctx, cs, spec, false)
	if last, err := m.journal.LastTurn(ctx, "n3"); err != nil || last != 0 {
		t.Fatalf("journal after the cap: last turn %d, %v; want deleted", last, err)
	}
}
