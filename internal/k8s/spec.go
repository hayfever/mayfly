// Package k8s builds and manages the Kubernetes objects that back a session.
package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

const (
	// LabelManaged marks every object owned by the control plane.
	LabelManaged = "mayfly.io/managed"
	// LabelSession carries the session ID.
	LabelSession = "mayfly.io/session"
	// LabelApp is the app label for session pods.
	LabelApp = "app.kubernetes.io/name"
	// AppName is the value of LabelApp for session pods.
	AppName = "mayfly-agent"
	// ConfigFile is the path of the mounted session configuration.
	ConfigFile = "/etc/mayfly/session.json"
	// WorkspaceDir is where the agent works.
	WorkspaceDir = "/workspace"
	// ArtifactsDir holds logs and the bundle while it is built.
	ArtifactsDir = "/artifacts"
	// PluginsDir holds the copied trees of the inline plugins.
	PluginsDir = "/opt/mayfly/plugins"
	// PluginSecretsDir holds one directory of secret files per inline plugin.
	PluginSecretsDir = "/var/run/mayfly/plugin-secrets"
	// PluginSocketsDir holds one Unix socket per sidecar plugin.
	PluginSocketsDir = "/run/mayfly/plugin-sockets"
	// ToolsDir holds the shim that the copy init containers run.
	ToolsDir = "/mayfly-tools"
	// PluginShim is the shim's path in the agent image.
	PluginShim = "/usr/local/bin/mayfly-plugin-shim"
	// PluginImageDir is the directory a plugin image must contain.
	PluginImageDir = "/mayfly-plugin"
	// GatewayKeyDir holds the gateway key file of a sidecar plugin.
	GatewayKeyDir = "/var/run/mayfly/gateway"
	// GatewayKeySecretKey is the session Secret key that signs sidecar call
	// tokens. Only a sidecar mounts it.
	GatewayKeySecretKey = "gateway-key"
	// LabelPlugin carries the plugin name on every service plugin object.
	LabelPlugin = "mayfly.io/plugin"
	// PluginAppName is the app label of service plugin objects. It differs
	// from AppName so the session selectors never match a plugin object.
	PluginAppName = "mayfly-plugin"
	// AnnotationSpecHash records the manifest hash a plugin object was
	// rendered from, so reconcile writes only what changed.
	AnnotationSpecHash = "mayfly.io/spec-hash"

	// WorkspaceSubPath and ArtifactsSubPath select directories inside the
	// per-session workspace volume.
	WorkspaceSubPath = "workspace"
	ArtifactsSubPath = "artifacts"

	agentUID = 65532
	agentGID = 65532
)

// Egress permits direct network access from a session pod.
type Egress struct {
	CIDR  string
	Ports []int32
}

// SessionSpec contains everything needed to launch one session pod.
type SessionSpec struct {
	Namespace           string
	SessionID           string
	PodName             string
	SecretName          string
	ConfigMapName       string
	Image               string
	ImagePullPolicy     string
	ImagePullSecrets    []string
	ServiceAccount      string
	TTL                 time.Duration
	WorkspaceLimit      string
	ArtifactLimit       string
	PersistentWorkspace bool
	PVCName             string
	StorageClass        string
	Resources           ResourceConfig
	Env                 map[string]string
	Labels              map[string]string
	EnableNetworkPolicy bool
	DNSNamespace        string
	PlatformNs          string
	PlatformLabels      map[string]string
	PlatformPort        int32
	Egress              []Egress
	Command             []string
	Args                []string
	SessionJSON         []byte
	Token               string
	// Plugins are the session's plugins. This package renders the inline and
	// sidecar placements; a service plugin needs no pod objects.
	Plugins []sessioncfg.Plugin
	// ToolsImage is runtime.image. It supplies the shim and never follows the
	// per-agent image override, so every pod that stages plugins has one.
	ToolsImage string
	// PluginVolumeLimit bounds the copied files of all inline plugins.
	PluginVolumeLimit string
	// GatewayKey signs the call tokens of this session's sidecars. It is the
	// session Secret's gateway-key entry and reaches only a sidecar container.
	GatewayKey []byte
	// NetworkPolicyName is the policy's own name. It must not follow the pod
	// name: a replacement pod changes the name, while the policy stays.
	NetworkPolicyName string
}

