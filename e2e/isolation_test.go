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
	curlOK      = 0
	curlRefused = 7  // the connection was refused (RST or ICMP unreachable)
	curlTimeout = 28 // the connection timed out: the packet was dropped

	// curlBlocked is not an exit code: the connection must not succeed. An
	// enforcing CNI either drops the packet (timeout) or refuses it (reset),
	// so exactly those two failures count. A missing binary or a DNS failure
	// is a broken probe, not a policy, and fails the check.
	//
	// Each blocked path below is paired with a control proving the path works
	// without the policy; see TestNetworkIsolation.
	curlBlocked     = -1
	probeTimeoutSec = "3"

	// listenerPort is where the test serves HTTP inside a session pod. Session
	// pods run no server of their own, so a blocked probe needs a listener:
	// against a closed port, every connection fails whether or not a policy
	// is enforced.
	listenerPort = 8081
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
	listener := fmt.Sprintf("http://%s:%d/", ipB, listenerPort)

	startListener(t, podB, listenerPort)

	// Controls from an unrestricted pod in the same namespace.
	expectCurl(t, probe, llm, curlOK, "probe pod reaches llmstub (control)")
	expectCurl(t, probe, fmt.Sprintf("http://%s:9099/healthz", llmIP), curlOK, "probe pod reaches llmstub by pod IP (control)")
	expectCurl(t, probe, fmt.Sprintf("http://%s:1/", llmIP), curlRefused, "closed port on a pod without a policy refuses (control)")

	// The listener runs inside session B. NetworkPolicy rules are additive, so
	// a temporary allow policy opens the two probe paths without touching the
	// policies the control plane created. The same URLs then answer from the
	// same pods, which makes the blocked checks below about the policy and not
	// about a dead port or a broken path. Deleting the allow policies is
	// likewise a real test of the generated ones, so the waits that follow
	// prove the deny is back at the CNI before anything is asserted on it.
	allowIngress := "e2e-allow-ingress-" + randomHex(3)
	allowEgress := "e2e-allow-egress-" + randomHex(3)
	applyAllowPolicy(t, allowIngress, b.ID, "Ingress")
	applyAllowPolicy(t, allowEgress, a.ID, "Egress")
	t.Cleanup(func() {
		_, _ = env.kubectl("delete", "networkpolicy", allowIngress, allowEgress, "--ignore-not-found")
	})
	waitReachable(t, probe, listener, "probe pod reaching the session listener when ingress is allowed (control)")
	waitReachable(t, podA, listener, "session A reaching the session listener when egress is allowed (control)")
	// Remove the egress allow first. B's ingress allow is still in place, so
	// A reaching B can only be stopped by A's own egress policy here.
	if _, err := env.kubectl("delete", "networkpolicy", allowEgress); err != nil {
		t.Fatal(err)
	}
	waitBlocked(t, podA, llm, "session A reaching llmstub once the temporary egress allow is gone")
	waitBlocked(t, podA, listener, "session A reaching session B by egress policy alone, with B's ingress still allowed")
	if _, err := env.kubectl("delete", "networkpolicy", allowIngress); err != nil {
		t.Fatal(err)
	}
	waitBlocked(t, probe, listener, "a pod in the namespace reaching a session once the temporary ingress allow is gone")

	t.Run("egress", func(t *testing.T) {
		if code, out := env.execIn(t, podA, "getent", "hosts", "kubernetes.default.svc.cluster.local"); code != 0 {
			t.Errorf("DNS is blocked from the session: exit %d: %s", code, out)
		}
		expectCurl(t, podA, controlPlane, curlOK, "session reaches the control plane")
		expectCurl(t, podA, "https://kubernetes.default.svc.cluster.local/version", curlBlocked, "Kubernetes API")
		expectCurl(t, podA, llm, curlBlocked, "LLM backend directly, bypassing the proxy")
		expectCurl(t, podA, mcp, curlBlocked, "MCP server directly, bypassing the proxy")
		expectCurl(t, podA, fmt.Sprintf("http://%s:9099/healthz", llmIP), curlBlocked, "another pod by pod IP")
		expectCurl(t, podA, listener, curlBlocked, "another session pod")
		expectCurl(t, podA, "http://169.254.169.254/latest/meta-data/", curlBlocked, "cloud metadata endpoint")
		expectCurl(t, podA, "https://1.1.1.1/", curlBlocked, "the internet")
	})
	t.Run("ingress", func(t *testing.T) {
		expectCurl(t, probe, listener, curlBlocked, "a pod in the namespace reaching a session")
	})
}

