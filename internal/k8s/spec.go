// Package k8s builds and manages the Kubernetes objects that back a session.
package k8s

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	// ArtifactsDir holds the bundle while it is built.
	ArtifactsDir = "/var/lib/mayfly/artifacts"

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
	return nil
}

func values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Secret returns the per-session bearer token secret.
func (s *SessionSpec) Secret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.SecretName,
			Namespace: s.Namespace,
			Labels:    s.labels(),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token": s.Token},
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
	rootfs := true
	deadline := int64(s.TTL.Seconds()) + 300

	env := []corev1.EnvVar{
		{Name: "MAYFLY_CONFIG", Value: ConfigFile},
		{Name: "HOME", Value: "/tmp"},
		{Name: "XDG_CACHE_HOME", Value: "/tmp/cache"},
		{Name: "NPM_CONFIG_CACHE", Value: "/tmp/npm"},
		{
			Name: "MAYFLY_SESSION_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.SecretName},
					Key:                  "token",
				},
			},
		},
	}
	for k, v := range s.Env {
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
			Containers: []corev1.Container{{
				Name:            "agent",
				Image:           s.Image,
				ImagePullPolicy: corev1.PullPolicy(s.ImagePullPolicy),
				Command:         s.Command,
				Args:            s.Args,
				Env:             env,
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             &yes,
					RunAsUser:                new(int64(agentUID)),
					RunAsGroup:               new(int64(agentGID)),
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &rootfs,
					Privileged:               &no,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: seccomp},
				},
				Resources: corev1.ResourceRequirements{
					Requests: quantities(s.Resources.Requests),
					Limits:   quantities(s.Resources.Limits),
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "config", MountPath: "/etc/mayfly", ReadOnly: true},
					{Name: "workspace", MountPath: WorkspaceDir},
					{Name: "artifacts", MountPath: "/var/lib/mayfly"},
					{Name: "tmp", MountPath: "/tmp"},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: s.ConfigMapName},
						},
					},
				},
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr(s.WorkspaceLimit)},
					},
				},
				{
					Name: "artifacts",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr(s.ArtifactLimit)},
					},
				},
				{
					Name: "tmp",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: quantityPtr("256Mi"),
						},
					},
				},
			},
		},
	}
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
			Name:      s.PodName,
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