// ResourceConfig holds resource quantities.
type ResourceConfig struct {
	Requests map[string]string
	Limits   map[string]string
}

func (s *SessionSpec) labels() map[string]string {
	out := map[string]string{
		LabelManaged:                   "true",
		LabelSession:                   s.SessionID,
		LabelApp:                       AppName,
		"app.kubernetes.io/managed-by": "mayfly",
	}
	for k, v := range s.Labels {
		out[k] = v
	}
	return out
}

// Validate checks the spec before any object is created.
func (s *SessionSpec) Validate() error {
	if s.Namespace == "" || s.SessionID == "" || s.PodName == "" {
		return fmt.Errorf("namespace, session id, and pod name are required")
	}
	if s.SecretName == "" || s.ConfigMapName == "" {
		return fmt.Errorf("secret and configmap names are required")
	}
	if s.Image == "" {
		return fmt.Errorf("image is required")
	}
	if len(s.SessionJSON) == 0 {
		return fmt.Errorf("session config is required")
	}
	if s.PlatformNs == "" {
		return fmt.Errorf("platform namespace is required")
	}
	if s.PlatformPort == 0 {
		s.PlatformPort = 8080
	}
	if s.WorkspaceLimit == "" {
		s.WorkspaceLimit = "1Gi"
	}
	if s.ArtifactLimit == "" {
		s.ArtifactLimit = "1Gi"
	}
	if s.PersistentWorkspace && s.PVCName == "" {
		s.PVCName = "mayfly-" + s.SessionID + "-workspace"
	}
	s.NetworkPolicyName = s.policyName()
	for _, q := range append(append([]string{}, values(s.Resources.Requests)...), values(s.Resources.Limits)...) {
		if _, err := resource.ParseQuantity(q); err != nil {
			return fmt.Errorf("invalid resource quantity %q: %w", q, err)
		}
	}
	if _, err := resource.ParseQuantity(s.WorkspaceLimit); err != nil {
		return fmt.Errorf("invalid workspace limit: %w", err)
	}
	if _, err := resource.ParseQuantity(s.ArtifactLimit); err != nil {
		return fmt.Errorf("invalid artifact limit: %w", err)
	}
	if len(s.inlinePlugins()) > 0 || len(s.sidecarPlugins()) > 0 {
		if s.ToolsImage == "" {
			return fmt.Errorf("tools image is required with plugin containers")
		}
	}
	if len(s.inlinePlugins()) > 0 {
		if _, err := resource.ParseQuantity(s.PluginVolumeLimit); err != nil {
			return fmt.Errorf("invalid plugin volume limit: %w", err)
		}
	}
	if len(s.sidecarPlugins()) > 0 {
		if len(s.GatewayKey) == 0 {
			return fmt.Errorf("gateway key is required with sidecar plugins")
		}
		for _, p := range s.sidecarPlugins() {
			if err := validateSidecarPlugin(p); err != nil {
				return fmt.Errorf("plugin %q: %w", p.Name, err)
			}
		}
	}
	return nil
}

// validateSidecarPlugin checks what the pod needs from one sidecar entry.
// sessioncfg validates the placement itself; these are the rendering
// invariants.
func validateSidecarPlugin(p sessioncfg.Plugin) error {
	if p.Socket == "" {
		return fmt.Errorf("socket is required")
	}
	if p.Command == "" {
		return fmt.Errorf("command is required")
	}
	if p.Sidecar != nil {
		if p.Sidecar.RunAsUser == 0 {
			return fmt.Errorf("run_as_user must not be 0")
		}
		for _, q := range append(append([]string{}, values(p.Sidecar.Requests)...), values(p.Sidecar.Limits)...) {
			if _, err := resource.ParseQuantity(q); err != nil {
				return fmt.Errorf("invalid resource quantity %q: %w", q, err)
			}
		}
	}
	return nil
}

