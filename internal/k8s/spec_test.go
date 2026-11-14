package k8s

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

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
	if spec.Secret().StringData["token"] != "secret-token" {
		t.Fatal("session token missing from secret")
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

func TestPodPluginInitContainers(t *testing.T) {
	spec := testSpec()
	spec.ToolsImage = "mayfly-agent:tools"
	spec.PluginVolumeLimit = "512Mi"
	spec.Plugins = []sessioncfg.Plugin{
		inlinePlugin("tickets", "example.com/tickets:1"),
		inlinePlugin("word-count", "example.com/word-count:1"),
		{Name: "notify", Placement: "sidecar"},
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := spec.Pod()

	init := pod.Spec.InitContainers
	if len(init) != 3 {
		t.Fatalf("init containers = %d, want tools and two inline plugins", len(init))
	}
	wantNames := []string{"mayfly-tools", "plugin-tickets", "plugin-word-count"}
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
