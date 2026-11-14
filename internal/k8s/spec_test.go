package k8s

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

func testSpec() *SessionSpec {
	return &SessionSpec{
		Namespace:           "mayfly",
		SessionID:           "abc123",
		PodName:             "mayfly-abc123",
		SecretName:          "mayfly-abc123",
		ConfigMapName:       "mayfly-abc123",
		Image:               "mayfly-agent:test",
		ImagePullPolicy:     "IfNotPresent",
		ServiceAccount:      "mayfly-agent",
		TTL:                 time.Hour,
		WorkspaceLimit:      "128Mi",
		ArtifactLimit:       "256Mi",
		PlatformNs:          "mayfly",
		PlatformLabels:      map[string]string{"app.kubernetes.io/name": "mayflyd"},
		PlatformPort:        8080,
		EnableNetworkPolicy: true,
		DNSNamespace:        "kube-system",
		SessionJSON:         []byte(`{"session_id":"abc123"}`),
		Token:               "secret-token",
		Command:             []string{"/usr/local/bin/agent"},
		Args:                []string{"--mode", "rpc"},
		ImagePullSecrets:    []string{"registry-credentials"},
		Env:                 map[string]string{"AGENT_MODE": "rpc"},
		Resources: ResourceConfig{
			Requests: map[string]string{"cpu": "100m"},
			Limits:   map[string]string{"memory": "1Gi"},
		},
	}
}

func TestSessionSpecValidate(t *testing.T) {
	if err := testSpec().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	bad := testSpec()
	bad.SessionJSON = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("expected missing session config error")
	}
}

func TestPodSecurityDefaults(t *testing.T) {
	spec := testSpec()
	pod := spec.Pod()
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("service account token must not be mounted")
	}
	if pod.Spec.SecurityContext == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("pod must run as non-root")
	}
	if pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != 65532 {
		t.Fatal("unexpected uid")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("seccomp profile must be RuntimeDefault")
	}
	container := pod.Spec.Containers[0]
	sc := container.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation must be disabled")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatal("root filesystem must be read-only")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatal("all capabilities must be dropped")
	}
	if len(sc.Capabilities.Add) != 0 {
		t.Fatal("no capabilities may be added")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("sessions must not restart")
	}
	if len(container.Command) != 1 || container.Command[0] != "/usr/local/bin/agent" || len(container.Args) != 2 {
		t.Fatalf("command = %v args=%v", container.Command, container.Args)
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "registry-credentials" {
		t.Fatalf("image pull secrets = %v", pod.Spec.ImagePullSecrets)
	}
	envFound := false
	for _, item := range container.Env {
		if item.Name == "AGENT_MODE" && item.Value == "rpc" {
			envFound = true
		}
	}
	if !envFound {
		t.Fatal("agent runtime env missing")
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != 3900 {
		t.Fatalf("active deadline = %v", pod.Spec.ActiveDeadlineSeconds)
	}
	mounts := map[string]bool{}
	for _, m := range container.VolumeMounts {
		mounts[m.Name] = m.ReadOnly
	}
	if !mounts["config"] {
		t.Fatal("config mount must be read-only")
	}
	if _, ok := mounts["workspace"]; !ok {
		t.Fatalf("workspace mount missing: %+v", container.VolumeMounts)
	}
	if _, ok := mounts["artifacts"]; !ok {
		t.Fatalf("artifacts mount missing: %+v", container.VolumeMounts)
	}
	if _, ok := mounts["tmp"]; !ok {
		t.Fatalf("tmp mount missing: %+v", container.VolumeMounts)
	}
	if got := pod.Labels[LabelSession]; got != "abc123" {
		t.Fatalf("session label = %q", got)
	}
}

func TestNetworkPolicyRestrictsEgress(t *testing.T) {
	spec := testSpec()
	spec.Egress = []Egress{{CIDR: "203.0.113.0/24", Ports: []int32{443}}}
	np := spec.NetworkPolicy()
	if len(np.Spec.Ingress) != 0 {
		t.Fatal("ingress must be denied")
	}
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("expected platform, DNS, and one extra egress rule, got %d", len(np.Spec.Egress))
	}
	if np.Spec.PodSelector.MatchLabels[LabelSession] != "abc123" {
		t.Fatal("policy selector does not target the session pod")
	}
	platform := np.Spec.Egress[0]
	if platform.Ports[0].Port.IntVal != 8080 {
		t.Fatal("platform port missing")
	}
	dns := np.Spec.Egress[1]
	if len(dns.Ports) != 2 {
		t.Fatal("DNS must allow UDP and TCP")
	}
}

