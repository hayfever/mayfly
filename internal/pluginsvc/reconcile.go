// Package pluginsvc runs the workloads of service plugins. On Kubernetes a
// Reconciler renders one Deployment, Service, NetworkPolicy, and gateway
// Secret per plugin. On the local backend a Local runs one shim child per
// plugin on a Unix socket. Both answer the gateway's ServiceURL question.
package pluginsvc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/plugin"
)

const (
	// defaultInterval matches plugins.reconcile_interval.
	defaultInterval = 60 * time.Second
	// serviceUID is the unprivileged user every service plugin runs as.
	serviceUID = 65532
	// portName is the name of the MCP port in the pod and the Service.
	portName = "mcp"
	// tmpLimit bounds the writable /tmp of the read-only root filesystem.
	tmpLimit = "64Mi"
	// toolsLimit bounds the volume the shim is copied into.
	toolsLimit = "64Mi"
	// keyReadTimeout bounds reading one gateway key when it is not cached.
	keyReadTimeout = 10 * time.Second
	// defaultPort and defaultPath match the manifest defaults, for a
	// Manifest built in Go rather than parsed.
	defaultPort = 8080
	defaultPath = "/mcp"
)

// RenderConfig holds the cluster facts a service object needs that the
// manifest does not carry.
type RenderConfig struct {
	// Namespace holds the objects.
	Namespace string
	// ToolsImage is runtime.image, which supplies the shim.
	ToolsImage string
	// ImagePullPolicy and ImagePullSecrets apply to plugin images.
	ImagePullPolicy  string
	ImagePullSecrets []string
	// PlatformNamespace and PlatformLabels identify the control plane pods
	// that the NetworkPolicy admits.
	PlatformNamespace string
	PlatformLabels    map[string]string
	// DNSNamespace is where cluster DNS runs.
	DNSNamespace string
}

// Options configures the reconciler. Client and Manifests are required.
type Options struct {
	Client    *k8s.Client
	Manifests func() []plugin.Manifest
	Render    RenderConfig
	// Interval is how often Reconcile runs. Zero uses 60s.
	Interval time.Duration
	// Prune deletes objects whose plugin left the bundle.
	Prune bool
	// Selected holds the plugins some loaded agent selects. A required
	// failure gates readiness only for these.
	Selected map[string]bool
	Logger   *slog.Logger
}

// Reconciler keeps the cluster objects of the bundle's service plugins in
// step with their manifests.
type Reconciler struct {
	opts   Options
	logger *slog.Logger

	mu        sync.Mutex
	manifests map[string]plugin.Manifest
	keys      map[string][]byte
	failures  map[string]error
}

// New returns a reconciler. It creates nothing until Reconcile runs.
func New(opts Options) (*Reconciler, error) {
	if opts.Client == nil {
		return nil, errors.New("a plugin client is required")
	}
	if opts.Manifests == nil {
		return nil, errors.New("a manifest source is required")
	}
	if opts.Render.Namespace == "" {
		return nil, errors.New("the plugins namespace is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Reconciler{
		opts:      opts,
		logger:    opts.Logger,
		manifests: map[string]plugin.Manifest{},
		keys:      map[string][]byte{},
		failures:  map[string]error{},
	}, nil
}

// Run reconciles at once and then every interval until ctx ends. Errors are
// logged; a required plugin's failure is reported by RequiredErr.
func (r *Reconciler) Run(ctx context.Context) {
	if err := r.Reconcile(ctx); err != nil {
		r.logger.Error("reconcile plugin services", "error", err)
	}
	ticker := time.NewTicker(r.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Reconcile(ctx); err != nil {
				r.logger.Error("reconcile plugin services", "error", err)
			}
		}
	}
}

