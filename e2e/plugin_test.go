//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The fixture plugin image, its manifests, and the fixture agents are described
// in e2e/plugins and e2e/agents.yaml. The tools are deterministic: a prompt
// line "tool: <name> <json>" makes llmstub call that tool next, so a test picks
// the tool and its arguments instead of guessing what a model would do.

// directive renders one scripted tool call for a prompt.
func directive(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return "tool: " + name + " " + string(raw)
}

// promptFor builds a prompt with one scripted call per line.
func promptFor(t *testing.T, calls ...string) string {
	t.Helper()
	return "plugin e2e check\n" + strings.Join(calls, "\n")
}

// waitToolResult waits for the tool result of one model-visible tool.
func waitToolResult(t *testing.T, id, tool string) event {
	t.Helper()
	return env.waitEvent(t, id, tool+" result", func(ev event) bool {
		name, _ := ev.Data["tool"].(string)
		return ev.Type == "tool_result" && name == tool
	})
}

// pluginJSON decodes the JSON text a fixture tool returns.
func pluginJSON[T any](t *testing.T, ev event) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(ev.Text), &out); err != nil {
		t.Fatalf("decode %q: %v", ev.Text, err)
	}
	return out
}

// closePluginSession closes a session and waits for it to complete, which also
// proves the plugins stop with it.
func closePluginSession(t *testing.T, id string) {
	t.Helper()
	r := env.admin(t, http.MethodPost, "/api/v1/sessions/"+id+"/close", map[string]string{"reason": "e2e"})
	expectStatus(t, r, http.StatusAccepted, "close session")
	env.waitStatus(t, id, "completed", 2*time.Minute)
}

// podByName reads one pod so a test can assert its containers and mounts.
func podByName(t *testing.T, name string) corev1.Pod {
	t.Helper()
	raw, err := env.kubectl("get", "pod", name, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := json.Unmarshal([]byte(raw), &pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

// volumeMount returns the mount at one path, or nil.
func volumeMount(container corev1.Container, path string) *corev1.VolumeMount {
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].MountPath == path {
			return &container.VolumeMounts[i]
		}
	}
	return nil
}

// envVar returns one environment variable of a container, or nil.
func envVar(container corev1.Container, name string) *corev1.EnvVar {
	for i := range container.Env {
		if container.Env[i].Name == name {
			return &container.Env[i]
		}
	}
	return nil
}

// tryExec runs a command in a pod and returns kubectl's exit code and output
// without failing the test, so a poll can survive a pod that is being deleted.
func tryExec(pod string, command ...string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := env.kubectlArgs(append([]string{"exec", pod, "--"}, command...)...)
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	return -1, string(out)
}

// TestPluginInlineSession runs an inline plugin end to end and checks the pod
// shape that carries it: the shim and copy init containers, and a read-only
// plugin tree in the agent container.
func TestPluginInlineSession(t *testing.T) {
	s := env.createSession(t, map[string]any{
		"agent":  "plugins-inline",
		"prompt": promptFor(t, directive(t, "plugin_inline_echo", map[string]any{"text": "hello inline"})),
	})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)

	result := waitToolResult(t, s.ID, "plugin_inline_echo")
	if strings.TrimSpace(result.Text) != "echo: hello inline" {
		t.Fatalf("inline tool result = %q", result.Text)
	}
	env.waitEvent(t, s.ID, "assistant reply", func(ev event) bool {
		return ev.Type == "assistant_message" && strings.Contains(ev.Text, "Completed.")
	})

	pod := podByName(t, "mayfly-"+s.ID)
	init := map[string]bool{}
	for _, container := range pod.Spec.InitContainers {
		init[container.Name] = true
	}
	for _, name := range []string{"mayfly-tools", "plugin-inline"} {
		if !init[name] {
			t.Errorf("pod has no %s init container; has %v", name, keys(init))
		}
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("pod has %d containers", len(pod.Spec.Containers))
	}
	mount := volumeMount(pod.Spec.Containers[0], "/opt/mayfly/plugins")
	if mount == nil {
		t.Fatalf("agent container has no /opt/mayfly/plugins mount")
	}
	if !mount.ReadOnly {
		t.Error("the plugin tree is mounted read-write")
	}
	closePluginSession(t, s.ID)
}

