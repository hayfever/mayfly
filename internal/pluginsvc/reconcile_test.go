package pluginsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// The local child test runs the real shim in front of the pluginhost fixture,
// so the test binary builds both once.
var (
	shimPath    string
	fixturePath string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mayfly-pluginsvc-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture directory: %v\n", err)
		os.Exit(1)
	}
	shimPath = filepath.Join(dir, "mayfly-plugin-shim")
	fixturePath = filepath.Join(dir, "fixture")
	for _, build := range []struct {
		output string
		pkg    string
	}{
		{shimPath, "../../cmd/mayfly-plugin-shim"},
		{fixturePath, "../../internal/pluginhost/testdata/fixture/main.go"},
	} {
		cmd := exec.Command("go", "build", "-o", build.output, build.pkg)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", build.pkg, err, out)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// serviceManifest is one tickets-like service plugin.
func serviceManifest(name string) plugin.Manifest {
	replicas := int32(2)
	return plugin.Manifest{
		SchemaVersion: 1,
		Name:          name,
		Version:       "0.1.0",
		Placement:     plugin.Service,
		Image:         "ghcr.io/example/mayfly-plugin-" + name + ":0.1.0",
		Command:       "server/server",
		Env:           map[string]string{"TICKETS_URL": "https://tickets.example.test/api"},
		Secrets:       []plugin.Secret{{Env: "TICKETS_TOKEN", Secret: "mayfly-plugin-tickets", Key: "token"}},
		Tools:         []plugin.Tool{{Name: "query", Idempotent: true}},
		Service: &plugin.ServiceSpec{
			Replicas:    &replicas,
			Concurrency: 16,
			Port:        8080,
			Path:        "/mcp",
			Transport:   "stdio",
			Resources: plugin.Resources{
				Requests: map[string]string{"cpu": "50m", "memory": "128Mi"},
				Limits:   map[string]string{"memory": "256Mi"},
			},
			Egress: []plugin.Egress{{CIDR: "10.20.0.0/16", Ports: []int32{443}}},
		},
	}
}

// endpointManifest is a service plugin that points at an mcp.servers entry.
func endpointManifest() plugin.Manifest {
	return plugin.Manifest{
		SchemaVersion: 1,
		Name:          "crm",
		Placement:     plugin.Service,
		Tools:         []plugin.Tool{{Name: "find_account", Idempotent: true}},
		Service:       &plugin.ServiceSpec{MCPServer: "crm"},
	}
}

// testRender is a complete RenderConfig for the fake cluster.
func testRender() RenderConfig {
	return RenderConfig{
		Namespace:         "plugins",
		ToolsImage:        "ghcr.io/example/mayfly-agent:test",
		ImagePullPolicy:   "IfNotPresent",
		PlatformNamespace: "mayfly",
		PlatformLabels: map[string]string{
			"app.kubernetes.io/name":      "mayfly",
			"app.kubernetes.io/component": "control-plane",
		},
		DNSNamespace: "kube-system",
	}
}

// newReconciler returns a reconciler and its fake clientset.
func newReconciler(t *testing.T, manifests *[]plugin.Manifest, prune bool) (*Reconciler, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	reconciler, err := New(Options{
		Client:    k8s.NewForClient(cs, "plugins"),
		Manifests: func() []plugin.Manifest { return *manifests },
		Render:    testRender(),
		Prune:     prune,
		Selected:  map[string]bool{"tickets": true},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return reconciler, cs
}

func TestReconcileCreates(t *testing.T) {
	manifests := []plugin.Manifest{serviceManifest("tickets"), endpointManifest()}
	reconciler, cs := newReconciler(t, &manifests, true)
	ctx := context.Background()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	object := plugin.ObjectName("tickets")
	// The bundle's own secret shares the plugin name. Reconcile must not adopt
	// it as the gateway key or overwrite it.
	if _, err := cs.CoreV1().Secrets("plugins").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mayfly-plugin-tickets", Namespace: "plugins"},
		Data:       map[string][]byte{"token": []byte("ticket-token")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	manifest := manifests[0]
	deployment, err := cs.AppsV1().Deployments("plugins").Get(ctx, object, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	for key, want := range map[string]string{
		k8s.LabelManaged: "true",
		k8s.LabelApp:     k8s.PluginAppName,
		k8s.LabelPlugin:  "tickets",
	} {
		if got := deployment.Labels[key]; got != want {
			t.Fatalf("deployment label %s = %q, want %q", key, got, want)
		}
	}
	if got := deployment.Annotations[k8s.AnnotationSpecHash]; got != manifest.SpecHash() {
		t.Fatalf("spec hash annotation = %q, want %q", got, manifest.SpecHash())
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 2 {
		t.Fatalf("replicas = %v", deployment.Spec.Replicas)
	}

	pod := deployment.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != serviceUID {
		t.Fatalf("pod security context = %+v", pod.SecurityContext)
	}
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Image != testRender().ToolsImage {
		t.Fatalf("init containers = %+v", pod.InitContainers)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %+v", pod.Containers)
	}
	container := pod.Containers[0]
	if len(container.Command) == 0 || !strings.HasSuffix(container.Command[0], "/mayfly-plugin-shim") {
		t.Fatalf("command = %v", container.Command)
	}
	joined := strings.Join(container.Command, " ")
	for _, want := range []string{"serve", "--key-file", k8s.GatewayKeyDir + "/key", "--max-concurrency 16"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("command %q is missing %q", joined, want)
		}
	}
	security := container.SecurityContext
	if security == nil || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
		t.Fatalf("read-only root = %+v", security)
	}
	if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
		t.Fatalf("allow privilege escalation = %+v", security.AllowPrivilegeEscalation)
	}
	if security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capabilities = %+v", security.Capabilities)
	}
	if security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("seccomp = %+v", security.SeccompProfile)
	}
	if len(container.Resources.Requests) != 2 || container.Resources.Requests.Cpu().String() != "50m" {
		t.Fatalf("requests = %v", container.Resources.Requests)
	}
	if container.Resources.Limits.Memory().String() != "256Mi" {
		t.Fatalf("limits = %v", container.Resources.Limits)
	}
	var tokenEnv *corev1.EnvVar
	for i, env := range container.Env {
		if env.Name == "TICKETS_TOKEN" {
			tokenEnv = &container.Env[i]
		}
	}
	if tokenEnv == nil || tokenEnv.ValueFrom == nil || tokenEnv.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("secret env = %+v", tokenEnv)
	}
	if tokenEnv.ValueFrom.SecretKeyRef.Name != "mayfly-plugin-tickets" || tokenEnv.ValueFrom.SecretKeyRef.Key != "token" {
		t.Fatalf("secret ref = %+v", tokenEnv.ValueFrom.SecretKeyRef)
	}

	if _, err := cs.CoreV1().Services("plugins").Get(ctx, object, metav1.GetOptions{}); err != nil {
		t.Fatalf("service: %v", err)
	}
	if _, err := cs.NetworkingV1().NetworkPolicies("plugins").Get(ctx, object, metav1.GetOptions{}); err != nil {
		t.Fatalf("network policy: %v", err)
	}
	secret, err := cs.CoreV1().Secrets("plugins").Get(ctx, plugin.GatewayKeyName("tickets"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("gateway secret: %v", err)
	}
	if len(secret.Data["key"]) != 32 {
		t.Fatalf("gateway key length = %d", len(secret.Data["key"]))
	}
	bundleSecret, err := cs.CoreV1().Secrets("plugins").Get(ctx, "mayfly-plugin-tickets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("bundle secret: %v", err)
	}
	if string(bundleSecret.Data["token"]) != "ticket-token" || len(bundleSecret.Data["key"]) != 0 {
		t.Fatalf("bundle secret was changed: %+v", bundleSecret.Data)
	}

	url, key, err := reconciler.ServiceURL("tickets")
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://mayfly-plugin-tickets.plugins.svc:8080/mcp" {
		t.Fatalf("service URL = %q", url)
	}
	if string(key) != string(secret.Data["key"]) {
		t.Fatal("ServiceURL key does not match the Secret")
	}

	// An mcpServer plugin needs no workload of ours.
	if _, err := cs.AppsV1().Deployments("plugins").Get(ctx, plugin.ObjectName("crm"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("endpoint plugin deployment: %v", err)
	}
}

func TestReconcileNoop(t *testing.T) {
	manifests := []plugin.Manifest{serviceManifest("tickets")}
	reconciler, cs := newReconciler(t, &manifests, true)
	ctx := context.Background()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	cs.ClearActions()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, action := range cs.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatalf("unchanged reconcile wrote: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

func TestReconcileUpdates(t *testing.T) {
	manifests := []plugin.Manifest{serviceManifest("tickets")}
	reconciler, cs := newReconciler(t, &manifests, true)
	ctx := context.Background()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	object := plugin.ObjectName("tickets")

	changed := serviceManifest("tickets")
	changed.Command = "server/other"
	manifests = []plugin.Manifest{changed}
	cs.ClearActions()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := cs.AppsV1().Deployments("plugins").Get(ctx, object, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Annotations[k8s.AnnotationSpecHash] != changed.SpecHash() {
		t.Fatalf("spec hash = %q", after.Annotations[k8s.AnnotationSpecHash])
	}
	command := strings.Join(after.Spec.Template.Spec.Containers[0].Command, " ")
	if !strings.HasSuffix(command, "server/other") {
		t.Fatalf("command = %q", command)
	}
	updated := false
	for _, action := range cs.Actions() {
		if action.GetVerb() == "update" && action.GetResource().Resource == "deployments" {
			updated = true
		}
	}
	if !updated {
		t.Fatal("the deployment was not updated")
	}
}

func TestReconcilePrune(t *testing.T) {
	manifests := []plugin.Manifest{serviceManifest("tickets"), serviceManifest("word")}
	reconciler, cs := newReconciler(t, &manifests, true)
	ctx := context.Background()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tickets", "word"} {
		if _, err := cs.AppsV1().Deployments("plugins").Get(ctx, plugin.ObjectName(name), metav1.GetOptions{}); err != nil {
			t.Fatalf("deployment %s: %v", name, err)
		}
	}

	manifests = []plugin.Manifest{serviceManifest("tickets")}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AppsV1().Deployments("plugins").Get(ctx, plugin.ObjectName("word"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pruned deployment: %v", err)
	}
	for _, object := range []func() error{
		func() error {
			_, err := cs.CoreV1().Services("plugins").Get(ctx, plugin.ObjectName("word"), metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.NetworkingV1().NetworkPolicies("plugins").Get(ctx, plugin.ObjectName("word"), metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().Secrets("plugins").Get(ctx, plugin.GatewayKeyName("word"), metav1.GetOptions{})
			return err
		},
	} {
		if err := object(); !apierrors.IsNotFound(err) {
			t.Fatalf("pruned object remains: %v", err)
		}
	}

	// A second cluster proves prune: false keeps the objects.
	kept := []plugin.Manifest{serviceManifest("tickets"), serviceManifest("word")}
	reconciler, cs = newReconciler(t, &kept, false)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	kept = []plugin.Manifest{serviceManifest("tickets")}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AppsV1().Deployments("plugins").Get(ctx, plugin.ObjectName("word"), metav1.GetOptions{}); err != nil {
		t.Fatalf("unpruned deployment: %v", err)
	}
}

func TestServiceNetworkPolicy(t *testing.T) {
	manifests := []plugin.Manifest{serviceManifest("tickets")}
	reconciler, cs := newReconciler(t, &manifests, true)
	ctx := context.Background()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	policy, err := cs.NetworkingV1().NetworkPolicies("plugins").Get(ctx, plugin.ObjectName("tickets"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.PolicyTypes) != 2 {
		t.Fatalf("policy types = %v", policy.Spec.PolicyTypes)
	}
	if len(policy.Spec.Ingress) != 1 {
		t.Fatalf("ingress = %+v", policy.Spec.Ingress)
	}
	ingress := policy.Spec.Ingress[0]
	if len(ingress.From) != 1 {
		t.Fatalf("ingress peers = %+v", ingress.From)
	}
	peer := ingress.From[0]
	if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "mayfly" {
		t.Fatalf("ingress namespace = %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels["app.kubernetes.io/component"] != "control-plane" {
		t.Fatalf("ingress pods = %+v", peer.PodSelector)
	}
	if len(ingress.Ports) != 1 || ingress.Ports[0].Port == nil || ingress.Ports[0].Port.IntVal != 8080 {
		t.Fatalf("ingress ports = %+v", ingress.Ports)
	}

	if len(policy.Spec.Egress) != 2 {
		t.Fatalf("egress = %+v", policy.Spec.Egress)
	}
	dns := policy.Spec.Egress[0]
	if len(dns.To) != 1 || dns.To[0].NamespaceSelector == nil ||
		dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
		t.Fatalf("dns peer = %+v", dns.To)
	}
	if len(dns.Ports) != 2 {
		t.Fatalf("dns ports = %+v", dns.Ports)
	}
	external := policy.Spec.Egress[1]
	if len(external.To) != 1 || external.To[0].IPBlock == nil || external.To[0].IPBlock.CIDR != "10.20.0.0/16" {
		t.Fatalf("egress peer = %+v", external.To)
	}
	if len(external.Ports) != 1 || external.Ports[0].Port == nil || external.Ports[0].Port.IntVal != 443 {
		t.Fatalf("egress ports = %+v", external.Ports)
	}
}

// TestReconcileHTTPTransport covers service.transport: http: the plugin runs
// its own command, gets the gateway key as an environment variable, and is
// probed on its own port.
func TestReconcileHTTPTransport(t *testing.T) {
	manifest := serviceManifest("tickets")
	manifest.Service.Transport = "http"
	manifest.Service.Port = 9090
	manifests := []plugin.Manifest{manifest}
	reconciler, cs := newReconciler(t, &manifests, true)
	ctx := context.Background()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	deployment, err := cs.AppsV1().Deployments("plugins").Get(ctx, plugin.ObjectName("tickets"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template.Spec
	if len(pod.InitContainers) != 0 {
		t.Fatalf("http transport has init containers: %+v", pod.InitContainers)
	}
	container := pod.Containers[0]
	if len(container.Command) != 1 || container.Command[0] != "server/server" {
		t.Fatalf("command = %v", container.Command)
	}
	var gatewayKey *corev1.EnvVar
	for i, env := range container.Env {
		if env.Name == "MAYFLY_GATEWAY_KEY" {
			gatewayKey = &container.Env[i]
		}
	}
	if gatewayKey == nil || gatewayKey.ValueFrom == nil || gatewayKey.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("gateway key env = %+v", gatewayKey)
	}
	if gatewayKey.ValueFrom.SecretKeyRef.Name != plugin.GatewayKeyName("tickets") {
		t.Fatalf("gateway key secret = %+v", gatewayKey.ValueFrom.SecretKeyRef)
	}
	if container.StartupProbe == nil || container.StartupProbe.TCPSocket == nil ||
		container.StartupProbe.TCPSocket.Port.IntVal != 9090 {
		t.Fatalf("startup probe = %+v", container.StartupProbe)
	}
	service, err := cs.CoreV1().Services("plugins").Get(ctx, plugin.ObjectName("tickets"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 9090 {
		t.Fatalf("service ports = %+v", service.Spec.Ports)
	}
	url, _, err := reconciler.ServiceURL("tickets")
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://mayfly-plugin-tickets.plugins.svc:9090/mcp" {
		t.Fatalf("service URL = %q", url)
	}
}

// TestLocalServiceChild runs the real shim in front of the fixture, reaches it
// through the gateway over the state directory socket, and stops it on
// shutdown.
func TestLocalServiceChild(t *testing.T) {
	stateDir := t.TempDir()
	workDir := t.TempDir()
	pidFile := filepath.Join(workDir, "pid")
	logFile := filepath.Join(workDir, "calls.jsonl")
	t.Setenv("FIXTURE_TOOLS", "query")
	t.Setenv("FIXTURE_PID_FILE", pidFile)
	t.Setenv("FIXTURE_LOG", logFile)

	manifest := plugin.Manifest{
		SchemaVersion: 1,
		Name:          "tickets",
		Placement:     plugin.Service,
		Local:         &plugin.LocalRun{Command: fixturePath},
		Tools:         []plugin.Tool{{Name: "query", Idempotent: true}},
		Service:       &plugin.ServiceSpec{Concurrency: 4, Transport: "stdio"},
		Dir:           workDir,
	}
	manifests := []plugin.Manifest{manifest}
	local, err := NewLocal(LocalConfig{
		StateDir:  stateDir,
		Manifests: func() []plugin.Manifest { return manifests },
		Shim:      shimPath,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := local.Start(ctx); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(stateDir, "plugins", "tickets.sock")
	waitReady(t, socket)

	mem := journal.NewMemory()
	if err := mem.Reserve(ctx, "s1", 10); err != nil {
		t.Fatal(err)
	}
	g := gateway.New(gateway.Options{Ledger: mem, Sequencer: mem, ServiceURL: local.ServiceURL})
	g.Register(gateway.GrantFor(sessioncfg.Config{
		SessionID: "s1",
		MaxSteps:  4,
		Plugins: []sessioncfg.Plugin{{
			Name:      "tickets",
			Placement: "service",
			Tools:     []sessioncfg.PluginTool{{Name: "query", Idempotent: true, TimeoutSeconds: 5}},
		}},
	}, "helper", nil))

	resp, err := g.Begin(ctx, "s1", gateway.BeginRequest{
		Turn:      1,
		Step:      1,
		Tool:      "plugin_tickets_query",
		Arguments: json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result == nil || resp.Result.Content != "echo: hello" {
		t.Fatalf("result = %+v", resp)
	}

	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("fixture log: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var call struct {
		Method string         `json:"method"`
		Meta   map[string]any `json:"meta"`
	}
	found := false
	for decoder.More() {
		if err := decoder.Decode(&call); err != nil {
			t.Fatalf("decode fixture log: %v", err)
		}
		if call.Method == "tools/call" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("fixture saw no tool call: %s", raw)
	}
	if call.Meta["mayfly.io/idempotency-key"] != "s1/1/1" {
		t.Fatalf("idempotency key = %v", call.Meta)
	}
	if call.Meta["mayfly.io/session"] != "s1" {
		t.Fatalf("session meta = %v", call.Meta)
	}

	pidRaw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fixture pid: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(pidRaw), "%d", &pid); err != nil {
		t.Fatalf("parse fixture pid: %v", err)
	}

	local.Close()
	if err := waitGone(socket, pid); err != nil {
		t.Fatal(err)
	}
}

// waitReady waits for a shim on a Unix socket to report a ready child.
func waitReady(t *testing.T, socket string) {
	t.Helper()
	client := socketClient(socket)
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get("http://plugin/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("shim never became ready: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitGone waits for the socket file to disappear and the process to exit.
func waitGone(socket string, pid int) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, socketErr := os.Stat(socket)
		processErr := syscall.Kill(pid, 0)
		if os.IsNotExist(socketErr) && processErr == syscall.ESRCH {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("child did not stop: socket=%v process=%v", socketErr, processErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// socketClient talks to one Unix socket.
func socketClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}
}