// expectCurl runs curl in a pod. want is an exit code, or curlBlocked for
// "must not connect", which accepts a drop or a refusal. Some destinations
// (metadata, internet) are unreachable in this cluster even without a policy,
// so only "did not connect" is meaningful there.
func expectCurl(t *testing.T, pod, target string, want int, what string) {
	t.Helper()
	code, out := curl(t, pod, target)
	if want == curlBlocked {
		if code != curlRefused && code != curlTimeout {
			t.Errorf("%s (%s): curl exit %d, want a dropped or refused connection: %s", what, target, code, strings.TrimSpace(out))
		}
		return
	}
	if code != want {
		t.Errorf("%s (%s): curl exit %d, want %d: %s", what, target, code, want, strings.TrimSpace(out))
	}
}

// waitReachable waits for a path that a control just unblocked to start
// working, covering the CNI's propagation of the changed policy.
func waitReachable(t *testing.T, pod, target, what string) {
	t.Helper()
	eventually(t, time.Minute, what, func() (bool, string) {
		code, out := curl(t, pod, target)
		if code == curlOK {
			return true, ""
		}
		return false, fmt.Sprintf("curl exit %d: %s", code, strings.TrimSpace(out))
	})
}

// waitBlocked waits for a connection that is currently allowed to become
// blocked again, covering the CNI's propagation of a restored policy.
func waitBlocked(t *testing.T, pod, target, what string) {
	t.Helper()
	eventually(t, time.Minute, what, func() (bool, string) {
		code, out := curl(t, pod, target)
		switch code {
		case curlOK:
			return false, "still reachable"
		case curlRefused, curlTimeout:
			return true, ""
		default:
			return false, fmt.Sprintf("curl exit %d: %s", code, strings.TrimSpace(out))
		}
	})
}

func curl(t *testing.T, pod, target string) (int, string) {
	t.Helper()
	return env.execIn(t, pod, "curl", "-ksS", "-o", "/dev/null", "-m", probeTimeoutSec, target)
}

// startListener serves HTTP inside a session pod and waits until the pod
// answers on its own loopback, so a later blocked probe cannot pass because
// nothing was listening.
func startListener(t *testing.T, pod string, port int) {
	t.Helper()
	script := fmt.Sprintf(
		`nohup node -e 'require("http").createServer((req,res)=>{res.end("ok")}).listen(%d,"0.0.0.0")' >/tmp/e2e-listener.log 2>&1 &`,
		port)
	if code, out := env.execIn(t, pod, "sh", "-c", script); code != 0 {
		t.Fatalf("start listener in %s: exit %d: %s", pod, code, strings.TrimSpace(out))
	}
	eventually(t, 30*time.Second, "listener in "+pod, func() (bool, string) {
		code, out := curl(t, pod, fmt.Sprintf("http://127.0.0.1:%d/", port))
		if code != curlOK {
			return false, fmt.Sprintf("curl exit %d: %s", code, strings.TrimSpace(out))
		}
		return true, ""
	})
}

// applyAllowPolicy creates one temporary NetworkPolicy that allows all traffic
// of policyType to or from a session's pod. Rules are additive, so this opens a
// path for a control without modifying or replacing the generated policy.
func applyAllowPolicy(t *testing.T, name, sessionID, policyType string) {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: %s
  namespace: %s
spec:
  podSelector:
    matchLabels:
      mayfly.io/session: %s
  policyTypes: [%s]
  %s:
    - {}
`, name, env.namespace, sessionID, policyType, strings.ToLower(policyType))
	if _, err := env.kubectlStdin(manifest, "apply", "-f", "-"); err != nil {
		t.Fatal(err)
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
