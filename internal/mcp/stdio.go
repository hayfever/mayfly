package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hayfever/mayfly/internal/sessioncfg"
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
	closed  atomic.Bool
	waitErr error
}

// newStdioTransport starts a server with the environment a stdio MCP server is
// allowed to see: PATH, HOME, TMPDIR, the locale, the TLS trust store, and the
// proxy settings of the runtime, plus env. Nothing else of os.Environ reaches
// the server.
func newStdioTransport(ctx context.Context, command string, args []string, env map[string]string) (*stdioTransport, error) {
	return newStdioTransportExact(ctx, command, args, sessioncfg.StdioEnv(env), "", nil)
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
		defer close(t.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			// Offer the line while the buffer has room. A select would race
			// t.done here, and a server that exits right after its answer
			// closes t.done before the waiting call reads that answer.
			select {
			case t.lines <- line:
				continue
			default:
			}
			// The buffer is full, so no call is reading. Wait for room, but
			// stop when the server is gone; Do drains what is buffered.
			select {
			case t.lines <- line:
			case <-t.done:
				return
			}
		}
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
	if t.closed.Load() {
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
	if t.closed.Load() {
		return fmt.Errorf("mcp stdio transport closed")
	}
	_, err := t.stdin.Write(append(payload, '\n'))
	return err
}

// Close ends the server: stdin first, then SIGTERM to the process group, then
// SIGKILL. Each step waits two seconds before the next.
//
// Close takes no call lock, so it also ends a call that is in flight: closing
// stdin and killing the process end the response stream, and the waiting call
// returns. A call lock here would make a shutdown wait for the tool's timeout.
func (t *stdioTransport) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = t.stdin.Close()

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
		return fmt.Errorf("mcp server exited (%w): %s", cause, stderr)
	}
	return fmt.Errorf("mcp server exited: %w", cause)
}
