package session

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/runner"
)

// TestPollPodsReplacesFailedPod covers a pod that failed without finalizing.
// The replacement runs in the background so other sessions keep being polled.
func TestPollPodsReplacesFailedPod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	m := testManager(t)
	m.cfg.Sessions.MaxRestarts = 2
	m.cfg.Runtime.Backend = "kubernetes"
	m.ctx = context.Background()
	m.kube = k8s.NewForClient(cs, "mayfly")
	fake := &fakeRunner{handle: runner.Handle{
		Backend:             "kubernetes",
		PodName:             "mayfly-s1-r1",
		PersistentWorkspace: true,
	}}
	m.runner = fake
	inst := restartInstance(t, m, runner.Handle{
		Backend:             "kubernetes",
		SessionID:           "s1",
		Namespace:           "mayfly",
		PodName:             "mayfly-s1",
		SecretName:          "mayfly-s1",
		ConfigMapName:       "mayfly-s1",
		PersistentWorkspace: true,
	})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mayfly-s1",
			Namespace: "mayfly",
			Labels: map[string]string{
				k8s.LabelManaged: "true",
				k8s.LabelSession: "s1",
				k8s.LabelApp:     k8s.AppName,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	if _, err := cs.CoreV1().Pods("mayfly").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	m.pollPods(context.Background())

	var starts []runner.Spec
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if starts = fake.specs(); len(starts) > 0 {
			break
		}
	}
	if len(starts) != 1 {
		t.Fatalf("replacement starts = %d, want 1", len(starts))
	}
	spec := starts[0]
	if spec.PodName != "mayfly-s1-r1" || !spec.Reuse || spec.Runtime.SessionID != "s1" {
		t.Fatalf("replacement spec = %+v", spec)
	}
	if _, err := cs.CoreV1().Pods("mayfly").Get(context.Background(), "mayfly-s1", metav1.GetOptions{}); err == nil {
		t.Fatal("the failed pod was not deleted")
	}
	inst.mu.Lock()
	restarts := inst.restarts
	handle := inst.handle.PodName
	inst.mu.Unlock()
	if restarts != 1 || handle != "mayfly-s1-r1" {
		t.Fatalf("restarts = %d handle = %q", restarts, handle)
	}
}

// TestPodStateFallsBackWhenClosing checks that a closing session does not wait
// for a container that can never start.
func TestPodStateFallsBackWhenClosing(t *testing.T) {
	m := testManager(t)
	m.cfg.Sessions.MaxRestarts = 2
	m.ctx = context.Background()
	m.runner = &fakeRunner{handle: runner.Handle{Backend: "kubernetes"}}
	inst := restartInstance(t, m, runner.Handle{Backend: "kubernetes", SessionID: "s1", PodName: "mayfly-s1"})
	inst.mu.Lock()
	inst.sess.Status = model.StatusClosing
	inst.mu.Unlock()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mayfly-s1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError"}},
			}},
		},
	}
	m.podState(inst, pod, time.Now().UTC())

	inst.mu.Lock()
	finalized, status := inst.finalized, inst.sess.Status
	inst.mu.Unlock()
	if !finalized || status != model.StatusFailed {
		t.Fatalf("finalized = %v status = %s, want failed", finalized, status)
	}
}

// TestPodMissingRestartsSession covers a pod deleted out from under a running
// session, which is what a node reclaim or a manual delete looks like.
func TestPodMissingRestartsSession(t *testing.T) {
	m := testManager(t)
	m.cfg.Sessions.MaxRestarts = 2
	m.cfg.Runtime.Backend = "kubernetes"
	m.ctx = context.Background()
	fake := &fakeRunner{handle: runner.Handle{
		Backend:             "kubernetes",
		PodName:             "mayfly-s1-r1",
		PersistentWorkspace: true,
	}}
	m.runner = fake
	inst := restartInstance(t, m, runner.Handle{
		Backend:             "kubernetes",
		SessionID:           "s1",
		Namespace:           "mayfly",
		PodName:             "mayfly-s1",
		SecretName:          "mayfly-s1",
		ConfigMapName:       "mayfly-s1",
		PersistentWorkspace: true,
	})
	inst.mu.Lock()
	inst.podMissingSince = time.Now().Add(-time.Minute)
	inst.mu.Unlock()

	m.podMissing(inst, time.Now().UTC())

	var starts []runner.Spec
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if starts = fake.specs(); len(starts) > 0 {
			break
		}
	}
	if len(starts) != 1 || starts[0].PodName != "mayfly-s1-r1" {
		t.Fatalf("replacement starts = %+v", starts)
	}
}

func TestReplacementAttempt(t *testing.T) {
	cases := map[string]int{
		"mayfly-s1":      0,
		"mayfly-s1-r1":   1,
		"mayfly-s1-r12":  12,
		"mayfly-s1-rx":   0,
		"mayfly-s1-r-1":  0,
		"unrelated-name": 0,
	}
	for name, want := range cases {
		if got := replacementAttempt(name); got != want {
			t.Fatalf("replacementAttempt(%q) = %d, want %d", name, got, want)
		}
	}
}
