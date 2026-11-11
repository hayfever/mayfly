package k8s

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