// inlinePlugins returns the plugins that place files in the pod.
func (s *SessionSpec) inlinePlugins() []sessioncfg.Plugin {
	var out []sessioncfg.Plugin
	for _, p := range s.Plugins {
		if p.Placement == "inline" {
			out = append(out, p)
		}
	}
	return out
}

// sidecarPlugins returns the plugins that run in their own container.
func (s *SessionSpec) sidecarPlugins() []sessioncfg.Plugin {
	var out []sessioncfg.Plugin
	for _, p := range s.Plugins {
		if p.Placement == "sidecar" {
			out = append(out, p)
		}
	}
	return out
}

// pluginSecretVolumeName is the volume that holds one inline plugin's secret
// files. Volume names are DNS labels while manifest names allow dots and
// underscores, so the volume name maps them.
func pluginSecretVolumeName(name string) string {
	return dnsName("plugin-secrets-", name)
}

// dnsName maps a name onto a DNS label. A name that does not fit keeps a hash
// suffix, so two long names cannot truncate to the same label.
func dnsName(prefix, name string) string {
	label := prefix + strings.NewReplacer(".", "-", "_", "-").Replace(strings.ToLower(name))
	if len(label) <= 63 {
		return label
	}
	sum := sha256.Sum256([]byte(label))
	suffix := hex.EncodeToString(sum[:])[:8]
	keep := 63 - len(suffix) - 1
	return strings.TrimRight(label[:keep], "-") + "-" + suffix
}

func values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// policyName is the network policy's own name. It must not follow the pod
// name: a replacement pod changes the pod name while the policy stays.
func (s *SessionSpec) policyName() string {
	if s.NetworkPolicyName != "" {
		return s.NetworkPolicyName
	}
	return "mayfly-" + s.SessionID + "-netpol"
}

// Secret returns the per-session credentials: the bearer token every internal
// route authenticates with, and the key that signs sidecar call tokens. The
// key is raw bytes, so both entries go in Data rather than StringData.
func (s *SessionSpec) Secret() *corev1.Secret {
	data := map[string][]byte{"token": []byte(s.Token)}
	if len(s.GatewayKey) > 0 {
		data[GatewayKeySecretKey] = append([]byte(nil), s.GatewayKey...)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.SecretName,
			Namespace: s.Namespace,
			Labels:    s.labels(),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// ConfigMap returns the per-session configuration document.
func (s *SessionSpec) ConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.ConfigMapName,
			Namespace: s.Namespace,
			Labels:    s.labels(),
		},
		Data: map[string]string{"session.json": string(s.SessionJSON)},
	}
}

// Pod returns the hardened session pod.
func (s *SessionSpec) Pod() *corev1.Pod {
	no := false
	yes := true
	seccomp := corev1.SeccompProfileTypeRuntimeDefault
	deadline := int64(s.TTL.Seconds()) + 300

	env := []corev1.EnvVar{
		{Name: sessioncfg.EnvConfig, Value: ConfigFile},
		{Name: "HOME", Value: "/tmp"},
		{Name: "XDG_CACHE_HOME", Value: "/tmp/cache"},
		{Name: "NPM_CONFIG_CACHE", Value: "/tmp/npm"},
		{
			Name: sessioncfg.EnvToken,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.SecretName},
					Key:                  "token",
				},
			},
		},
	}
	for k, v := range s.Env {
		if sessioncfg.IsReservedEnv(k) {
			// A repeated name would let the later value win.
			continue
		}
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.PodName,
			Namespace: s.Namespace,
			Labels:    s.labels(),
			Annotations: map[string]string{
				LabelSession: s.SessionID,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  &no,
			EnableServiceLinks:            &no,
			ServiceAccountName:            s.ServiceAccount,
			ImagePullSecrets:              imagePullSecrets(s.ImagePullSecrets),
			TerminationGracePeriodSeconds: new(int64(20)),
			ActiveDeadlineSeconds:         &deadline,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				RunAsUser:      new(int64(agentUID)),
				RunAsGroup:     new(int64(agentGID)),
				FSGroup:        new(int64(agentGID)),
				SeccompProfile: &corev1.SeccompProfile{Type: seccomp},
			},
			InitContainers: s.initContainers(),
			Containers: []corev1.Container{{
				Name:            "agent",
				Image:           s.Image,
				ImagePullPolicy: corev1.PullPolicy(s.ImagePullPolicy),
				Command:         s.Command,
				Args:            s.Args,
				Env:             env,
				SecurityContext: s.containerSecurityContext(),
				Resources: corev1.ResourceRequirements{
					Requests: quantities(s.Resources.Requests),
					Limits:   quantities(s.Resources.Limits),
				},
				VolumeMounts: s.volumeMounts(),
			}},
			Volumes: s.volumes(),
		},
	}
}

