package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// target is a reachable control-plane API, one port-forward or direct URL.
type target struct {
	base string
	// lost receives one error when the port-forward exits before stop. It is
	// nil for a direct URL.
	lost <-chan error
	stop func()
}

var forwardingRE = regexp.MustCompile(`(?:127\.0\.0\.1|\[::1\]):(\d+) -> \d+`)

// forwardPort returns the local port in a kubectl port-forward line.
func forwardPort(line string) (string, bool) {
	m := forwardingRE.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// kubectlArgs builds the port-forward command. Global flags come before the
// verb, as kubectl requires.
func kubectlArgs(opts options) []string {
	args := make([]string, 0, 10)
	if opts.kubeconfig != "" {
		args = append(args, "--kubeconfig", opts.kubeconfig)
	}
	if opts.kubeContext != "" {
		args = append(args, "--context", opts.kubeContext)
	}
	args = append(args,
		"--namespace", opts.namespace,
		"port-forward", "svc/"+opts.service, ":8080",
	)
	return args
}

// openTarget resolves the API endpoint. A direct URL needs no cluster access;
// otherwise kubectl port-forward publishes the control-plane Service on a
// local port. The returned target is healthy before openTarget returns.
func openTarget(ctx context.Context, opts options) (*target, error) {
	if opts.baseURL != "" {
		base := strings.TrimRight(opts.baseURL, "/")
		if err := waitHealthy(ctx, base, 15*time.Second); err != nil {
			return nil, err
		}
		return &target{base: base, stop: func() {}}, nil
	}
	fwd, err := startPortForward(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := waitHealthy(ctx, fwd.base, 60*time.Second); err != nil {
		fwd.stop()
		return nil, err
	}
	return &target{base: fwd.base, lost: fwd.lost, stop: fwd.stop}, nil
}

// portForward is one kubectl port-forward process. kubectl resolves the
// Service to a pod at startup and exits when that pod is gone, so process exit
// means the measured endpoint moved.
type portForward struct {
	base     string
	cmd      *exec.Cmd
	exited   chan struct{}
	lost     chan error
	stderr   *bytes.Buffer
	stopOnce sync.Once
}

// startPortForward starts kubectl and waits for the local port it reports.
func startPortForward(ctx context.Context, opts options) (*portForward, error) {
	cmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(opts)...)
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = write
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, fmt.Errorf("start kubectl port-forward: %w", err)
	}
	_ = write.Close()
	fwd := &portForward{
		cmd:    cmd,
		exited: make(chan struct{}),
		lost:   make(chan error, 1),
		stderr: stderr,
	}
	go func() {
		_ = cmd.Wait()
		close(fwd.exited)
		_ = read.Close()
		select {
		case fwd.lost <- fmt.Errorf("port-forward to svc/%s exited: %s", opts.service, strings.TrimSpace(stderr.String())):
		default:
		}
	}()
	// Keep reading after the port is reported so kubectl cannot block on a
	// full stdout pipe.
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
			fwd.stop()
			return nil, fmt.Errorf("port-forward to svc/%s exited: %s", opts.service, strings.TrimSpace(stderr.String()))
		}
		fwd.base = "http://127.0.0.1:" + port
		return fwd, nil
	case <-ctx.Done():
		fwd.stop()
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		fwd.stop()
		return nil, fmt.Errorf("port-forward to svc/%s did not report a local port within 30s", opts.service)
	}
}

func (f *portForward) stop() {
	f.stopOnce.Do(func() {
		if f.cmd.Process != nil {
			_ = f.cmd.Process.Kill()
		}
		select {
		case <-f.exited:
		case <-time.After(10 * time.Second):
			fmt.Fprintf(os.Stderr, "loadtest: port-forward to %s did not exit after SIGKILL\n", f.base)
		}
	})
}

// waitHealthy polls /healthz until the control plane answers or the deadline
// passes. The probes need no token.
func waitHealthy(ctx context.Context, base string, timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("/healthz returned %d", resp.StatusCode)
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			if last == nil {
				last = errors.New("deadline passed")
			}
			return fmt.Errorf("control plane at %s is not healthy: %w", base, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