// TestPluginEnvIsolated checks the process environment of an inline plugin: the
// manifest's env and the MAYFLY identity arrive, the session token and the
// control-plane config do not.
func TestPluginEnvIsolated(t *testing.T) {
	s := env.createSession(t, map[string]any{
		"agent":  "plugins-inline",
		"prompt": promptFor(t, directive(t, "plugin_inline_env", nil)),
	})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)

	envs := pluginJSON[struct {
		Plugin    string `json:"plugin"`
		Mode      string `json:"mode"`
		Session   string `json:"session"`
		Workspace string `json:"workspace"`
		Home      string `json:"home"`
		TokenSet  bool   `json:"token_set"`
		ConfigSet bool   `json:"config_set"`
		InlineEnv string `json:"inline_env"`
	}](t, waitToolResult(t, s.ID, "plugin_inline_env"))

	if envs.TokenSet {
		t.Error("the plugin process can read MAYFLY_SESSION_TOKEN")
	}
	if envs.ConfigSet {
		t.Error("the plugin process can read MAYFLY_CONFIG")
	}
	if envs.Plugin != "inline" || envs.Mode != "inline" {
		t.Errorf("plugin identity = %q/%q", envs.Plugin, envs.Mode)
	}
	if envs.Session != s.ID {
		t.Errorf("plugin session = %q, want %q", envs.Session, s.ID)
	}
	if envs.Workspace != "/workspace" {
		t.Errorf("plugin workspace = %q", envs.Workspace)
	}
	if envs.InlineEnv != "inline-value" {
		t.Errorf("manifest env = %q", envs.InlineEnv)
	}
	if !strings.HasPrefix(envs.Home, "/tmp/plugins/inline/") {
		t.Errorf("plugin HOME = %q", envs.Home)
	}
}

// TestPluginResumeAfterPodDelete deletes the session pod while a
// non-idempotent inline call is in flight. The replacement must not repeat the
// call: the gateway answers with the interrupted result and the workspace
// counter stays at one.
func TestPluginResumeAfterPodDelete(t *testing.T) {
	s := env.createSession(t, map[string]any{
		"agent":  "plugins-inline",
		"prompt": promptFor(t, directive(t, "plugin_inline_slow", nil)),
	})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)
	pod := "mayfly-" + s.ID

	// The plugin writes the counter before it sleeps, so the file proves the
	// call reached the plugin and is still in flight.
	eventually(t, 90*time.Second, "slow call in flight", func() (bool, string) {
		code, out := tryExec(pod, "cat", "/workspace/.plugin-calls")
		return code == 0 && strings.TrimSpace(out) == "1", fmt.Sprintf("exit %d: %q", code, strings.TrimSpace(out))
	})

	if _, err := env.kubectl("delete", "pod", pod, "--grace-period=0", "--force"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 90*time.Second, "replacement pod", func() (bool, string) {
		names := env.podNames(t, s.ID)
		for _, name := range names {
			if strings.HasSuffix(name, "-r1") {
				return true, strings.Join(names, ", ")
			}
		}
		return false, strings.Join(names, ", ")
	})

	result := env.waitEvent(t, s.ID, "interrupted tool result", func(ev event) bool {
		tool, _ := ev.Data["tool"].(string)
		interrupted, _ := ev.Data["interrupted"].(bool)
		return ev.Type == "tool_result" && tool == "plugin_inline_slow" && interrupted
	})
	if !strings.Contains(result.Text, "was interrupted by a container restart") {
		t.Fatalf("interrupted result = %q", result.Text)
	}
	env.waitEvent(t, s.ID, "assistant reply after resume", func(ev event) bool {
		return ev.Type == "assistant_message" && strings.Contains(ev.Text, "Completed.")
	})

	replacement := "mayfly-" + s.ID + "-r1"
	code, out := tryExec(replacement, "cat", "/workspace/.plugin-calls")
	if code != 0 || strings.TrimSpace(out) != "1" {
		t.Fatalf("workspace counter after resume = %q (exit %d), want 1; the tool ran again", strings.TrimSpace(out), code)
	}
}

// TestPluginMissingDirFailsSession uses an image without /mayfly-plugin. The
// copy init container fails, so the session never starts and ends failed.
func TestPluginMissingDirFailsSession(t *testing.T) {
	s := env.createSession(t, map[string]any{
		"agent":  "plugins-missing",
		"prompt": "a plugin image without /mayfly-plugin",
	})

	detail := waitCopyFailure(t, s.ID, 2*time.Minute)
	if !strings.Contains(detail, "/mayfly-plugin") {
		t.Fatalf("copy container output = %q, want the missing directory", detail)
	}
	got := env.waitStatus(t, s.ID, "failed", 3*time.Minute)
	if got.Error == "" {
		t.Error("failed session carries no error")
	}
}