// containerSecurityContext is the hardened identity of every container in the
// pod, including the copy init containers.
func (s *SessionSpec) containerSecurityContext() *corev1.SecurityContext {
	no := false
	yes := true
	seccomp := corev1.SeccompProfileTypeRuntimeDefault
	rootfs := true
	return &corev1.SecurityContext{
		RunAsNonRoot:             &yes,
		RunAsUser:                new(int64(agentUID)),
		RunAsGroup:               new(int64(agentGID)),
		AllowPrivilegeEscalation: &no,
		ReadOnlyRootFilesystem:   &rootfs,
		Privileged:               &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: seccomp},
	}
}

// initContainers copies the shim and the inline plugin trees into the pod,
// then starts one native sidecar per sidecar plugin. A pod without plugins has
// no init containers, so it renders exactly as it did before plugins existed.
func (s *SessionSpec) initContainers() []corev1.Container {
	inline := s.inlinePlugins()
	sidecars := s.sidecarPlugins()
	if len(inline) == 0 && len(sidecars) == 0 {
		return nil
	}
	containers := []corev1.Container{s.toolsContainer()}
	for _, p := range inline {
		containers = append(containers, s.pluginCopyContainer(p))
	}
	// A sidecar is an init container with restartPolicy Always: the kubelet
	// starts it after the copies finish, waits for its startup probe, and keeps
	// it running for the pod's lifetime. Starting the copies first keeps a slow
	// plugin image from delaying the pod.
	for _, p := range sidecars {
		containers = append(containers, s.sidecarContainer(p))
	}
	return containers
}

// toolsContainer copies the shim out of the tools image, so the plugin copy
// containers and the sidecars can run it whatever their own image holds.
func (s *SessionSpec) toolsContainer() corev1.Container {
	return corev1.Container{
		Name:            "mayfly-tools",
		Image:           s.ToolsImage,
		ImagePullPolicy: corev1.PullPolicy(s.ImagePullPolicy),
		Command:         []string{PluginShim, "install-self", ToolsDir + "/mayfly-plugin-shim"},
		SecurityContext: s.containerSecurityContext(),
		Resources: corev1.ResourceRequirements{
			Requests: copyRequests(),
			Limits:   copyLimits(),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "mayfly-tools", MountPath: ToolsDir},
		},
	}
}

// pluginCopyContainer copies one inline plugin's tree into the plugins
// volume. The plugin image's entrypoint never runs, so an image with only
// /mayfly-plugin works even when its base is scratch.
func (s *SessionSpec) pluginCopyContainer(p sessioncfg.Plugin) corev1.Container {
	return corev1.Container{
		Name:            plugin.ContainerName(p.Name),
		Image:           p.Image,
		ImagePullPolicy: corev1.PullPolicy(s.ImagePullPolicy),
		Command: []string{
			ToolsDir + "/mayfly-plugin-shim",
			"copy", PluginImageDir, PluginsDir + "/" + p.Name,
		},
		SecurityContext: s.containerSecurityContext(),
		Resources: corev1.ResourceRequirements{
			Requests: copyRequests(),
			Limits:   copyLimits(),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "mayfly-tools", MountPath: ToolsDir, ReadOnly: true},
			{Name: "plugins", MountPath: PluginsDir},
		},
	}
}

