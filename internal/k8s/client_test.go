package k8s

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCreateAndDeleteSessionWithWorkspace(t *testing.T) {
	cs := fake.NewSimpleClientset()
	client := NewForClient(cs, "mayfly")
	spec := testSpec()
	spec.PersistentWorkspace = true
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := client.CreateSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims("mayfly").Get(ctx, spec.PVCName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("workspace claim: %v", err)
	}
	if pvc.Labels[LabelSession] != spec.SessionID {
		t.Fatalf("claim labels = %v", pvc.Labels)
	}
	if _, err := cs.CoreV1().Pods("mayfly").Get(ctx, spec.PodName, metav1.GetOptions{}); err != nil {
		t.Fatalf("session pod: %v", err)
	}

	if err := client.DeleteSession(ctx, spec, 0); err != nil {
		t.Fatal(err)
	}
	for _, get := range []func() error{
		func() error {
			_, err := cs.CoreV1().Pods("mayfly").Get(ctx, spec.PodName, metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().PersistentVolumeClaims("mayfly").Get(ctx, spec.PVCName, metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().Secrets("mayfly").Get(ctx, spec.SecretName, metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().ConfigMaps("mayfly").Get(ctx, spec.ConfigMapName, metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.NetworkingV1().NetworkPolicies("mayfly").Get(ctx, spec.NetworkPolicyName, metav1.GetOptions{})
			return err
		},
	} {
		if err := get(); !apierrors.IsNotFound(err) {
			t.Fatalf("object was not deleted: %v", err)
		}
	}
}

func TestGarbageCollectRemovesSessionsWithoutPods(t *testing.T) {
	cs := fake.NewSimpleClientset()
	client := NewForClient(cs, "mayfly")
	ctx := context.Background()

	gone := testSpec()
	gone.SessionID = "gone"
	gone.PodName = "mayfly-gone"
	gone.SecretName = "mayfly-gone"
	gone.ConfigMapName = "mayfly-gone"
	gone.PersistentWorkspace = true
	gone.PVCName = ""
	gone.NetworkPolicyName = ""
	if err := gone.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := client.CreateSession(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := client.DeletePod(ctx, gone.PodName, 0); err != nil {
		t.Fatal(err)
	}

	alive := testSpec()
	alive.SessionID = "alive"
	alive.PodName = "mayfly-alive"
	alive.SecretName = "mayfly-alive"
	alive.ConfigMapName = "mayfly-alive"
	alive.PersistentWorkspace = true
	alive.PVCName = ""
	alive.NetworkPolicyName = ""
	if err := alive.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := client.CreateSession(ctx, alive); err != nil {
		t.Fatal(err)
	}

	removed, err := client.GarbageCollect(ctx, map[string]struct{}{"alive": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "gone" {
		t.Fatalf("removed sessions = %v", removed)
	}
	for _, get := range []func() error{
		func() error {
			_, err := cs.CoreV1().ConfigMaps("mayfly").Get(ctx, gone.ConfigMapName, metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().Secrets("mayfly").Get(ctx, gone.SecretName, metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().PersistentVolumeClaims("mayfly").Get(ctx, gone.PVCName, metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.NetworkingV1().NetworkPolicies("mayfly").Get(ctx, gone.NetworkPolicyName, metav1.GetOptions{})
			return err
		},
	} {
		if err := get(); !apierrors.IsNotFound(err) {
			t.Fatalf("orphaned object was not deleted: %v", err)
		}
	}
	if _, err := cs.CoreV1().ConfigMaps("mayfly").Get(ctx, alive.ConfigMapName, metav1.GetOptions{}); err != nil {
		t.Fatalf("live session configmap was removed: %v", err)
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("mayfly").Get(ctx, alive.PVCName, metav1.GetOptions{}); err != nil {
		t.Fatalf("live session claim was removed: %v", err)
	}
}

// TestOrphanSessionsReportsObjects covers a session that lost its pod but
// kept the objects a replacement needs, next to one that is only a leftover.
func TestOrphanSessionsReportsObjects(t *testing.T) {
	cs := fake.NewSimpleClientset()
	client := NewForClient(cs, "mayfly")
	ctx := context.Background()

	full := testSpec()
	full.SessionID = "a"
	full.PodName = "mayfly-a"
	full.SecretName = "mayfly-a"
	full.ConfigMapName = "mayfly-a"
	full.PersistentWorkspace = true
	full.PVCName = "mayfly-a-workspace"
	full.NetworkPolicyName = ""
	if err := full.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := client.CreateSession(ctx, full); err != nil {
		t.Fatal(err)
	}
	if err := client.DeletePod(ctx, full.PodName, 0); err != nil {
		t.Fatal(err)
	}

	leftover := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mayfly-b",
			Namespace: "mayfly",
			Labels: map[string]string{
				LabelManaged: "true",
				LabelApp:     AppName,
				LabelSession: "b",
			},
		},
	}
	if _, err := cs.CoreV1().ConfigMaps("mayfly").Create(ctx, leftover, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	got, err := client.OrphanSessions(ctx, map[string]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("orphans = %+v, want 2 sessions", got)
	}
	a := got["a"]
	if a.ConfigMap != "mayfly-a" || a.Secret != "mayfly-a" || a.PVC != "mayfly-a-workspace" {
		t.Fatalf("session a objects = %+v", a)
	}
	b := got["b"]
	if b.ConfigMap != "mayfly-b" || b.Secret != "" || b.PVC != "" {
		t.Fatalf("session b objects = %+v", b)
	}
}

// TestDeleteSessionRemovesConfigBeforePod keeps cleanup from passing through
// a state adoption reads as resumable: a ConfigMap, Secret, and claim with no
// pod. A control plane that stops there would bring a finalized session back.
// TestGCIgnoresPluginObjects proves that the session selector never matches a
// service plugin object, so garbage collection leaves it alone.
func TestGCIgnoresPluginObjects(t *testing.T) {
	cs := fake.NewSimpleClientset()
	client := NewForClient(cs, "mayfly")
	ctx := context.Background()

	labels := map[string]string{
		LabelManaged:       "true",
		LabelApp:           PluginAppName,
		LabelPlugin:        "tickets",
		AnnotationSpecHash: "hash",
	}
	removeHash := func(m map[string]string) map[string]string {
		out := map[string]string{}
		for key, value := range m {
			if key != AnnotationSpecHash {
				out[key] = value
			}
		}
		return out
	}
	object := "mayfly-plugin-tickets"
	key, err := client.EnsurePluginSecret(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: object, Namespace: "mayfly", Labels: removeHash(labels)},
		Data:       map[string][]byte{"key": []byte("0123456789abcdef0123456789abcdef")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(key) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("gateway key = %q", key)
	}
	if err := client.ApplyPluginObjects(ctx, PluginObjects{
		Deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: object, Namespace: "mayfly", Labels: removeHash(labels), Annotations: map[string]string{AnnotationSpecHash: "hash"}},
		},
		Service: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: object, Namespace: "mayfly", Labels: removeHash(labels), Annotations: map[string]string{AnnotationSpecHash: "hash"}},
		},
		NetworkPolicy: &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: object, Namespace: "mayfly", Labels: removeHash(labels), Annotations: map[string]string{AnnotationSpecHash: "hash"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	gone := testSpec()
	gone.SessionID = "gone"
	gone.PodName = "mayfly-gone"
	gone.SecretName = "mayfly-gone"
	gone.ConfigMapName = "mayfly-gone"
	gone.PersistentWorkspace = true
	gone.PVCName = ""
	gone.NetworkPolicyName = ""
	if err := gone.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := client.CreateSession(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := client.DeletePod(ctx, gone.PodName, 0); err != nil {
		t.Fatal(err)
	}

	removed, err := client.GarbageCollect(ctx, map[string]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "gone" {
		t.Fatalf("removed sessions = %v", removed)
	}
	if _, err := cs.AppsV1().Deployments("mayfly").Get(ctx, object, metav1.GetOptions{}); err != nil {
		t.Fatalf("plugin deployment was collected: %v", err)
	}
	if _, err := cs.CoreV1().Services("mayfly").Get(ctx, object, metav1.GetOptions{}); err != nil {
		t.Fatalf("plugin service was collected: %v", err)
	}
	if _, err := cs.NetworkingV1().NetworkPolicies("mayfly").Get(ctx, object, metav1.GetOptions{}); err != nil {
		t.Fatalf("plugin network policy was collected: %v", err)
	}
	if _, err := cs.CoreV1().Secrets("mayfly").Get(ctx, object, metav1.GetOptions{}); err != nil {
		t.Fatalf("plugin secret was collected: %v", err)
	}

	orphans, err := client.OrphanSessions(ctx, map[string]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := orphans[""]; ok {
		t.Fatalf("orphans report a plugin object: %v", orphans)
	}
}

func TestCreateSessionRollbackLogsFailedDeletes(t *testing.T) {
	cs := fake.NewSimpleClientset()
	client := NewForClient(cs, "mayfly")
	spec := testSpec()
	spec.PersistentWorkspace = true
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	podErr := errors.New("pod denied by the dead-letter fixture")
	claimErr := errors.New("claim is protected")
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, podErr
	})
	cs.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, claimErr
	})
	cs.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, spec.ConfigMapName)
	})
	var logs bytes.Buffer
	client.log = slog.New(slog.NewTextHandler(&logs, nil))

	err := client.CreateSession(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateSession succeeded with a denied pod")
	}
	if !errors.Is(err, podErr) || !strings.Contains(err.Error(), "create session pod") {
		t.Fatalf("CreateSession error = %v, want the pod create error", err)
	}

	var deleted []string
	for _, action := range cs.Actions() {
		if action.GetVerb() == "delete" {
			deleted = append(deleted, action.GetResource().Resource)
		}
	}
	want := []string{"persistentvolumeclaims", "networkpolicies", "configmaps", "secrets"}
	if !slices.Equal(deleted, want) {
		t.Fatalf("rollback deletes = %v, want %v", deleted, want)
	}

	out := logs.String()
	if !strings.Contains(out, "persistentvolumeclaim/"+spec.PVCName) {
		t.Errorf("failed claim delete was not logged: %s", out)
	}
	if !strings.Contains(out, "session="+spec.SessionID) {
		t.Errorf("rollback log does not name the session: %s", out)
	}
	if !strings.Contains(out, claimErr.Error()) {
		t.Errorf("rollback log does not carry the delete error: %s", out)
	}
	if strings.Contains(out, "configmap/"+spec.ConfigMapName) {
		t.Errorf("rollback logged an already-absent configmap: %s", out)
	}
}

func TestDeleteSessionRemovesConfigBeforePod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	client := NewForClient(cs, "mayfly")
	spec := testSpec()
	spec.PersistentWorkspace = true
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := client.CreateSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	configAtPodDelete := errors.New("pod not deleted")
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		_, configAtPodDelete = cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), "mayfly", spec.ConfigMapName)
		return false, nil, nil
	})

	if err := client.DeleteSession(ctx, spec, 0); err != nil {
		t.Fatal(err)
	}
	if !apierrors.IsNotFound(configAtPodDelete) {
		t.Fatalf("configmap when the pod was deleted: %v, want already deleted", configAtPodDelete)
	}
}