func TestSecretAndConfigMap(t *testing.T) {
	spec := testSpec()
	secret := spec.Secret()
	if string(secret.Data["token"]) != "secret-token" {
		t.Fatal("session token missing from secret")
	}
	if _, ok := secret.Data[GatewayKeySecretKey]; ok {
		t.Fatal("a session without sidecars must not carry a gateway key")
	}
	spec.GatewayKey = []byte{0xff, 0x00, 0x01}
	if got := spec.Secret().Data[GatewayKeySecretKey]; !bytes.Equal(got, []byte{0xff, 0x00, 0x01}) {
		t.Fatalf("gateway key = %x", got)
	}
	if spec.ConfigMap().Data["session.json"] != `{"session_id":"abc123"}` {
		t.Fatal("session config missing from configmap")
	}
}

func TestPodIgnoresReservedEnv(t *testing.T) {
	spec := testSpec()
	spec.Env = map[string]string{
		"MAYFLY_SESSION_TOKEN": "attacker-token",
		"MAYFLY_CONFIG":        "/tmp/other.json",
		"AGENT_MODE":           "rpc",
	}
	pod := spec.Pod()
	env := map[string]corev1.EnvVar{}
	for _, item := range pod.Spec.Containers[0].Env {
		if _, exists := env[item.Name]; exists {
			t.Fatalf("duplicate env %s", item.Name)
		}
		env[item.Name] = item
	}
	if token, ok := env["MAYFLY_SESSION_TOKEN"]; !ok || token.Value != "" || token.ValueFrom == nil {
		t.Fatalf("session token must come from the secret: %+v", token)
	}
	if cfg, ok := env["MAYFLY_CONFIG"]; !ok || cfg.Value != ConfigFile {
		t.Fatalf("config path was overridden: %+v", cfg)
	}
	if env["AGENT_MODE"].Value != "rpc" {
		t.Fatal("literal env was dropped")
	}
}