// sidecarContainer runs one sidecar plugin behind the shim on the session's
// socket volume. The shim owns the plugin process, verifies the gateway's call
// tokens, and answers the startup probe once the plugin lists its tools.
func (s *SessionSpec) sidecarContainer(p sessioncfg.Plugin) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	env := []corev1.EnvVar{
		{Name: "MAYFLY_PLUGIN_NAME", Value: p.Name},
		{Name: "MAYFLY_PLUGIN_DIR", Value: PluginImageDir},
		{Name: "MAYFLY_SESSION_ID", Value: s.SessionID},
	}
	mounts := []corev1.VolumeMount{
		{Name: "mayfly-tools", MountPath: ToolsDir, ReadOnly: true},
		{Name: "plugin-sockets", MountPath: PluginSocketsDir},
		{Name: "gateway-key", MountPath: GatewayKeyDir, ReadOnly: true},
		{Name: sidecarTmpVolumeName(p.Name), MountPath: "/tmp"},
	}
	workspace := ""
	if p.Sidecar != nil {
		workspace = p.Sidecar.Workspace
	}
	if workspace == "read" || workspace == "write" {
		env = append(env, corev1.EnvVar{Name: "MAYFLY_WORKSPACE_DIR", Value: WorkspaceDir})
		mount := corev1.VolumeMount{Name: "workspace", MountPath: WorkspaceDir, ReadOnly: workspace == "read"}
		if s.PersistentWorkspace {
			mount.SubPath = WorkspaceSubPath
		}
		mounts = append(mounts, mount)
	}
	for _, name := range sortedKeys(p.Env) {
		env = append(env, corev1.EnvVar{Name: name, Value: p.Env[name]})
	}
	for _, secret := range p.Secrets {
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

	command := []string{
		ToolsDir + "/mayfly-plugin-shim", "serve",
		"--listen", "unix:" + p.Socket,
		"--key-file", GatewayKeyDir + "/" + GatewayKeySecretKey,
		"--session", s.SessionID,
		"--init-timeout", sidecarInitTimeout(p).String(),
		"--", p.Command,
	}
	command = append(command, p.Args...)

	var requests, limits corev1.ResourceList
	if p.Sidecar != nil {
		requests = quantities(p.Sidecar.Requests)
		limits = quantities(p.Sidecar.Limits)
	}
	return corev1.Container{
		Name:            plugin.ContainerName(p.Name),
		Image:           p.Image,
		ImagePullPolicy: corev1.PullPolicy(s.ImagePullPolicy),
		Command:         command,
		// A plugin image keeps its tree in /mayfly-plugin, so a relative
		// command resolves there whatever the image's WORKDIR is.
		WorkingDir:      PluginImageDir,
		RestartPolicy:   &always,
		Env:             env,
		SecurityContext: s.sidecarSecurityContext(p),
		Resources: corev1.ResourceRequirements{
			Requests: requests,
			Limits:   limits,
		},
		VolumeMounts: mounts,
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{
					ToolsDir + "/mayfly-plugin-shim", "probe", "--listen", "unix:" + p.Socket,
				}},
			},
			PeriodSeconds:  2,
			TimeoutSeconds: 5,
			// The shim's own budget, plus thirty seconds of slack: a slow but
			// working plugin must not be restarted by the kubelet.
			FailureThreshold: int32(sidecarInitTimeout(p)/(2*time.Second)) + 15,
		},
	}
}