// Reconcile creates, updates, and prunes the service plugin objects.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	desired := serviceManifests(r.opts.Manifests())
	failures := map[string]error{}
	keys := map[string][]byte{}
	var errs []error

	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		manifest := desired[name]
		fresh, err := newGatewayKey()
		if err != nil {
			failures[name] = err
			errs = append(errs, fmt.Errorf("plugin %q: %w", name, err))
			continue
		}
		key, err := r.opts.Client.EnsurePluginSecret(ctx, gatewaySecret(manifest, r.opts.Render.Namespace, fresh))
		if err != nil {
			failures[name] = err
			errs = append(errs, fmt.Errorf("plugin %q: %w", name, err))
			continue
		}
		objects, err := renderService(manifest, r.opts.Render, key)
		if err != nil {
			failures[name] = err
			errs = append(errs, fmt.Errorf("plugin %q: %w", name, err))
			continue
		}
		if err := r.opts.Client.ApplyPluginObjects(ctx, objects); err != nil {
			failures[name] = err
			errs = append(errs, fmt.Errorf("plugin %q: %w", name, err))
			continue
		}
		keys[name] = key
	}

	if r.opts.Prune {
		if err := r.prune(ctx, desired); err != nil {
			errs = append(errs, err)
		}
	}

	r.mu.Lock()
	r.manifests = desired
	r.keys = keys
	r.failures = failures
	r.mu.Unlock()
	return errors.Join(errs...)
}

// prune deletes the objects of service plugins that the bundle no longer
// holds. It lists every kind, because a create that failed partway can leave
// a Service, a NetworkPolicy, or a Secret with no Deployment.
func (r *Reconciler) prune(ctx context.Context, desired map[string]plugin.Manifest) error {
	existing, err := r.opts.Client.PluginObjectNames(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range existing {
		if _, ok := desired[name]; ok {
			continue
		}
		if err := r.opts.Client.DeletePluginObjects(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("prune plugin %q: %w", name, err))
			continue
		}
		r.logger.Info("pruned plugin service", "plugin", name)
	}
	return errors.Join(errs...)
}

// ServiceURL returns a service plugin's endpoint and gateway key for the
// gateway. It reads the key from the Secret, so a Secret that was deleted and
// recreated is picked up without a restart. The cached key is the fallback
// when the cluster cannot be read.
func (r *Reconciler) ServiceURL(name string) (string, []byte, error) {
	r.mu.Lock()
	manifest, ok := r.manifests[name]
	cached := r.keys[name]
	r.mu.Unlock()
	if !ok {
		manifest, ok = findService(r.opts.Manifests(), name)
		if !ok {
			return "", nil, fmt.Errorf("plugin %q is not a service plugin", name)
		}
	}
	if manifest.Service == nil || manifest.Service.MCPServer != "" {
		return "", nil, fmt.Errorf("plugin %q is not a service workload", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), keyReadTimeout)
	key, err := r.opts.Client.PluginGatewayKey(ctx, name)
	cancel()
	if err != nil {
		if len(cached) == 0 {
			return "", nil, err
		}
		key = cached
	} else {
		r.mu.Lock()
		r.keys[name] = key
		r.mu.Unlock()
	}
	port := manifest.Service.Port
	if port == 0 {
		port = defaultPort
	}
	path := manifest.Service.Path
	if path == "" {
		path = defaultPath
	}
	host := plugin.ObjectName(name) + "." + r.opts.Render.Namespace + ".svc"
	return fmt.Sprintf("http://%s:%d%s", host, port, path), key, nil
}

// RequiredErr reports the last reconcile failures of required plugins that a
// loaded agent selects. /readyz reports these; other failures stay in the log.
func (r *Reconciler) RequiredErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.failures))
	for name := range r.failures {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		manifest, ok := r.manifests[name]
		if !ok || !manifest.Required || !r.opts.Selected[name] {
			continue
		}
		errs = append(errs, fmt.Errorf("plugin %q: %w", name, r.failures[name]))
	}
	return errors.Join(errs...)
}

// serviceManifests indexes the manifests that need a workload. A plugin that
// names an mcp.servers entry creates nothing.
func serviceManifests(manifests []plugin.Manifest) map[string]plugin.Manifest {
	out := map[string]plugin.Manifest{}
	for _, manifest := range manifests {
		if manifest.Placement != plugin.Service {
			continue
		}
		if manifest.Service != nil && manifest.Service.MCPServer != "" {
			continue
		}
		out[manifest.Name] = manifest
	}
	return out
}

// findService looks up one service workload in a manifest list.
func findService(manifests []plugin.Manifest, name string) (plugin.Manifest, bool) {
	for _, manifest := range manifests {
		if manifest.Name != name {
			continue
		}
		if manifest.Placement != plugin.Service || manifest.Service == nil || manifest.Service.MCPServer != "" {
			return plugin.Manifest{}, false
		}
		return manifest, true
	}
	return plugin.Manifest{}, false
}

