//go:build e2e

// Package e2e drives a deployed Mayfly release the way webhook senders,
// operators, and session pods do: over HTTP and kubectl, against real pods,
// NetworkPolicies, admission, and queue storage. hack/e2e.sh deploys the
// release under test; docs/testing.md describes the scenarios.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// env is the release under test, shared by every test in the package.
var env *cluster

func TestMain(m *testing.M) {
	c, err := connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: connect to the release under test:", err)
		os.Exit(1)
	}
	env = c
	code := m.Run()
	if code == 0 {
		if err := c.waitNoSessionObjects(90 * time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: leaked session objects:", err)
			code = 1
		}
	}
	c.stopPortForward()
	os.Exit(code)
}

// cluster is a connection to one Mayfly release.
type cluster struct {
	kubeContext string
	namespace   string
	release     string
	service     string
	fixedURL    string

	token         string
	webhookSecret string
	slackSecret   string
	agentImage    string

	http *http.Client

	mu   sync.Mutex
	base string
	pf   *exec.Cmd
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func connect() (*cluster, error) {
	c := &cluster{
		kubeContext: getenv("MAYFLY_E2E_KUBECONTEXT", ""),
		namespace:   getenv("MAYFLY_E2E_NAMESPACE", "mayfly-e2e"),
		release:     getenv("MAYFLY_E2E_RELEASE", "mayfly"),
		fixedURL:    strings.TrimRight(getenv("MAYFLY_E2E_URL", ""), "/"),
		http:        &http.Client{Timeout: 30 * time.Second},
	}
	c.service = getenv("MAYFLY_E2E_SERVICE", c.release)

	secretName := getenv("MAYFLY_E2E_SECRET", "mayfly-e2e-secrets")
	secret, err := c.secretData(secretName)
	if err != nil {
		return nil, err
	}
	c.token = getenv("MAYFLY_E2E_TOKEN", secret["MAYFLY_API_TOKEN"])
	c.webhookSecret = getenv("MAYFLY_E2E_WEBHOOK_SECRET", secret["MAYFLY_WEBHOOK_SECRET"])
	c.slackSecret = getenv("MAYFLY_E2E_SLACK_SECRET", secret["MAYFLY_SLACK_SECRET"])
	if c.token == "" || c.webhookSecret == "" || c.slackSecret == "" {
		return nil, fmt.Errorf("secret %s must hold MAYFLY_API_TOKEN, MAYFLY_WEBHOOK_SECRET, and MAYFLY_SLACK_SECRET", secretName)
	}

	raw, err := c.kubectl("get", "configmap", c.release+"-config", "-o", "jsonpath={.data.config\\.yaml}")
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Runtime struct {
			Image string `yaml:"image"`
		} `yaml:"runtime"`
	}
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("parse release config: %w", err)
	}
	c.agentImage = cfg.Runtime.Image
	if c.agentImage == "" {
		return nil, errors.New("release config has no runtime.image")
	}

	if c.fixedURL != "" {
		c.base = c.fixedURL
	} else if err := c.startPortForward(); err != nil {
		return nil, err
	}
	if err := c.waitReady(90 * time.Second); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *cluster) secretData(name string) (map[string]string, error) {
	raw, err := c.kubectl("get", "secret", name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range s.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("secret %s key %s: %w", name, k, err)
		}
		out[k] = string(b)
	}
	return out, nil
}

func (c *cluster) kubectlArgs(args ...string) []string {
	full := []string{}
	if c.kubeContext != "" {
		full = append(full, "--context", c.kubeContext)
	}
	full = append(full, "-n", c.namespace)
	return append(full, args...)
}

// kubectl runs kubectl against the release namespace and returns stdout.
func (c *cluster) kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", c.kubectlArgs(args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// execIn runs a command in a pod and returns its exit code and combined output.
// A failure to reach the pod at all fails the test; a non-zero exit does not.
func (c *cluster) execIn(t *testing.T, pod string, command ...string) (int, string) {
	t.Helper()
	args := c.kubectlArgs(append([]string{"exec", pod, "--"}, command...)...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("kubectl exec %s %v: %v: %s", pod, command, err, out)
	return -1, ""
}

var forwardingRE = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// startPortForward forwards a random local port to the control plane Service.
// It must be restarted whenever the control plane pod is replaced.
func (c *cluster) startPortForward() error {
	cmd := exec.Command("kubectl", c.kubectlArgs("port-forward", "svc/"+c.service, ":8080")...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start port-forward: %w", err)
	}
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		sent := false
		for scanner.Scan() {
			if m := forwardingRE.FindStringSubmatch(scanner.Text()); m != nil && !sent {
				found <- m[1]
				sent = true
			}
		}
	}()
	select {
	case port := <-found:
		c.mu.Lock()
		c.base = "http://127.0.0.1:" + port
		c.pf = cmd
		c.mu.Unlock()
		return nil
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errors.New("port-forward to the control plane did not start within 30s")
	}
}