// sidecarInitTimeout is the budget the shim gives the plugin to start and list
// its tools. It mirrors the manifest's initTimeout, which the shim cannot read.
func sidecarInitTimeout(p sessioncfg.Plugin) time.Duration {
	timeout := time.Duration(p.InitTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return timeout
}

// sidecarSecurityContext is the hardened pod identity with the plugin's own
// non-root UID, so two plugins never share a user.
func (s *SessionSpec) sidecarSecurityContext(p sessioncfg.Plugin) *corev1.SecurityContext {
	sc := s.containerSecurityContext()
	if p.Sidecar != nil {
		sc.RunAsUser = new(p.Sidecar.RunAsUser)
	}
	return sc
}

// sidecarTmpVolumeName is one sidecar's private scratch directory. Each plugin
// gets its own, so one plugin cannot read another's temporary files.
func sidecarTmpVolumeName(name string) string {
	return dnsName("plugin-tmp-", name)
}

// sortedKeys returns a map's keys in order, so a rendered pod is stable.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// copyRequests and copyLimits size a copy container; the work is a file copy
// under a 1 GiB backstop, so the pod budget stays small.
func copyRequests() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("10m"),
		corev1.ResourceMemory: resource.MustParse("32Mi"),
	}
}

func copyLimits() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
}

func (s *SessionSpec) volumeMounts() []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/etc/mayfly", ReadOnly: true},
	}
	if s.PersistentWorkspace {
		mounts = append(mounts,
			corev1.VolumeMount{Name: "workspace", MountPath: WorkspaceDir, SubPath: WorkspaceSubPath},
			corev1.VolumeMount{Name: "workspace", MountPath: ArtifactsDir, SubPath: ArtifactsSubPath},
		)
	} else {
		mounts = append(mounts,
			corev1.VolumeMount{Name: "workspace", MountPath: WorkspaceDir},
			corev1.VolumeMount{Name: "artifacts", MountPath: ArtifactsDir},
		)
	}
	mounts = append(mounts, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"})
	if sidecars := s.sidecarPlugins(); len(sidecars) > 0 {
		// The runtime dials each socket. The volume carries no secret: a call
		// needs a token the gateway signs, which this container never holds.
		mounts = append(mounts, corev1.VolumeMount{Name: "plugin-sockets", MountPath: PluginSocketsDir})
	}
	if inline := s.inlinePlugins(); len(inline) > 0 {
		mounts = append(mounts, corev1.VolumeMount{Name: "plugins", MountPath: PluginsDir, ReadOnly: true})
		for _, p := range inline {
			if len(p.Secrets) == 0 {
				continue
			}
			mounts = append(mounts, corev1.VolumeMount{
				Name:      pluginSecretVolumeName(p.Name),
				MountPath: PluginSecretsDir + "/" + p.Name,
				ReadOnly:  true,
			})
		}
	}
	return mounts
}

func (s *SessionSpec) volumes() []corev1.Volume {
	volumes := []corev1.Volume{{
		Name: "config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: s.ConfigMapName},
			},
		},
	}}
	if s.PersistentWorkspace {
		volumes = append(volumes, corev1.Volume{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: s.PVCName},
			},
		})
	} else {
		volumes = append(volumes,
			corev1.Volume{
				Name: "workspace",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr(s.WorkspaceLimit)},
				},
			},
			corev1.Volume{
				Name: "artifacts",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr(s.ArtifactLimit)},
				},
			},
		)
	}
	volumes = append(volumes, corev1.Volume{
		Name: "tmp",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: quantityPtr("256Mi"),
			},
		},
	})
	inline := s.inlinePlugins()
	sidecars := s.sidecarPlugins()
	if len(inline) > 0 || len(sidecars) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: "mayfly-tools",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr("64Mi")},
			},
		})
	}
	if len(inline) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: "plugins",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr(s.PluginVolumeLimit)},
			},
		})
		for _, p := range inline {
			if len(p.Secrets) == 0 {
				continue
			}
			volumes = append(volumes, s.pluginSecretVolume(p))
		}
	}
	if len(sidecars) > 0 {
		volumes = append(volumes,
			corev1.Volume{
				Name: "plugin-sockets",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						Medium:    corev1.StorageMediumMemory,
						SizeLimit: quantityPtr("1Mi"),
					},
				},
			},
			s.gatewayKeyVolume(),
		)
		for _, p := range sidecars {
			volumes = append(volumes, corev1.Volume{
				Name: sidecarTmpVolumeName(p.Name),
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						Medium:    corev1.StorageMediumMemory,
						SizeLimit: quantityPtr("64Mi"),
					},
				},
			})
		}
	}
	return volumes
}