// newGatewayKey returns one 32-byte service gateway key.
func newGatewayKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate gateway key: %w", err)
	}
	return key, nil
}

// gatewaySecret is the Secret that holds one service plugin's gateway key.
// EnsurePluginSecret creates it only when it is absent.
func gatewaySecret(manifest plugin.Manifest, namespace string, key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      plugin.GatewayKeyName(manifest.Name),
			Namespace: namespace,
			Labels:    pluginLabels(manifest.Name),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"key": key},
	}
}

// renderService builds the three workload objects of one service plugin.
func renderService(manifest plugin.Manifest, cfg RenderConfig, key []byte) (k8s.PluginObjects, error) {
	if manifest.Service == nil {
		return k8s.PluginObjects{}, errors.New("service settings are required")
	}
	deployment, err := renderDeployment(manifest, cfg, key)
	if err != nil {
		return k8s.PluginObjects{}, err
	}
	labels := pluginLabels(manifest.Name)
	annotations := map[string]string{k8s.AnnotationSpecHash: renderHash(manifest, cfg)}
	return k8s.PluginObjects{
		Deployment:    deployment,
		Service:       renderServiceObject(manifest, cfg, labels, annotations),
		NetworkPolicy: renderNetworkPolicy(manifest, cfg, labels, annotations),
	}, nil
}

// renderAnnotations identify the rendered Deployment and its pods: the
// manifest and the RenderConfig, so a changed tools image or command updates
// the Deployment, and the gateway key, so a recreated Secret rolls the pods.
func renderAnnotations(manifest plugin.Manifest, cfg RenderConfig, key []byte) map[string]string {
	return map[string]string{
		k8s.AnnotationSpecHash:       renderHash(manifest, cfg),
		k8s.AnnotationGatewayKeyHash: keyHash(key),
	}
}

