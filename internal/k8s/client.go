package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/version"
)

const managedSelector = LabelManaged + "=true," + LabelApp + "=" + AppName

// Client wraps the Kubernetes API with session-specific operations.
type Client struct {
	cs        kubernetes.Interface
	namespace string
	// log is nil in production, where the process default applies.
	log *slog.Logger
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

// WithNamespace returns a client for another namespace that shares the
// clientset. The service reconciler uses it for plugins.namespace.
func (c *Client) WithNamespace(namespace string) *Client {
	return &Client{cs: c.cs, namespace: namespace, log: c.log}
}

// logger returns the client's logger, or the process default when unset.
func (c *Client) logger() *slog.Logger {
	if c.log != nil {
		return c.log
	}
	return slog.Default()
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

// rollbackDelete is one object CreateSession created and the call that
// removes it.
type rollbackDelete struct {
	object string
	remove func(context.Context) error
}

// CreateSession creates the secret, config map, network policy, then the pod.
// On failure it removes everything it created.
func (c *Client) CreateSession(ctx context.Context, spec *SessionSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	created := []rollbackDelete{}
	// rollback removes the created objects newest first. CreateSession
	// reports only the create error that caused the rollback, so a failed
	// delete is logged here instead; the remaining deletes still run. A
	// NotFound delete succeeded: the object is already gone.
	rollback := func() {
		for i := len(created) - 1; i >= 0; i-- {
			err := created[i].remove(context.Background())
			if err != nil && !apierrors.IsNotFound(err) {
				c.logger().Error("session rollback delete failed",
					"session", spec.SessionID,
					"object", created[i].object,
					"error", err)
			}
		}
	}
	if _, err := c.cs.CoreV1().Secrets(spec.Namespace).Create(ctx, spec.Secret(), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create session secret: %w", err)
	}
	created = append(created, rollbackDelete{
		object: "secret/" + spec.SecretName,
		remove: func(ctx context.Context) error {
			return c.cs.CoreV1().Secrets(spec.Namespace).Delete(ctx, spec.SecretName, metav1.DeleteOptions{})
		},
	})
	if _, err := c.cs.CoreV1().ConfigMaps(spec.Namespace).Create(ctx, spec.ConfigMap(), metav1.CreateOptions{}); err != nil {
		rollback()
		return fmt.Errorf("create session configmap: %w", err)
	}
	created = append(created, rollbackDelete{
		object: "configmap/" + spec.ConfigMapName,
		remove: func(ctx context.Context) error {
			return c.cs.CoreV1().ConfigMaps(spec.Namespace).Delete(ctx, spec.ConfigMapName, metav1.DeleteOptions{})
		},
	})
	if spec.EnableNetworkPolicy {
		if _, err := c.cs.NetworkingV1().NetworkPolicies(spec.Namespace).Create(ctx, spec.NetworkPolicy(), metav1.CreateOptions{}); err != nil {
			rollback()
			return fmt.Errorf("create session network policy: %w", err)
		}
		created = append(created, rollbackDelete{
			object: "networkpolicy/" + spec.NetworkPolicyName,
			remove: func(ctx context.Context) error {
				return c.cs.NetworkingV1().NetworkPolicies(spec.Namespace).Delete(ctx, spec.NetworkPolicyName, metav1.DeleteOptions{})
			},
		})
	}
	if spec.PersistentWorkspace {
		if _, err := c.cs.CoreV1().PersistentVolumeClaims(spec.Namespace).Create(ctx, spec.PersistentVolumeClaim(), metav1.CreateOptions{}); err != nil {
			rollback()
			return fmt.Errorf("create session workspace claim: %w", err)
		}
		created = append(created, rollbackDelete{
			object: "persistentvolumeclaim/" + spec.PVCName,
			remove: func(ctx context.Context) error {
				return c.cs.CoreV1().PersistentVolumeClaims(spec.Namespace).Delete(ctx, spec.PVCName, metav1.DeleteOptions{})
			},
		})
	}
	if _, err := c.cs.CoreV1().Pods(spec.Namespace).Create(ctx, spec.Pod(), metav1.CreateOptions{}); err != nil {
		rollback()
		return fmt.Errorf("create session pod: %w", err)
	}
	return nil
}

// CreatePod creates only the pod. A replacement container reuses the existing
// Secret, ConfigMap, NetworkPolicy, and workspace claim.
func (c *Client) CreatePod(ctx context.Context, spec *SessionSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if _, err := c.cs.CoreV1().Pods(spec.Namespace).Create(ctx, spec.Pod(), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create replacement session pod: %w", err)
	}
	return nil
}

// DeletePod removes one pod and ignores an already-absent pod.
func (c *Client) DeletePod(ctx context.Context, name string, grace time.Duration) error {
	graceSeconds := int64(grace.Seconds())
	err := c.cs.CoreV1().Pods(c.namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &graceSeconds,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod: %w", err)
	}
	return nil
}

// DeleteSession removes the pod and its supporting objects. Missing objects
// are ignored.
//
// The ConfigMap goes first. Adoption needs it to resume a session, so a
// control plane that stops partway through this cleanup cannot bring a
// finalized session back; adoption deletes a pod whose ConfigMap is gone.
func (c *Client) DeleteSession(ctx context.Context, spec *SessionSpec, grace time.Duration) error {
	var errs []error
	graceSeconds := int64(grace.Seconds())
	if err := c.cs.CoreV1().ConfigMaps(spec.Namespace).Delete(ctx, spec.ConfigMapName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete configmap: %w", err))
	}
	if err := c.cs.CoreV1().Pods(spec.Namespace).Delete(ctx, spec.PodName, metav1.DeleteOptions{
		GracePeriodSeconds: &graceSeconds,
	}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete pod: %w", err))
	}
	if spec.NetworkPolicyName != "" {
		if err := c.cs.NetworkingV1().NetworkPolicies(spec.Namespace).Delete(ctx, spec.NetworkPolicyName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete network policy: %w", err))
		}
	}
	if err := c.cs.CoreV1().Secrets(spec.Namespace).Delete(ctx, spec.SecretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete secret: %w", err))
	}
	if spec.PersistentWorkspace && spec.PVCName != "" {
		if err := c.cs.CoreV1().PersistentVolumeClaims(spec.Namespace).Delete(ctx, spec.PVCName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete workspace claim: %w", err))
		}
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

// OrphanObjects holds the supporting object names of one session that has no
// pod.
type OrphanObjects struct {
	ConfigMap string
	Secret    string
	PVC       string
}

// OrphanSessions reports the labelled objects of sessions that have no live
// pod and are not in live. A session with all three objects can be resumed
// with a replacement pod; one missing any of them can only be collected.
func (c *Client) OrphanSessions(ctx context.Context, live map[string]struct{}) (map[string]OrphanObjects, error) {
	out := map[string]OrphanObjects{}
	orphan := func(session string) bool {
		if session == "" {
			return false
		}
		_, ok := live[session]
		return !ok
	}
	configs, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return nil, fmt.Errorf("list session configmaps: %w", err)
	}
	for _, cm := range configs.Items {
		if !orphan(cm.Labels[LabelSession]) {
			continue
		}
		o := out[cm.Labels[LabelSession]]
		o.ConfigMap = cm.Name
		out[cm.Labels[LabelSession]] = o
	}
	secrets, err := c.cs.CoreV1().Secrets(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return nil, fmt.Errorf("list session secrets: %w", err)
	}
	for _, secret := range secrets.Items {
		if !orphan(secret.Labels[LabelSession]) {
			continue
		}
		o := out[secret.Labels[LabelSession]]
		o.Secret = secret.Name
		out[secret.Labels[LabelSession]] = o
	}
	claims, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return nil, fmt.Errorf("list session claims: %w", err)
	}
	for _, claim := range claims.Items {
		if !orphan(claim.Labels[LabelSession]) {
			continue
		}
		o := out[claim.Labels[LabelSession]]
		o.PVC = claim.Name
		out[claim.Labels[LabelSession]] = o
	}
	return out, nil
}

// GarbageCollect removes the labelled objects of sessions that no longer have
// a pod. A session's only live state is its pod and its in-memory record, so
// once the pod is gone those objects can never be managed again and a claim
// keeps costing money. It returns the session IDs it cleaned.
func (c *Client) GarbageCollect(ctx context.Context, live map[string]struct{}) ([]string, error) {
	orphan := func(session string) bool {
		if session == "" {
			return false
		}
		_, ok := live[session]
		return !ok
	}
	cleaned := map[string]struct{}{}
	var errs []error

	configs, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return nil, fmt.Errorf("list session configmaps: %w", err)
	}
	for _, cm := range configs.Items {
		if !orphan(cm.Labels[LabelSession]) {
			continue
		}
		cleaned[cm.Labels[LabelSession]] = struct{}{}
		if err := c.cs.CoreV1().ConfigMaps(c.namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete configmap %s: %w", cm.Name, err))
		}
	}
	secrets, err := c.cs.CoreV1().Secrets(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		errs = append(errs, fmt.Errorf("list session secrets: %w", err))
	}
	for _, secret := range secrets.Items {
		if !orphan(secret.Labels[LabelSession]) {
			continue
		}
		cleaned[secret.Labels[LabelSession]] = struct{}{}
		if err := c.cs.CoreV1().Secrets(c.namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete secret %s: %w", secret.Name, err))
		}
	}
	claims, err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		errs = append(errs, fmt.Errorf("list session claims: %w", err))
	}
	for _, claim := range claims.Items {
		if !orphan(claim.Labels[LabelSession]) {
			continue
		}
		cleaned[claim.Labels[LabelSession]] = struct{}{}
		if err := c.cs.CoreV1().PersistentVolumeClaims(c.namespace).Delete(ctx, claim.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete claim %s: %w", claim.Name, err))
		}
	}
	policies, err := c.cs.NetworkingV1().NetworkPolicies(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		errs = append(errs, fmt.Errorf("list session network policies: %w", err))
	}
	for _, policy := range policies.Items {
		if !orphan(policy.Labels[LabelSession]) {
			continue
		}
		cleaned[policy.Labels[LabelSession]] = struct{}{}
		if err := c.cs.NetworkingV1().NetworkPolicies(c.namespace).Delete(ctx, policy.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete network policy %s: %w", policy.Name, err))
		}
	}

	out := make([]string, 0, len(cleaned))
	for session := range cleaned {
		out = append(out, session)
	}
	sort.Strings(out)
	return out, errors.Join(errs...)
}