func (c *cluster) stopPortForward() {
	c.mu.Lock()
	pf := c.pf
	c.pf = nil
	c.mu.Unlock()
	if pf != nil && pf.Process != nil {
		_ = pf.Process.Kill()
		_ = pf.Wait()
	}
}

func (c *cluster) url(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base + path
}

func (c *cluster) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := c.http.Get(c.url("/readyz"))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("control plane not ready after %s: %s", timeout, last)
}

// restartControlPlane replaces the control plane pod and reconnects. Session
// pods and queue storage are left alone, as on a real rollout or node drain.
func (c *cluster) restartControlPlane(t *testing.T) {
	t.Helper()
	c.restartDeployment(t, false)
}

// killControlPlane force-deletes the control plane pod, so an in-flight
// request cannot finish during graceful shutdown. The Deployment recreates it.
func (c *cluster) killControlPlane(t *testing.T) {
	t.Helper()
	c.restartDeployment(t, true)
}

func (c *cluster) restartDeployment(t *testing.T, force bool) {
	t.Helper()
	deploy := "deployment/" + c.release
	if force {
		out, err := c.kubectl("get", "pods",
			"-l", "app.kubernetes.io/instance="+c.release+",app.kubernetes.io/component=control-plane",
			"-o", "name")
		if err != nil {
			t.Fatal(err)
		}
		names := strings.Fields(out)
		if len(names) == 0 {
			t.Fatal("control-plane pod not found")
		}
		if _, err := c.kubectl("delete", "pod", strings.TrimPrefix(names[0], "pod/"), "--grace-period=0", "--force"); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := c.kubectl("rollout", "restart", deploy); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.kubectl("rollout", "status", deploy, "--timeout=180s"); err != nil {
		t.Fatal(err)
	}
	if c.fixedURL == "" {
		c.stopPortForward()
		if err := c.startPortForward(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.waitReady(90 * time.Second); err != nil {
		t.Fatal(err)
	}
}

// sessionObjects lists the Kubernetes objects Mayfly created for a session,
// or for every session when id is empty.
func (c *cluster) sessionObjects(id string) ([]string, error) {
	selector := "mayfly.io/managed=true"
	if id != "" {
		selector = "mayfly.io/session=" + id
	}
	out, err := c.kubectl("get", "pods,secrets,configmaps,networkpolicies,persistentvolumeclaims", "-l", selector, "-o", "name")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (c *cluster) waitNoSessionObjects(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		objs, err := c.sessionObjects("")
		if err == nil && len(objs) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("still present after %s: %s", timeout, strings.Join(objs, ", "))
		}
		time.Sleep(2 * time.Second)
	}
}

// forwardService forwards a random local port to a Service and returns the
// base URL plus a stop function. Tests use it to reach the deterministic
// stubs, which are not exposed through the control plane.
func (c *cluster) forwardService(t *testing.T, service string, remotePort int) (string, func()) {
	t.Helper()
	cmd := exec.Command("kubectl", c.kubectlArgs("port-forward", "svc/"+service, fmt.Sprintf(":%d", remotePort))...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start port-forward to %s: %v", service, err)
	}
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		sent := false
		for scanner.Scan() {
			if m := forwardingRE.FindStringSubmatch(scanner.Text()); m != nil && !sent {
				found <- m[1]
				sent = true
			}
		}
	}()
	select {
	case port := <-found:
		return "http://127.0.0.1:" + port, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("port-forward to %s did not start within 30s", service)
		return "", nil
	}
}

// podNames lists the pods currently labeled for a session.
func (c *cluster) podNames(t *testing.T, id string) []string {
	t.Helper()
	out, err := c.kubectl("get", "pods", "-l", "mayfly.io/session="+id, "-o", "name")
	if err != nil {
		return nil
	}
	names := []string{}
	for _, name := range strings.Fields(out) {
		names = append(names, strings.TrimPrefix(name, "pod/"))
	}
	return names
}
