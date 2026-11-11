// Package runner starts and stops session execution backends.
package runner

import (
	"context"
	"time"

	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// Spec is everything a backend needs to start one session.
type Spec struct {
	Backend       string
	SessionID     string
	Namespace     string
	PodName       string
	SecretName    string
	ConfigMapName string
	StateDir      string
	Token         string
	Env           map[string]string
	Runtime       sessioncfg.Config
	K8sSpec       any // *k8s.SessionSpec, set by the Kubernetes backend caller
}

// Handle identifies a running session for later operations.
type Handle struct {
	Backend       string
	SessionID     string
	Namespace     string
	PodName       string
	SecretName    string
	ConfigMapName string
	StateDir      string
	NetworkPolicy bool
	Done          <-chan struct{}
}

// Runner is a session execution backend.
type Runner interface {
	Start(ctx context.Context, spec Spec) (Handle, error)
	// Stop asks the runtime to exit and removes its resources. force bypasses
	// graceful shutdown.
	Stop(ctx context.Context, handle Handle, force bool) error
	Logs(ctx context.Context, handle Handle, maxBytes int64) (string, error)
}

// WaitDone waits for a local runtime to exit, or for the context to end.
func WaitDone(ctx context.Context, done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}