// GetPod returns one pod.
func (c *Client) GetPod(ctx context.Context, name string) (*corev1.Pod, error) {
	return c.cs.CoreV1().Pods(c.namespace).Get(ctx, name, metav1.GetOptions{})
}

// SessionKeys are the credentials stored for one session.
type SessionKeys struct {
	// Token authenticates every internal route the runtime calls.
	Token string
	// GatewayKey signs the call tokens of this session's sidecars. It is empty
	// for a session without one.
	GatewayKey []byte
}

// SessionKeys reads the credentials stored for a session secret.
func (c *Client) SessionKeys(ctx context.Context, secretName string) (SessionKeys, error) {
	secret, err := c.cs.CoreV1().Secrets(c.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return SessionKeys{}, err
	}
	return SessionKeys{
		Token:      string(secret.Data["token"]),
		GatewayKey: append([]byte(nil), secret.Data[GatewayKeySecretKey]...),
	}, nil
}

// SessionConfig reads the mounted session configuration.
func (c *Client) SessionConfig(ctx context.Context, configMapName string) ([]byte, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return []byte(cm.Data["session.json"]), nil
}

// pluginSelector matches the objects of every service plugin. It selects on
// the plugin app label, so the session selectors never see them.
const pluginSelector = LabelManaged + "=true," + LabelApp + "=" + PluginAppName

