//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// curl exit codes that matter here.
const (
	curlOK          = 0
	curlRefused     = 7  // the packet reached the destination, which said no
	curlTimeout     = 28 // the packet was dropped, which is what a NetworkPolicy does
	probeTimeoutSec = "3"
)

// TestNetworkIsolation checks the per-session NetworkPolicy on a real CNI.
// Every blocked path is paired with a control that proves the path would
// otherwise work, so a cluster that ignores NetworkPolicy fails here.
func TestNetworkIsolation(t *testing.T) {
	a := env.createSession(t, map[string]any{"prompt": "isolation probe A", "agent": "e2e"})
	b := env.createSession(t, map[string]any{"prompt": "isolation probe B", "agent": "e2e"})
	env.waitStatus(t, a.ID, "running", 3*time.Minute)
	env.waitStatus(t, b.ID, "running", 3*time.Minute)
	podA, podB := "mayfly-"+a.ID, "mayfly-"+b.ID
	ipB := podIP(t, podB)

	probe := startProbe(t)
	llmIP := podIPBySelector(t, "app.kubernetes.io/name=llmstub")
	controlPlane := fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/healthz", env.service, env.namespace)
	llm := fmt.Sprintf("http://llmstub.%s.svc.cluster.local:9099/healthz", env.namespace)
	mcp := fmt.Sprintf("http://mcpstub.%s.svc.cluster.local:9100/healthz", env.namespace)

	// Controls from an unrestricted pod in the same namespace.
	expectCurl(t, probe, llm, curlOK, "probe pod reaches llmstub (control)")
	expectCurl(t, probe, fmt.Sprintf("http://%s:1/", llmIP), curlRefused, "closed port on a pod without a policy refuses (control)")

	t.Run("egress", func(t *testing.T) {
		if code, out := env.execIn(t, podA, "getent", "hosts", "kubernetes.default.svc.cluster.local"); code != 0 {
			t.Errorf("DNS is blocked from the session: exit %d: %s", code, out)
		}
		expectCurl(t, podA, controlPlane, curlOK, "session reaches the control plane")
		expectCurl(t, podA, "https://kubernetes.default.svc.cluster.local/version", curlTimeout, "Kubernetes API")
		expectCurl(t, podA, llm, curlTimeout, "LLM backend directly, bypassing the proxy")
		expectCurl(t, podA, mcp, curlTimeout, "MCP server directly, bypassing the proxy")
		expectCurl(t, podA, fmt.Sprintf("http://%s:1/", ipB), curlTimeout, "another session pod")
		expectCurl(t, podA, "http://169.254.169.254/latest/meta-data/", -1, "cloud metadata endpoint")
		expectCurl(t, podA, "https://1.1.1.1/", -1, "the internet")
	})
	t.Run("ingress", func(t *testing.T) {
		expectCurl(t, probe, fmt.Sprintf("http://%s:1/", ipB), curlTimeout, "a pod in the namespace reaching a session")
	})
}

// expectCurl runs curl in a pod. want -1 accepts any failure: some
// destinations (metadata, internet) are unreachable on kind even without a
// policy, so only "did not connect" is meaningful there.
func expectCurl(t *testing.T, pod, target string, want int, what string) {
	t.Helper()
	code, out := env.execIn(t, pod, "curl", "-ksS", "-o", "/dev/null", "-m", probeTimeoutSec, target)
	switch {
	case want == -1 && code != curlOK:
	case code == want:
	default:
		t.Errorf("%s (%s): curl exit %d, want %d: %s", what, target, code, want, strings.TrimSpace(out))
	}
}

func podIP(t *testing.T, name string) string {
	t.Helper()
	var ip string
	eventually(t, time.Minute, "IP of "+name, func() (bool, string) {
		out, err := env.kubectl("get", "pod", name, "-o", "jsonpath={.status.podIP}")
		ip = strings.TrimSpace(out)
		if err != nil {
			return false, err.Error()
		}
		return ip != "", "no IP yet"
	})
	return ip
}

func podIPBySelector(t *testing.T, selector string) string {
	t.Helper()
	out, err := env.kubectl("get", "pod", "-l", selector, "-o", "jsonpath={.items[0].status.podIP}")
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("pod IP for %s: %v %q", selector, err, out)
	}
	return strings.TrimSpace(out)
}

// startProbe runs a pod without a NetworkPolicy, from the agent image (which
// has curl), and deletes it when the test ends.
func startProbe(t *testing.T) string {
	t.Helper()
	name := "e2e-probe-" + randomHex(3)
	overrides := `{"spec":{"automountServiceAccountToken":false,"securityContext":{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"probe","image":"` + env.agentImage + `","command":["sleep","600"],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}`
	if _, err := env.kubectl("run", name, "--image", env.agentImage, "--restart=Never",
		"--labels", "app.kubernetes.io/name=mayfly-e2e-probe", "--overrides", overrides); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = env.kubectl("delete", "pod", name, "--wait=false", "--grace-period=0") })
	if _, err := env.kubectl("wait", "--for=condition=Ready", "pod/"+name, "--timeout=120s"); err != nil {
		t.Fatal(err)
	}
	return name
}
