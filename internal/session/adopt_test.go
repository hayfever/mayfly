package session

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// TestAdoptResumesPodlessSession covers a control-plane stop between a
// replacement's pod deletion and its start: the ConfigMap, Secret, and claim
// survive, so startup must start a replacement instead of collecting them.
func TestAdoptResumesPodlessSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset()
	m := testManager(t)
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	m.cfg.Runtime.Backend = "kubernetes"
	m.cfg.Sessions.MaxRestarts = 2
	m.journal = journal.NewMemory()
	m.kube = k8s.NewForClient(cs, "mayfly")
	m.stateDir = t.TempDir()
	fakeRunner := &fakeRunner{handle: runner.Handle{
		Backend:             "kubernetes",
		SessionID:           "p1",
		Namespace:           "mayfly",
		PodName:             "mayfly-p1-r1",
		SecretName:          "mayfly-p1",
		ConfigMapName:       "mayfly-p1",
		PVCName:             "mayfly-p1-workspace",
		PersistentWorkspace: true,
	}}
	m.runner = fakeRunner

	cfg := sessioncfg.Config{
		Version:    sessioncfg.Version,
		SessionID:  "p1",
		Agent:      "default",
		Model:      "stub",
		Title:      "orphan",
		TTLSeconds: 600,
		CreatedAt:  time.Now().UTC(),
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec := &k8s.SessionSpec{
		Namespace:           "mayfly",
		SessionID:           "p1",
		PodName:             "mayfly-p1",
		SecretName:          "mayfly-p1",
		ConfigMapName:       "mayfly-p1",
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
	client := k8s.NewForClient(cs, "mayfly")
	if err := client.CreateSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	// The control plane stopped after deleting the old pod.
	if err := client.DeletePod(ctx, spec.PodName, 0); err != nil {
		t.Fatal(err)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())

	var starts []runner.Spec
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if starts = fakeRunner.specs(); len(starts) > 0 {
			break
		}
	}
	if len(starts) != 1 {
		t.Fatalf("replacement starts = %d, want 1", len(starts))
	}
	if starts[0].PodName != "mayfly-p1-r1" {
		t.Fatalf("replacement pod = %q", starts[0].PodName)
	}
	replacement, ok := starts[0].K8sSpec.(*k8s.SessionSpec)
	if !ok || replacement == nil {
		t.Fatalf("replacement spec = %#v", starts[0].K8sSpec)
	}
	if replacement.PVCName != "mayfly-p1-workspace" || !replacement.PersistentWorkspace {
		t.Fatalf("replacement claims = %q persistent=%v", replacement.PVCName, replacement.PersistentWorkspace)
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("mayfly").Get(ctx, "mayfly-p1-workspace", metav1.GetOptions{}); err != nil {
		t.Fatalf("workspace claim was deleted: %v", err)
	}
}

// TestAdoptDeletesPodOfFinalizedSession covers a control plane that stopped
// partway through cleaning up a finalized session. Cleanup removes the
// ConfigMap first, so the pod that is left must not come back as a session.
func TestAdoptDeletesPodOfFinalizedSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset()
	m := testManager(t)
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	m.cfg.Runtime.Backend = "kubernetes"
	m.cfg.Sessions.MaxRestarts = 2
	m.journal = journal.NewMemory()
	m.kube = k8s.NewForClient(cs, "mayfly")
	m.stateDir = t.TempDir()
	starts := &fakeRunner{}
	m.runner = starts

	spec := &k8s.SessionSpec{
		Namespace:           "mayfly",
		SessionID:           "f1",
		PodName:             "mayfly-f1",
		SecretName:          "mayfly-f1",
		ConfigMapName:       "mayfly-f1",
		Image:               "example/agent:1",
		PlatformNs:          "mayfly",
		PersistentWorkspace: true,
		SessionJSON:         []byte(`{}`),
		Token:               "token",
		TTL:                 time.Hour,
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := m.kube.CreateSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := cs.CoreV1().ConfigMaps("mayfly").Delete(ctx, "mayfly-f1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())

	if _, ok := m.instance("f1"); ok {
		t.Fatal("finalized session was adopted")
	}
	if _, err := cs.CoreV1().Pods("mayfly").Get(ctx, "mayfly-f1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod of finalized session: %v, want deleted", err)
	}
	if _, err := cs.CoreV1().Secrets("mayfly").Get(ctx, "mayfly-f1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("secret of finalized session: %v, want collected", err)
	}
	if n := len(starts.specs()); n != 0 {
		t.Fatalf("replacements started = %d, want 0", n)
	}
}

// stopRecorder is a runner whose Stop takes a while, like a pod delete.
type stopRecorder struct {
	fakeRunner
	stopped atomic.Bool
}

func (s *stopRecorder) Stop(context.Context, runner.Handle, bool) error {
	time.Sleep(200 * time.Millisecond)
	s.stopped.Store(true)
	return nil
}

// TestShutdownWaitsForCleanup keeps a control plane that finalizes a session
// and then exits from leaving that session's objects behind for the next
// adoption to resume.
func TestShutdownWaitsForCleanup(t *testing.T) {
	m := testManager(t)
	m.journal = journal.NewMemory()
	rec := &stopRecorder{}
	m.runner = rec
	inst := journalInstance(t, m)
	inst.sess.Status = model.StatusRunning
	inst.handle = runner.Handle{Backend: "kubernetes", SessionID: "s1", PodName: "mayfly-s1"}
	ctx := context.Background()
	if err := m.journal.Append(ctx, journal.Entry{SessionID: "s1", Turn: 3, Step: 0, Name: "user"}); err != nil {
		t.Fatal(err)
	}

	m.finalize(inst, model.StatusCompleted, "closed", "")
	m.Shutdown(ctx)

	if !rec.stopped.Load() {
		t.Fatal("Shutdown returned before the finalized session's resources were deleted")
	}
	if last, err := m.journal.LastTurn(ctx, "s1"); err != nil || last != 0 {
		t.Fatalf("journal after cleanup: last turn %d, %v; want deleted", last, err)
	}
}

// TestAdoptKeepsArtifactsAgentAndIdempotencyKey covers a control-plane
// restart under a running scheduled session: the adopted session must still
// read its run artifact, report its agent, and answer a repeated Create for
// the job that started it.
func TestAdoptKeepsArtifactsAgentAndIdempotencyKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset()
	m := testManager(t)
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	m.cfg.Runtime.Backend = "kubernetes"
	m.journal = journal.NewMemory()
	m.kube = k8s.NewForClient(cs, "mayfly")
	m.stateDir = t.TempDir()
	m.runner = &fakeRunner{}

	const key = "runs/nightly/job-1.json"
	const content = `{"run_id":"job-1"}`
	putArtifact(t, m, key, content)
	cfg := sessioncfg.Config{
		Version:    sessioncfg.Version,
		SessionID:  "a1",
		Agent:      "default",
		Model:      "stub",
		Title:      "scheduled",
		TTLSeconds: 600,
		CreatedAt:  time.Now().UTC(),
		Artifacts: []sessioncfg.Artifact{{
			Key:         key,
			SHA256:      digestOf(content),
			Bytes:       int64(len(content)),
			ContentType: "application/json",
		}},
		IdempotencyKey: "job-1",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec := &k8s.SessionSpec{
		Namespace:     "mayfly",
		SessionID:     "a1",
		PodName:       "mayfly-a1",
		SecretName:    "mayfly-a1",
		ConfigMapName: "mayfly-a1",
		Image:         "example/agent:1",
		PlatformNs:    "mayfly",
		SessionJSON:   raw,
		Token:         "token",
		TTL:           time.Hour,
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := m.kube.CreateSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	// The API server turns stringData into data; the fake client does not.
	secret, err := cs.CoreV1().Secrets("mayfly").Get(ctx, "mayfly-a1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secret.Data = map[string][]byte{"token": []byte("token")}
	if _, err := cs.CoreV1().Secrets("mayfly").Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())

	sess, ok := m.Get("a1")
	if !ok {
		t.Fatal("session was not adopted")
	}
	if sess.Agent != "default" {
		t.Fatalf("adopted agent = %q, want default", sess.Agent)
	}
	window, err := m.ReadArtifact(ctx, "a1", "token", key, 0, 1<<10)
	if err != nil {
		t.Fatalf("read artifact after adopt: %v", err)
	}
	if string(window.Content) != content {
		t.Fatalf("artifact content = %q", window.Content)
	}
	again, err := m.Create(ctx, CreateOptions{Prompt: "again", IdempotencyKey: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != "a1" {
		t.Fatalf("repeated create started %q, want adopted a1", again.ID)
	}
}
