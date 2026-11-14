package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

type fakeRunner struct {
	mu     sync.Mutex
	starts []runner.Spec
	handle runner.Handle
}

func (f *fakeRunner) Start(_ context.Context, spec runner.Spec) (runner.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, spec)
	return f.handle, nil
}

func (f *fakeRunner) specs() []runner.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runner.Spec(nil), f.starts...)
}

func (f *fakeRunner) Stop(context.Context, runner.Handle, bool) error { return nil }

func (f *fakeRunner) Logs(context.Context, runner.Handle, int64) (string, error) { return "", nil }

func TestLocalRestartReusesStateAndResumes(t *testing.T) {
	m := testManager(t)
	m.cfg.Runtime.Backend = "local"
	m.cfg.Runtime.AgentBinary = "/bin/true"
	m.cfg.Sessions.MaxRestarts = 2
	m.stateDir = t.TempDir()
	m.journal = nil // restart does not journal from the control plane
	done := make(chan struct{})
	fake := &fakeRunner{handle: runner.Handle{
		Backend:   "local",
		SessionID: "s1",
		StateDir:  "/state/s1",
		Done:      done,
	}}
	m.runner = fake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	defer close(done)

	inst := &instance{
		sess: model.Session{
			ID:        "s1",
			Status:    model.StatusRunning,
			ExpiresAt: time.Now().Add(time.Hour),
		},
		token:     "session-token",
		tokenHash: hashToken("session-token"),
		runtimeCfg: sessioncfg.Config{
			Version:      sessioncfg.Version,
			SessionID:    "s1",
			Model:        "model",
			PlatformURL:  "http://mayfly:8080",
			LLMBaseURL:   "http://mayfly:8080/internal/v1/llm",
			WorkspaceDir: "/workspace",
			ArtifactsDir: "/artifacts",
			MaxSteps:     1,
		},
		image:  "agent:test",
		handle: runner.Handle{Backend: "local", SessionID: "s1", StateDir: "/state/s1"},
	}
	m.sessions["s1"] = inst
	m.tokenIndex[inst.tokenHash] = "s1"
	m.hub.Create("s1")

	m.restart(inst, "test failure")

	starts := fake.specs()
	if len(starts) != 1 {
		t.Fatalf("starts = %d", len(starts))
	}
	spec := starts[0]
	if spec.Runtime.SessionID != "s1" {
		t.Fatalf("replacement runtime config = %+v", spec.Runtime)
	}
	if spec.Token != "session-token" || spec.StateDir != "/state/s1" || spec.Reuse {
		t.Fatalf("replacement spec = %+v", spec)
	}
	inst.mu.Lock()
	restarts := inst.restarts
	backend := inst.handle.Backend
	inst.mu.Unlock()
	if restarts != 1 || backend != "local" {
		t.Fatalf("restarts = %d handle = %+v", restarts, inst.handle)
	}
}

type failingRunner struct{ starts int }

func (f *failingRunner) Start(context.Context, runner.Spec) (runner.Handle, error) {
	f.starts++
	return runner.Handle{}, errors.New("image pull failed")
}

func (f *failingRunner) Stop(context.Context, runner.Handle, bool) error { return nil }

func (f *failingRunner) Logs(context.Context, runner.Handle, int64) (string, error) {
	return "", nil
}

// restartInstance registers a running session whose container just failed.
func restartInstance(t *testing.T, m *Manager, handle runner.Handle) *instance {
	t.Helper()
	inst := &instance{
		sess: model.Session{
			ID:        "s1",
			Status:    model.StatusRunning,
			ExpiresAt: time.Now().Add(time.Hour),
		},
		token:     "session-token",
		tokenHash: hashToken("session-token"),
		runtimeCfg: sessioncfg.Config{
			Version:      sessioncfg.Version,
			SessionID:    "s1",
			Model:        "model",
			PlatformURL:  "http://mayfly:8080",
			LLMBaseURL:   "http://mayfly:8080/internal/v1/llm",
			WorkspaceDir: "/workspace",
			ArtifactsDir: "/artifacts",
			MaxSteps:     1,
		},
		image:      "agent:test",
		handle:     handle,
		launchedAt: time.Now(),
	}
	m.sessions["s1"] = inst
	m.tokenIndex[inst.tokenHash] = "s1"
	m.journal = journal.NewMemory()
	m.hub.Create("s1")
	m.hub.Seed("s1", 10)
	return inst
}

func TestRestartFallsBackAfterLastAttempt(t *testing.T) {
	m := testManager(t)
	m.cfg.Sessions.MaxRestarts = 1
	m.cfg.Runtime.Backend = "local"
	m.ctx = context.Background()
	fake := &failingRunner{}
	m.runner = fake
	inst := restartInstance(t, m, runner.Handle{Backend: "local", SessionID: "s1", StateDir: t.TempDir()})

	m.restart(inst, "test failure")

	if fake.starts != 1 {
		t.Fatalf("replacement attempts = %d, want 1", fake.starts)
	}
	inst.mu.Lock()
	finalized, status, restarts := inst.finalized, inst.sess.Status, inst.restarts
	inst.mu.Unlock()
	if !finalized || status != model.StatusFailed {
		t.Fatalf("session finalized = %v status = %s, want failed", finalized, status)
	}
	if restarts != 1 {
		t.Fatalf("restarts = %d", restarts)
	}
}

