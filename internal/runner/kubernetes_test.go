package runner

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

func TestKubernetesReplacementReusesSessionResources(t *testing.T) {
	cs := fake.NewSimpleClientset()
	client := k8s.NewForClient(cs, "mayfly")
	spec := &k8s.SessionSpec{
		Namespace:           "mayfly",
		SessionID:           "abc123",
		PodName:             "mayfly-abc123-r1",
		SecretName:          "mayfly-abc123",
		ConfigMapName:       "mayfly-abc123",
		Image:               "agent:test",
		TTL:                 10 * time.Minute,
		WorkspaceLimit:      "128Mi",
		PersistentWorkspace: true,
		PVCName:             "mayfly-abc123-workspace",
		PlatformNs:          "mayfly",
		PlatformPort:        8080,
		SessionJSON:         []byte(`{"session_id":"abc123"}`),
		Token:               "token",
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	original := `{"session_id":"abc123"}`
	cm := spec.ConfigMap()
	cm.Data["session.json"] = original
	if _, err := cs.CoreV1().ConfigMaps("mayfly").Create(context.Background(), cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	runner := NewKubernetes(client)
	runtimeCfg := sessioncfg.Config{Version: sessioncfg.Version, SessionID: "abc123", Model: "m"}
	h, err := runner.Start(context.Background(), Spec{
		Backend:       "kubernetes",
		SessionID:     "abc123",
		Namespace:     "mayfly",
		PodName:       spec.PodName,
		SecretName:    spec.SecretName,
		ConfigMapName: spec.ConfigMapName,
		Runtime:       runtimeCfg,
		Reuse:         true,
		K8sSpec:       spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.PodName != spec.PodName || h.PVCName != spec.PVCName || !h.PersistentWorkspace {
		t.Fatalf("handle = %+v", h)
	}
	stored, err := cs.CoreV1().ConfigMaps("mayfly").Get(context.Background(), spec.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A replacement reads the journal on startup, so its config is unchanged.
	if stored.Data["session.json"] != original {
		t.Fatalf("replacement rewrote the session config: %s", stored.Data["session.json"])
	}
	pod, err := cs.CoreV1().Pods("mayfly").Get(context.Background(), spec.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Volumes[1].PersistentVolumeClaim == nil || pod.Spec.Volumes[1].PersistentVolumeClaim.ClaimName != spec.PVCName {
		t.Fatalf("pod volumes = %+v", pod.Spec.Volumes)
	}
}