// renderHash identifies the objects that a manifest and a RenderConfig
// render.
func renderHash(manifest plugin.Manifest, cfg RenderConfig) string {
	raw, err := json.Marshal(struct {
		Manifest string
		Render   RenderConfig
	}{manifest.SpecHash(), cfg})
	if err != nil {
		// A manifest and a RenderConfig hold only strings and maps of
		// strings, so encoding cannot fail.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// keyHash identifies the gateway key a pod was rendered with.
func keyHash(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

// pluginLabels are the labels of every object and pod of one service plugin.
func pluginLabels(name string) map[string]string {
	return map[string]string{
		k8s.LabelManaged:               "true",
		k8s.LabelApp:                   k8s.PluginAppName,
		k8s.LabelPlugin:                name,
		"app.kubernetes.io/managed-by": "mayfly",
	}
}

// pluginSelector identifies the pods of one service plugin.
func pluginSelector(name string) map[string]string {
	return map[string]string{
		k8s.LabelApp:    k8s.PluginAppName,
		k8s.LabelPlugin: name,
	}
}

// renderDeployment builds the plugin Deployment. The shim owns the process in
// stdio mode; an http plugin listens on its own port and gets the gateway key
// as an environment variable.
func renderDeployment(manifest plugin.Manifest, cfg RenderConfig, key []byte) (*appsv1.Deployment, error) {
	service := manifest.Service
	if service == nil {
		return nil, errors.New("service settings are required")
	}
	port := service.Port
	if port == 0 {
		port = defaultPort
	}
	no := false
	yes := true
	uid := int64(serviceUID)
	seccomp := corev1.SeccompProfileTypeRuntimeDefault

	env := []corev1.EnvVar{
		{Name: "MAYFLY_PLUGIN_NAME", Value: manifest.Name},
		{Name: "MAYFLY_PLUGIN_DIR", Value: k8s.PluginImageDir},
	}
	for _, name := range sortedKeys(manifest.Env) {
		env = append(env, corev1.EnvVar{Name: name, Value: manifest.Env[name]})
	}
	for _, secret := range manifest.Secrets {
		env = append(env, corev1.EnvVar{
			Name: secret.Env,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secret.Secret},
					Key:                  secret.Key,
				},
			},
		})
	}

	container := corev1.Container{
		Name:            "plugin",
		Image:           manifest.Image,
		ImagePullPolicy: corev1.PullPolicy(cfg.ImagePullPolicy),
		Env:             env,
		WorkingDir:      k8s.PluginImageDir,
		Ports: []corev1.ContainerPort{{
			Name:          portName,
			ContainerPort: port,
			Protocol:      corev1.ProtocolTCP,
		}},
		SecurityContext: hardenedContext(uid),
		Resources: corev1.ResourceRequirements{
			Requests: quantities(service.Resources.Requests),
			Limits:   quantities(service.Resources.Limits),
		},
	}
	volumes := []corev1.Volume{{
		Name: "tmp",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: quantityPtr(tmpLimit),
			},
		},
	}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	var initContainers []corev1.Container

	switch service.Transport {
	case "stdio", "":
		command := []string{
			k8s.ToolsDir + "/mayfly-plugin-shim", "serve",
			"--listen", "tcp::" + strconv.Itoa(int(port)),
			"--key-file", k8s.GatewayKeyDir + "/key",
			"--max-concurrency", strconv.Itoa(service.Concurrency),
			"--init-timeout", manifest.InitTimeout().String(),
			"--", manifest.Command,
		}
		command = append(command, manifest.Args...)
		container.Command = command
		container.StartupProbe = shimProbe(port, 30)
		container.ReadinessProbe = shimProbe(port, 0)
		mounts = append(mounts,
			corev1.VolumeMount{Name: "mayfly-tools", MountPath: k8s.ToolsDir, ReadOnly: true},
			corev1.VolumeMount{Name: "gateway-key", MountPath: k8s.GatewayKeyDir, ReadOnly: true},
		)
		volumes = append(volumes,
			corev1.Volume{
				Name: "mayfly-tools",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr(toolsLimit)},
				},
			},
			gatewayKeyVolume(manifest.Name),
		)
		initContainers = []corev1.Container{toolsContainer(cfg, uid)}
	case "http":
		container.Command = []string{manifest.Command}
		container.Args = append([]string(nil), manifest.Args...)
		container.Env = append(container.Env, gatewayKeyEnv(manifest.Name))
		container.StartupProbe = socketProbe(port, 30)
		container.ReadinessProbe = socketProbe(port, 0)
	default:
		return nil, fmt.Errorf("unknown service.transport %q", service.Transport)
	}
	container.VolumeMounts = mounts

	replicas := int32(1)
	if service.Replicas != nil {
		replicas = *service.Replicas
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        plugin.ObjectName(manifest.Name),
			Namespace:   cfg.Namespace,
			Labels:      pluginLabels(manifest.Name),
			Annotations: renderAnnotations(manifest, cfg, key),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: pluginSelector(manifest.Name)},
			Template: corev1.PodTemplateSpec{
				// The pod template carries the same hashes, so a change to
				// the rendered objects or to the gateway key rolls the pods.
				ObjectMeta: metav1.ObjectMeta{
					Labels:      pluginLabels(manifest.Name),
					Annotations: renderAnnotations(manifest, cfg, key),
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyAlways,
					AutomountServiceAccountToken: &no,
					EnableServiceLinks:           &no,
					ImagePullSecrets:             localObjects(cfg.ImagePullSecrets),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &yes,
						RunAsUser:    &uid,
						RunAsGroup:   &uid,
						// Kubelet writes a Secret volume as root:root
						// without a group, and the container cannot read
						// its key. FSGroup makes the projected files
						// readable by the plugin's group.
						FSGroup:        &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: seccomp},
					},
					InitContainers: initContainers,
					Containers:     []corev1.Container{container},
					Volumes:        volumes,
				},
			},
		},
	}, nil
}

// hardenedContext is the security context of a plugin container.
func hardenedContext(uid int64) *corev1.SecurityContext {
	no := false
	yes := true
	rootfs := true
	return &corev1.SecurityContext{
		RunAsNonRoot:             &yes,
		RunAsUser:                &uid,
		RunAsGroup:               &uid,
		AllowPrivilegeEscalation: &no,
		ReadOnlyRootFilesystem:   &rootfs,
		Privileged:               &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// toolsContainer copies the shim out of the tools image.
func toolsContainer(cfg RenderConfig, uid int64) corev1.Container {
	return corev1.Container{
		Name:            "mayfly-tools",
		Image:           cfg.ToolsImage,
		ImagePullPolicy: corev1.PullPolicy(cfg.ImagePullPolicy),
		Command:         []string{k8s.PluginShim, "install-self", k8s.ToolsDir + "/mayfly-plugin-shim"},
		SecurityContext: hardenedContext(uid),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "mayfly-tools", MountPath: k8s.ToolsDir}},
	}
}