func TestPersistentWorkspaceUsesOnePVC(t *testing.T) {
	spec := testSpec()
	spec.PersistentWorkspace = true
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if spec.PVCName != "mayfly-abc123-workspace" {
		t.Fatalf("PVC name = %q", spec.PVCName)
	}
	pvc := spec.PersistentVolumeClaim()
	if pvc.Name != spec.PVCName || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("PVC = %+v", pvc)
	}
	// The claim holds the workspace and the bundle staging directory.
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "384Mi" {
		t.Fatalf("PVC size = %s", got.String())
	}
	if pvc.Spec.StorageClassName != nil {
		t.Fatalf("unconfigured storage class = %q", *pvc.Spec.StorageClassName)
	}
	spec.StorageClass = "fast"
	if got := spec.PersistentVolumeClaim().Spec.StorageClassName; got == nil || *got != "fast" {
		t.Fatalf("storage class = %v", got)
	}
	if got := spec.NetworkPolicy().Name; got != "mayfly-abc123-netpol" {
		t.Fatalf("network policy name = %q", got)
	}
	pod := spec.Pod()
	var mounts []corev1.VolumeMount
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "workspace" {
			mounts = append(mounts, m)
		}
	}
	if len(mounts) != 2 {
		t.Fatalf("workspace mounts = %+v", mounts)
	}
	if mounts[0].MountPath != WorkspaceDir || mounts[0].SubPath != WorkspaceSubPath {
		t.Fatalf("workspace mount = %+v", mounts[0])
	}
	if mounts[1].MountPath != ArtifactsDir || mounts[1].SubPath != ArtifactsSubPath {
		t.Fatalf("artifacts mount = %+v", mounts[1])
	}
	var claim string
	for _, v := range pod.Spec.Volumes {
		if v.Name == "workspace" {
			if v.PersistentVolumeClaim == nil {
				t.Fatal("workspace volume is not a PVC")
			}
			claim = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claim != spec.PVCName {
		t.Fatalf("workspace claim = %q", claim)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("replacement is handled by the control plane, not the kubelet")
	}
}

func TestSessionSpecValidatePlugins(t *testing.T) {
	spec := testSpec()
	spec.Plugins = []sessioncfg.Plugin{{Name: "tickets", Placement: "inline"}}
	if err := spec.Validate(); err == nil {
		t.Fatal("expected a missing tools image error")
	}
	spec.ToolsImage = "mayfly-agent:tools"
	if err := spec.Validate(); err == nil {
		t.Fatal("expected a missing plugin volume limit error")
	}
	spec.PluginVolumeLimit = "lots"
	if err := spec.Validate(); err == nil {
		t.Fatal("expected an invalid plugin volume limit error")
	}
	spec.PluginVolumeLimit = "512Mi"
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// A sidecar needs the shim and a key to sign its call tokens, but no
	// plugin volume: nothing is copied into the pod.
	sidecarOnly := testSpec()
	sidecarOnly.Plugins = []sessioncfg.Plugin{sidecarPlugin("notify", "example.com/notify:1")}
	if err := sidecarOnly.Validate(); err == nil {
		t.Fatal("expected a missing tools image error")
	}
	sidecarOnly.ToolsImage = "mayfly-agent:tools"
	if err := sidecarOnly.Validate(); err == nil {
		t.Fatal("expected a missing gateway key error")
	}
	sidecarOnly.GatewayKey = make([]byte, 32)
	if err := sidecarOnly.Validate(); err == nil {
		t.Fatal("expected a missing sidecar signing key error")
	}
	withSidecarKeys(sidecarOnly)
	if err := sidecarOnly.Validate(); err != nil {
		t.Fatalf("Validate sidecar: %v", err)
	}
	for _, v := range sidecarOnly.Pod().Spec.Volumes {
		if v.Name == "plugins" {
			t.Fatal("a sidecar-only pod must not stage a plugin volume")
		}
	}

	// A service plugin needs no pod objects, so it needs neither setting.
	serviceOnly := testSpec()
	serviceOnly.Plugins = []sessioncfg.Plugin{{Name: "crm", Placement: "service"}}
	if err := serviceOnly.Validate(); err != nil {
		t.Fatalf("Validate service plugin: %v", err)
	}
}

func TestPodWithoutPluginsUnchanged(t *testing.T) {
	want, err := os.ReadFile("testdata/pod.json")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(testSpec().Pod(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if string(body) != string(want) {
		t.Fatalf("pod without plugins changed:\n%s", body)
	}
}

func TestPodServiceOnlyUnchanged(t *testing.T) {
	spec := testSpec()
	spec.Plugins = []sessioncfg.Plugin{{
		Name:      "crm",
		Placement: "service",
		Tools:     []sessioncfg.PluginTool{{Name: "lookup", TimeoutSeconds: 30}},
	}}
	want, err := os.ReadFile("testdata/pod.json")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(spec.Pod(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if string(body) != string(want) {
		t.Fatalf("service plugins changed the pod:\n%s", body)
	}
}

func inlinePlugin(name, image string) sessioncfg.Plugin {
	return sessioncfg.Plugin{
		Name:               name,
		Placement:          "inline",
		Image:              image,
		Dir:                PluginsDir + "/" + name,
		Command:            "node",
		Workdir:            "plugin",
		InitTimeoutSeconds: 30,
		MaxOutputBytes:     4096,
		Tools:              []sessioncfg.PluginTool{{Name: "run", TimeoutSeconds: 30}},
	}
}

// sidecarPlugin is one sidecar manifest as the runtime resolves it: a socket,
// the shim's command, and the container's size and identity.
func sidecarPlugin(name, image string) sessioncfg.Plugin {
	return sessioncfg.Plugin{
		Name:               name,
		Placement:          "sidecar",
		Image:              image,
		Command:            "bin/" + name,
		Args:               []string{"--log-level", "info"},
		Socket:             PluginSocketPath(name),
		Env:                map[string]string{"NOTIFY_CHANNEL": "#ops", "NOTIFY_LEVEL": "info"},
		Secrets:            []sessioncfg.PluginSecret{{Env: "NOTIFY_WEBHOOK_URL", Secret: "mayfly-plugin-notify", Key: "webhook-url"}},
		InitTimeoutSeconds: 30,
		MaxOutputBytes:     4096,
		Sidecar: &sessioncfg.PluginSidecar{
			Requests:  map[string]string{"cpu": "10m", "memory": "32Mi"},
			Limits:    map[string]string{"memory": "64Mi"},
			RunAsUser: 65532,
			Workspace: "read",
		},
		Tools: []sessioncfg.PluginTool{{Name: "post", TimeoutSeconds: 30}},
	}
}

// sidecarSpec is a session with the shim available and a gateway key, which a
// pod with sidecars needs.
func sidecarSpec() *SessionSpec {
	spec := testSpec()
	spec.ToolsImage = "mayfly-agent:tools"
	spec.GatewayKey = make([]byte, 32)
	return spec
}

// withSidecarKeys derives the per-plugin key of every sidecar in the spec, as
// the session manager does before it renders the pod.
func withSidecarKeys(spec *SessionSpec) *SessionSpec {
	spec.SidecarKeys = map[string][]byte{}
	for _, p := range spec.Plugins {
		if p.Placement == "sidecar" {
			spec.SidecarKeys[p.Name] = plugin.SidecarKey(spec.GatewayKey, p.Name)
		}
	}
	return spec
}

func TestPodPluginInitContainers(t *testing.T) {
	spec := testSpec()
	spec.ToolsImage = "mayfly-agent:tools"
	spec.PluginVolumeLimit = "512Mi"
	spec.GatewayKey = make([]byte, 32)
	spec.Plugins = []sessioncfg.Plugin{
		inlinePlugin("tickets", "example.com/tickets:1"),
		inlinePlugin("word-count", "example.com/word-count:1"),
		sidecarPlugin("notify", "example.com/notify:1"),
	}
	withSidecarKeys(spec)
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := spec.Pod()

	init := pod.Spec.InitContainers
	if len(init) != 4 {
		t.Fatalf("init containers = %d, want tools, two inline copies, and one sidecar", len(init))
	}
	wantNames := []string{"mayfly-tools", "plugin-tickets", "plugin-word-count", "plugin-notify"}
	for i, name := range wantNames {
		if init[i].Name != name {
			t.Fatalf("init container %d = %q, want %q", i, init[i].Name, name)
		}
	}

	tools := init[0]
	if tools.Image != spec.ToolsImage {
		t.Fatalf("tools image = %q", tools.Image)
	}
	if !reflect.DeepEqual(tools.Command, []string{PluginShim, "install-self", ToolsDir + "/mayfly-plugin-shim"}) {
		t.Fatalf("tools command = %v", tools.Command)
	}
	if got := tools.VolumeMounts; len(got) != 1 || got[0].Name != "mayfly-tools" || got[0].MountPath != ToolsDir || got[0].ReadOnly {
		t.Fatalf("tools mounts = %+v", got)
	}

	agent := pod.Spec.Containers[0]
	for i, p := range spec.Plugins[:2] {
		container := init[i+1]
		if container.Image != p.Image {
			t.Fatalf("%s image = %q", p.Name, container.Image)
		}
		wantCommand := []string{ToolsDir + "/mayfly-plugin-shim", "copy", PluginImageDir, PluginsDir + "/" + p.Name}
		if !reflect.DeepEqual(container.Command, wantCommand) {
			t.Fatalf("%s command = %v, want %v", p.Name, container.Command, wantCommand)
		}
		if !reflect.DeepEqual(container.SecurityContext, agent.SecurityContext) {
			t.Fatalf("%s security context differs from the agent container's", p.Name)
		}
		if len(container.Env) != 0 || len(container.EnvFrom) != 0 {
			t.Fatalf("%s must not receive environment", p.Name)
		}
		assertQuantity(t, p.Name+" cpu request", container.Resources.Requests, corev1.ResourceCPU, "10m")
		assertQuantity(t, p.Name+" memory request", container.Resources.Requests, corev1.ResourceMemory, "32Mi")
		assertQuantity(t, p.Name+" memory limit", container.Resources.Limits, corev1.ResourceMemory, "128Mi")
		mounts := map[string]corev1.VolumeMount{}
		for _, m := range container.VolumeMounts {
			mounts[m.Name] = m
		}
		if mount, ok := mounts["mayfly-tools"]; !ok || !mount.ReadOnly || mount.MountPath != ToolsDir {
			t.Fatalf("%s tools mount = %+v", p.Name, mount)
		}
		if mount, ok := mounts["plugins"]; !ok || mount.ReadOnly || mount.MountPath != PluginsDir {
			t.Fatalf("%s plugins mount = %+v", p.Name, mount)
		}
	}

	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		volumes[v.Name] = v
	}
	if got := volumes["mayfly-tools"].EmptyDir; got == nil || got.SizeLimit == nil || got.SizeLimit.String() != "64Mi" {
		t.Fatalf("mayfly-tools volume = %+v", volumes["mayfly-tools"])
	}
	if got := volumes["plugins"].EmptyDir; got == nil || got.SizeLimit == nil || got.SizeLimit.String() != "512Mi" {
		t.Fatalf("plugins volume = %+v", volumes["plugins"])
	}

	var pluginsMount *corev1.VolumeMount
	for i := range agent.VolumeMounts {
		if agent.VolumeMounts[i].Name == "plugins" {
			pluginsMount = &agent.VolumeMounts[i]
		}
	}
	if pluginsMount == nil || !pluginsMount.ReadOnly || pluginsMount.MountPath != PluginsDir {
		t.Fatalf("agent plugins mount = %+v", pluginsMount)
	}
}

func TestPodPluginSecretsProjected(t *testing.T) {
	spec := testSpec()
	spec.ToolsImage = "mayfly-agent:tools"
	spec.PluginVolumeLimit = "512Mi"
	secrets := inlinePlugin("word_count", "example.com/word-count:1")
	secrets.Secrets = []sessioncfg.PluginSecret{
		{Env: "WORD_STATS_TOKEN", Secret: "mayfly-plugin-word-stats", Key: "token"},
		{Env: "WORD_STATS_KEY", Secret: "mayfly-plugin-word-stats", Key: "api-key"},
		{Env: "SHARED_TOKEN", Secret: "mayfly-plugin-shared", Key: "token"},
	}
	secrets.SecretsDir = PluginSecretsDir + "/" + secrets.Name
	spec.Plugins = []sessioncfg.Plugin{secrets}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := spec.Pod()

	var volume *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "plugin-secrets-word-count" {
			volume = &pod.Spec.Volumes[i]
		}
	}
	if volume == nil {
		t.Fatalf("secret volume missing: %+v", pod.Spec.Volumes)
	}
	projected := volume.Projected
	if projected == nil {
		t.Fatalf("secret volume is not projected: %+v", volume.VolumeSource)
	}
	if projected.DefaultMode == nil || *projected.DefaultMode != 0o400 {
		t.Fatalf("default mode = %v", projected.DefaultMode)
	}
	if len(projected.Sources) != 2 {
		t.Fatalf("sources = %d, want one per Secret", len(projected.Sources))
	}
	items := map[string]corev1.KeyToPath{}
	for _, source := range projected.Sources {
		if source.Secret == nil {
			t.Fatalf("source is not a secret: %+v", source)
		}
		for _, item := range source.Secret.Items {
			if item.Mode == nil || *item.Mode != 0o400 {
				t.Fatalf("%s mode = %v, want 0400", item.Path, item.Mode)
			}
			items[item.Path] = item
		}
	}
	if got := items["WORD_STATS_TOKEN"]; got.Key != "token" {
		t.Fatalf("WORD_STATS_TOKEN key = %q", got.Key)
	}
	if got := items["WORD_STATS_KEY"]; got.Key != "api-key" {
		t.Fatalf("WORD_STATS_KEY key = %q", got.Key)
	}
	if got := items["SHARED_TOKEN"]; got.Key != "token" {
		t.Fatalf("SHARED_TOKEN key = %q", got.Key)
	}

	var mount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		m := pod.Spec.Containers[0].VolumeMounts[i]
		if m.Name == "plugin-secrets-word-count" {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil || !mount.ReadOnly || mount.MountPath != PluginSecretsDir+"/word_count" {
		t.Fatalf("secret mount = %+v", mount)
	}
	// No other container may read the secret files.
	for _, container := range pod.Spec.InitContainers {
		for _, m := range container.VolumeMounts {
			if m.Name == "plugin-secrets-word-count" {
				t.Fatalf("%s mounts the plugin secret volume", container.Name)
			}
		}
	}
}

// assertQuantity checks one resource quantity without leaning on Quantity's
// pointer methods, which a map index cannot call.
func assertQuantity(t *testing.T, what string, list corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()
	got, ok := list[name]
	if !ok {
		t.Fatalf("%s is unset", what)
	}
	if got.String() != want {
		t.Fatalf("%s = %s, want %s", what, got.String(), want)
	}
}

// TestPodSidecarContainers covers one sidecar plugin's container: a native
// sidecar after mayfly-tools, its shim command, its identity, its resources,
// its secret env, its mounts, and its startup probe.
func TestPodSidecarContainers(t *testing.T) {
	spec := sidecarSpec()
	p := sidecarPlugin("notify", "example.com/notify:1")
	spec.Plugins = []sessioncfg.Plugin{p}
	withSidecarKeys(spec)
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := spec.Pod()

	if len(pod.Spec.InitContainers) != 2 {
		t.Fatalf("init containers = %+v, want tools and the sidecar", pod.Spec.InitContainers)
	}
	if pod.Spec.InitContainers[0].Name != "mayfly-tools" {
		t.Fatalf("first init container = %q", pod.Spec.InitContainers[0].Name)
	}
	sidecar := pod.Spec.InitContainers[1]
	if sidecar.Name != "plugin-notify" {
		t.Fatalf("sidecar name = %q", sidecar.Name)
	}
	if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatalf("sidecar restart policy = %v", sidecar.RestartPolicy)
	}
	if sidecar.Image != p.Image || sidecar.WorkingDir != PluginImageDir {
		t.Fatalf("sidecar image = %q workdir = %q", sidecar.Image, sidecar.WorkingDir)
	}
	wantCommand := []string{
		ToolsDir + "/mayfly-plugin-shim", "serve",
		"--listen", "unix:" + p.Socket,
		"--key-file", GatewayKeyDir + "/" + GatewayKeySecretKey,
		"--session", "abc123",
		"--init-timeout", "30s",
		"--", "bin/notify", "--log-level", "info",
	}
	if !reflect.DeepEqual(sidecar.Command, wantCommand) {
		t.Fatalf("sidecar command = %v, want %v", sidecar.Command, wantCommand)
	}

	env := map[string]corev1.EnvVar{}
	for _, item := range sidecar.Env {
		if _, exists := env[item.Name]; exists {
			t.Fatalf("duplicate env %s", item.Name)
		}
		env[item.Name] = item
	}
	if env["MAYFLY_PLUGIN_NAME"].Value != "notify" || env["MAYFLY_PLUGIN_DIR"].Value != PluginImageDir {
		t.Fatalf("plugin identity env = %+v", sidecar.Env)
	}
	if env["MAYFLY_SESSION_ID"].Value != "abc123" {
		t.Fatalf("session env = %+v", env["MAYFLY_SESSION_ID"])
	}
	if env["MAYFLY_WORKSPACE_DIR"].Value != WorkspaceDir {
		t.Fatalf("workspace env = %+v", env["MAYFLY_WORKSPACE_DIR"])
	}
	if env["NOTIFY_CHANNEL"].Value != "#ops" {
		t.Fatalf("manifest env = %+v", env["NOTIFY_CHANNEL"])
	}
	secret := env["NOTIFY_WEBHOOK_URL"]
	if secret.ValueFrom == nil || secret.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("secret env = %+v", secret)
	}
	if got := secret.ValueFrom.SecretKeyRef; got.Name != "mayfly-plugin-notify" || got.Key != "webhook-url" {
		t.Fatalf("secretKeyRef = %+v", got)
	}

	assertQuantity(t, "sidecar cpu request", sidecar.Resources.Requests, corev1.ResourceCPU, "10m")
	assertQuantity(t, "sidecar memory request", sidecar.Resources.Requests, corev1.ResourceMemory, "32Mi")
	assertQuantity(t, "sidecar memory limit", sidecar.Resources.Limits, corev1.ResourceMemory, "64Mi")

	sc := sidecar.SecurityContext
	agentContext := pod.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 65532 {
		t.Fatalf("sidecar security context = %+v", sc)
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("sidecar security context is not hardened: %+v", sc)
	}
	if agentContext == nil || *agentContext.RunAsUser != 65532 {
		t.Fatalf("agent security context = %+v", agentContext)
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, m := range sidecar.VolumeMounts {
		mounts[m.Name] = m
	}
	if mount, ok := mounts["mayfly-tools"]; !ok || !mount.ReadOnly || mount.MountPath != ToolsDir {
		t.Fatalf("sidecar tools mount = %+v", mount)
	}
	if mount, ok := mounts[sidecarSocketVolumeName("notify")]; !ok || mount.ReadOnly || mount.MountPath != PluginSocketDir("notify") {
		t.Fatalf("sidecar sockets mount = %+v", mount)
	}
	if mount, ok := mounts[gatewayKeyVolumeName("notify")]; !ok || !mount.ReadOnly || mount.MountPath != GatewayKeyDir {
		t.Fatalf("sidecar gateway key mount = %+v", mount)
	}
	if mount, ok := mounts["plugin-tmp-notify"]; !ok || mount.MountPath != "/tmp" {
		t.Fatalf("sidecar tmp mount = %+v", mount)
	}
	if mount, ok := mounts["workspace"]; !ok || !mount.ReadOnly || mount.MountPath != WorkspaceDir {
		t.Fatalf("sidecar workspace mount = %+v", mount)
	}

	probe := sidecar.StartupProbe
	if probe == nil || probe.Exec == nil {
		t.Fatalf("sidecar startup probe = %+v", probe)
	}
	wantProbe := []string{ToolsDir + "/mayfly-plugin-shim", "probe", "--listen", "unix:" + p.Socket}
	if !reflect.DeepEqual(probe.Exec.Command, wantProbe) {
		t.Fatalf("probe command = %v, want %v", probe.Exec.Command, wantProbe)
	}
	// The probe must outlast the shim's init budget (30s here).
	if probe.FailureThreshold < 15 || probe.PeriodSeconds != 2 {
		t.Fatalf("probe limits = %+v", probe)
	}

	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		volumes[v.Name] = v
	}
	sockets := volumes[sidecarSocketVolumeName("notify")].EmptyDir
	if sockets == nil || sockets.Medium != corev1.StorageMediumMemory || sockets.SizeLimit == nil || sockets.SizeLimit.String() != "1Mi" {
		t.Fatalf("sockets volume = %+v", volumes[sidecarSocketVolumeName("notify")])
	}
	keyVolume := volumes[gatewayKeyVolumeName("notify")].Secret
	if keyVolume == nil || keyVolume.SecretName != "mayfly-abc123" {
		t.Fatalf("gateway key volume = %+v", volumes[gatewayKeyVolumeName("notify")])
	}
	wantKey := GatewayKeySecretKeyFor("notify")
	if len(keyVolume.Items) != 1 || keyVolume.Items[0].Key != wantKey || keyVolume.Items[0].Path != GatewayKeySecretKey {
		t.Fatalf("gateway key items = %+v", keyVolume.Items)
	}
	if keyVolume.DefaultMode == nil || *keyVolume.DefaultMode != 0o400 {
		t.Fatalf("gateway key mode = %v", keyVolume.DefaultMode)
	}

	// The runtime dials the socket and needs no key of its own.
	agentMounts := map[string]corev1.VolumeMount{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		agentMounts[m.Name] = m
	}
	if mount, ok := agentMounts[sidecarSocketVolumeName("notify")]; !ok || mount.ReadOnly || mount.MountPath != PluginSocketDir("notify") {
		t.Fatalf("agent sockets mount = %+v", mount)
	}
	for _, container := range append([]corev1.Container{pod.Spec.Containers[0]}, pod.Spec.InitContainers...) {
		if container.Name == sidecar.Name {
			continue
		}
		for _, m := range container.VolumeMounts {
			if strings.HasPrefix(m.Name, "gateway-key-") || m.MountPath == GatewayKeyDir {
				t.Fatalf("%s mounts a gateway key", container.Name)
			}
		}
	}
}

// TestPodGatewayKeyOnlyInSidecars keeps the signing key and the sidecar's
// secret out of the agent container and out of every copy container.
func TestPodGatewayKeyOnlyInSidecars(t *testing.T) {
	spec := sidecarSpec()
	spec.PluginVolumeLimit = "512Mi"
	spec.Plugins = []sessioncfg.Plugin{
		inlinePlugin("tickets", "example.com/tickets:1"),
		sidecarPlugin("notify", "example.com/notify:1"),
	}
	withSidecarKeys(spec)
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := spec.Pod()

	var agent corev1.Container
	for _, c := range pod.Spec.Containers {
		if c.Name == "agent" {
			agent = c
		}
	}
	if agent.Name == "" {
		t.Fatal("agent container missing")
	}
	for _, item := range agent.Env {
		if item.Name == "NOTIFY_WEBHOOK_URL" || item.Name == GatewayKeySecretKey {
			t.Fatalf("agent env carries %s", item.Name)
		}
		if item.ValueFrom != nil && item.ValueFrom.SecretKeyRef != nil && item.ValueFrom.SecretKeyRef.Key == GatewayKeySecretKey {
			t.Fatalf("agent env reads the gateway key")
		}
	}
	if agent.VolumeMounts == nil {
		t.Fatal("agent mounts missing")
	}
	for _, m := range agent.VolumeMounts {
		if strings.HasPrefix(m.Name, "gateway-key-") || m.MountPath == GatewayKeyDir {
			t.Fatalf("agent mounts a sidecar key")
		}
	}

	secret := spec.Secret().Data
	if len(secret[GatewayKeySecretKey]) != 32 {
		t.Fatalf("session secret gateway key = %x", secret[GatewayKeySecretKey])
	}
	if len(secret[GatewayKeySecretKeyFor("notify")]) != 32 {
		t.Fatalf("sidecar secret key = %x", secret[GatewayKeySecretKeyFor("notify")])
	}
	if string(secret["token"]) != "secret-token" {
		t.Fatal("session token missing from secret")
	}

	// Only the sidecar may reference the manifest's secret.
	sawGatewayMount := false
	for _, container := range append([]corev1.Container{agent}, pod.Spec.InitContainers...) {
		for _, item := range container.Env {
			if item.Name != "NOTIFY_WEBHOOK_URL" {
				continue
			}
			if container.Name != "plugin-notify" {
				t.Fatalf("%s reads the sidecar secret", container.Name)
			}
		}
		for _, m := range container.VolumeMounts {
			if strings.HasPrefix(m.Name, "gateway-key-") {
				if container.Name != "plugin-notify" {
					t.Fatalf("%s mounts a sidecar key", container.Name)
				}
				sawGatewayMount = true
			}
		}
	}
	if !sawGatewayMount {
		t.Fatal("the sidecar does not mount the gateway key")
	}
}

// TestSidecarsDoNotShareKeyOrSocket gives each sidecar its own signing key and
// its own socket volume, so one plugin cannot sign a token its sibling accepts
// or replace its sibling's socket.
func TestSidecarsDoNotShareKeyOrSocket(t *testing.T) {
	spec := sidecarSpec()
	spec.Plugins = []sessioncfg.Plugin{
		sidecarPlugin("alpha", "example.com/alpha:1"),
		sidecarPlugin("beta", "example.com/beta:1"),
	}
	withSidecarKeys(spec)
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := spec.Pod()

	secret := spec.Secret().Data
	alphaKey := secret[GatewayKeySecretKeyFor("alpha")]
	betaKey := secret[GatewayKeySecretKeyFor("beta")]
	if len(alphaKey) != 32 || len(betaKey) != 32 {
		t.Fatalf("sidecar keys = %x / %x", alphaKey, betaKey)
	}
	if bytes.Equal(alphaKey, betaKey) {
		t.Fatal("two sidecars share a signing key")
	}
	if !bytes.Equal(alphaKey, plugin.SidecarKey(spec.GatewayKey, "alpha")) {
		t.Fatal("the secret does not hold the derived key")
	}

	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		volumes[v.Name] = v
	}
	if _, shared := volumes["gateway-key"]; shared {
		t.Fatal("a shared gateway key volume remains")
	}
	if _, shared := volumes["plugin-sockets"]; shared {
		t.Fatal("a shared socket volume remains")
	}
	for name, item := range map[string]string{"alpha": GatewayKeySecretKeyFor("alpha"), "beta": GatewayKeySecretKeyFor("beta")} {
		vol := volumes[gatewayKeyVolumeName(name)].Secret
		if vol == nil || len(vol.Items) != 1 || vol.Items[0].Key != item || vol.Items[0].Path != GatewayKeySecretKey {
			t.Fatalf("%s key volume = %+v", name, vol)
		}
	}

	sidecars := map[string]corev1.Container{}
	for _, c := range pod.Spec.InitContainers {
		sidecars[c.Name] = c
	}
	seen := map[string]string{}
	for _, name := range []string{"alpha", "beta"} {
		c, ok := sidecars[plugin.ContainerName(name)]
		if !ok {
			t.Fatalf("sidecar %s missing", name)
		}
		var socket, key *corev1.VolumeMount
		for i := range c.VolumeMounts {
			switch c.VolumeMounts[i].Name {
			case sidecarSocketVolumeName(name):
				socket = &c.VolumeMounts[i]
			case gatewayKeyVolumeName(name):
				key = &c.VolumeMounts[i]
			}
		}
		if socket == nil || socket.MountPath != PluginSocketDir(name) || socket.ReadOnly {
			t.Fatalf("%s socket mount = %+v", name, socket)
		}
		if other, ok := seen[socket.Name]; ok {
			t.Fatalf("%s and %s share socket volume %q", other, name, socket.Name)
		}
		seen[socket.Name] = name
		if key == nil || key.MountPath != GatewayKeyDir || !key.ReadOnly {
			t.Fatalf("%s key mount = %+v", name, key)
		}
	}

	var agent corev1.Container
	for _, c := range pod.Spec.Containers {
		if c.Name == "agent" {
			agent = c
		}
	}
	agentMounts := map[string]corev1.VolumeMount{}
	for _, m := range agent.VolumeMounts {
		agentMounts[m.Name] = m
	}
	for name := range map[string]bool{"alpha": true, "beta": true} {
		if mount, ok := agentMounts[sidecarSocketVolumeName(name)]; !ok || mount.MountPath != PluginSocketDir(name) {
			t.Fatalf("agent %s socket mount = %+v", name, mount)
		}
		if _, ok := agentMounts[gatewayKeyVolumeName(name)]; ok {
			t.Fatalf("agent mounts the %s key", name)
		}
	}
}