// AnnotationGatewayKeyHash records the gateway key a plugin pod was rendered
// with. A Secret that is deleted and recreated changes the hash, so the
// Deployment update rolls the pods onto the new key.
const AnnotationGatewayKeyHash = "mayfly.io/gateway-key-hash"

// PluginObjects is the three workload objects that back one service plugin.
// The gateway Secret is separate, because the Deployment is rendered with the
// hash of the key the Secret holds.
type PluginObjects struct {
	Deployment    *appsv1.Deployment
	Service       *corev1.Service
	NetworkPolicy *networkingv1.NetworkPolicy
}

// EnsurePluginSecret creates the gateway Secret when it is absent and returns
// the key it holds: the fresh key on the first create, and the key already in
// the cluster after that, so a reconcile never rotates a live key and never
// leaves the pods with the wrong one.
func (c *Client) EnsurePluginSecret(ctx context.Context, secret *corev1.Secret) ([]byte, error) {
	if secret == nil {
		return nil, errors.New("a plugin secret is required")
	}
	if len(secret.Data["key"]) == 0 {
		return nil, fmt.Errorf("plugin secret %s holds no key", secret.Name)
	}
	secrets := c.cs.CoreV1().Secrets(c.namespace)
	switch _, err := secrets.Get(ctx, secret.Name, metav1.GetOptions{}); {
	case apierrors.IsNotFound(err):
		if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("create plugin secret: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("get plugin secret: %w", err)
	}
	return c.pluginKey(ctx, secret.Name)
}

// ApplyPluginObjects creates the missing objects and updates the ones whose
// rendered annotations changed.
func (c *Client) ApplyPluginObjects(ctx context.Context, objects PluginObjects) error {
	if objects.Deployment == nil || objects.Service == nil || objects.NetworkPolicy == nil {
		return errors.New("plugin objects need a deployment, a service, and a network policy")
	}
	if err := c.applyPluginDeployment(ctx, objects.Deployment); err != nil {
		return err
	}
	if err := c.applyPluginService(ctx, objects.Service); err != nil {
		return err
	}
	return c.applyPluginNetworkPolicy(ctx, objects.NetworkPolicy)
}

// applyPluginDeployment creates the Deployment, or updates it when the
// annotations the renderer set, the spec hash and the gateway key hash,
// differ from the stored ones.
func (c *Client) applyPluginDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	deployments := c.cs.AppsV1().Deployments(c.namespace)
	current, err := deployments.Get(ctx, desired.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := deployments.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create plugin deployment: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get plugin deployment: %w", err)
	case sameAnnotations(current.Annotations, desired.Annotations):
		return nil
	}
	desired.ResourceVersion = current.ResourceVersion
	if _, err := deployments.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update plugin deployment: %w", err)
	}
	return nil
}