// gatewayKeyVolume projects only the key of the plugin's gateway Secret. The
// mode is group-readable, and the pod sets FSGroup, so the shim reads the key
// as an unprivileged user.
func gatewayKeyVolume(name string) corev1.Volume {
	mode := int32(0o440)
	return corev1.Volume{
		Name: "gateway-key",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  plugin.GatewayKeyName(name),
				DefaultMode: &mode,
				Items:       []corev1.KeyToPath{{Key: "key", Path: "key", Mode: &mode}},
			},
		},
	}
}

// gatewayKeyEnv passes the gateway key to an http plugin.
func gatewayKeyEnv(name string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: "MAYFLY_GATEWAY_KEY",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: plugin.GatewayKeyName(name)},
				Key:                  "key",
			},
		},
	}
}

// shimProbe runs the shim's readiness command against a listening shim. A
// failure threshold above 3 turns the probe into a startup probe.
func shimProbe(port int32, failureThreshold int32) *corev1.Probe {
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{
				k8s.ToolsDir + "/mayfly-plugin-shim", "probe",
				"--listen", "tcp::" + strconv.Itoa(int(port)),
			}},
		},
		PeriodSeconds:    2,
		TimeoutSeconds:   3,
		FailureThreshold: 3,
	}
	if failureThreshold > 0 {
		probe.FailureThreshold = failureThreshold
	}
	return probe
}

// socketProbe reports whether an http plugin listens on its port.
func socketProbe(port int32, failureThreshold int32) *corev1.Probe {
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
		},
		PeriodSeconds:    2,
		TimeoutSeconds:   3,
		FailureThreshold: 3,
	}
	if failureThreshold > 0 {
		probe.FailureThreshold = failureThreshold
	}
	return probe
}

// renderServiceObject exposes the plugin on the cluster network.
func renderServiceObject(manifest plugin.Manifest, cfg RenderConfig, labels, annotations map[string]string) *corev1.Service {
	port := manifest.Service.Port
	if port == 0 {
		port = defaultPort
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        plugin.ObjectName(manifest.Name),
			Namespace:   cfg.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: pluginSelector(manifest.Name),
			Ports: []corev1.ServicePort{{
				Name:       portName,
				Port:       port,
				TargetPort: intstr.FromInt32(port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// renderNetworkPolicy admits only the control plane and allows egress only to
// DNS and the manifest's CIDRs.
func renderNetworkPolicy(manifest plugin.Manifest, cfg RenderConfig, labels, annotations map[string]string) *networkingv1.NetworkPolicy {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	dnsPort := intstr.FromInt32(53)
	port := manifest.Service.Port
	if port == 0 {
		port = defaultPort
	}
	mcpPort := intstr.FromInt32(port)

	ingress := []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": cfg.PlatformNamespace,
			}},
			PodSelector: &metav1.LabelSelector{MatchLabels: cfg.PlatformLabels},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &mcpPort}},
	}}
	egress := []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": cfg.DNSNamespace,
			}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &dnsPort},
			{Protocol: &tcp, Port: &dnsPort},
		},
	}}
	for _, rule := range manifest.Service.Egress {
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(rule.Ports))
		for _, allowed := range rule.Ports {
			allowedPort := intstr.FromInt32(allowed)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &allowedPort})
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: rule.CIDR}}},
			Ports: ports,
		})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:        plugin.ObjectName(manifest.Name),
			Namespace:   cfg.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: pluginSelector(manifest.Name)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

// sortedKeys returns a map's keys in a deterministic order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// quantities converts resource strings into a ResourceList.
func quantities(in map[string]string) corev1.ResourceList {
	if len(in) == 0 {
		return nil
	}
	out := corev1.ResourceList{}
	for name, value := range in {
		out[corev1.ResourceName(name)] = resource.MustParse(value)
	}
	return out
}

// quantityPtr parses one resource quantity.
func quantityPtr(value string) *resource.Quantity {
	q := resource.MustParse(value)
	return &q
}

// localObjects converts image pull secret names.
func localObjects(names []string) []corev1.LocalObjectReference {
	out := make([]corev1.LocalObjectReference, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		out = append(out, corev1.LocalObjectReference{Name: name})
	}
	return out
}
