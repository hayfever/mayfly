package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/hayfever/mayfly/internal/k8s"
)

// Kubernetes runs sessions as pods.
type Kubernetes struct {
	client *k8s.Client
}

// NewKubernetes returns a Kubernetes backend.
func NewKubernetes(client *k8s.Client) *Kubernetes {
	return &Kubernetes{client: client}
}

// Start creates the session pod and its supporting objects, or only a
// replacement pod when the caller reuses an existing session.
func (k *Kubernetes) Start(ctx context.Context, spec Spec) (Handle, error) {
	pod, ok := spec.K8sSpec.(*k8s.SessionSpec)
	if !ok || pod == nil {
		return Handle{}, fmt.Errorf("kubernetes backend requires K8sSpec")
	}
	if spec.Reuse {
		if err := k.client.CreatePod(ctx, pod); err != nil {
			return Handle{}, err
		}
	} else if err := k.client.CreateSession(ctx, pod); err != nil {
		return Handle{}, err
	}
	return Handle{
		Backend:             "kubernetes",
		Namespace:           pod.Namespace,
		PodName:             pod.PodName,
		SecretName:          pod.SecretName,
		ConfigMapName:       pod.ConfigMapName,
		PVCName:             pod.PVCName,
		PersistentWorkspace: pod.PersistentWorkspace,
		NetworkPolicyName:   pod.NetworkPolicyName,
	}, nil
}

// Stop deletes the pod and its supporting objects.
func (k *Kubernetes) Stop(ctx context.Context, handle Handle, force bool) error {
	grace := 10 * time.Second
	if force {
		grace = 0
	}
	spec := &k8s.SessionSpec{
		Namespace:           handle.Namespace,
		PodName:             handle.PodName,
		SecretName:          handle.SecretName,
		ConfigMapName:       handle.ConfigMapName,
		PVCName:             handle.PVCName,
		PersistentWorkspace: handle.PersistentWorkspace,
		NetworkPolicyName:   handle.NetworkPolicyName,
	}
	return k.client.DeleteSession(ctx, spec, grace)
}

// Logs returns the tail of the agent container log.
func (k *Kubernetes) Logs(ctx context.Context, handle Handle, maxBytes int64) (string, error) {
	return k.client.Logs(ctx, handle.PodName, maxBytes)
}