func TestRestartRefusesWithoutPersistentWorkspace(t *testing.T) {
	m := testManager(t)
	m.cfg.Sessions.MaxRestarts = 2
	m.ctx = context.Background()
	fake := &fakeRunner{handle: runner.Handle{Backend: "kubernetes"}}
	m.runner = fake
	inst := restartInstance(t, m, runner.Handle{
		Backend:             "kubernetes",
		SessionID:           "s1",
		PodName:             "mayfly-s1",
		PersistentWorkspace: false,
	})

	m.restart(inst, "pod failed")

	if starts := fake.specs(); len(starts) != 0 {
		t.Fatalf("started %d replacements without persistent storage", len(starts))
	}
	inst.mu.Lock()
	finalized, status := inst.finalized, inst.sess.Status
	inst.mu.Unlock()
	if !finalized || status != model.StatusFailed {
		t.Fatalf("finalized = %v status = %s, want failed", finalized, status)
	}
}

// kubeRestartManager wires a manager whose session pod "mayfly-s1" failed and
// whose pod deletes fail the first failures times.
func kubeRestartManager(t *testing.T, failures int) (*Manager, *fake.Clientset, *instance, *fakeRunner) {
	t.Helper()
	backoff := podDeleteBackoff
	podDeleteBackoff = time.Millisecond
	t.Cleanup(func() { podDeleteBackoff = backoff })

	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mayfly-s1", Namespace: "mayfly"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	})
	var deletes atomic.Int64
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		if deletes.Add(1) <= int64(failures) {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	m := testManager(t)
	m.cfg.Sessions.MaxRestarts = 2
	m.cfg.Runtime.Backend = "kubernetes"
	m.ctx = context.Background()
	m.kube = k8s.NewForClient(cs, "mayfly")
	fr := &fakeRunner{handle: runner.Handle{Backend: "kubernetes", PodName: "mayfly-s1-r1", PersistentWorkspace: true}}
	m.runner = fr
	inst := restartInstance(t, m, runner.Handle{
		Backend:             "kubernetes",
		SessionID:           "s1",
		Namespace:           "mayfly",
		PodName:             "mayfly-s1",
		SecretName:          "mayfly-s1",
		ConfigMapName:       "mayfly-s1",
		PersistentWorkspace: true,
	})
	return m, cs, inst, fr
}

// TestRestartRetriesOldPodDelete covers a transient API error while the
// replacement removes the failed pod. The old pod holds the workspace claim,
// so it must be gone before the replacement starts, not only at cleanup.
func TestRestartRetriesOldPodDelete(t *testing.T) {
	m, cs, inst, fr := kubeRestartManager(t, 2)

	m.restart(inst, "pod failed")

	if _, err := cs.CoreV1().Pods("mayfly").Get(context.Background(), "mayfly-s1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old pod still exists after replacement: %v", err)
	}
	if n := len(fr.specs()); n != 1 {
		t.Fatalf("replacements started = %d, want 1", n)
	}
	inst.mu.Lock()
	stale := append([]string(nil), inst.stalePods...)
	inst.mu.Unlock()
	if len(stale) != 0 {
		t.Fatalf("stale pods = %v, want none", stale)
	}
}

// TestRestartKeepsStalePodWhenDeleteKeepsFailing checks the retry is bounded:
// the replacement still starts, and cleanup owns the pod.
func TestRestartKeepsStalePodWhenDeleteKeepsFailing(t *testing.T) {
	m, cs, inst, fr := kubeRestartManager(t, 1000)

	m.restart(inst, "pod failed")

	if _, err := cs.CoreV1().Pods("mayfly").Get(context.Background(), "mayfly-s1", metav1.GetOptions{}); err != nil {
		t.Fatalf("old pod: %v", err)
	}
	if n := len(fr.specs()); n != 1 {
		t.Fatalf("replacements started = %d, want 1", n)
	}
	inst.mu.Lock()
	stale := append([]string(nil), inst.stalePods...)
	inst.mu.Unlock()
	if len(stale) != 1 || stale[0] != "mayfly-s1" {
		t.Fatalf("stale pods = %v, want [mayfly-s1]", stale)
	}
}

// TestRestartKeepsPlugins checks a replacement container receives the same
// plugin section and the grant is not registered twice.
func TestRestartKeepsPlugins(t *testing.T) {
	wordCount := parseManifest(t, t.TempDir(), wordCountManifest, false)
	m := pluginManager(t, wordCount)
	m.cfg.Runtime.Backend = "local"
	runner := &fakeRunner{handle: runner.Handle{Backend: "local", SessionID: "s1"}}
	m.runner = runner
	m.SetGateway(newFakeGateway())
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}

	session, err := m.Create(context.Background(), CreateOptions{Prompt: "hello", Agent: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	inst, ok := m.instance(session.ID)
	if !ok {
		t.Fatal("session is not registered")
	}
	m.restart(inst, "container exited")

	starts := runner.specs()
	if len(starts) != 2 {
		t.Fatalf("starts = %d, want 2", len(starts))
	}
	first, replacement := starts[0].Runtime.Plugins, starts[1].Runtime.Plugins
	if len(first) != 1 || len(replacement) != 1 {
		t.Fatalf("plugin sections = %d and %d", len(first), len(replacement))
	}
	if first[0].Name != replacement[0].Name || first[0].Placement != replacement[0].Placement {
		t.Fatalf("plugin section changed: %+v -> %+v", first[0], replacement[0])
	}
	if first[0].Dir != replacement[0].Dir || first[0].SecretsDir != replacement[0].SecretsDir {
		t.Fatalf("plugin paths changed: %+v -> %+v", first[0], replacement[0])
	}
	if first[0].Command != "./word-count" {
		t.Fatalf("local command = %q", first[0].Command)
	}
}