// waitCopyFailure watches the session's pods for a copy init container that
// exited, and returns its log. Replacements give the check more than one pod.
func waitCopyFailure(t *testing.T, id string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		raw, err := env.kubectl("get", "pods", "-l", "mayfly.io/session="+id, "-o", "json")
		if err == nil {
			var list struct {
				Items []corev1.Pod `json:"items"`
			}
			if json.Unmarshal([]byte(raw), &list) == nil {
				for _, pod := range list.Items {
					if !copyContainerFailed(pod) {
						continue
					}
					out, logErr := env.kubectl("logs", pod.Name, "-c", "plugin-missing")
					if logErr == nil && strings.TrimSpace(out) != "" {
						return out
					}
					last = "the copy container failed but its log was not readable"
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no copy init container failed within %s; last state: %s", timeout, last)
	return ""
}

// copyContainerFailed reports an inline copy init container that exited.
func copyContainerFailed(pod corev1.Pod) bool {
	for _, status := range pod.Status.InitContainerStatuses {
		if !strings.HasPrefix(status.Name, "plugin-") {
			continue
		}
		if status.State.Terminated != nil || status.LastTerminationState.Terminated != nil {
			return true
		}
	}
	return false
}

// TestMCPServerIdempotencyKey checks that an HTTP mcp.servers call goes through
// the gateway and carries the ledger key.
func TestMCPServerIdempotencyKey(t *testing.T) {
	base, stop := env.forwardService(t, "mcpstub", 9100)
	defer stop()
	resetMCPCounts(t, base)

	s := env.createSession(t, map[string]any{
		"agent":  "e2e",
		"prompt": "check the mcp idempotency key " + randomHex(4),
	})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)
	env.waitEvent(t, s.ID, "MCP tool result", func(ev event) bool {
		tool, _ := ev.Data["tool"].(string)
		return ev.Type == "tool_result" && tool == "mcp_echo"
	})

	meta := mcpCallMeta(t, base, "echo")
	key, _ := meta["mayfly.io/idempotency-key"].(string)
	want := regexp.MustCompile(`^` + regexp.QuoteMeta(s.ID) + `/\d+/\d+$`)
	if !want.MatchString(key) {
		t.Fatalf("idempotency key = %q, want %s/<turn>/<step>", key, s.ID)
	}
	if session, _ := meta["mayfly.io/session"].(string); session != s.ID {
		t.Fatalf("call session = %q, want %q", session, s.ID)
	}
}

// mcpCallMeta reads the last call metadata the MCP stub recorded.
func mcpCallMeta(t *testing.T, base, tool string) map[string]any {
	t.Helper()
	resp, err := env.http.Get(base + "/meta")
	if err != nil {
		t.Fatalf("read mcp meta: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read mcp meta: %s", resp.Status)
	}
	var out struct {
		Meta map[string]map[string]any `json:"meta"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode mcp meta: %v", err)
	}
	meta, ok := out.Meta[tool]
	if !ok {
		t.Fatalf("mcp stub saw no call to %q; meta: %v", tool, out.Meta)
	}
	return meta
}

// TestPluginServiceShared checks the reconciler's objects, one replica, and
// that two sessions reach the same process state.
func TestPluginServiceShared(t *testing.T) {
	waitPluginService(t, "counter")

	first := env.createSession(t, map[string]any{
		"agent":  "plugins-service",
		"prompt": promptFor(t, directive(t, "plugin_counter_state", nil)),
	})
	env.waitStatus(t, first.ID, "running", 3*time.Minute)
	stateFirst := pluginJSON[struct {
		Total    int            `json:"total"`
		Sessions map[string]int `json:"sessions"`
	}](t, waitToolResult(t, first.ID, "plugin_counter_state"))
	if stateFirst.Sessions[first.ID] == 0 {
		t.Fatalf("first session not counted: %+v", stateFirst)
	}

	second := env.createSession(t, map[string]any{
		"agent":  "plugins-service",
		"prompt": promptFor(t, directive(t, "plugin_counter_state", nil)),
	})
	env.waitStatus(t, second.ID, "running", 3*time.Minute)
	stateSecond := pluginJSON[struct {
		Total    int            `json:"total"`
		Sessions map[string]int `json:"sessions"`
	}](t, waitToolResult(t, second.ID, "plugin_counter_state"))

	if stateSecond.Total <= stateFirst.Total {
		t.Errorf("second session saw total %d, first saw %d: the service did not share its state",
			stateSecond.Total, stateFirst.Total)
	}
	if stateSecond.Sessions[first.ID] == 0 || stateSecond.Sessions[second.ID] == 0 {
		t.Errorf("the service did not count both sessions: %+v", stateSecond.Sessions)
	}

	out, err := env.kubectl("get", "deployment", "mayfly-plugin-counter",
		"-o", "jsonpath={.spec.replicas}/{.status.readyReplicas}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "1/1" {
		t.Fatalf("counter deployment replicas = %q, want 1/1", strings.TrimSpace(out))
	}
}

// TestPluginServiceNetworkPolicy checks that only the control plane reaches a
// service, and that the service reaches only its egress CIDRs.
func TestPluginServiceNetworkPolicy(t *testing.T) {
	waitPluginService(t, "counter")
	target := fmt.Sprintf("http://mayfly-plugin-counter.%s.svc.cluster.local:8080/mcp", env.namespace)

	// The service is up: a session's call reaches it through the gateway.
	s := env.createSession(t, map[string]any{
		"agent":  "plugins-service",
		"prompt": promptFor(t, directive(t, "plugin_counter_state", nil)),
	})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)
	waitToolResult(t, s.ID, "plugin_counter_state")

	probe := startProbe(t)
	allow := "e2e-allow-plugin-" + randomHex(3)
	applyPluginAllowPolicy(t, allow, "counter", "Ingress")
	t.Cleanup(func() {
		_, _ = env.kubectl("delete", "networkpolicy", allow, "--ignore-not-found")
	})
	waitReachable(t, probe, target, "an unrestricted pod reaching the plugin once ingress is allowed (control)")
	if _, err := env.kubectl("delete", "networkpolicy", allow); err != nil {
		t.Fatal(err)
	}
	waitBlocked(t, probe, target, "an unrestricted pod reaching the plugin once the temporary allow is gone")
	expectCurl(t, "mayfly-"+s.ID, target, curlBlocked, "a session pod reaching the service directly")

	pod := pluginPod(t, "counter")
	control := "llmstub." + env.namespace + ".svc.cluster.local"
	mcp := "mcpstub." + env.namespace + ".svc.cluster.local"
	if code := dialFromPod(t, pod, control, 9099); code != 0 {
		t.Errorf("the service cannot reach its allowed egress CIDR (exit %d)", code)
	}
	if code := dialFromPod(t, pod, mcp, 9100); code == 0 {
		t.Error("the service reached a port outside its egress rules")
	}
	if code := dialFromPod(t, pod, "kubernetes.default.svc.cluster.local", 443); code == 0 {
		t.Error("the service reached the Kubernetes API")
	}
}

// applyPluginAllowPolicy opens one direction for a service plugin's pods,
// without touching the generated policy. Rules are additive.
func applyPluginAllowPolicy(t *testing.T, name, plugin, policyType string) {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: %s
  namespace: %s
spec:
  podSelector:
    matchLabels:
      mayfly.io/plugin: %s
  policyTypes: [%s]
  %s:
    - {}
`, name, env.namespace, plugin, policyType, strings.ToLower(policyType))
	if _, err := env.kubectlStdin(manifest, "apply", "-f", "-"); err != nil {
		t.Fatal(err)
	}
}

// pluginPod returns the single ready pod of a service plugin.
func pluginPod(t *testing.T, plugin string) string {
	t.Helper()
	var name string
	eventually(t, time.Minute, "pod of plugin "+plugin, func() (bool, string) {
		out, err := env.kubectl("get", "pods",
			"-l", "app.kubernetes.io/name=mayfly-plugin,mayfly.io/plugin="+plugin,
			"-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return false, err.Error()
		}
		names := strings.Fields(out)
		if len(names) != 1 {
			return false, fmt.Sprintf("pods: %v", names)
		}
		name = names[0]
		return true, ""
	})
	return name
}

// dialFromPod opens one TCP connection from inside a pod and returns the
// command's exit code: 0 connected, non-zero refused, timed out, or
// unresolvable.
func dialFromPod(t *testing.T, pod, host string, port int) int {
	t.Helper()
	script := fmt.Sprintf(`const net = require("net");
const socket = net.connect({host: %q, port: %d});
const done = (ok) => { socket.destroy(); process.exit(ok ? 0 : 1); };
socket.setTimeout(3000, () => done(false));
socket.on("connect", () => done(true));
socket.on("error", () => done(false));`, host, port)
	code, _ := tryExec(pod, "node", "-e", script)
	return code
}

// waitPluginService waits until a service plugin's objects exist and its
// single replica is ready.
func waitPluginService(t *testing.T, name string) {
	t.Helper()
	object := "mayfly-plugin-" + name
	for _, kind := range []string{"deployment", "service", "networkpolicy", "secret"} {
		eventually(t, time.Minute, kind+" "+object+" exists", func() (bool, string) {
			if _, err := env.kubectl("get", kind, object); err != nil {
				return false, err.Error()
			}
			return true, ""
		})
	}
	eventually(t, 3*time.Minute, "service plugin "+name+" ready", func() (bool, string) {
		out, err := env.kubectl("get", "deployment", object,
			"-o", "jsonpath={.status.readyReplicas}/{.status.replicas}")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(out) == "1/1", strings.TrimSpace(out)
	})
}

// TestPluginSidecarSession runs a sidecar plugin and checks where its secret
// lives: the sidecar process reads it, the agent container cannot.
func TestPluginSidecarSession(t *testing.T) {
	secret := ensureSidecarSecret(t)
	s := env.createSession(t, map[string]any{
		"agent": "plugins-sidecar",
		"prompt": promptFor(t,
			directive(t, "plugin_sidecar_echo", map[string]any{"text": "hello sidecar"}),
			directive(t, "plugin_sidecar_env", nil),
		),
	})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)

	echo := waitToolResult(t, s.ID, "plugin_sidecar_echo")
	if strings.TrimSpace(echo.Text) != "echo: hello sidecar" {
		t.Fatalf("sidecar tool result = %q", echo.Text)
	}
	envs := pluginJSON[struct {
		Mode          string `json:"mode"`
		Session       string `json:"session"`
		TokenSet      bool   `json:"token_set"`
		SidecarEnv    string `json:"sidecar_env"`
		SidecarSecret string `json:"sidecar_secret"`
	}](t, waitToolResult(t, s.ID, "plugin_sidecar_env"))
	if envs.Mode != "sidecar" || envs.Session != s.ID {
		t.Errorf("sidecar identity = %+v", envs)
	}
	if envs.TokenSet {
		t.Error("the sidecar plugin process can read MAYFLY_SESSION_TOKEN")
	}
	if envs.SidecarEnv != "sidecar-value" {
		t.Errorf("sidecar env = %q", envs.SidecarEnv)
	}
	if envs.SidecarSecret != secret {
		t.Errorf("sidecar secret = %q, want the value from the Secret", envs.SidecarSecret)
	}

	pod := podByName(t, "mayfly-"+s.ID)
	for _, container := range pod.Spec.Containers {
		if container.Name == "agent" {
			if envVar(container, "E2E_SIDECAR_SECRET") != nil {
				t.Error("the agent container declares the sidecar secret")
			}
		}
	}
	if code, out := tryExec("mayfly-"+s.ID, "sh", "-c", `printf '%s' "${E2E_SIDECAR_SECRET:-}"`); code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("agent container read the sidecar secret: exit %d: %q", code, strings.TrimSpace(out))
	}
	env.waitEvent(t, s.ID, "assistant reply", func(ev event) bool {
		return ev.Type == "assistant_message" && strings.Contains(ev.Text, "Completed.")
	})
	closePluginSession(t, s.ID)
}