// applyPluginService creates the Service, or updates it when a rendered
// annotation changed. The cluster-assigned addresses stay, because they are
// immutable.
func (c *Client) applyPluginService(ctx context.Context, desired *corev1.Service) error {
	services := c.cs.CoreV1().Services(c.namespace)
	current, err := services.Get(ctx, desired.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := services.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create plugin service: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get plugin service: %w", err)
	case sameAnnotations(current.Annotations, desired.Annotations):
		return nil
	}
	desired.ResourceVersion = current.ResourceVersion
	desired.Spec.ClusterIP = current.Spec.ClusterIP
	desired.Spec.ClusterIPs = current.Spec.ClusterIPs
	if _, err := services.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update plugin service: %w", err)
	}
	return nil
}

// applyPluginNetworkPolicy creates the NetworkPolicy, or updates it when a
// rendered annotation changed.
func (c *Client) applyPluginNetworkPolicy(ctx context.Context, desired *networkingv1.NetworkPolicy) error {
	policies := c.cs.NetworkingV1().NetworkPolicies(c.namespace)
	current, err := policies.Get(ctx, desired.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := policies.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create plugin network policy: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get plugin network policy: %w", err)
	case sameAnnotations(current.Annotations, desired.Annotations):
		return nil
	}
	desired.ResourceVersion = current.ResourceVersion
	if _, err := policies.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update plugin network policy: %w", err)
	}
	return nil
}

