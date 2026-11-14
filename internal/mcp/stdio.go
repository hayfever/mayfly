package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrNotSent reports a request that never reached the server process, so the
// server cannot have acted on it.
var ErrNotSent = errors.New("mcp: request not sent")

type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	stderr  *tailBuffer
	done    chan struct{}
	mu      sync.Mutex
	closed  bool
	waitErr error
}

func newStdioTransport(ctx context.Context, command string, args []string, env map[string]string) (*stdioTransport, error) {
	return newStdioTransportExact(ctx, command, args, mergeEnv(env), "", nil)
}

// newStdioTransportExact starts the server with a complete environment.
// stderr, when not nil, receives the same bytes as the internal tail buffer.
func newStdioTransportExact(_ context.Context, command string, args, env []string, dir string, stderr io.Writer) (*stdioTransport, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = append([]string(nil), env...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	t := &stdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		lines:  make(chan []byte, 64),
		stderr: newTailBuffer(8192),
		done:   make(chan struct{}),
	}
	if stderr == nil {
		cmd.Stderr = t.stderr
	} else {
		cmd.Stderr = io.MultiWriter(t.stderr, stderr)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case t.lines <- line:
			case <-t.done:
				return
			}
		}
		close(t.lines)
	}()
	go func() {
		t.waitErr = cmd.Wait()
		close(t.done)
	}()
	return t, nil
}

// Exited reports the end of the server process.
func (t *stdioTransport) Exited() <-chan struct{} { return t.done }

func (t *stdioTransport) Do(ctx context.Context, id int64, payload []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, fmt.Errorf("mcp stdio transport closed")
	}
	if _, err := t.stdin.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSent, t.exitError(err))
	}
	for {
		select {
		case line, ok := <-t.lines:
			if !ok {
				return nil, t.exitError(io.EOF)
			}
			var resp rpcResponse
			if err := json.Unmarshal(line, &resp); err != nil {
				continue
			}
			if len(resp.ID) > 0 && matchesID(resp.ID, id) {
				return line, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (t *stdioTransport) Notify(_ context.Context, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fmt.Errorf("mcp stdio transport closed")
	}
	_, err := t.stdin.Write(append(payload, '\n'))
	return err
}

// Close ends the server: stdin first, then SIGTERM to the process group, then
// SIGKILL. Each step waits two seconds before the next.
func (t *stdioTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	_ = t.stdin.Close()
	t.mu.Unlock()

	if waitDone(t.done, 2*time.Second) {
		return nil
	}
	if t.cmd.Process != nil {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
	}
	if waitDone(t.done, 2*time.Second) {
		return nil
	}
	if t.cmd.Process != nil {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	}
	<-t.done
	return nil
}

// waitDone reports whether done closed within d.
func waitDone(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func (t *stdioTransport) exitError(cause error) error {
	stderr := strings.TrimSpace(t.stderr.String())
	if stderr != "" {
		return fmt.Errorf("mcp server exited (%v): %s", cause, stderr)
	}
	return fmt.Errorf("mcp server exited: %w", cause)
}

func mergeEnv(extra map[string]string) []string {
	merged := map[string]string{}
	order := []string{}
	add := func(k, v string) {
		if _, ok := merged[k]; !ok {
			order = append(order, k)
		}
		merged[k] = v
	}
	for _, entry := range os.Environ() {
		k, v, _ := strings.Cut(entry, "=")
		add(k, v)
	}
	for k, v := range extra {
		add(k, v)
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out
}
