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
	"path"
	"strings"
	"sync"
	"sync/atomic"
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

	// pluginImage and missingImage are the fixture plugin images of the
	// plugin tests; see resolvePluginImages.
	pluginImage  string
	missingImage string

	http *http.Client

	mu   sync.Mutex
	base string
	pf   *portForward

	// forwardMu serializes replacement of the port-forward without holding
	// mu, so reading base never waits for kubectl.
	forwardMu sync.Mutex
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// resolvePluginImages finds the fixture plugin images. hack/e2e.sh builds them,
// loads them into the cluster, and passes their names, which works on any
// cluster. Without those variables the suite builds and imports them into the
// k3d cluster itself, so running the package directly still works.
func (c *cluster) resolvePluginImages() error {
	c.pluginImage = getenv("MAYFLY_E2E_PLUGIN_IMAGE", "")
	c.missingImage = getenv("MAYFLY_E2E_PLUGIN_MISSING_IMAGE", "")
	if c.pluginImage != "" {
		if c.missingImage == "" {
			return errors.New("MAYFLY_E2E_PLUGIN_MISSING_IMAGE is required with MAYFLY_E2E_PLUGIN_IMAGE")
		}
		return nil
	}
	context := c.contextName()
	clusterName := strings.TrimPrefix(context, "k3d-")
	if clusterName == context {
		return fmt.Errorf("MAYFLY_E2E_PLUGIN_IMAGE is required against context %q; hack/e2e.sh sets it", context)
	}
	c.pluginImage = "mayfly-e2e-plugin:e2e"
	c.missingImage = "mayfly-e2e-plugin-missing:e2e"
	for _, image := range []struct{ stage, name string }{
		{"plugin", c.pluginImage},
		{"missing", c.missingImage},
	} {
		cmd := exec.Command("docker", "build", "--target", image.stage, "-t", image.name, "plugins")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build plugin image %s: %w: %s", image.name, err, strings.TrimSpace(string(out)))
		}
	}
	args := append([]string{"image", "import", "--cluster", clusterName}, c.pluginImage, c.missingImage)
	if out, err := exec.Command("k3d", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("import plugin images into k3d: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// contextName is the kubectl context the suite drives.
func (c *cluster) contextName() string {
	if c.kubeContext != "" {
		return c.kubeContext
	}
	out, err := exec.Command("kubectl", "config", "current-context").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// imageAllowed applies agents.allowed_images as the control plane does: an
// empty list allows every image, otherwise one path.Match pattern must match.
func imageAllowed(image string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, image); err == nil && ok {
			return true
		}
	}
	return false
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
		Agents struct {
			AllowedImages []string `yaml:"allowed_images"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("parse release config: %w", err)
	}
	c.agentImage = cfg.Runtime.Image
	if c.agentImage == "" {
		return nil, errors.New("release config has no runtime.image")
	}
	if err := c.resolvePluginImages(); err != nil {
		return nil, err
	}
	for _, image := range []string{c.pluginImage, c.missingImage} {
		if !imageAllowed(image, cfg.Agents.AllowedImages) {
			return nil, fmt.Errorf("plugin image %q is not in agents.allowed_images %v", image, cfg.Agents.AllowedImages)
		}
	}

	if c.fixedURL != "" {
		c.base = c.fixedURL
	} else if _, err := c.startPortForward(); err != nil {
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

// kubectlStdin runs kubectl against the release namespace with input on stdin.
func (c *cluster) kubectlStdin(input string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", c.kubectlArgs(args...)...)
	cmd.Stdin = strings.NewReader(input)
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

// portForward is one kubectl port-forward process. kubectl resolves a
// Service to a single pod when it starts and exits when that pod goes away,
// so the process exit is the signal that the local port is stale.
type portForward struct {
	base     string
	cmd      *exec.Cmd
	exited   chan struct{}
	stopped  atomic.Bool
	stopOnce sync.Once
}

func (p *portForward) alive() bool {
	if p.stopped.Load() {
		return false
	}
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

func (p *portForward) stop() {
	p.stopOnce.Do(func() {
		p.stopped.Store(true)
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-p.exited:
		case <-time.After(10 * time.Second):
			fmt.Fprintf(os.Stderr, "e2e: port-forward to %s did not exit after SIGKILL\n", p.base)
		}
	})
}

// startPortForward replaces any existing forward with a new one to the
// control plane Service. It must run after the control plane pod is replaced.
func (c *cluster) startPortForward() (string, error) {
	return c.startForward(60 * time.Second)
}

// startForward is startPortForward with the caller's deadline.
func (c *cluster) startForward(deadline time.Duration) (string, error) {
	p, err := c.forward("svc/"+c.service, 8080, deadline)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	old := c.pf
	c.pf = p
	c.base = p.base
	c.mu.Unlock()
	if old != nil && old != p {
		old.stop()
	}
	return p.base, nil
}

// liveForward returns a base URL whose port-forward is still alive, replacing
// a forward whose kubectl process exited. kubectl exits when the pod it
// resolved at startup is deleted, so a control-plane restart leaves the local
// port refusing connections until the forward is replaced.
func (c *cluster) liveForward(deadline time.Duration) (string, error) {
	if c.fixedURL != "" {
		return c.fixedURL, nil
	}
	c.forwardMu.Lock()
	defer c.forwardMu.Unlock()
	c.mu.Lock()
	pf := c.pf
	c.mu.Unlock()
	if pf != nil && pf.alive() {
		return pf.base, nil
	}
	return c.startForward(deadline)
}

// forward retries kubectl until it reports a local port or the deadline
// passes. kubectl resolves the Service to a ready pod once, at startup, and
// exits when it finds none, so a replacement control plane whose endpoint is
// not published yet is retried instead of failing the test.
func (c *cluster) forward(target string, remotePort int, deadline time.Duration) (*portForward, error) {
	var last error
	for end := time.Now().Add(deadline); ; {
		p, err := c.forwardOnce(target, remotePort)
		if err == nil {
			return p, nil
		}
		last = err
		if time.Now().After(end) {
			return nil, last
		}
		time.Sleep(2 * time.Second)
	}
}

// forwardOnce starts one kubectl port-forward and waits for the local port it
// reports, or for kubectl to exit.
func (c *cluster) forwardOnce(target string, remotePort int) (*portForward, error) {
	cmd := exec.Command("kubectl", c.kubectlArgs("port-forward", target, fmt.Sprintf(":%d", remotePort))...)
	// The read end of a pipe of our own, not of StdoutPipe: Wait closes a
	// StdoutPipe, and calling Wait while the scanner reads it is a race.
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = write
	var stderr bytes.Buffer
	// The stderr copy under E2E_ARTIFACTS makes a dropped call provable after
	// the run; the in-memory buffer still reports a forward that never
	// started. Capture cannot change the suite's outcome.
	logs := forwardLogFile(forwardLogSeq.Add(1))
	if logs != nil {
		cmd.Stderr = io.MultiWriter(&stderr, logs)
	} else {
		cmd.Stderr = &stderr
	}
	if err := cmd.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		if logs != nil {
			_ = logs.Close()
		}
		return nil, fmt.Errorf("start port-forward to %s: %w", target, err)
	}
	_ = write.Close()
	p := &portForward{cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		if logs != nil {
			_ = logs.Close()
		}
		close(p.exited)
		_ = read.Close()
	}()
	// Keep reading after the port is reported: kubectl blocks once its output
	// fills, so abandoning the stream could stall the forward later.
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(read)
		reported := false
		for scanner.Scan() {
			if reported {
				continue
			}
			if port, ok := forwardPort(scanner.Text()); ok {
				reported = true
				found <- port
			}
		}
		if !reported {
			close(found)
		}
	}()
	select {
	case port, ok := <-found:
		if !ok {
			p.stop()
			return nil, fmt.Errorf("port-forward to %s exited: %s", target, strings.TrimSpace(stderr.String()))
		}
		p.base = "http://127.0.0.1:" + port
		return p, nil
	case <-time.After(15 * time.Second):
		p.stop()
		return nil, fmt.Errorf("port-forward to %s did not start within 15s", target)
	}
}

func (c *cluster) stopPortForward() {
	c.mu.Lock()
	pf := c.pf
	c.pf = nil
	c.base = ""
	c.mu.Unlock()
	if pf != nil {
		pf.stop()
	}
}

// url returns an absolute URL for path. It replaces a port-forward whose
// kubectl process exited, so a test that fails after a restart cannot leave a
// stale forward behind for the next test.
func (c *cluster) url(path string) string {
	base, err := c.liveForward(60 * time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: reconnect the port-forward:", err)
		c.mu.Lock()
		base = c.base
		c.mu.Unlock()
	}
	return base + path
}

func (c *cluster) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	reported := false
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if remaining > 60*time.Second {
			remaining = 60 * time.Second
		}
		base, err := c.liveForward(remaining)
		if err != nil {
			last = "reconnect the port-forward: " + err.Error()
			time.Sleep(time.Second)
			continue
		}
		resp, err := c.http.Get(base + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = resp.Status
		} else {
			last = err.Error()
			if forwardBroken(err) && !reported {
				// kubectl keeps the local port open while the remote process
				// is not listening, so a refused request does not prove the
				// forward is stale. Report it once and keep retrying.
				fmt.Fprintf(os.Stderr, "e2e: /readyz through the port-forward: %v\n", err)
				reported = true
			}
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
		if _, err := c.startPortForward(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.waitReady(90 * time.Second); err != nil {
		t.Fatal(err)
	}
}

// sessionObjects lists the Kubernetes objects Mayfly created for a session,
// or for every session when id is empty. Service plugin objects carry
// mayfly.io/managed=true too, so the session app label keeps them out of the
// leak check: they outlive every session by design.
func (c *cluster) sessionObjects(id string) ([]string, error) {
	selector := "mayfly.io/managed=true,app.kubernetes.io/name=mayfly-agent"
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
	pf, err := c.forward("svc/"+service, remotePort, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return pf.base, pf.stop
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