// TestPluginSidecarSocketRequiresToken calls the sidecar socket the way
// run_command in the agent container could, without a call token.
func TestPluginSidecarSocketRequiresToken(t *testing.T) {
	ensureSidecarSecret(t)
	s := env.createSession(t, map[string]any{
		"agent":  "plugins-sidecar",
		"prompt": promptFor(t, directive(t, "plugin_sidecar_echo", map[string]any{"text": "socket probe"})),
	})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)
	// A completed call proves the socket serves authenticated requests.
	waitToolResult(t, s.ID, "plugin_sidecar_echo")

	socket := "/run/mayfly/plugin-sockets/sidecar.sock"
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	code, out := tryExec("mayfly-"+s.ID, "curl", "-sS", "-o", "/dev/null",
		"-w", "%{http_code}", "--unix-socket", socket,
		"-X", "POST", "-H", "Content-Type: application/json",
		"--data", body, "http://localhost/mcp")
	status := strings.TrimSpace(out)
	if code != 0 {
		t.Fatalf("curl to the sidecar socket: exit %d: %s", code, status)
	}
	if status != "401" {
		t.Fatalf("unauthenticated socket call = %s, want 401", status)
	}
}

// ensureSidecarSecret creates the Secret the sidecar manifest reads, so the
// agent container has a value it must not be able to see.
func ensureSidecarSecret(t *testing.T) string {
	t.Helper()
	value := "sidecar-" + randomHex(8)
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: mayfly-plugin-e2e
  namespace: %s
type: Opaque
stringData:
  sidecar-token: %q
`, env.namespace, value)
	if _, err := env.kubectlStdin(manifest, "apply", "-f", "-"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.kubectl("delete", "secret", "mayfly-plugin-e2e", "--ignore-not-found")
	})
	return value
}