// gatewayKeyVolume projects only the session secret's gateway-key entry, so a
// sidecar cannot read the session token that sits in the same Secret.
func (s *SessionSpec) gatewayKeyVolume() corev1.Volume {
	mode := int32(0o400)
	return corev1.Volume{
		Name: "gateway-key",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  s.SecretName,
				DefaultMode: &mode,
				Items: []corev1.KeyToPath{{
					Key:  GatewayKeySecretKey,
					Path: GatewayKeySecretKey,
					Mode: &mode,
				}},
			},
		},
	}
}

// pluginSecretVolume projects one inline plugin's secrets as files. The
// runtime reads them and passes them only to that plugin's process, so the
// agent container mounts the volume but no other container does.
func (s *SessionSpec) pluginSecretVolume(p sessioncfg.Plugin) corev1.Volume {
	mode := int32(0o400)
	var sources []corev1.VolumeProjection
	index := map[string]int{}
	for _, secret := range p.Secrets {
		i, ok := index[secret.Secret]
		if !ok {
			i = len(sources)
			index[secret.Secret] = i
			sources = append(sources, corev1.VolumeProjection{
				Secret: &corev1.SecretProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: secret.Secret},
				},
			})
		}
		sources[i].Secret.Items = append(sources[i].Secret.Items, corev1.KeyToPath{
			Key:  secret.Key,
			Path: secret.Env,
			Mode: &mode,
		})
	}
	return corev1.Volume{
		Name: pluginSecretVolumeName(p.Name),
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources:     sources,
				DefaultMode: &mode,
			},
		},
	}
}

// PersistentVolumeClaim returns the workspace claim shared by a session and
// all of its replacement containers. The claim holds the agent workspace and
// the bundle staging directory, so it is sized for both limits.
func (s *SessionSpec) PersistentVolumeClaim() *corev1.PersistentVolumeClaim {
	storage := resource.MustParse(s.WorkspaceLimit)
	storage.Add(resource.MustParse(s.ArtifactLimit))
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.PVCName,
			Namespace: s.Namespace,
			Labels:    s.labels(),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storage,
				},
			},
		},
	}
	if s.StorageClass != "" {
		claim.Spec.StorageClassName = &s.StorageClass
	}
	return claim
}

// NetworkPolicy denies all ingress and limits egress to the control plane,
// DNS, and explicitly configured CIDRs.
func (s *SessionSpec) NetworkPolicy() *networkingv1.NetworkPolicy {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	port53 := intstr.FromInt32(53)
	portHTTP := intstr.FromInt32(s.PlatformPort)

	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": s.PlatformNs,
				}},
				PodSelector: &metav1.LabelSelector{MatchLabels: s.PlatformLabels},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &portHTTP}},
		},
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": s.DNSNamespace,
				}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: &port53},
				{Protocol: &tcp, Port: &port53},
			},
		},
	}
	for _, rule := range s.Egress {
		if strings.TrimSpace(rule.CIDR) == "" {
			continue
		}
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(rule.Ports))
		for _, p := range rule.Ports {
			proto := tcp
			port := intstr.FromInt32(p)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &port})
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: rule.CIDR}}},
			Ports: ports,
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.policyName(),
			Namespace: s.Namespace,
			Labels:    s.labels(),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelSession: s.SessionID}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress:      egress,
		},
	}
}

func imagePullSecrets(names []string) []corev1.LocalObjectReference {
	out := make([]corev1.LocalObjectReference, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) != "" {
			out = append(out, corev1.LocalObjectReference{Name: name})
		}
	}
	return out
}

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

func quantityPtr(value string) *resource.Quantity {
	q := resource.MustParse(value)
	return &q
}
