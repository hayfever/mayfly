package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime.Image = "example/agent:1"
	return &Manager{
		cfg:        cfg,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:      store,
		hub:        hub.New(100),
		sessions:   map[string]*instance{},
		tokenIndex: map[string]string{},
		artifacts:  map[string]model.ArtifactInfo{},
	}
}

func TestPrepareAttachesImagePullSecrets(t *testing.T) {
	m := testManager(t)
	m.cfg.Runtime.ImagePullSecrets = []string{"registry-credentials", " "}
	inst := &instance{
		sess: model.Session{ID: "abc123"},
		runtimeCfg: sessioncfg.Config{
			Version:      sessioncfg.Version,
			SessionID:    "abc123",
			Model:        "model",
			PlatformURL:  "http://mayfly:8080",
			LLMBaseURL:   "http://mayfly:8080/internal/v1/llm",
			WorkspaceDir: "/workspace",
			ArtifactsDir: "/var/lib/mayfly/artifacts",
			MaxSteps:     1,
		},
	}
	spec, err := m.prepare(context.Background(), inst, "mayfly-abc123", "token", 30*time.Minute, "example/agent:1", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pod := spec.Pod()
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "registry-credentials" {
		t.Fatalf("image pull secrets = %v", pod.Spec.ImagePullSecrets)
	}
}

func TestRuntimeConnectedMarksSessionRunning(t *testing.T) {
	m := testManager(t)
	inst := &instance{
		sess:      model.Session{ID: "s1", Status: model.StatusPending},
		tokenHash: hashToken("token"),
	}
	m.sessions["s1"] = inst
	m.tokenIndex[inst.tokenHash] = "s1"

	m.RuntimeConnected("s1")
	sess, ok := m.Get("s1")
	if !ok {
		t.Fatal("session disappeared")
	}
	if sess.Status != model.StatusRunning || sess.StartedAt == nil {
		t.Fatalf("status = %s started = %v", sess.Status, sess.StartedAt)
	}
	m.RuntimeConnected("s1")
	running := 0
	for _, ev := range m.hub.EventsAfter("s1", 0) {
		if ev.Type == model.EventStatus && ev.Text == "Session running." {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running events = %d, want 1", running)
	}
}

func TestReserveEnforcesMaxConcurrent(t *testing.T) {
	m := testManager(t)
	m.cfg.Sessions.MaxConcurrent = 1
	first := &instance{sess: model.Session{ID: "a"}, tokenHash: hashToken("a")}

	m.mu.Lock()
	err := m.reserveLocked(first)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	err = m.reserveLocked(&instance{sess: model.Session{ID: "b"}, tokenHash: hashToken("b")})
	m.mu.Unlock()
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("second reservation error = %v, want ErrCapacity", err)
	}

	first.mu.Lock()
	first.finalized = true
	first.mu.Unlock()
	m.mu.Lock()
	err = m.reserveLocked(&instance{sess: model.Session{ID: "c"}, tokenHash: hashToken("c")})
	m.mu.Unlock()
	if err != nil {
		t.Fatalf("finalized sessions must free capacity: %v", err)
	}
}

func TestReapForgetsFinishedSessionsAfterRetention(t *testing.T) {
	m := testManager(t)
	m.cfg.Sessions.Retention = config.Duration(30 * time.Minute)
	m.hub.Append("old", model.Event{Type: model.EventSystem, Text: "finished"})
	m.hub.Append("new", model.Event{Type: model.EventSystem, Text: "just closed"})

	oldClosed := time.Now().UTC().Add(-2 * time.Hour)
	recentClosed := time.Now().UTC()
	m.sessions["old"] = &instance{
		sess:      model.Session{ID: "old", Status: model.StatusCompleted, ClosedAt: &oldClosed},
		finalized: true,
		tokenHash: "h",
	}
	m.sessions["new"] = &instance{
		sess:      model.Session{ID: "new", Status: model.StatusCompleted, ClosedAt: &recentClosed},
		finalized: true,
		tokenHash: "h2",
	}
	m.tokenIndex["h"] = "old"

	m.reap()

	if _, ok := m.Get("old"); ok {
		t.Fatal("session past retention was kept")
	}
	if m.hub.LastSeq("old") != 0 {
		t.Fatal("hub stream was not removed")
	}
	if _, ok := m.tokenIndex["h"]; ok {
		t.Fatal("token survived session removal")
	}
	if _, ok := m.Get("new"); !ok {
		t.Fatal("session inside retention was removed")
	}
}

func TestReadyFollowsLifecycleStateOnly(t *testing.T) {
	m := testManager(t)
	if err := m.Ready(); err == nil {
		t.Fatal("ready before start")
	}
	m.started.Store(true)
	if err := m.Ready(); err != nil {
		t.Fatalf("ready after start: %v", err)
	}
	// The artifact store is deliberately not part of readiness: it can be
	// replaced while the process keeps serving live sessions.
	m.store = nil
	if err := m.Ready(); err != nil {
		t.Fatalf("store state leaked into readiness: %v", err)
	}
	m.started.Store(false)
	if err := m.Ready(); err == nil {
		t.Fatal("ready after shutdown")
	}
}

func TestArtifactSurvivesSessionRetention(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	payload := []byte("retained bundle")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	key := "sessions/s1/bundle.tar.gz"
	if _, err := m.store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	m.setArtifact("s1", model.ArtifactInfo{Key: key, Size: int64(len(payload)), SHA256: digest})

	closed := time.Now().UTC().Add(-time.Hour)
	m.cfg.Sessions.Retention = config.Duration(time.Minute)
	m.sessions["s1"] = &instance{
		sess:      model.Session{ID: "s1", Status: model.StatusCompleted, ClosedAt: &closed},
		finalized: true,
	}

	if sess, ok := m.Get("s1"); !ok || sess.Artifact == nil || sess.Artifact.Key != key {
		t.Fatalf("live session lost its artifact: %+v", sess)
	}

	m.reap()
	if _, ok := m.Get("s1"); ok {
		t.Fatal("session past retention was kept")
	}
	info, ok := m.ArtifactInfo("s1")
	if !ok || info.Key != key {
		t.Fatalf("artifact metadata lost with the session: %+v", info)
	}
	rc, size, err := m.OpenArtifact(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(payload)) || !bytes.Equal(got, payload) {
		t.Fatalf("download mismatch: size=%d body=%q", size, got)
	}
}

type nopRunner struct{}

func (nopRunner) Start(context.Context, runner.Spec) (runner.Handle, error) {
	return runner.Handle{}, nil
}

func (nopRunner) Stop(context.Context, runner.Handle, bool) error { return nil }

func (nopRunner) Logs(context.Context, runner.Handle, int64) (string, error) {
	return "", nil
}

func TestReapFailsPendingSessionThatNeverConnects(t *testing.T) {
	m := testManager(t)
	m.runner = nopRunner{}
	m.cfg.Sessions.ConnectTimeout = config.Duration(time.Minute)
	m.cfg.Sessions.Retention = config.Duration(time.Hour)
	m.cfg.Sessions.IdleTimeout = config.Duration(time.Hour)
	old := time.Now().UTC().Add(-5 * time.Minute)
	token := "stuck"
	m.sessions["p1"] = &instance{
		sess: model.Session{
			ID:           "p1",
			Status:       model.StatusPending,
			Backend:      "local",
			ExpiresAt:    time.Now().UTC().Add(time.Hour),
			LastActivity: old,
		},
		tokenHash:  hashToken(token),
		launchedAt: old,
	}
	m.tokenIndex[hashToken(token)] = "p1"
	// A session launched now is still inside the connect window.
	m.sessions["p2"] = &instance{
		sess: model.Session{
			ID:           "p2",
			Status:       model.StatusPending,
			Backend:      "local",
			ExpiresAt:    time.Now().UTC().Add(time.Hour),
			LastActivity: time.Now().UTC(),
		},
		launchedAt: time.Now().UTC(),
	}

	m.reap()

	stuck, ok := m.Get("p1")
	if !ok {
		t.Fatal("session vanished")
	}
	if stuck.Status != model.StatusFailed {
		t.Fatalf("stuck session status = %s, want failed", stuck.Status)
	}
	if fresh, _ := m.Get("p2"); fresh.Status != model.StatusPending {
		t.Fatalf("fresh session status = %s, want pending", fresh.Status)
	}
}

// TestCreateMarksResumable checks the runtime gets the signal behavior that
// matches where its workspace lives: local restates in place, Kubernetes needs
// the persistent claim.
func TestCreateMarksResumable(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	m.runner = nopRunner{}
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}

	local, err := m.Create(ctx, CreateOptions{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	inst, ok := m.instance(local.ID)
	if !ok {
		t.Fatal("created session is not registered")
	}
	if !inst.runtimeCfg.Resumable {
		t.Fatalf("local backend Resumable = false, want true")
	}

	m.cfg.Runtime.Backend = "kubernetes"
	m.cfg.Sessions.PersistentWorkspace = new(false)
	remote, err := m.Create(ctx, CreateOptions{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	inst, ok = m.instance(remote.ID)
	if !ok {
		t.Fatal("created session is not registered")
	}
	if inst.runtimeCfg.Resumable {
		t.Fatalf("kubernetes without a persistent workspace Resumable = true, want false")
	}

	m.cfg.Sessions.PersistentWorkspace = new(true)
	persistent, err := m.Create(ctx, CreateOptions{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	inst, _ = m.instance(persistent.ID)
	if !inst.runtimeCfg.Resumable {
		t.Fatalf("kubernetes with a persistent workspace Resumable = false, want true")
	}

	// max_restarts: 0 starts no replacement, so nothing would resume a turn
	// a signal interrupted.
	m.cfg.Sessions.MaxRestarts = 0
	noRestarts, err := m.Create(ctx, CreateOptions{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	inst, _ = m.instance(noRestarts.ID)
	if inst.runtimeCfg.Resumable {
		t.Fatalf("max_restarts 0 Resumable = true, want false")
	}
}