// sameAnnotations reports whether the live object already carries every
// annotation the renderer set. The spec hash and the gateway key hash are both
// compared, so a changed tools image or a recreated Secret updates the object.
func sameAnnotations(current, desired map[string]string) bool {
	for key, value := range desired {
		if current[key] != value {
			return false
		}
	}
	return true
}

// DeletePluginObjects removes the four objects of one service plugin. Missing
// objects are ignored, because prune runs against whatever the cluster holds.
func (c *Client) DeletePluginObjects(ctx context.Context, name string) error {
	var errs []error
	object := plugin.ObjectName(name)
	if err := c.cs.AppsV1().Deployments(c.namespace).Delete(ctx, object, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete plugin deployment: %w", err))
	}
	if err := c.cs.CoreV1().Services(c.namespace).Delete(ctx, object, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete plugin service: %w", err))
	}
	if err := c.cs.NetworkingV1().NetworkPolicies(c.namespace).Delete(ctx, object, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete plugin network policy: %w", err))
	}
	if err := c.cs.CoreV1().Secrets(c.namespace).Delete(ctx, plugin.GatewayKeyName(name), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete plugin secret: %w", err))
	}
	return errors.Join(errs...)
}

// PluginObjectNames lists the plugin label of every object that backs a
// service plugin, across all four kinds. Reconcile prunes a name that the
// bundle no longer holds even when a create failed partway and left no
// Deployment behind.
func (c *Client) PluginObjectNames(ctx context.Context) ([]string, error) {
	list := metav1.ListOptions{LabelSelector: pluginSelector}
	names := map[string]struct{}{}
	record := func(name string) {
		if name != "" {
			names[name] = struct{}{}
		}
	}
	deployments, err := c.cs.AppsV1().Deployments(c.namespace).List(ctx, list)
	if err != nil {
		return nil, fmt.Errorf("list plugin deployments: %w", err)
	}
	for _, object := range deployments.Items {
		record(object.Labels[LabelPlugin])
	}
	services, err := c.cs.CoreV1().Services(c.namespace).List(ctx, list)
	if err != nil {
		return nil, fmt.Errorf("list plugin services: %w", err)
	}
	for _, object := range services.Items {
		record(object.Labels[LabelPlugin])
	}
	policies, err := c.cs.NetworkingV1().NetworkPolicies(c.namespace).List(ctx, list)
	if err != nil {
		return nil, fmt.Errorf("list plugin network policies: %w", err)
	}
	for _, object := range policies.Items {
		record(object.Labels[LabelPlugin])
	}
	secrets, err := c.cs.CoreV1().Secrets(c.namespace).List(ctx, list)
	if err != nil {
		return nil, fmt.Errorf("list plugin secrets: %w", err)
	}
	for _, object := range secrets.Items {
		record(object.Labels[LabelPlugin])
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// PluginGatewayKey reads the gateway key of one service plugin.
func (c *Client) PluginGatewayKey(ctx context.Context, name string) ([]byte, error) {
	return c.pluginKey(ctx, plugin.GatewayKeyName(name))
}

// pluginKey reads one gateway key Secret by object name.
func (c *Client) pluginKey(ctx context.Context, secretName string) ([]byte, error) {
	secret, err := c.cs.CoreV1().Secrets(c.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get plugin secret: %w", err)
	}
	key := secret.Data["key"]
	if len(key) == 0 {
		return nil, fmt.Errorf("plugin secret %s has no key", secretName)
	}
	return key, nil
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
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(io.LimitReader(stream, maxBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
