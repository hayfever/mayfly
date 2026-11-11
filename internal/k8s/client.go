package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/hayden/mayfly/internal/version"
)

const managedSelector = LabelManaged + "=true," + LabelApp + "=" + AppName

// Client wraps the Kubernetes API with session-specific operations.
type Client struct {
	cs        kubernetes.Interface
	namespace string
}

// NewClient builds a client from in-cluster configuration or kubeconfig.
func NewClient(namespace string) (*Client, error) {
	cfg, err := restConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	return &Client{cs: cs, namespace: namespace}, nil
}

// NewForClient wraps an existing clientset, used by tests.
func NewForClient(cs kubernetes.Interface, namespace string) *Client {
	return &Client{cs: cs, namespace: namespace}
}

func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		cfg.QPS = 20
		cfg.Burst = 40
		cfg.Timeout = 30 * time.Second
		cfg.UserAgent = "mayflyd/" + version.Version
		return cfg, nil
	}
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config and no home directory: %w", err)
		}
		path = filepath.Join(home, ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %s: %w", path, err)
	}
	cfg.QPS = 20
	cfg.Burst = 40
	cfg.Timeout = 30 * time.Second
	cfg.UserAgent = "mayflyd/" + version.Version
	return cfg, nil
}

// CreateSession creates the secret, config map, network policy, then the pod.
// On failure it removes everything it created.
func (c *Client) CreateSession(ctx context.Context, spec *SessionSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	created := []func(context.Context) error{}
	rollback := func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = created[i](context.Background())
		}
	}
	if _, err := c.cs.CoreV1().Secrets(spec.Namespace).Create(ctx, spec.Secret(), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create session secret: %w", err)
	}
	created = append(created, func(ctx context.Context) error {
		return c.cs.CoreV1().Secrets(spec.Namespace).Delete(ctx, spec.SecretName, metav1.DeleteOptions{})
	})
	if _, err := c.cs.CoreV1().ConfigMaps(spec.Namespace).Create(ctx, spec.ConfigMap(), metav1.CreateOptions{}); err != nil {
		rollback()
		return fmt.Errorf("create session configmap: %w", err)
	}
	created = append(created, func(ctx context.Context) error {
		return c.cs.CoreV1().ConfigMaps(spec.Namespace).Delete(ctx, spec.ConfigMapName, metav1.DeleteOptions{})
	})
	if spec.EnableNetworkPolicy {
		if _, err := c.cs.NetworkingV1().NetworkPolicies(spec.Namespace).Create(ctx, spec.NetworkPolicy(), metav1.CreateOptions{}); err != nil {
			rollback()
			return fmt.Errorf("create session network policy: %w", err)
		}
		created = append(created, func(ctx context.Context) error {
			return c.cs.NetworkingV1().NetworkPolicies(spec.Namespace).Delete(ctx, spec.PodName, metav1.DeleteOptions{})
		})
	}
	if _, err := c.cs.CoreV1().Pods(spec.Namespace).Create(ctx, spec.Pod(), metav1.CreateOptions{}); err != nil {
		rollback()
		return fmt.Errorf("create session pod: %w", err)
	}
	return nil
}

// DeleteSession removes the pod and its supporting objects. Missing objects
// are ignored.
func (c *Client) DeleteSession(ctx context.Context, spec *SessionSpec, grace time.Duration) error {
	var errs []error
	graceSeconds := int64(grace.Seconds())
	if err := c.cs.CoreV1().Pods(spec.Namespace).Delete(ctx, spec.PodName, metav1.DeleteOptions{
		GracePeriodSeconds: &graceSeconds,
	}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete pod: %w", err))
	}
	if spec.EnableNetworkPolicy {
		if err := c.cs.NetworkingV1().NetworkPolicies(spec.Namespace).Delete(ctx, spec.PodName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete network policy: %w", err))
		}
	}
	if err := c.cs.CoreV1().ConfigMaps(spec.Namespace).Delete(ctx, spec.ConfigMapName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete configmap: %w", err))
	}
	if err := c.cs.CoreV1().Secrets(spec.Namespace).Delete(ctx, spec.SecretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete secret: %w", err))
	}
	return errors.Join(errs...)
}

// SessionPods lists pods owned by the control plane.
func (c *Client) SessionPods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return nil, fmt.Errorf("list session pods: %w", err)
	}
	return list.Items, nil
}

// GetPod returns one pod.
func (c *Client) GetPod(ctx context.Context, name string) (*corev1.Pod, error) {
	return c.cs.CoreV1().Pods(c.namespace).Get(ctx, name, metav1.GetOptions{})
}

// SessionToken reads the token stored for a session secret.
func (c *Client) SessionToken(ctx context.Context, secretName string) (string, error) {
	secret, err := c.cs.CoreV1().Secrets(c.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(secret.Data["token"]), nil
}

// SessionConfig reads the mounted session configuration.
func (c *Client) SessionConfig(ctx context.Context, configMapName string) ([]byte, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return []byte(cm.Data["session.json"]), nil
}

// Logs returns the tail of the agent container log.
func (c *Client) Logs(ctx context.Context, podName string, maxBytes int64) (string, error) {
	tail := int64(2000)
	opts := &corev1.PodLogOptions{
		Container:  "agent",
		TailLines:  &tail,
		LimitBytes: &maxBytes,
	}
	stream, err := c.cs.CoreV1().Pods(c.namespace).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, maxBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
